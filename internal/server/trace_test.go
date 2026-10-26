package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
)

type otlpSpan struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Name         string `json:"name"`
	Kind         int    `json:"kind"`
	Start        string `json:"startTimeUnixNano"`
	End          string `json:"endTimeUnixNano"`
	Attributes   []struct {
		Key   string         `json:"key"`
		Value map[string]any `json:"value"`
	} `json:"attributes"`
	Status *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
}

func (s otlpSpan) attr(key string) any {
	for _, a := range s.Attributes {
		if a.Key == key {
			for _, v := range a.Value {
				return v
			}
		}
	}
	return nil
}

// collector is a fake OTLP/HTTP endpoint.
type collector struct {
	mu      sync.Mutex
	spans   []otlpSpan
	service string
	headers http.Header
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []struct {
					Key   string         `json:"key"`
					Value map[string]any `json:"value"`
				} `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []otlpSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json" || json.Unmarshal(body, &req) != nil {
		http.Error(w, "bad export: "+string(body), 400)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = r.Header.Clone()
	for _, rs := range req.ResourceSpans {
		for _, a := range rs.Resource.Attributes {
			if a.Key == "service.name" {
				c.service, _ = a.Value["stringValue"].(string)
			}
		}
		for _, ss := range rs.ScopeSpans {
			c.spans = append(c.spans, ss.Spans...)
		}
	}
}

// trace waits for the spans of one trace, at least n of them.
func (c *collector) trace(t *testing.T, id string, n int) []otlpSpan {
	t.Helper()
	for i := 0; i < 100; i++ {
		c.mu.Lock()
		var out []otlpSpan
		for _, s := range c.spans {
			if s.TraceID == id {
				out = append(out, s)
			}
		}
		c.mu.Unlock()
		if len(out) >= n {
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("trace %s: fewer than %d spans arrived", id, n)
	return nil
}

func TestTraces(t *testing.T) {
	old := traceInterval
	traceInterval = 50 * time.Millisecond
	defer func() { traceInterval = old }()
	col := &collector{}
	cs := httptest.NewServer(col)
	defer cs.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=s%3Dcret")
	ts := testServerWith(t, func(c *config.Config) {
		c.OTelEndpoint = cs.URL + "/v1/traces"
		c.OTelHeaders = "x-api-key=s%3Dcret"
		c.OTelServiceName = "bookstore-api"
		c.OTelSampleRatio = 1
	})
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})

	// a request in the caller's trace
	const traceID, parent = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	r := do(t, ts, "GET", "/books?select=title,authors(name)&title=eq.Guards!%20Guards!", "",
		"traceparent", "00-"+traceID+"-"+parent+"-01", "Authorization", user)
	if r.status != 200 {
		t.Fatalf("read: %d %s", r.status, r.body)
	}
	spans := col.trace(t, traceID, 5) // request, jwt, transaction, two statements
	byName := map[string]otlpSpan{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	root, ok := byName["GET /books"]
	if !ok || root.ParentSpanID != parent || root.Kind != 2 || root.attr("http.route") != "/books" ||
		root.attr("http.response.status_code") != "200" || root.attr("firebirdrest.role") != "web_user" {
		t.Fatalf("server span: %+v", spans)
	}
	if j := byName["jwt"]; j.ParentSpanID != root.SpanID {
		t.Errorf("jwt span: %+v", j)
	}
	tx := byName["transaction"]
	if tx.ParentSpanID != root.SpanID || tx.attr("firebirdrest.read_only") != true || tx.attr("firebirdrest.tx_outcome") != "reuse" {
		t.Errorf("transaction span: %+v", tx)
	}
	// statement spans are named after the table, schema-qualified on Firebird 6
	stmt := func(op, table string) otlpSpan {
		for name, s := range byName {
			if name == op+" "+table || name == op+" PUBLIC."+table {
				return s
			}
		}
		t.Errorf("no %s %s span in %v", op, table, spans)
		return otlpSpan{}
	}
	books, authors := stmt("SELECT", "BOOKS"), stmt("SELECT", "AUTHORS")
	for _, st := range []otlpSpan{books, authors} {
		q, _ := st.attr("db.query.text").(string)
		if st.ParentSpanID != tx.SpanID || st.Kind != 3 || st.attr("db.system.name") != "firebirdsql" || !strings.Contains(q, "?") {
			t.Errorf("statement span: %+v", st)
		}
		if strings.Contains(q, "Guards") {
			t.Errorf("a value reached the span: %s", q)
		}
	}
	if books.End > authors.Start && len(books.End) == len(authors.Start) {
		t.Errorf("statement spans overlap: %s > %s", books.End, authors.Start)
	}
	col.mu.Lock()
	if col.service != "bookstore-api" || col.headers.Get("X-Api-Key") != "s=cret" {
		t.Errorf("service %q headers %v", col.service, col.headers)
	}
	col.mu.Unlock()

	// a write: committed transaction, INSERT statement
	const writeTrace = "11111111111111111111111111111111"
	do(t, ts, "POST", "/tags", `{"name":"traced"}`, "traceparent", "00-"+writeTrace+"-"+parent+"-01",
		"Authorization", user, "Prefer", "tx=rollback")
	ws := col.trace(t, writeTrace, 4)
	names := map[string]bool{}
	for _, s := range ws {
		names[s.Name] = true
		if s.Name == "transaction" && s.attr("firebirdrest.tx_outcome") != "rollback" {
			t.Errorf("write transaction: %+v", s)
		}
	}
	if !names["POST /tags"] || !(names["INSERT TAGS"] || names["INSERT PUBLIC.TAGS"]) {
		t.Errorf("write spans: %v", names)
	}

	// an error marks the span
	const errTrace = "22222222222222222222222222222222"
	do(t, ts, "GET", "/nope", "", "traceparent", "00-"+errTrace+"-"+parent+"-01")
	es := col.trace(t, errTrace, 1)
	if es[0].Name != "GET /nope" || es[0].attr("error.type") != "FBRST205" || es[0].attr("http.response.status_code") != "404" {
		t.Errorf("error span: %+v", es[0])
	}

	// a caller that doesn't sample: nothing is recorded
	const unsampled = "33333333333333333333333333333333"
	do(t, ts, "GET", "/books?select=id&limit=1", "", "traceparent", "00-"+unsampled+"-"+parent+"-00")
	time.Sleep(300 * time.Millisecond)
	col.mu.Lock()
	for _, s := range col.spans {
		if s.TraceID == unsampled {
			t.Errorf("unsampled request traced: %+v", s)
		}
	}
	col.mu.Unlock()
	if m := metricsOf(serverOf(t, ts)); !strings.Contains(m, "firebirdrest_trace_spans_exported_total") {
		t.Errorf("metrics: no trace counters")
	}
}

func TestTraceparentAndSummary(t *testing.T) {
	for _, c := range []struct {
		h       string
		ok, smp bool
	}{
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", true, true},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", true, false},
		{"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra", true, true},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra", false, false},
		{"00-00000000000000000000000000000000-00f067aa0ba902b7-01", false, false},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", false, false},
		{"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", false, false},
		{"00-4bf92f3577b34da6a3ce929d0e0e47-00f067aa0ba902b7-01", false, false},
		{"00-zzf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", false, false},
		{"", false, false},
	} {
		_, _, smp, ok := parseTraceparent(c.h)
		if ok != c.ok || smp != c.smp {
			t.Errorf("%q: ok %v sampled %v", c.h, ok, smp)
		}
	}
	for q, want := range map[string]string{
		`SELECT t0."TITLE" FROM "PUBLIC"."BOOKS" t0 WHERE t0."ID" = ?`:                        "SELECT PUBLIC.BOOKS",
		`SELECT t0."NAME" FROM "AUTHORS" t0`:                                                  "SELECT AUTHORS",
		`INSERT INTO "TAGS" ("NAME") VALUES (?)`:                                              "INSERT TAGS",
		`UPDATE "BOOKS" t0 SET "PRICE" = ? WHERE t0."ID" = ?`:                                 "UPDATE BOOKS",
		`DELETE FROM "TAGS" t0 WHERE t0."NAME" = ?`:                                           "DELETE TAGS",
		`EXECUTE PROCEDURE "PLACE_ORDER"(?, ?)`:                                               "EXECUTE PROCEDURE",
		`EXECUTE BLOCK (p1 TYPE OF COLUMN "TAGS"."NAME" = ?) AS BEGIN END`:                    "EXECUTE BLOCK",
		`WITH RECURSIVE n AS (SELECT 1 FROM RDB$DATABASE) SELECT * FROM n`:                    "SELECT",
		`SELECT COUNT(*) FROM (SELECT 1 FROM "BOOKS" t0)`:                                     "SELECT",
		`SELECT RDB$SET_CONTEXT('USER_TRANSACTION', ?, ?) FROM RDB$DATABASE`:                  "SELECT RDB$DATABASE",
		`SELECT * FROM "BOOKS_BY_COUNTRY"(?) t0`:                                              "SELECT BOOKS_BY_COUNTRY",
		`SELECT * FROM (WITH RECURSIVE n AS (SELECT 1 FROM RDB$DATABASE) SELECT 1 FROM n) t0`: "SELECT",
	} {
		op, table := sqlSummary(q)
		got := op
		if table != "" {
			got += " " + table
		}
		if got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
}
