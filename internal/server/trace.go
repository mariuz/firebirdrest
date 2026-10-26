package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariuz/firebirdrest/internal/query"
)

// Traces: with otel-traces-endpoint set, every request becomes an
// OpenTelemetry trace exported over OTLP/HTTP (JSON encoding), so it shows
// up next to the traces of the services that call FirebirdREST in Jaeger,
// Tempo, Honeycomb or any OpenTelemetry collector. Like the metrics, it is
// written by hand: the OpenTelemetry SDK would bring a newer Go toolchain
// and gRPC for what is a few structs and one POST.
//
// A request's span has these children:
//
//	jwt                 verifying the bearer token (when one is sent)
//	transaction         BEGIN to COMMIT/ROLLBACK, or the reused read transaction's use
//	  <operation> <table>  one per SQL statement, from its execution until the
//	                       next statement starts or the transaction ends (so it
//	                       includes fetching its rows)
//
// An incoming W3C traceparent header makes the request part of the caller's
// trace and its sampled flag decides; without one, otel-traces-sample-ratio
// of the requests are traced. Statement spans carry the SQL text, which has
// placeholders, never values; spans never carry the query string or bodies.

type traceID [16]byte
type spanID [8]byte

type span struct {
	t       *tracer
	trace   traceID
	id      spanID
	parent  spanID // zero for a root
	name    string
	kind    int // OTLP: 1 internal, 2 server, 3 client
	start   time.Time
	end     time.Time
	mu      sync.Mutex
	attrs   []attr
	errMsg  string // status error when set
	isError bool
}

type attr struct {
	key string
	val any // string, int64, bool or float64
}

type spanCtxKey struct{}

// spanFrom returns the request's span, or nil when it isn't traced.
func spanFrom(ctx context.Context) *span {
	sp, _ := ctx.Value(spanCtxKey{}).(*span)
	return sp
}

// child starts a span under sp; nil when sp is nil, so callers need no checks.
func (sp *span) child(name string, kind int, start time.Time) *span {
	if sp == nil {
		return nil
	}
	c := &span{t: sp.t, trace: sp.trace, parent: sp.id, name: name, kind: kind, start: start}
	c.id = newSpanID()
	return c
}

func (sp *span) set(key string, val any) {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	for i := range sp.attrs {
		if sp.attrs[i].key == key {
			sp.attrs[i].val = val
			sp.mu.Unlock()
			return
		}
	}
	sp.attrs = append(sp.attrs, attr{key, val})
	sp.mu.Unlock()
}

func (sp *span) setName(name string) {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	sp.name = name
	sp.mu.Unlock()
}

func (sp *span) fail(msg string) {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	sp.isError, sp.errMsg = true, msg
	sp.mu.Unlock()
}

// finish ends the span at t and queues it for export.
func (sp *span) finish(t time.Time) {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	if !sp.end.IsZero() {
		sp.mu.Unlock()
		return
	}
	sp.end = t
	sp.mu.Unlock()
	sp.t.queue(sp)
}

// traceparent returns the W3C header value that names sp as the parent.
func (sp *span) traceparent() string {
	return "00-" + hex.EncodeToString(sp.trace[:]) + "-" + hex.EncodeToString(sp.id[:]) + "-01"
}

func newSpanID() spanID {
	var id spanID
	rand.Read(id[:])
	return id
}

// stmtSpans times the statements of one transaction: a statement's span
// runs until the next one starts or the transaction ends.
type stmtSpans struct {
	parent *span
	open   *span
}

func (s *stmtSpans) start(sqlText string) {
	if s == nil || s.parent == nil {
		return
	}
	now := time.Now()
	s.open.finish(now)
	op, table := sqlSummary(sqlText)
	name := op
	if table != "" {
		name += " " + table
	}
	sp := s.parent.child(name, 3, now)
	sp.set("db.system.name", "firebirdsql")
	sp.set("db.operation.name", op)
	if table != "" {
		sp.set("db.collection.name", table)
	}
	sp.set("db.query.text", sqlText)
	s.open = sp
}

func (s *stmtSpans) end(t time.Time) {
	if s == nil {
		return
	}
	s.open.finish(t)
	s.open = nil
}

// sqlSummary returns a statement's operation (SELECT, INSERT...) and its
// main table, unquoted, for span names: low-cardinality, no values.
func sqlSummary(q string) (op, table string) {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return "SQL", ""
	}
	op = strings.ToUpper(fields[0])
	if op == "WITH" {
		return "SELECT", "" // a CTE: its tables are inside
	}
	after := map[string]string{"SELECT": "FROM", "DELETE": "FROM", "INSERT": "INTO", "UPDATE": "", "MERGE": "INTO", "UPDATE OR INSERT": "INTO"}
	kw, ok := after[op]
	if !ok {
		if op == "EXECUTE" && len(fields) > 1 {
			op = "EXECUTE " + strings.ToUpper(fields[1])
		}
		return op, ""
	}
	for i, f := range fields {
		if i == 0 && kw != "" {
			continue
		}
		if kw == "" || strings.EqualFold(f, kw) {
			j := i + 1
			if kw == "" {
				j = 1
			}
			if j < len(fields) {
				// "BOOKS", "PUBLIC"."BOOKS" or a procedure call "PROC"(?, ?)
				name, _, _ := strings.Cut(fields[j], "(")
				name = strings.TrimRight(strings.ReplaceAll(name, `"`, ""), ",)")
				if name != "" {
					return op, name
				}
			}
			return op, ""
		}
	}
	return op, ""
}

// parseTraceparent reads a W3C traceparent header. ok is false when it is
// absent or malformed (the request then starts a trace of its own).
func parseTraceparent(h string) (tr traceID, parent spanID, sampled, ok bool) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) < 4 || len(parts[0]) != 2 || parts[0] == "ff" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return tr, parent, false, false
	}
	if parts[0] == "00" && len(parts) != 4 {
		return tr, parent, false, false
	}
	if _, err := hex.Decode(tr[:], []byte(parts[1])); err != nil || tr == (traceID{}) {
		return tr, parent, false, false
	}
	if _, err := hex.Decode(parent[:], []byte(parts[2])); err != nil || parent == (spanID{}) {
		return tr, parent, false, false
	}
	flags, err := strconv.ParseUint(parts[3], 16, 8)
	if err != nil {
		return tr, parent, false, false
	}
	return tr, parent, flags&1 == 1, true
}

// tracer batches finished spans and posts them to the OTLP endpoint.
type tracer struct {
	endpoint string
	headers  http.Header
	service  string
	ratio    float64
	log      *slog.Logger
	client   *http.Client

	ch      chan *span
	done    chan struct{}
	stopped chan struct{}

	exported atomic.Uint64
	dropped  atomic.Uint64
	failures atomic.Uint64
}

const (
	traceQueue = 4096
	traceBatch = 512
)

// traceInterval is how often queued spans are exported (a variable for tests).
var traceInterval = 5 * time.Second

func newTracer(endpoint string, headers http.Header, service string, ratio float64, log *slog.Logger) *tracer {
	t := &tracer{endpoint: endpoint, headers: headers, service: service, ratio: ratio, log: log,
		client: &http.Client{Timeout: 10 * time.Second},
		ch:     make(chan *span, traceQueue), done: make(chan struct{}), stopped: make(chan struct{})}
	go t.run()
	return t
}

// startRequest returns the server span for a request, or nil when the
// request isn't sampled.
func (t *tracer) startRequest(r *http.Request, start time.Time) *span {
	if t == nil {
		return nil
	}
	sp := &span{t: t, kind: 2, start: start, name: r.Method}
	if tr, parent, sampled, ok := parseTraceparent(r.Header.Get("traceparent")); ok {
		if !sampled {
			return nil
		}
		sp.trace, sp.parent = tr, parent
	} else {
		if t.ratio < 1 && randFloat() >= t.ratio {
			return nil
		}
		rand.Read(sp.trace[:])
	}
	sp.id = newSpanID()
	sp.set("http.request.method", r.Method)
	sp.set("url.path", r.URL.Path)
	sp.set("url.scheme", "http")
	if ua := r.UserAgent(); ua != "" {
		sp.set("user_agent.original", ua)
	}
	return sp
}

func randFloat() float64 {
	var b [8]byte
	rand.Read(b[:])
	return float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
}

func (t *tracer) queue(sp *span) {
	select {
	case t.ch <- sp:
	default:
		t.dropped.Add(1)
	}
}

func (t *tracer) run() {
	defer close(t.stopped)
	tick := time.NewTicker(traceInterval)
	defer tick.Stop()
	var batch []*span
	flush := func() {
		if len(batch) > 0 {
			t.export(batch)
			batch = nil
		}
	}
	for {
		select {
		case sp := <-t.ch:
			batch = append(batch, sp)
			if len(batch) >= traceBatch {
				flush()
			}
		case <-tick.C:
			flush()
		case <-t.done:
			for {
				select {
				case sp := <-t.ch:
					batch = append(batch, sp)
				default:
					flush()
					return
				}
			}
		}
	}
}

// close exports what is queued and stops.
func (t *tracer) close() {
	if t == nil {
		return
	}
	close(t.done)
	<-t.stopped
}

func (t *tracer) export(spans []*span) {
	body := t.encode(spans)
	req, err := http.NewRequest(http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		t.failures.Add(1)
		return
	}
	for k, v := range t.headers {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		t.failures.Add(1)
		t.log.Warn("exporting traces failed", "endpoint", t.endpoint, "spans", len(spans), "err", err)
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.failures.Add(1)
		t.log.Warn("exporting traces failed", "endpoint", t.endpoint, "spans", len(spans), "status", resp.StatusCode)
		return
	}
	t.exported.Add(uint64(len(spans)))
}

// encode writes an OTLP ExportTraceServiceRequest in the protobuf JSON
// mapping: ids as hex, 64-bit integers as strings, enums as numbers.
func (t *tracer) encode(spans []*span) []byte {
	var b []byte
	str := func(s string) { b = query.AppendJSON(b, s) }
	kv := func(key string, val any) {
		b = append(b, `{"key":`...)
		str(key)
		b = append(b, `,"value":{`...)
		switch v := val.(type) {
		case string:
			b = append(b, `"stringValue":`...)
			str(v)
		case int64:
			b = append(b, `"intValue":"`...)
			b = strconv.AppendInt(b, v, 10)
			b = append(b, '"')
		case int:
			b = append(b, `"intValue":"`...)
			b = strconv.AppendInt(b, int64(v), 10)
			b = append(b, '"')
		case bool:
			b = append(b, `"boolValue":`...)
			b = strconv.AppendBool(b, v)
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				v = 0
			}
			b = append(b, `"doubleValue":`...)
			b = strconv.AppendFloat(b, v, 'g', -1, 64)
		default:
			b = append(b, `"stringValue":`...)
			str(fmt.Sprint(v))
		}
		b = append(b, `}}`...)
	}
	b = append(b, `{"resourceSpans":[{"resource":{"attributes":[`...)
	kv("service.name", t.service)
	b = append(b, ',')
	kv("service.version", Version)
	b = append(b, ',')
	kv("telemetry.sdk.name", "firebirdrest")
	b = append(b, `]},"scopeSpans":[{"scope":{"name":"github.com/mariuz/firebirdrest","version":`...)
	str(Version)
	b = append(b, `},"spans":[`...)
	for i, sp := range spans {
		if i > 0 {
			b = append(b, ',')
		}
		sp.mu.Lock()
		b = append(b, `{"traceId":"`...)
		b = hex.AppendEncode(b, sp.trace[:])
		b = append(b, `","spanId":"`...)
		b = hex.AppendEncode(b, sp.id[:])
		b = append(b, '"')
		if sp.parent != (spanID{}) {
			b = append(b, `,"parentSpanId":"`...)
			b = hex.AppendEncode(b, sp.parent[:])
			b = append(b, '"')
		}
		b = append(b, `,"name":`...)
		str(sp.name)
		b = append(b, `,"kind":`...)
		b = strconv.AppendInt(b, int64(sp.kind), 10)
		b = append(b, `,"startTimeUnixNano":"`...)
		b = strconv.AppendInt(b, sp.start.UnixNano(), 10)
		b = append(b, `","endTimeUnixNano":"`...)
		b = strconv.AppendInt(b, sp.end.UnixNano(), 10)
		b = append(b, `","attributes":[`...)
		for j, a := range sp.attrs {
			if j > 0 {
				b = append(b, ',')
			}
			kv(a.key, a.val)
		}
		b = append(b, ']')
		if sp.isError {
			b = append(b, `,"status":{"code":2,"message":`...)
			str(sp.errMsg)
			b = append(b, '}')
		}
		b = append(b, '}')
		sp.mu.Unlock()
	}
	b = append(b, `]}]}]}`...)
	return b
}

// parseOTLPHeaders reads OTEL_EXPORTER_OTLP_HEADERS: "k1=v1,k2=v2", values
// URL-encoded.
func parseOTLPHeaders(s string) http.Header {
	h := http.Header{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		if dv, err := url.QueryUnescape(strings.TrimSpace(v)); err == nil {
			v = dv
		}
		h.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	return h
}
