package server

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// metrics are served in the Prometheus text format at /metrics on the admin
// port. They are kept by hand, without a client library: a few counters and
// one histogram are all FirebirdREST needs.
type metrics struct {
	start time.Time

	mu       sync.Mutex
	requests map[reqKey]uint64     // by method and status
	duration map[string]*histogram // by method
	errors   map[string]uint64     // error responses by code

	reloads       atomic.Uint64
	reloadErrors  atomic.Uint64
	lastLoadNanos atomic.Int64 // how long the last successful schema load took
	lastLoadAt    atomic.Int64 // unix seconds of the last successful load

	jwtCacheHits   atomic.Uint64
	jwtCacheMisses atomic.Uint64
	jwksFetches    atomic.Uint64
	jwksErrors     atomic.Uint64

	eventStreams   atomic.Int64  // open /events streams
	eventDelivered atomic.Uint64 // notifications written to streams
	eventErrors    atomic.Uint64 // times the subscription to the database was lost
	eventsMu       sync.Mutex
	events         map[string]uint64 // notifications received from the database, by event
}

// eventReceived counts a notification from the database. The label set is
// the configured event names, so it stays bounded.
func (m *metrics) eventReceived(name string) {
	m.eventsMu.Lock()
	if m.events == nil {
		m.events = map[string]uint64{}
	}
	m.events[name]++
	m.eventsMu.Unlock()
}

type reqKey struct {
	method string
	status int
}

// buckets are the request duration histogram bounds, in seconds.
var buckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type histogram struct {
	counts []uint64 // per bucket, not cumulative; the last one is +Inf
	sum    float64
	n      uint64
}

func newMetrics() *metrics {
	return &metrics{start: time.Now(), requests: map[reqKey]uint64{},
		duration: map[string]*histogram{}, errors: map[string]uint64{}}
}

// metricMethod keeps the method label to a fixed set, so arbitrary methods
// sent by clients can't create new series.
func metricMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	}
	return "OTHER"
}

func (m *metrics) request(method string, status int, d time.Duration, errCode string) {
	method = metricMethod(method)
	sec := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[reqKey{method, status}]++
	h := m.duration[method]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(buckets)+1)}
		m.duration[method] = h
	}
	i := sort.SearchFloat64s(buckets, sec) // first bound >= sec
	h.counts[i]++
	h.sum += sec
	h.n++
	if errCode != "" {
		m.errors[errCode]++
	}
}

// Metrics serves /metrics.
func (s *Server) Metrics() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.writeMetrics(w)
	})
}

func (s *Server) writeMetrics(out io.Writer) {
	m := s.m
	var b strings.Builder
	head := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	num := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

	cat := s.catalog.Load()
	fbVersion := ""
	if cat != nil {
		fbVersion = cat.Version
	}
	head("firebirdrest_build_info", "gauge", "Version of FirebirdREST and of the Firebird server.")
	fmt.Fprintf(&b, "firebirdrest_build_info{version=%s,firebird=%s} 1\n", lv(Version), lv(fbVersion))
	head("firebirdrest_start_time_seconds", "gauge", "Start time of the process, in unix seconds.")
	fmt.Fprintf(&b, "firebirdrest_start_time_seconds %d\n", m.start.Unix())

	// requests
	m.mu.Lock()
	keys := make([]reqKey, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		return keys[i].status < keys[j].status
	})
	head("firebirdrest_requests_total", "counter", "HTTP requests served, by method and status.")
	for _, k := range keys {
		fmt.Fprintf(&b, "firebirdrest_requests_total{method=%s,status=\"%d\"} %d\n", lv(k.method), k.status, m.requests[k])
	}
	methods := make([]string, 0, len(m.duration))
	for k := range m.duration {
		methods = append(methods, k)
	}
	sort.Strings(methods)
	head("firebirdrest_request_duration_seconds", "histogram", "Time to serve a request, by method.")
	for _, meth := range methods {
		h := m.duration[meth]
		var cum uint64
		for i, le := range buckets {
			cum += h.counts[i]
			fmt.Fprintf(&b, "firebirdrest_request_duration_seconds_bucket{method=%s,le=%s} %d\n", lv(meth), lv(num(le)), cum)
		}
		cum += h.counts[len(buckets)]
		fmt.Fprintf(&b, "firebirdrest_request_duration_seconds_bucket{method=%s,le=\"+Inf\"} %d\n", lv(meth), cum)
		fmt.Fprintf(&b, "firebirdrest_request_duration_seconds_sum{method=%s} %s\n", lv(meth), num(h.sum))
		fmt.Fprintf(&b, "firebirdrest_request_duration_seconds_count{method=%s} %d\n", lv(meth), h.n)
	}
	codes := make([]string, 0, len(m.errors))
	for k := range m.errors {
		codes = append(codes, k)
	}
	sort.Strings(codes)
	head("firebirdrest_errors_total", "counter", "Error responses, by error code (FBRST... or the Firebird SQLSTATE).")
	for _, c := range codes {
		fmt.Fprintf(&b, "firebirdrest_errors_total{code=%s} %d\n", lv(c), m.errors[c])
	}
	m.mu.Unlock()

	// connection pools and statement caches, per Firebird role
	type poolRow struct {
		role string
		rp   *rolePool
	}
	s.mu.Lock()
	var pools []poolRow
	if s.basep != nil {
		pools = append(pools, poolRow{"", s.basep})
	}
	for role, rp := range s.pools {
		pools = append(pools, poolRow{role, rp})
	}
	s.mu.Unlock()
	sort.Slice(pools, func(i, j int) bool { return pools[i].role < pools[j].role })
	type stat struct {
		name, typ, help string
		val             func(poolRow) (float64, bool)
	}
	stats := []stat{
		{"firebirdrest_db_pool_max_connections", "gauge", "Maximum open connections of the pool (db-pool).",
			func(p poolRow) (float64, bool) { return float64(p.rp.db.Stats().MaxOpenConnections), true }},
		{"firebirdrest_db_pool_open_connections", "gauge", "Open connections, in use or idle.",
			func(p poolRow) (float64, bool) { return float64(p.rp.db.Stats().OpenConnections), true }},
		{"firebirdrest_db_pool_in_use_connections", "gauge", "Connections in use.",
			func(p poolRow) (float64, bool) { return float64(p.rp.db.Stats().InUse), true }},
		{"firebirdrest_db_pool_idle_connections", "gauge", "Idle connections.",
			func(p poolRow) (float64, bool) { return float64(p.rp.db.Stats().Idle), true }},
		{"firebirdrest_db_pool_waits_total", "counter", "Times a request waited for a free connection.",
			func(p poolRow) (float64, bool) { return float64(p.rp.db.Stats().WaitCount), true }},
		{"firebirdrest_db_pool_wait_seconds_total", "counter", "Total time spent waiting for a free connection.",
			func(p poolRow) (float64, bool) { return p.rp.db.Stats().WaitDuration.Seconds(), true }},
		{"firebirdrest_stmt_cache_hits_total", "counter", "Statements run from the prepared statement cache.",
			func(p poolRow) (float64, bool) {
				if p.rp.cache == nil {
					return 0, false
				}
				return float64(p.rp.cache.hits.Load()), true
			}},
		{"firebirdrest_stmt_cache_misses_total", "counter", "Statements not in the prepared statement cache.",
			func(p poolRow) (float64, bool) {
				if p.rp.cache == nil {
					return 0, false
				}
				return float64(p.rp.cache.misses.Load()), true
			}},
		{"firebirdrest_stmt_cache_entries", "gauge", "Prepared statements in the cache.",
			func(p poolRow) (float64, bool) {
				if p.rp.cache == nil {
					return 0, false
				}
				return float64(p.rp.cache.len()), true
			}},
	}
	for _, st := range stats {
		head(st.name, st.typ, st.help+" The role label is empty for the connection without a role, used for the schema cache.")
		for _, p := range pools {
			if v, ok := st.val(p); ok {
				fmt.Fprintf(&b, "%s{role=%s} %s\n", st.name, lv(p.role), num(v))
			}
		}
	}

	// schema cache
	head("firebirdrest_schema_reloads_total", "counter", "Schema cache loads, including the first one.")
	fmt.Fprintf(&b, "firebirdrest_schema_reloads_total %d\n", m.reloads.Load())
	head("firebirdrest_schema_reload_errors_total", "counter", "Failed schema cache loads.")
	fmt.Fprintf(&b, "firebirdrest_schema_reload_errors_total %d\n", m.reloadErrors.Load())
	head("firebirdrest_schema_load_duration_seconds", "gauge", "Time the last successful schema cache load took.")
	fmt.Fprintf(&b, "firebirdrest_schema_load_duration_seconds %s\n", num(time.Duration(m.lastLoadNanos.Load()).Seconds()))
	head("firebirdrest_schema_load_time_seconds", "gauge", "Time of the last successful schema cache load, in unix seconds.")
	fmt.Fprintf(&b, "firebirdrest_schema_load_time_seconds %d\n", m.lastLoadAt.Load())
	if cat != nil {
		head("firebirdrest_schema_relations", "gauge", "Tables and views in the schema cache, by SQL schema.")
		for _, sc := range cat.Schemas {
			fmt.Fprintf(&b, "firebirdrest_schema_relations{schema=%s} %d\n", lv(sc.Name), len(sc.Relations))
		}
		head("firebirdrest_schema_routines", "gauge", "Procedures and functions in the schema cache, by SQL schema.")
		for _, sc := range cat.Schemas {
			fmt.Fprintf(&b, "firebirdrest_schema_routines{schema=%s} %d\n", lv(sc.Name), len(sc.Routines))
		}
	}

	// JWT
	if t := s.tracer; t != nil {
		head("firebirdrest_trace_spans_exported_total", "counter", "Spans exported to the OTLP endpoint.")
		fmt.Fprintf(&b, "firebirdrest_trace_spans_exported_total %d\n", t.exported.Load())
		head("firebirdrest_trace_spans_dropped_total", "counter", "Spans dropped because the export queue was full.")
		fmt.Fprintf(&b, "firebirdrest_trace_spans_dropped_total %d\n", t.dropped.Load())
		head("firebirdrest_trace_export_errors_total", "counter", "Failed exports to the OTLP endpoint.")
		fmt.Fprintf(&b, "firebirdrest_trace_export_errors_total %d\n", t.failures.Load())
	}
	if s.events != nil {
		head("firebirdrest_event_streams", "gauge", "Open /events streams.")
		fmt.Fprintf(&b, "firebirdrest_event_streams %d\n", m.eventStreams.Load())
		head("firebirdrest_events_received_total", "counter", "Event notifications received from the database, by event.")
		m.eventsMu.Lock()
		names := make([]string, 0, len(m.events))
		for n := range m.events {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&b, "firebirdrest_events_received_total{event=%s} %d\n", lv(n), m.events[n])
		}
		m.eventsMu.Unlock()
		head("firebirdrest_events_delivered_total", "counter", "Event notifications written to streams.")
		fmt.Fprintf(&b, "firebirdrest_events_delivered_total %d\n", m.eventDelivered.Load())
		head("firebirdrest_event_subscription_errors_total", "counter", "Times the event subscription to the database was lost and renewed.")
		fmt.Fprintf(&b, "firebirdrest_event_subscription_errors_total %d\n", m.eventErrors.Load())
	}
	head("firebirdrest_jwt_cache_hits_total", "counter", "Tokens found in the JWT validation cache.")
	fmt.Fprintf(&b, "firebirdrest_jwt_cache_hits_total %d\n", m.jwtCacheHits.Load())
	head("firebirdrest_jwt_cache_misses_total", "counter", "Tokens validated because they were not in the cache.")
	fmt.Fprintf(&b, "firebirdrest_jwt_cache_misses_total %d\n", m.jwtCacheMisses.Load())
	if s.cfg.JWTJWKSURL != "" {
		head("firebirdrest_jwks_fetches_total", "counter", "Fetches of jwt-jwks-url.")
		fmt.Fprintf(&b, "firebirdrest_jwks_fetches_total %d\n", m.jwksFetches.Load())
		head("firebirdrest_jwks_fetch_errors_total", "counter", "Failed fetches of jwt-jwks-url.")
		fmt.Fprintf(&b, "firebirdrest_jwks_fetch_errors_total %d\n", m.jwksErrors.Load())
	}

	// Go runtime
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	head("go_goroutines", "gauge", "Number of goroutines.")
	fmt.Fprintf(&b, "go_goroutines %d\n", runtime.NumGoroutine())
	head("go_memstats_heap_alloc_bytes", "gauge", "Bytes of allocated heap objects.")
	fmt.Fprintf(&b, "go_memstats_heap_alloc_bytes %d\n", ms.HeapAlloc)
	head("go_memstats_sys_bytes", "gauge", "Bytes of memory obtained from the operating system.")
	fmt.Fprintf(&b, "go_memstats_sys_bytes %d\n", ms.Sys)
	head("go_gc_cycles_total", "counter", "Completed garbage collection cycles.")
	fmt.Fprintf(&b, "go_gc_cycles_total %d\n", ms.NumGC)

	io.WriteString(out, b.String())
}

// lv quotes a label value as the Prometheus text format wants it.
func lv(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) + `"`
}
