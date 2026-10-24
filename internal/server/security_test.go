package server

import (
	"encoding/json"
	"fmt"
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
	// -check warnings: no way to authenticate, and the CORS wildcard
	if len(cfg.Warnings) != 2 {
		t.Errorf("warnings: %v", cfg.Warnings)
	}
	t.Setenv("FBREST_JWT_SECRET", "short")
	t.Setenv("FBREST_SERVER_CORS_ALLOWED_ORIGINS", "https://app.example")
	cfg, _ = config.Load("")
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "shorter than 32") {
		t.Errorf("short secret warning: %v", cfg.Warnings)
	}
	if got := config.RedactURI("user:p@ss@host:3050/db.fdb"); got != "user:***@host:3050/db.fdb" {
		t.Errorf("RedactURI: %s", got)
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

// TestBulkInsert covers multi-row inserts, which run in EXECUTE BLOCK chunks.
func TestBulkInsert(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 250; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		switch i % 3 { // rows with different column sets share a block
		case 0:
			fmt.Fprintf(&sb, `{"name":"Bulk %03d","country":"X","born":"1950-01-%02d"}`, i, 1+i%28)
		case 1:
			fmt.Fprintf(&sb, `{"name":"Bulk %03d","country":"X"}`, i)
		default:
			fmt.Fprintf(&sb, `{"country":"X","name":"Bulk %03d","born":null}`, i)
		}
	}
	sb.WriteString("]")
	r := do(t, ts, "POST", "/authors?select=id,name,born&order=name", sb.String(), "Authorization", user,
		"Prefer", "return=representation,count=exact,tx=rollback")
	if r.status != 201 || r.header.Get("Content-Range") != "*/250" {
		t.Fatalf("bulk insert: %d %s %.200s", r.status, r.header.Get("Content-Range"), r.body)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(r.body), &got); err != nil || len(got) != 250 {
		t.Fatalf("representation: %v %d", err, len(got))
	}
	if got[0]["name"] != "Bulk 000" || got[0]["born"] != "1950-01-01" || got[1]["born"] != nil || got[249]["name"] != "Bulk 249" {
		t.Errorf("rows: %v %v %v", got[0], got[1], got[249])
	}
	if r = do(t, ts, "GET", "/authors?select=id&country=eq.X", ""); r.body != "[]" {
		t.Errorf("rolled back rows visible: %.100s", r.body)
	}

	// a row that fails stops the whole request, nothing is kept
	bad := `[{"name":"Ok","country":"Y"},{"name":"Ok 2","country":"Y"},{"nope":1}]`
	if r = do(t, ts, "POST", "/authors", bad, "Authorization", user); r.status != 400 || !strings.Contains(r.body, "FBRST204") {
		t.Errorf("bad row: %d %s", r.status, r.body)
	}
	bad = `[{"name":"Ok","country":"Y"},{"name":null,"country":"Y"}]` // NOT NULL violation in row 2
	if r = do(t, ts, "POST", "/authors", bad, "Authorization", user); r.status != 400 {
		t.Errorf("null name: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/authors?select=id&country=eq.Y", ""); r.body != "[]" {
		t.Errorf("partial insert kept: %.100s", r.body)
	}

	// upserts in bulk, both resolutions, with a composite-key table
	up := `[{"book_id":1,"tag_id":1},{"book_id":1,"tag_id":2},{"book_id":2,"tag_id":2}]`
	r = do(t, ts, "POST", "/book_tags?select=book_id,tag_id", up, "Authorization", user, "Prefer", "resolution=ignore-duplicates,return=representation,tx=rollback")
	if r.status != 201 || !strings.Contains(r.body, `"tag_id":2`) {
		t.Errorf("ignore-duplicates bulk: %d %s", r.status, r.body)
	}
	up = `[{"id":1,"name":"fantasy"},{"id":2,"name":"science-fiction"},{"id":9001,"name":"bulk-tag"}]`
	r = do(t, ts, "POST", "/tags?select=id,name&order=id", up, "Authorization", user, "Prefer", "resolution=merge-duplicates,return=representation,tx=rollback")
	if r.status != 201 || !strings.Contains(r.body, `"bulk-tag"`) {
		t.Errorf("merge-duplicates bulk: %d %s", r.status, r.body)
	}

	// binary and text BLOB values travel through the block parameters
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQAY3Y2wAAAAAElFTkSuQmCC"
	cov := `[{"book_id":3,"image":"` + png + `"},{"book_id":4,"image":"` + png + `","mime":"image/png"}]`
	r = do(t, ts, "POST", "/covers?select=book_id,image", cov, "Authorization", user, "Prefer", "return=representation,tx=rollback")
	if r.status == 400 && strings.Contains(r.body, "Malformed string") && strings.Contains(do(t, ts, "GET", "/", "").body, "Firebird 6") {
		t.Log("binary writes need the driver fix on Firebird 6 (branch fix-fb6-octets)")
	} else if r.status != 201 || strings.Count(r.body, png) != 2 {
		t.Errorf("blob bulk: %d %.200s", r.status, r.body)
	}
	bk := `[{"title":"Bulk A","author_id":1,"summary":"long text ` + strings.Repeat("x", 5000) + `"},{"title":"Bulk B","author_id":1,"summary":null}]`
	r = do(t, ts, "POST", "/books?select=title,summary", bk, "Authorization", user, "Prefer", "return=representation,tx=rollback")
	if r.status != 201 || !strings.Contains(r.body, strings.Repeat("x", 5000)) {
		t.Errorf("text blob bulk: %d %.200s", r.status, r.body)
	}
}

// TestBulkInsertMinimal covers return=minimal bulk inserts, which use the
// batch API on Firebird 4+ and EXECUTE BLOCKs before.
func TestBulkInsertMinimal(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 300; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		if i%2 == 0 { // two column shapes: two batches
			fmt.Fprintf(&sb, `{"name":"Min %03d","country":"M","born":"1960-02-%02d"}`, i, 1+i%28)
		} else {
			fmt.Fprintf(&sb, `{"name":"Min %03d","country":"M"}`, i)
		}
	}
	sb.WriteString("]")
	r := do(t, ts, "POST", "/authors", sb.String(), "Authorization", user, "Prefer", "count=exact")
	if r.status != 201 || r.header.Get("Content-Range") != "*/300" || r.body != "" {
		t.Fatalf("minimal bulk insert: %d %s %.200s", r.status, r.header.Get("Content-Range"), r.body)
	}
	r = do(t, ts, "GET", "/authors?select=name,born&country=eq.M&order=name&limit=2", "")
	jsonEq(t, r.body, `[{"name":"Min 000","born":"1960-02-01"},{"name":"Min 001","born":null}]`)
	r = do(t, ts, "GET", "/authors?select=count()&country=eq.M", "")
	jsonEq(t, r.body, `[{"count":300}]`)
	// a failing row fails the request and keeps nothing
	bad := `[{"name":"Min X","country":"N"},{"name":null,"country":"N"}]`
	if r = do(t, ts, "POST", "/authors", bad, "Authorization", user); r.status != 400 {
		t.Errorf("null in batch: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/authors?select=id&country=eq.N", ""); r.body != "[]" {
		t.Errorf("partial batch kept: %.100s", r.body)
	}
	// rollback applies to batches too
	if r = do(t, ts, "POST", "/authors", `[{"name":"Min R","country":"R"},{"name":"Min S","country":"R"}]`, "Authorization", user, "Prefer", "tx=rollback"); r.status != 201 {
		t.Errorf("rollback batch: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/authors?select=id&country=eq.R", ""); r.body != "[]" {
		t.Errorf("rolled-back batch visible: %.100s", r.body)
	}
	if r = do(t, ts, "DELETE", "/authors?country=eq.M", "", "Authorization", user); r.status != 204 {
		t.Errorf("cleanup: %d %s", r.status, r.body)
	}

	// scaled numerics, integers, booleans and nulls through the batch encoder
	books := `[{"title":"Batch 1","author_id":1,"price":9.99,"published":2001,"in_stock":false},
		{"title":"Batch 2","author_id":1,"price":"12","published":null,"in_stock":true},
		{"title":"Batch 3","author_id":1,"price":7.5,"published":1999,"in_stock":true}]`
	if r = do(t, ts, "POST", "/books", books, "Authorization", user); r.status != 201 {
		t.Fatalf("books batch: %d %s", r.status, r.body)
	}
	r = do(t, ts, "GET", "/books?select=title,price,published,in_stock&title=like.Batch*&order=title", "")
	jsonEq(t, r.body, `[{"title":"Batch 1","price":9.99,"published":2001,"in_stock":false},
		{"title":"Batch 2","price":12,"published":null,"in_stock":true},
		{"title":"Batch 3","price":7.5,"published":1999,"in_stock":true}]`)
	// a price with too many decimals or a title too long is handled by the
	// block path, which reports the error
	tooLong := `[{"title":"` + strings.Repeat("t", 201) + `","author_id":1},{"title":"Batch 4","author_id":1}]`
	if r = do(t, ts, "POST", "/books", tooLong, "Authorization", user); r.status != 400 {
		t.Errorf("too long title: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/books?select=title&title=eq.Batch%204", ""); r.body != "[]" {
		t.Errorf("row kept after the failed request: %s", r.body)
	}
	if r = do(t, ts, "DELETE", "/books?title=like.Batch*", "", "Authorization", user); r.status != 204 {
		t.Errorf("cleanup: %d %s", r.status, r.body)
	}
}
