package server

// Integration tests. They need a Firebird server with examples/bookstore.sql loaded:
//
//	FBREST_TEST_DB_URI=authenticator:authenticator@localhost:3050/var/lib/firebird/data/test.fdb go test ./...
//
// Write tests send Prefer: tx=rollback so the database is left unchanged.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
)

const testSecret = "reallyreallyreallyreallyverysafe1234"

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	uri := os.Getenv("FBREST_TEST_DB_URI")
	if uri == "" {
		t.Skip("FBREST_TEST_DB_URI not set")
	}
	cfg := &config.Config{DBURI: uri, DBAnonRole: "web_anon", DBPool: 4, DBSafeUpdates: true, DBAggregates: true,
		JWTSecret: testSecret, JWTRoleClaim: "role", CORSOrigins: "*"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(func() { ts.Close(); s.Close() })
	return ts
}

func token(t *testing.T, claims jwt.MapClaims) string {
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

type resp struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, ts *httptest.Server, method, path, body string, headers ...string) resp {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r.StatusCode, r.Header, string(b)}
}

func jsonEq(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("invalid JSON %q: %v", got, err)
	}
	json.Unmarshal([]byte(want), &w)
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("\n got: %s\nwant: %s", gb, wb)
	}
}

func TestRead(t *testing.T) {
	ts := testServer(t)
	cases := []struct{ path, want string }{
		{"/authors?select=name&order=name&limit=2", `[{"name":"Stanislaw Lem"},{"name":"Terry Pratchett"}]`},
		{"/books?select=title,author:authors(name)&id=eq.3", `[{"title":"The Dispossessed","author":{"name":"Ursula K. Le Guin"}}]`},
		{"/authors?select=name,books(title)&id=eq.3", `[{"name":"Stanislaw Lem","books":[{"title":"Solaris"}]}]`},
		{"/books?select=id,tags(name)&id=eq.1&tags.order=name.desc", `[{"id":1,"tags":[{"name":"humour"},{"name":"fantasy"}]}]`},
		{"/books?select=id,tags!inner(name)&tags.name=eq.science-fiction&order=id", `[{"id":3,"tags":[{"name":"science-fiction"}]},{"id":5,"tags":[{"name":"science-fiction"}]}]`},
		{"/books?select=id&or=(id.eq.1,id.eq.5)&order=id.desc", `[{"id":5},{"id":1}]`},
		{"/books?select=id&title=ilike.*GOD*", `[{"id":2}]`},
		{"/books?select=id&title=starting.The", `[{"id":3}]`},
		{"/books?select=id&in_stock=is.false", `[{"id":5}]`},
		{"/books?select=id&id=in.(2,4)&order=id", `[{"id":2},{"id":4}]`},
		{"/books?select=price&id=eq.4", `[{"price":7.25}]`},
		{"/authors?select=born&id=eq.1", `[{"born":"1948-04-28"}]`},
		{"/books_with_authors?select=title&author=eq.Stanislaw%20Lem", `[{"title":"Solaris"}]`},
		{"/rpc/books_by_country?country=Poland&select=title", `[{"title":"Solaris"}]`},
		{"/rpc/add_vat?amount=100&rate=10", `110.00`},
		// aggregates
		{"/books?select=count()", `[{"count":5}]`},
		{"/books?select=author_id,n:count(),price.sum()&order=author_id", `[{"author_id":1,"n":2,"sum":18.49},{"author_id":2,"n":2,"sum":19.25},{"author_id":3,"n":1,"sum":10.40}]`},
		{"/authors?select=name,books(count)&id=eq.2", `[{"name":"Ursula K. Le Guin","books":[{"count":2}]}]`},
		{"/books?select=published.min(),published.max()", `[{"min":1961,"max":1992}]`},
		// spread embeds and select order
		{"/books?select=title,...authors(author:name)&id=eq.5", `[{"title":"Solaris","author":"Stanislaw Lem"}]`},
		{"/books?select=authors(name),id&id=eq.5", `[{"authors":{"name":"Stanislaw Lem"},"id":5}]`},
		// computed relationships and fields
		{"/books?select=id,similar_books(title)&id=in.(1,5)&order=id", `[{"id":1,"similar_books":[{"title":"Small Gods"}]},{"id":5,"similar_books":[]}]`},
		{"/books?select=id,similar_books!inner(id)&author_id=eq.2&order=id", `[{"id":3,"similar_books":[{"id":4}]},{"id":4,"similar_books":[{"id":3}]}]`},
		{"/books?select=id,similar_books(count)&id=eq.5", `[{"id":5,"similar_books":[{"count":0}]}]`},
		{"/authors?select=name,author_stats(*)&id=eq.3", `[{"name":"Stanislaw Lem","author_stats":{"book_count":1,"avg_price":10.40}}]`},
		{"/authors?select=name,...author_stats(n:book_count)&id=eq.1", `[{"name":"Terry Pratchett","n":2}]`},
		{"/books?select=id,price_with_vat,label&id=eq.4", `[{"id":4,"price_with_vat":8.63,"label":"A Wizard of Earthsea (1968)"}]`},
		{"/books?select=id&price_with_vat=gt.12&order=price_with_vat.desc", `[{"id":3},{"id":5}]`},
		// packaged routines
		{"/rpc/catalog.search?term=god", `[{"id":2,"title":"Small Gods"}]`},
		{"/rpc/catalog.book_count?author_id=2", `2`},
	}
	for _, c := range cases {
		r := do(t, ts, "GET", c.path, "")
		if r.status != 200 {
			t.Errorf("%s: status %d: %s", c.path, r.status, r.body)
			continue
		}
		jsonEq(t, r.body, c.want)
	}
}

func TestAggregateEdgeCases(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	// an author without books gets a zero count; a book without author gets null spread columns
	r := do(t, ts, "POST", "/authors?select=name,books(count)", `{"name":"Nobody"}`,
		"Authorization", user, "Prefer", "return=representation", "Prefer", "tx=rollback")
	jsonEq(t, r.body, `[{"name":"Nobody","books":[{"count":0}]}]`)
	r = do(t, ts, "POST", "/books?select=title,...authors(name)", `{"title":"Anonymous"}`,
		"Authorization", user, "Prefer", "return=representation", "Prefer", "tx=rollback")
	jsonEq(t, r.body, `[{"title":"Anonymous","name":null}]`)
	// count=exact counts groups
	r = do(t, ts, "GET", "/books?select=in_stock,count()", "", "Prefer", "count=exact")
	if got := r.header.Get("Content-Range"); got != "0-1/2" {
		t.Errorf("content-range %q", got)
	}
	if r := do(t, ts, "GET", "/rpc/catalog.internal_helper", ""); r.status != 404 {
		t.Errorf("private package function exposed: %d", r.status)
	}
}

func TestCountAndRange(t *testing.T) {
	ts := testServer(t)
	r := do(t, ts, "GET", "/books?select=id&order=id", "", "Range", "1-2", "Prefer", "count=exact")
	if r.status != 206 || r.header.Get("Content-Range") != "1-2/5" {
		t.Errorf("status %d range %q", r.status, r.header.Get("Content-Range"))
	}
	jsonEq(t, r.body, `[{"id":2},{"id":3}]`)
}

func TestFormats(t *testing.T) {
	ts := testServer(t)
	r := do(t, ts, "GET", "/books?select=id,title&id=eq.1", "", "Accept", "text/csv")
	if r.body != "id,title\n1,Guards! Guards!\n" {
		t.Errorf("csv: %q", r.body)
	}
	r = do(t, ts, "GET", "/books?select=id&id=eq.1", "", "Accept", "application/vnd.pgrst.object+json")
	jsonEq(t, r.body, `{"id":1}`)
	r = do(t, ts, "GET", "/books?select=id", "", "Accept", "application/vnd.pgrst.object+json")
	if r.status != 406 {
		t.Errorf("singular with many rows: %d", r.status)
	}
	r = do(t, ts, "GET", "/", "")
	if r.status != 200 || !strings.Contains(r.body, `"/rpc/place_order"`) {
		t.Errorf("openapi: %d", r.status)
	}
}

func TestAuth(t *testing.T) {
	ts := testServer(t)
	if r := do(t, ts, "GET", "/customers", ""); r.status != 401 {
		t.Errorf("anon customers: %d %s", r.status, r.body)
	}
	user := token(t, jwt.MapClaims{"role": "web_user", "sub": "ana@example.com", "exp": time.Now().Add(time.Hour).Unix()})
	r := do(t, ts, "GET", "/customers?select=email&id=eq.1", "", "Authorization", "Bearer "+user)
	if r.status != 200 {
		t.Fatalf("user customers: %d %s", r.status, r.body)
	}
	jsonEq(t, r.body, `[{"email":"ana@example.com"}]`)
	r = do(t, ts, "GET", "/rpc/whoami", "", "Authorization", "Bearer "+user)
	jsonEq(t, r.body, `"ana@example.com as WEB_USER"`)

	expired := token(t, jwt.MapClaims{"role": "web_user", "exp": time.Now().Add(-time.Hour).Unix()})
	if r := do(t, ts, "GET", "/books", "", "Authorization", "Bearer "+expired); r.status != 401 {
		t.Errorf("expired token: %d", r.status)
	}
	bad := token(t, jwt.MapClaims{"role": "nobody_role"})
	if r := do(t, ts, "GET", "/books", "", "Authorization", "Bearer "+bad); r.status != 403 {
		t.Errorf("ungranted role: %d %s", r.status, r.body)
	}
}

func TestWrites(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	rb := "tx=rollback"

	r := do(t, ts, "POST", "/authors?select=name,books(title)", `[{"name":"A"},{"name":"B","country":"X"}]`,
		"Authorization", user, "Prefer", "return=representation", "Prefer", rb)
	if r.status != 201 {
		t.Fatalf("insert: %d %s", r.status, r.body)
	}
	jsonEq(t, r.body, `[{"name":"A","books":[]},{"name":"B","books":[]}]`)

	r = do(t, ts, "POST", "/tags", `{"name":"fantasy"}`, "Authorization", user, "Prefer", rb)
	if r.status != 409 {
		t.Errorf("duplicate: %d %s", r.status, r.body)
	}

	r = do(t, ts, "POST", "/tags?on_conflict=name&select=name", `[{"name":"fantasy"},{"name":"new-tag"}]`,
		"Authorization", user, "Prefer", "return=representation,resolution=ignore-duplicates", "Prefer", rb)
	jsonEq(t, r.body, `[{"name":"new-tag"}]`)

	r = do(t, ts, "POST", "/tags?select=id,name", `{"id":1,"name":"Fantasy"}`,
		"Authorization", user, "Prefer", "return=representation,resolution=merge-duplicates", "Prefer", rb)
	jsonEq(t, r.body, `[{"id":1,"name":"Fantasy"}]`)

	r = do(t, ts, "PATCH", "/books?id=eq.5&select=id,in_stock", `{"in_stock":true}`,
		"Authorization", user, "Prefer", "return=representation", "Prefer", rb)
	jsonEq(t, r.body, `[{"id":5,"in_stock":true}]`)

	if r = do(t, ts, "PATCH", "/books", `{"price":1}`, "Authorization", user, "Prefer", rb); r.status != 400 {
		t.Errorf("unfiltered patch: %d", r.status)
	}

	r = do(t, ts, "PUT", "/tags?id=eq.99", `{"id":99,"name":"put"}`,
		"Authorization", user, "Prefer", "return=representation", "Prefer", rb)
	jsonEq(t, r.body, `[{"id":99,"name":"put"}]`)

	r = do(t, ts, "DELETE", "/book_tags?book_id=eq.1&select=tag_id", "",
		"Authorization", user, "Prefer", "return=representation", "Prefer", rb)
	jsonEq(t, r.body, `[{"tag_id":1},{"tag_id":3}]`)

	r = do(t, ts, "POST", "/rpc/place_order", `{"customer_id":1,"book_id":2}`, "Authorization", user, "Prefer", rb)
	if r.status != 200 || !strings.Contains(r.body, "order_id") {
		t.Errorf("rpc: %d %s", r.status, r.body)
	}

	// nothing was committed
	r = do(t, ts, "GET", "/tags?name=eq.new-tag", "")
	jsonEq(t, r.body, `[]`)
}
