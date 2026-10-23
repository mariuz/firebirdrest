package server

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
)

func TestTimingHeader(t *testing.T) {
	tm := timing{jwt: 1500 * time.Microsecond, parse: 200 * time.Microsecond,
		response: 300 * time.Microsecond, tx: 5 * time.Millisecond}
	want := "jwt;dur=1.5, parse;dur=0.2, transaction;dur=4.5, response;dur=0.3"
	if got := tm.header(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := (&timing{}).header(); got != "jwt;dur=0.0, parse;dur=0.0, response;dur=0.0" {
		t.Errorf("no transaction: %q", got)
	}
}

func TestTxWriter(t *testing.T) {
	held := newTxWriter()
	held.Header().Set("Content-Range", "0-1/*")
	held.WriteHeader(201)
	held.WriteHeader(500) // only the first status counts, like net/http
	held.Write([]byte("ab"))
	held.Write([]byte("cd"))
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Profile", "public")
	held.flush(rec, func(h http.Header) { h.Set("Server-Timing", "x") })
	if rec.Code != 201 || rec.Body.String() != "abcd" || rec.Header().Get("Content-Range") != "0-1/*" ||
		rec.Header().Get("Content-Profile") != "public" || rec.Header().Get("Server-Timing") != "x" {
		t.Errorf("flushed %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
}

func TestMetricsFormat(t *testing.T) {
	m := newMetrics()
	m.request("GET", 200, 3*time.Millisecond, "")
	m.request("GET", 200, 2*time.Second, "")
	m.request("GET", 404, 1*time.Millisecond, "FBRST205")
	m.request("BREW", 405, time.Millisecond, "FBRST117")
	s := &Server{cfg: &config.Config{}, m: m, pools: map[string]*rolePool{}} // no pools, no catalog
	out := metricsText(t, s)
	for _, want := range []string{
		`firebirdrest_requests_total{method="GET",status="200"} 2`,
		`firebirdrest_requests_total{method="GET",status="404"} 1`,
		`firebirdrest_requests_total{method="OTHER",status="405"} 1`,
		`firebirdrest_request_duration_seconds_bucket{method="GET",le="0.001"} 1`,
		`firebirdrest_request_duration_seconds_bucket{method="GET",le="0.005"} 2`,
		`firebirdrest_request_duration_seconds_bucket{method="GET",le="2.5"} 3`,
		`firebirdrest_request_duration_seconds_bucket{method="GET",le="+Inf"} 3`,
		`firebirdrest_request_duration_seconds_count{method="GET"} 3`,
		`firebirdrest_errors_total{code="FBRST205"} 1`,
		"# TYPE firebirdrest_request_duration_seconds histogram",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	checkExposition(t, out)
	if lv("a\"b\\c\nd") != `"a\"b\\c\nd"` || lv("ä") != `"ä"` {
		t.Errorf("label escaping: %s %s", lv("a\"b\\c\nd"), lv("ä"))
	}
}

func metricsText(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Metrics().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type %q", ct)
	}
	return rec.Body.String()
}

var sampleLine = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\.)*",?)*\})? (-?[0-9.]+(e[+-]?[0-9]+)?|\+Inf|NaN)$`)

// checkExposition checks every line against the Prometheus text format, and
// that each sample belongs to a declared metric.
func checkExposition(t *testing.T, out string) {
	t.Helper()
	declared := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			declared[strings.Fields(line)[2]] = true
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			continue
		}
		if !sampleLine.MatchString(line) {
			t.Errorf("malformed line %q", line)
			continue
		}
		name := line[:strings.IndexAny(line, "{ ")]
		base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")
		if !declared[name] && !declared[base] {
			t.Errorf("sample %q has no TYPE", name)
		}
	}
}

// TestObservability runs requests against the database and checks the
// Server-Timing header, held responses and the metrics they leave.
func TestObservability(t *testing.T) {
	uri := os.Getenv("FBREST_TEST_DB_URI")
	if uri == "" {
		t.Skip("FBREST_TEST_DB_URI not set")
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{rsaJWK("k1", &key.PublicKey)}})
	}))
	defer idp.Close()
	cfg := &config.Config{DBURI: uri, DBAnonRole: "web_anon", DBPool: 4, DBSafeUpdates: true, DBAggregates: true,
		DBPreparedStatements: 64, DBRequestContext: "auto", JWTSecret: testSecret, JWTRoleClaim: "role",
		JWTJWKSURL: idp.URL, JWTJWKSRefresh: 3600, JWTCacheMaxEntries: 100, ServerTiming: true, DBPlan: true}
	ts, s, err := startServerS(t, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Server-Timing on reads, writes and RPC
	timingRe := regexp.MustCompile(`^jwt;dur=[0-9.]+, parse;dur=[0-9.]+, transaction;dur=[0-9.]+, response;dur=[0-9.]+$`)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/books?select=title,authors(name)&limit=2", ""},
		{"GET", "/rpc/add_vat?amount=100&rate=10", ""},
		{"POST", "/authors", `{"name":"Timing Test","country":"Nowhere"}`},
		{"DELETE", "/authors?name=eq.Timing%20Test", ""},
	} {
		r := do(t, ts, c.method, c.path, c.body, "Authorization", user)
		if r.status >= 300 {
			t.Fatalf("%s %s: %d %s", c.method, c.path, r.status, r.body)
		}
		if st := r.header.Get("Server-Timing"); !timingRe.MatchString(st) {
			t.Errorf("%s %s: Server-Timing %q", c.method, c.path, st)
		}
	}
	r := do(t, ts, "GET", "/books?select=title", "", "Accept", "application/vnd.pgrst.plan")
	if r.status != 200 || r.header.Get("Server-Timing") == "" {
		t.Errorf("plan: %d Server-Timing %q", r.status, r.header.Get("Server-Timing"))
	}

	// headers of a response that turned into an error don't leak into it
	r = do(t, ts, "GET", "/books?offset=1000", "", "Prefer", "count=exact")
	if r.status != 416 || r.header.Get("Content-Range") != "" {
		t.Errorf("416: status %d, Content-Range %q", r.status, r.header.Get("Content-Range"))
	}
	// tx=rollback still returns the held response
	r = do(t, ts, "POST", "/authors", `{"name":"Rolled Back","country":"Nowhere"}`,
		"Authorization", user, "Prefer", "tx=rollback,return=representation")
	if r.status != 201 || !strings.Contains(r.body, "Rolled Back") {
		t.Errorf("tx=rollback: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/authors?name=eq.Rolled%20Back", ""); r.body != "[]" {
		t.Errorf("rolled back row visible: %s", r.body)
	}

	// a token from the JWKS URL, then the same token from the cache
	rs := "Bearer " + sign(t, jwt.SigningMethodRS256, key, "k1",
		jwt.MapClaims{"role": "web_user", "exp": time.Now().Add(time.Hour).Unix()})
	for range 2 {
		if r = do(t, ts, "GET", "/books?select=id&limit=1", "", "Authorization", rs); r.status != 200 {
			t.Fatalf("JWKS token: %d %s", r.status, r.body)
		}
	}
	if r = do(t, ts, "GET", "/books", "", "Authorization", "Bearer "+sign(t, jwt.SigningMethodRS256, key, "k2", nil)); r.status != 401 {
		t.Errorf("unknown kid: %d %s", r.status, r.body)
	}

	out := metricsText(t, s)
	checkExposition(t, out)
	sample := func(name string) float64 {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, name+" ") {
				v, _ := strconv.ParseFloat(strings.TrimPrefix(line, name+" "), 64)
				return v
			}
		}
		t.Errorf("no sample %s", name)
		return -1
	}
	if v := sample(`firebirdrest_requests_total{method="POST",status="201"}`); v != 2 {
		t.Errorf("POST 201: %v", v)
	}
	if v := sample(`firebirdrest_errors_total{code="FBRST103"}`); v != 1 {
		t.Errorf("FBRST103 errors: %v", v)
	}
	if v := sample(`firebirdrest_jwt_cache_hits_total`); v < 1 {
		t.Errorf("JWT cache hits: %v", v)
	}
	if v := sample(`firebirdrest_jwks_fetches_total`); v < 1 {
		t.Errorf("JWKS fetches: %v", v)
	}
	if v := sample(`firebirdrest_db_pool_max_connections{role="WEB_USER"}`); v != 4 {
		t.Errorf("web_user pool size: %v", v)
	}
	if v := sample(`firebirdrest_stmt_cache_misses_total{role="WEB_ANON"}`); v < 1 {
		t.Errorf("statement cache misses: %v", v)
	}
	if v := sample(`firebirdrest_schema_reloads_total`); v != 1 {
		t.Errorf("schema loads: %v", v)
	}
	if !strings.Contains(out, `firebirdrest_schema_relations{schema=`) || !strings.Contains(out, "firebirdrest_build_info{version=") {
		t.Errorf("schema or build info missing:\n%s", out)
	}
}
