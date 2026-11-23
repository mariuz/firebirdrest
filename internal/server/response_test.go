package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
)

func TestParseResponseHeaders(t *testing.T) {
	got, err := parseResponseHeaders(`[{"Location": "/books?id=eq.1"}, {"Set-Cookie": "a=1", "Set-Cookie": "b=2"}]`)
	if err != nil || len(got) != 3 || got[0] != [2]string{"Location", "/books?id=eq.1"} || got[2] != [2]string{"Set-Cookie", "b=2"} {
		t.Errorf("got %q, %v", got, err)
	}
	if got, err := parseResponseHeaders(`[]`); err != nil || len(got) != 0 {
		t.Errorf("empty: %q, %v", got, err)
	}
	for _, bad := range []string{`{"a": "b"}`, `[{"a": 1}]`, `["a"]`, `[{"a b": "c"}]`, `[{"X": "a\r\nb"}]`, `[{"a": "b"}] x`, `[{"a": "b"}`} {
		if _, err := parseResponseHeaders(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestResponseFromSQL(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	call := func(status any, headers string, extra ...string) resp {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"status_code": status, "headers": headers})
		return do(t, ts, "POST", "/rpc/set_response", string(body), append([]string{"Authorization", user}, extra...)...)
	}

	r := call(201, `[{"Location": "/books?id=eq.1"}, {"X-Custom": "yes"}]`)
	if r.status != 201 || r.header.Get("Location") != "/books?id=eq.1" || r.header.Get("X-Custom") != "yes" {
		t.Errorf("status and headers: %d %v %s", r.status, r.header, r.body)
	}
	// longer than one context variable: continued in response.headers.2, .3 ...
	long := strings.Repeat("x", 600)
	r = call(nil, `[{"Set-Cookie": "session=`+long+`; HttpOnly"}, {"Set-Cookie": "theme=dark"}]`)
	if r.status != 204 || len(r.header.Values("Set-Cookie")) != 2 || r.header.Values("Set-Cookie")[0] != "session="+long+"; HttpOnly" {
		t.Errorf("long headers: %d %q", r.status, r.header.Values("Set-Cookie"))
	}
	// applied also when the transaction is rolled back on request
	r = call(202, "", "Prefer", "tx=rollback")
	if r.status != 202 {
		t.Errorf("tx=rollback: %d %s", r.status, r.body)
	}
	// a procedure that calls set_response (found through RDB$DEPENDENCIES)
	r = do(t, ts, "POST", "/rpc/add_author", `{"name":"Iain M. Banks","country":"UK"}`, "Authorization", user, "Prefer", "tx=rollback")
	if r.status != 201 || !strings.HasPrefix(r.header.Get("Location"), "/authors?id=eq.") {
		t.Errorf("add_author: %d %v %s", r.status, r.header, r.body)
	}
	// headers managed by the HTTP server are ignored
	r = call(200, `[{"Content-Length": "999"}, {"X-Ok": "1"}]`)
	if r.status != 200 || r.header.Get("X-Ok") != "1" || r.header.Get("Content-Length") == "999" {
		t.Errorf("ignored headers: %d %v", r.status, r.header)
	}
	for _, c := range []struct {
		status  any
		headers string
		code    string
	}{
		{99, "", "FBRST140"},
		{700, "", "FBRST140"},
		{nil, `{"Location": "/"}`, "FBRST139"},
		{nil, `[{"Location": 1}]`, "FBRST139"},
		{nil, `[{"Bad Name": "x"}]`, "FBRST139"},
	} {
		r := call(c.status, c.headers)
		if r.status != 500 || !strings.Contains(r.body, c.code) {
			t.Errorf("%v %s: %d %s", c.status, c.headers, r.status, r.body)
		}
	}

	// a selectable procedure in a (reused) read transaction: the header
	// doesn't leak into the next requests
	for i := 0; i < 3; i++ {
		r = do(t, ts, "GET", "/rpc/cached_titles?limit=1", "")
		if r.status != 200 || r.header.Get("Cache-Control") != "public, max-age=300" {
			t.Fatalf("cached_titles: %d %v %s", r.status, r.header, r.body)
		}
		for j := 0; j < 3; j++ {
			if r := do(t, ts, "GET", "/books?select=id&limit=1", ""); r.header.Get("Cache-Control") != "" {
				t.Fatalf("Cache-Control leaked into /books: %v", r.header)
			}
		}
	}
}

func TestTxEnd(t *testing.T) {
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	ts := testServerWith(t, func(c *config.Config) { c.DBTxEnd = "rollback" })
	r := do(t, ts, "POST", "/authors", `{"name":"Never Saved"}`, "Authorization", user, "Prefer", "tx=commit")
	if r.status != 201 || strings.Contains(r.header.Get("Preference-Applied"), "tx=") {
		t.Errorf("tx=commit with db-tx-end = rollback: %d %v %s", r.status, r.header, r.body)
	}
	if r := do(t, ts, "GET", "/authors?name=eq.Never%20Saved", ""); r.body != "[]" {
		t.Errorf("the insert was committed: %s", r.body)
	}
	r = do(t, ts, "POST", "/authors", `{"name":"Never Saved"}`, "Authorization", user, "Prefer", "tx=commit, handling=strict")
	if r.status != 400 || !strings.Contains(r.body, "FBRST130") {
		t.Errorf("strict: %d %s", r.status, r.body)
	}

	ts = testServerWith(t, func(c *config.Config) { c.DBTxEnd = "commit" })
	r = do(t, ts, "POST", "/authors", `{"name":"Never Saved"}`, "Authorization", user, "Prefer", "tx=rollback, handling=strict")
	if r.status != 400 || !strings.Contains(r.body, "db-tx-end") {
		t.Errorf("tx=rollback with db-tx-end = commit, strict: %d %s", r.status, r.body)
	}

	ts = testServerWith(t, func(c *config.Config) { c.DBTxEnd = "rollback-allow-override" })
	r = do(t, ts, "POST", "/authors", `{"name":"Never Saved"}`, "Authorization", user, "Prefer", "tx=rollback")
	if r.status != 201 || !strings.Contains(r.header.Get("Preference-Applied"), "tx=rollback") {
		t.Errorf("allow-override: %d %v", r.status, r.header)
	}
}

func TestTraceHeader(t *testing.T) {
	ts := testServerWith(t, func(c *config.Config) { c.TraceHeader = "X-Request-Id" })
	for _, path := range []string{"/books?select=id&limit=1", "/nothing"} {
		r := do(t, ts, "GET", path, "", "X-Request-Id", "req-42", "Origin", "https://app.example")
		if r.header.Get("X-Request-Id") != "req-42" {
			t.Errorf("%s: %v", path, r.header)
		}
		if !strings.Contains(r.header.Get("Access-Control-Expose-Headers"), "X-Request-Id") {
			t.Errorf("%s: not exposed to browsers: %v", path, r.header)
		}
	}
	if r := do(t, ts, "GET", "/books?select=id&limit=1", ""); r.header.Get("X-Request-Id") != "" {
		t.Errorf("echoed without a request header: %v", r.header)
	}
	req := httptest.NewRequest("GET", "/books", nil)
	req.Header.Set("X-Request-Id", "req-7")
	vars := requestVars(&reqCtx{r: req, traceHeader: "X-Request-Id"})
	found := false
	for _, v := range vars {
		found = found || v == [2]string{"request.header.x-request-id", "req-7"}
	}
	if !found {
		t.Errorf("request.header.x-request-id not set: %q", vars)
	}
}

func TestRootSpec(t *testing.T) {
	ts := testServerWith(t, func(c *config.Config) { c.DBRootSpec = "root_spec" })
	r := do(t, ts, "GET", "/", "")
	if r.status != 200 || !strings.HasPrefix(r.header.Get("Content-Type"), "application/openapi+json") {
		t.Fatalf("%d %v %s", r.status, r.header, r.body)
	}
	jsonEq(t, r.body, `{"openapi": "3.0.3", "info": {"title": "Bookstore", "version": "1"}, "paths": {}}`)
	// the API itself is unchanged
	if r := do(t, ts, "GET", "/books?select=id&id=eq.1", ""); r.body != `[{"id":1}]` {
		t.Errorf("books: %s", r.body)
	}
	ts = testServerWith(t, func(c *config.Config) { c.DBRootSpec = "no_such_spec" })
	if r := do(t, ts, "GET", "/", ""); r.status != 404 || !strings.Contains(r.body, "db-root-spec") {
		t.Errorf("missing routine: %d %s", r.status, r.body)
	}
	ts = testServerWith(t, func(c *config.Config) { c.DBRootSpec = "notify" })
	if r := do(t, ts, "GET", "/", ""); r.status != 500 || !strings.Contains(r.body, "FBRST141") {
		t.Errorf("routine without output: %d %s", r.status, r.body)
	}
}

func TestPoolAcquisitionTimeout(t *testing.T) {
	ts := testServerWith(t, func(c *config.Config) { c.DBPool = 1; c.DBPoolAcquireTimeout = 1 })
	if r := do(t, ts, "GET", "/books?select=id&limit=1", ""); r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	s := serverOf(t, ts)
	p, err := s.pool(context.Background(), "web_anon")
	if err != nil {
		t.Fatal(err)
	}
	p.closeReads()
	conn, err := p.db.Conn(context.Background()) // the only connection
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := do(t, ts, "GET", "/books?select=id&limit=1", "")
	if r.status != 504 || !strings.Contains(r.body, "FBRST003") || time.Since(start) > 5*time.Second {
		t.Errorf("busy pool: %d %s after %s", r.status, r.body, time.Since(start))
	}
	conn.Close()
	if r := do(t, ts, "GET", "/books?select=id&limit=1", ""); r.status != 200 {
		t.Errorf("after release: %d %s", r.status, r.body)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestLogQuery(t *testing.T) {
	cfgFor := func(logQuery bool) (*httptest.Server, *syncBuf) {
		ts := testServer(t) // for the URI and the skip
		base := serverOf(t, ts).cfg
		cfg := *base
		cfg.LogQuery = logQuery
		cfg.TraceHeader = "X-Request-Id"
		buf := &syncBuf{}
		s, err := New(context.Background(), &cfg, slog.New(slog.NewTextHandler(buf, nil)))
		if err != nil {
			t.Fatal(err)
		}
		h := httptest.NewServer(s)
		t.Cleanup(func() { h.Close(); s.Close() })
		return h, buf
	}
	ts, buf := cfgFor(true)
	do(t, ts, "GET", "/books?select=title&id=eq.2", "", "X-Request-Id", "abc-1")
	out := buf.String()
	if !strings.Contains(out, "msg=query") || !strings.Contains(out, `\"BOOKS\" t0`) || !strings.Contains(out, "role=web_anon") {
		t.Errorf("no statement logged:\n%s", out)
	}
	if strings.Contains(out, "abc-1") && !strings.Contains(out, "x-request-id=abc-1") {
		t.Errorf("trace header logged oddly:\n%s", out)
	}
	if !strings.Contains(out, "x-request-id=abc-1") {
		t.Errorf("request log lacks the trace header:\n%s", out)
	}
	// a plain read reaches no setter: no read-back round trip
	if strings.Contains(out, "response.status") {
		t.Errorf("response variables read for /books:\n%s", out)
	}
	do(t, ts, "GET", "/rpc/cached_titles?limit=1", "")
	if !strings.Contains(buf.String(), "response.status") {
		t.Errorf("response variables not read for cached_titles:\n%s", buf.String())
	}
	ts, buf = cfgFor(false)
	do(t, ts, "GET", "/books?select=title&id=eq.2", "")
	if strings.Contains(buf.String(), "msg=query") {
		t.Errorf("statements logged with log-query off:\n%s", buf.String())
	}
}

func TestDumpSchema(t *testing.T) {
	ts := testServer(t)
	cfg := *serverOf(t, ts).cfg
	out, err := DumpSchema(context.Background(), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Version string
		Schemas []struct {
			Relations []struct {
				Name         string
				Kind         string
				Columns      []struct{ Name, Type string }
				SetsResponse bool
			}
			ForeignKeys []struct {
				From, To string
				ViaView  bool
			}
			Routines []struct {
				Name, Kind   string
				SetsResponse bool
			}
			SetsResponse bool
		}
	}
	if err := json.Unmarshal(out, &d); err != nil || len(d.Schemas) == 0 {
		t.Fatalf("%v\n%s", err, out)
	}
	s := d.Schemas[0]
	if d.Version == "" || !s.SetsResponse || len(s.Relations) < 8 {
		t.Errorf("dump: %s", out)
	}
	var books, viaView, setResp bool
	for _, r := range s.Relations {
		books = books || r.Name == "books" && r.Kind == "table" && len(r.Columns) > 3
	}
	for _, k := range s.ForeignKeys {
		viaView = viaView || k.ViaView
	}
	for _, r := range s.Routines {
		setResp = setResp || r.Name == "set_response" && r.Kind == "executable"
	}
	if !books || !viaView || !setResp {
		t.Errorf("books %v, view keys %v, set_response %v:\n%s", books, viaView, setResp, out)
	}
	setters := map[string]bool{}
	for _, r := range s.Routines {
		setters[r.Name] = r.SetsResponse
	}
	for _, r := range s.Relations {
		setters["/"+r.Name] = r.SetsResponse
	}
	want := map[string]bool{"set_response": true, "cached_titles": true, "add_author": true,
		"add_vat": false, "books_by_country": false, "/books": false, "/authors": false}
	for name, w := range want {
		if setters[name] != w {
			t.Errorf("%s: setsResponse %v, want %v", name, setters[name], w)
		}
	}
}

var _ = io.Discard
var _ = http.StatusOK
