package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
)

// TestSecurityHardening covers the fixes of the security review: request
// shape limits, response headers, CORS, error scrubbing and pool keys.
func TestSecurityHardening(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})

	// a logic tree nested beyond the limit is refused, not parsed quadratically
	deep := "or=(" + strings.Repeat("or(", 39) + "id.eq.1" + strings.Repeat(")", 40)
	if r := do(t, ts, "GET", "/books?select=id&"+deep, ""); r.status != 400 || !strings.Contains(r.body, "nested deeper") {
		t.Errorf("deep logic tree: %d %s", r.status, r.body)
	}
	ok := "or=(" + strings.Repeat("or(", 9) + "id.eq.1" + strings.Repeat(")", 10)
	if r := do(t, ts, "GET", "/books?select=id&"+ok, ""); r.status != 200 || r.body != `[{"id":1}]` {
		t.Errorf("10-level logic tree: %d %s", r.status, r.body)
	}

	// every response says it is data, not a document
	r := do(t, ts, "GET", "/books?select=id&limit=1", "")
	if r.header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(r.header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("headers: %v", r.header)
	}
	if r = do(t, ts, "GET", "/nope", ""); r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("error headers: %v", r.header)
	}
	// a value served as HTML is a download, never a page in the API's origin
	r = do(t, ts, "GET", "/books?select=title&id=eq.1", "", "Accept", "text/html")
	if r.status != 200 || r.header.Get("Content-Disposition") != "attachment" {
		t.Errorf("text/html: %d %v", r.status, r.header)
	}
	if r = do(t, ts, "GET", "/books?select=title&id=eq.1", "", "Accept", "text/plain"); r.header.Get("Content-Disposition") != "" {
		t.Errorf("text/plain is not a download: %v", r.header)
	}

	// CORS: the wildcard never reflects the origin with credentials
	r = do(t, ts, "GET", "/books?select=id&limit=1", "", "Origin", "https://evil.example")
	if r.header.Get("Access-Control-Allow-Origin") != "*" || r.header.Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("wildcard CORS: %v", r.header)
	}
	listed := testServerWith(t, func(c *config.Config) { c.CORSOrigins = "https://app.example, https://admin.example" })
	r = do(t, listed, "GET", "/books?select=id&limit=1", "", "Origin", "https://admin.example")
	if r.header.Get("Access-Control-Allow-Origin") != "https://admin.example" || r.header.Get("Access-Control-Allow-Credentials") != "true" ||
		!strings.Contains(r.header.Get("Vary"), "Origin") {
		t.Errorf("listed origin: %v", r.header)
	}
	if r = do(t, listed, "GET", "/books?select=id&limit=1", "", "Origin", "https://evil.example"); r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("unlisted origin allowed: %v", r.header)
	}

	// permission errors don't name the database login
	r = do(t, ts, "GET", "/customers", "")
	if r.status != 401 || strings.Contains(r.body, "Effective user") || strings.Contains(strings.ToLower(r.body), "authenticator") {
		t.Errorf("permission error leaks the login: %d %s", r.status, r.body)
	}

	// bodies are bounded, and read before a transaction starts
	small := testServerWith(t, func(c *config.Config) { c.ServerMaxBody = 1 })
	big := `{"name":"` + strings.Repeat("x", 1<<20) + `","country":"X"}`
	if r = do(t, small, "POST", "/authors", big, "Authorization", user); r.status != 413 || !strings.Contains(r.body, "FBRST134") {
		t.Errorf("oversized body: %d %.100s", r.status, r.body)
	}

	// role names are one pool whatever their case
	for _, role := range []string{"web_user", "WEB_USER", "Web_User"} {
		if r = do(t, ts, "GET", "/customers?select=id&limit=1", "", "Authorization", "Bearer "+token(t, jwt.MapClaims{"role": role})); r.status != 200 {
			t.Errorf("role %s: %d %s", role, r.status, r.body)
		}
	}
	m := metricsText(t, serverOf(t, ts))
	if strings.Count(m, "firebirdrest_db_pool_max_connections{role=\"WEB_USER\"}") != 1 || strings.Contains(m, `role="Web_User"`) {
		t.Errorf("one pool per role expected:\n%s", m)
	}

	// an in.() list is bounded: a few thousand parameters kill the connection
	many := make([]string, 1001)
	for i := range many {
		many[i] = strconv.Itoa(i)
	}
	if r = do(t, ts, "GET", "/books?select=id&id=in.("+strings.Join(many, ",")+")", ""); r.status != 400 || !strings.Contains(r.body, "more than 1000") {
		t.Errorf("1001-value in list: %d %.120s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/books?select=id&id=in.("+strings.Join(many[:1000], ",")+")&order=id", ""); r.status != 200 || !strings.HasPrefix(r.body, `[{"id":1}`) {
		t.Errorf("1000-value in list: %d %.120s", r.status, r.body)
	}

	// a Range whose end overflows the limit arithmetic can't remove the limit
	r = do(t, ts, "GET", "/books?select=id&limit=2", "", "Range", "0-9223372036854775807")
	if r.status != 416 {
		t.Errorf("overflowing Range: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/books?select=id&order=id", "", "Range", "1-2"); r.status != 200 || r.body != `[{"id":2},{"id":3}]` {
		t.Errorf("normal Range: %d %s", r.status, r.body)
	}

	// an unmapped error is generic for the client
	if msg := toAPIError(errTest("dial tcp 10.0.0.1:3050: secret host")).Message; strings.Contains(msg, "10.0.0.1") {
		t.Errorf("internal error leaked: %s", msg)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestConfigBooleans(t *testing.T) {
	t.Setenv("FBREST_DB_URI", "u:p@h/db")
	t.Setenv("FBREST_ADMIN_PPROF", "")
	t.Setenv("FBREST_DB_SAFE_UPDATES", " ")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminPprof || !cfg.DBSafeUpdates {
		t.Errorf("empty values must keep the defaults: pprof=%v safe=%v", cfg.AdminPprof, cfg.DBSafeUpdates)
	}
	if cfg.ServerTimeout != 60 || cfg.ServerMaxBody != 16 {
		t.Errorf("defaults: timeout=%d body=%d", cfg.ServerTimeout, cfg.ServerMaxBody)
	}
	_ = http.StatusOK
}

func TestOpenAPIPrivileges(t *testing.T) {
	ts := testServer(t) // follow-privileges is the default
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	paths := func(r resp) map[string]any {
		var doc struct{ Paths map[string]any }
		if err := json.Unmarshal([]byte(r.body), &doc); err != nil {
			t.Fatalf("openapi: %d %.200s", r.status, r.body)
		}
		return doc.Paths
	}
	anon := paths(do(t, ts, "GET", "/", ""))
	if _, ok := anon["/customers"]; ok {
		t.Error("anonymous document lists customers, which web_anon can't read")
	}
	books, _ := anon["/books"].(map[string]any)
	if books == nil || books["get"] == nil || books["post"] != nil {
		t.Errorf("anonymous /books should have get only: %v", books)
	}
	if anon["/rpc/add_vat"] == nil || anon["/rpc/catalog.book_count"] == nil || anon["/rpc/place_order"] != nil {
		t.Errorf("anonymous routines: add_vat %v, catalog %v, place_order %v", anon["/rpc/add_vat"], anon["/rpc/catalog.book_count"], anon["/rpc/place_order"])
	}
	asUser := paths(do(t, ts, "GET", "/", "", "Authorization", user))
	customers, _ := asUser["/customers"].(map[string]any)
	if customers == nil || customers["get"] == nil || customers["post"] == nil || customers["delete"] == nil {
		t.Errorf("web_user /customers should have every method: %v", customers)
	}
	if asUser["/rpc/place_order"] == nil {
		t.Error("web_user can execute place_order")
	}

	all := testServerWith(t, func(c *config.Config) { c.OpenAPIMode = "ignore-privileges" })
	if p := paths(do(t, all, "GET", "/", "")); p["/customers"] == nil || p["/rpc/place_order"] == nil {
		t.Error("ignore-privileges lists everything")
	}
	off := testServerWith(t, func(c *config.Config) { c.OpenAPIMode = "disabled" })
	if r := do(t, off, "GET", "/", ""); r.status != 404 {
		t.Errorf("disabled: %d %s", r.status, r.body)
	}
	noAnon := testServerWith(t, func(c *config.Config) { c.DBAnonRole = "" })
	if r := do(t, noAnon, "GET", "/", ""); r.status != 401 {
		t.Errorf("no anonymous role: %d %s", r.status, r.body)
	}
}
