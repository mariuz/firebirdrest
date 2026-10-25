package server

// Integration tests. They need a Firebird server with examples/bookstore.sql loaded:
//
//	FBREST_TEST_DB_URI=authenticator:authenticator@localhost:3050/var/lib/firebird/data/test.fdb go test ./...
//
// Write tests send Prefer: tx=rollback so the database is left unchanged.

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
)

const testSecret = "reallyreallyreallyreallyverysafe1234"

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	return testServerWith(t, nil)
}

// testServerWith starts a test server; change, if not nil, adjusts the config.
func testServerWith(t *testing.T, change func(*config.Config)) *httptest.Server {
	t.Helper()
	uri := os.Getenv("FBREST_TEST_DB_URI")
	if uri == "" {
		t.Skip("FBREST_TEST_DB_URI not set")
	}
	cfg := &config.Config{DBURI: uri, DBAnonRole: "web_anon", DBPool: 4, DBSafeUpdates: true, DBAggregates: true, DBPreparedStatements: 64, DBRequestContext: "auto",
		JWTSecret: testSecret, JWTRoleClaim: "role", CORSOrigins: "*"}
	if change != nil {
		change(cfg)
	}
	ts, err := startServer(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func startServer(t *testing.T, cfg *config.Config) (*httptest.Server, error) {
	ts, _, err := startServerS(t, cfg)
	return ts, err
}

// startServerS also returns the Server, for its metrics.
func startServerS(t *testing.T, cfg *config.Config) (*httptest.Server, *Server, error) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(context.Background(), cfg, log)
	if err != nil {
		return nil, nil, err
	}
	ts := httptest.NewServer(s)
	serversByURL[ts.URL] = s
	t.Cleanup(func() { ts.Close(); s.Close(); delete(serversByURL, ts.URL) })
	return ts, s, nil
}

var serversByURL = map[string]*Server{}

// serverOf returns the Server behind a test server.
func serverOf(t *testing.T, ts *httptest.Server) *Server {
	t.Helper()
	s := serversByURL[ts.URL]
	if s == nil {
		t.Fatal("unknown test server")
	}
	return s
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
		if http.CanonicalHeaderKey(headers[i]) == "Content-Type" {
			req.Header.Set(headers[i], headers[i+1])
		} else {
			req.Header.Add(headers[i], headers[i+1])
		}
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
		// to-many spread: one array per column, or plain values for aggregates
		{"/authors?select=name,...books(title,price)&books.order=price&id=eq.1", `[{"name":"Terry Pratchett","title":["Small Gods","Guards! Guards!"],"price":[8.50,9.99]}]`},
		{"/books?select=id,...tags(tag:name)&tags.order=name&id=eq.1", `[{"id":1,"tag":["fantasy","humour"]}]`},
		{"/authors?select=name,...books(count(),total:price.sum())&id=eq.3", `[{"name":"Stanislaw Lem","count":1,"total":10.40}]`},
		{"/books?select=id,...similar_books(similar:title)&id=eq.5", `[{"id":5,"similar":[]}]`},
		{"/authors?select=name,...books(title,...tags(tag:name))&books.order=id&id=eq.3", `[{"name":"Stanislaw Lem","title":["Solaris"],"tag":[["science-fiction"]]}]`},
		// computed relationships and fields
		{"/books?select=id,similar_books(title)&id=in.(1,5)&order=id", `[{"id":1,"similar_books":[{"title":"Small Gods"}]},{"id":5,"similar_books":[]}]`},
		{"/books?select=id,similar_books!inner(id)&author_id=eq.2&order=id", `[{"id":3,"similar_books":[{"id":4}]},{"id":4,"similar_books":[{"id":3}]}]`},
		{"/books?select=id,similar_books(count)&id=eq.5", `[{"id":5,"similar_books":[{"count":0}]}]`},
		{"/authors?select=name,author_stats(*)&id=eq.3", `[{"name":"Stanislaw Lem","author_stats":{"book_count":1,"avg_price":10.40}}]`},
		{"/authors?select=name,...author_stats(n:book_count)&id=eq.1", `[{"name":"Terry Pratchett","n":2}]`},
		{"/books?select=id,price_with_vat,label&id=eq.4", `[{"id":4,"price_with_vat":8.63,"label":"A Wizard of Earthsea (1968)"}]`},
		{"/books?select=id&price_with_vat=gt.12&order=price_with_vat.desc", `[{"id":3},{"id":5}]`},
		// related order: by a column of a to-one embed
		{"/books?select=id,authors(name)&order=authors(name),id", `[{"id":5,"authors":{"name":"Stanislaw Lem"}},{"id":1,"authors":{"name":"Terry Pratchett"}},{"id":2,"authors":{"name":"Terry Pratchett"}},{"id":3,"authors":{"name":"Ursula K. Le Guin"}},{"id":4,"authors":{"name":"Ursula K. Le Guin"}}]`},
		{"/books?select=id,writer:authors(name)&order=writer(name).desc,id&limit=2", `[{"id":3,"writer":{"name":"Ursula K. Le Guin"}},{"id":4,"writer":{"name":"Ursula K. Le Guin"}}]`},
		{"/books?select=id,...authors(author:name)&order=authors(name).desc,id.desc", `[{"id":4,"author":"Ursula K. Le Guin"},{"id":3,"author":"Ursula K. Le Guin"},{"id":2,"author":"Terry Pratchett"},{"id":1,"author":"Terry Pratchett"},{"id":5,"author":"Stanislaw Lem"}]`},
		{"/books?select=id,authors(name)&authors.country=eq.UK&order=authors(name).nullsfirst,id", `[{"id":3,"authors":null},{"id":4,"authors":null},{"id":5,"authors":null},{"id":1,"authors":{"name":"Terry Pratchett"}},{"id":2,"authors":{"name":"Terry Pratchett"}}]`},
		{"/books?select=id,authors(name)&order=authors(born).desc&author_id=neq.2&limit=1", `[{"id":1,"authors":{"name":"Terry Pratchett"}}]`},
		{"/tags?select=name,books(id,authors(name))&books.order=authors(name).desc,id&name=eq.fantasy", `[{"name":"fantasy","books":[{"id":4,"authors":{"name":"Ursula K. Le Guin"}},{"id":1,"authors":{"name":"Terry Pratchett"}},{"id":2,"authors":{"name":"Terry Pratchett"}}]}]`},
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
	// an author without books: empty arrays, or a zero count
	r = do(t, ts, "POST", "/authors?select=name,...books(title)", `{"name":"Nobody"}`,
		"Authorization", user, "Prefer", "return=representation", "Prefer", "tx=rollback")
	jsonEq(t, r.body, `[{"name":"Nobody","title":[]}]`)
	r = do(t, ts, "POST", "/authors?select=name,...books(count())", `{"name":"Nobody"}`,
		"Authorization", user, "Prefer", "return=representation", "Prefer", "tx=rollback")
	jsonEq(t, r.body, `[{"name":"Nobody","count":0}]`)
	if r := do(t, ts, "GET", "/authors?select=name,...books(name:title)", ""); r.status != 400 {
		t.Errorf("duplicate key: %d", r.status)
	}
	if r := do(t, ts, "GET", "/rpc/catalog.internal_helper", ""); r.status != 404 {
		t.Errorf("private package function exposed: %d", r.status)
	}
	// related order: to-one embeds only, and only when embedded
	for path, code := range map[string]string{
		"/authors?select=name,books(title)&order=books(title)": "FBRST126",
		"/books?select=title&order=authors(name)":              "FBRST108",
	} {
		if r := do(t, ts, "GET", path, ""); r.status != 400 || !strings.Contains(r.body, `"code":"`+code+`"`) {
			t.Errorf("%s: %d %s", path, r.status, r.body)
		}
	}
}

func TestRequestContextDetection(t *testing.T) {
	uri := os.Getenv("FBREST_TEST_DB_URI")
	if uri == "" {
		t.Skip("FBREST_TEST_DB_URI not set")
	}
	cfg := &config.Config{DBURI: uri, DBPool: 1, DBRequestContext: "auto"}
	s, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// whoami() reads request.jwt.claim.sub, so the variables must be set
	if !s.catalog.Load().Default().UsesRequestContext {
		t.Error("the example database reads request.* variables but detection said no")
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
	// anonymous: the document lists what web_anon may use
	if r.status != 200 || !strings.Contains(r.body, `"/rpc/add_vat"`) || strings.Contains(r.body, `"/rpc/place_order"`) {
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

	// limited PATCH/DELETE: order + limit/offset pick the rows (prices 9.99, 8.50, 12.00, 7.25, 10.40)
	rep := "return=representation"
	r = do(t, ts, "PATCH", "/books?order=price&limit=2&select=id,price", `{"price":1}`,
		"Authorization", user, "Prefer", rep, "Prefer", rb, "Prefer", "count=exact")
	jsonEq(t, r.body, `[{"id":4,"price":1.00},{"id":2,"price":1.00}]`)
	if cr := r.header.Get("Content-Range"); cr != "0-1/2" {
		t.Errorf("limited patch Content-Range %q", cr)
	}
	r = do(t, ts, "PATCH", "/books?order=price.desc&offset=1&limit=2&select=id", `{"in_stock":false}`,
		"Authorization", user, "Prefer", rep, "Prefer", rb)
	jsonEq(t, r.body, `[{"id":5},{"id":1}]`)
	// ties are broken by the primary key
	r = do(t, ts, "PATCH", "/books?author_id=in.(1,2)&order=author_id&limit=3&select=id", `{"in_stock":false}`,
		"Authorization", user, "Prefer", rep, "Prefer", rb)
	jsonEq(t, r.body, `[{"id":1},{"id":2},{"id":3}]`)
	r = do(t, ts, "PATCH", "/books?order=id&limit=0&select=id", `{"price":1}`,
		"Authorization", user, "Prefer", rep, "Prefer", rb)
	jsonEq(t, r.body, `[]`)
	r = do(t, ts, "DELETE", "/book_tags?order=book_id.desc,tag_id&limit=2&select=book_id,tag_id", "",
		"Authorization", user, "Prefer", rep, "Prefer", rb, "Prefer", "count=exact")
	jsonEq(t, r.body, `[{"book_id":5,"tag_id":2},{"book_id":4,"tag_id":1}]`)
	if cr := r.header.Get("Content-Range"); cr != "0-1/2" {
		t.Errorf("limited delete Content-Range %q", cr)
	}
	for path, code := range map[string]string{
		"/books?limit=2":           "FBRST127", // limit without order
		"/books?order=id&offset=2": "FBRST127", // offset without limit
	} {
		if r = do(t, ts, "PATCH", path, `{"price":1}`, "Authorization", user, "Prefer", rb); r.status != 400 ||
			!strings.Contains(r.body, code) {
			t.Errorf("PATCH %s: %d %s", path, r.status, r.body)
		}
		if r = do(t, ts, "DELETE", path, "", "Authorization", user, "Prefer", rb); r.status != 400 ||
			!strings.Contains(r.body, code) {
			t.Errorf("DELETE %s: %d %s", path, r.status, r.body)
		}
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

	// a PSQL exception becomes a 400 that leads with the exception's message
	r = do(t, ts, "POST", "/rpc/place_order", `{"customer_id":1,"book_id":5}`, "Authorization", user, "Prefer", rb)
	if r.status != 400 || !strings.Contains(r.body, `"message":"This book is out of stock"`) ||
		!strings.Contains(r.body, `"hint":"exception OUT_OF_STOCK"`) {
		t.Errorf("exception: %d %s", r.status, r.body)
	}

	// nothing was committed
	r = do(t, ts, "GET", "/tags?name=eq.new-tag", "")
	jsonEq(t, r.body, `[]`)
}

// The statement cache must stay correct and deadlock-free when the pool is
// tiny and the cache keeps evicting, and must let DDL through once idle.
func TestStatementCacheUnderLoad(t *testing.T) {
	uri := os.Getenv("FBREST_TEST_DB_URI")
	if uri == "" {
		t.Skip("FBREST_TEST_DB_URI not set")
	}
	cfg := &config.Config{DBURI: uri, DBAnonRole: "web_anon", DBPool: 2, DBSafeUpdates: true,
		DBPreparedStatements: 4, DBPreparedStatementsIdle: 1, DBRequestContext: "always", JWTRoleClaim: "role"}
	s, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	defer func() { ts.Close(); s.Close() }()

	var wg sync.WaitGroup
	errs := make(chan string, 1000)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				id := 1 + (w+i)%5
				// 10 distinct SQL texts (limit varies) against a 4-entry cache
				r := do(t, ts, "GET", fmt.Sprintf("/books?select=id&id=eq.%d&limit=%d", id, 1+i%10), "")
				if r.status != 200 || r.body != fmt.Sprintf(`[{"id":%d}]`, id) {
					errs <- fmt.Sprintf("%d %s", r.status, r.body)
					return
				}
			}
		}(w)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("requests deadlocked")
	}
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	rp := s.pools["WEB_ANON"] // pools are keyed by the upper-cased role
	rp.cache.mu.Lock()
	n := rp.cache.ll.Len()
	rp.cache.mu.Unlock()
	if n == 0 || n > 4 {
		t.Errorf("cache holds %d statements, want 1..4", n)
	}
	// idle statements are released
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rp.cache.mu.Lock()
		n = rp.cache.ll.Len()
		rp.cache.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if n != 0 {
		t.Errorf("%d statements still cached after the idle timeout", n)
	}
}

func TestRPCParams(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	multi := "params=multiple-objects"

	// selectable procedure: one query over every call, shaped as a whole
	r := do(t, ts, "POST", "/rpc/books_by_country?select=title&order=title", `[{"country":"UK"},{"country":"Poland"}]`,
		"Prefer", multi)
	if r.status != 200 {
		t.Fatalf("multiple-objects selectable: %d %s", r.status, r.body)
	}
	jsonEq(t, r.body, `[{"title":"Guards! Guards!"},{"title":"Small Gods"},{"title":"Solaris"}]`)
	if pa := r.header.Get("Preference-Applied"); !strings.Contains(pa, multi) {
		t.Errorf("Preference-Applied %q", pa)
	}
	r = do(t, ts, "POST", "/rpc/books_by_country?select=title&limit=1", `[{"country":"UK"},{"country":"Poland"}]`,
		"Prefer", multi, "Prefer", "count=exact")
	if cr := r.header.Get("Content-Range"); cr != "0-0/3" {
		t.Errorf("combined count: Content-Range %q", cr)
	}
	r = do(t, ts, "POST", "/rpc/books_by_country?select=title&price=lt.10&order=price.desc&limit=1",
		`[{"country":"UK"},{"country":"Poland"},{"country":"USA"}]`, "Prefer", multi)
	jsonEq(t, r.body, `[{"title":"Guards! Guards!"}]`)
	r = do(t, ts, "POST", "/rpc/books_by_country", `[]`, "Prefer", multi, "Prefer", "count=exact")
	jsonEq(t, r.body, `[]`)
	if cr := r.header.Get("Content-Range"); cr != "*/0" {
		t.Errorf("empty Content-Range %q", cr)
	}
	// many calls (more than the 256 contexts a statement may have) in one statement
	var callList []string
	for i := 0; i < 600; i++ {
		callList = append(callList, `{"country":"UK"}`)
	}
	r = do(t, ts, "POST", "/rpc/books_by_country?select=title&order=title&offset=1198", "["+strings.Join(callList, ",")+"]",
		"Prefer", multi, "Prefer", "count=exact")
	if r.status != 206 || r.body != `[{"title":"Small Gods"},{"title":"Small Gods"}]` || r.header.Get("Content-Range") != "1198-1199/1200" {
		t.Errorf("600 calls: %d %s %q", r.status, r.body, r.header.Get("Content-Range"))
	}
	callList = append(callList, callList...) // 1200: over the limit
	if r = do(t, ts, "POST", "/rpc/books_by_country", "["+strings.Join(callList, ",")+"]", "Prefer", multi); r.status != 400 || !strings.Contains(r.body, "FBRST128") {
		t.Errorf("too many calls: %d %s", r.status, r.body)
	}

	// function: an array of results; a single object is one call
	r = do(t, ts, "POST", "/rpc/add_vat", `[{"amount":100},{"amount":100,"rate":10},{"amount":"50"}]`, "Prefer", multi)
	jsonEq(t, r.body, `[119.00,110.00,59.50]`)
	r = do(t, ts, "POST", "/rpc/add_vat", `{"amount":100}`, "Prefer", multi)
	jsonEq(t, r.body, `[119.00]`)

	// executable procedure: every call in the same transaction, all or nothing
	r = do(t, ts, "POST", "/rpc/place_order", `[{"customer_id":1,"book_id":1},{"customer_id":2,"book_id":2,"qty":3}]`,
		"Authorization", user, "Prefer", multi, "Prefer", "tx=rollback")
	var orders []map[string]int
	if err := json.Unmarshal([]byte(r.body), &orders); err != nil || r.status != 200 || len(orders) != 2 ||
		orders[1]["order_id"] != orders[0]["order_id"]+1 {
		t.Errorf("multiple-objects executable: %d %s", r.status, r.body)
	}
	r = do(t, ts, "POST", "/rpc/place_order", `[{"customer_id":1,"book_id":1},{"customer_id":1,"book_id":5}]`,
		"Authorization", user, "Prefer", multi)
	if r.status != 400 || !strings.Contains(r.body, "out of stock") {
		t.Errorf("failing call: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/orders?select=id&customer_id=eq.1&order=id", "", "Authorization", user); r.body != `[{"id":1}]` {
		t.Errorf("the batch with a failing call was not rolled back: %s", r.body)
	}

	// single-object: the whole body reaches the only (text) parameter
	doc := `{"event":"signup","tags":["a","b"],"n":1.5}`
	r = do(t, ts, "POST", "/rpc/log_request", doc, "Authorization", user, "Prefer", "params=single-object")
	var logged struct{ ID int }
	if err := json.Unmarshal([]byte(r.body), &logged); err != nil || r.status != 200 || logged.ID == 0 {
		t.Fatalf("single-object: %d %s", r.status, r.body)
	}
	r = do(t, ts, "GET", fmt.Sprintf("/api_log?select=payload&id=eq.%d", logged.ID), "", "Authorization", user)
	jsonEq(t, r.body, `[{"payload":`+strconv.Quote(doc)+`}]`)
	do(t, ts, "DELETE", fmt.Sprintf("/api_log?id=eq.%d", logged.ID), "", "Authorization", user) // committed above; clean up

	// errors
	for _, c := range []struct{ path, body, prefer, code string }{
		{"/rpc/add_vat", `[{"amount":1},{"nope":1}]`, multi, "FBRST202"},       // unknown argument
		{"/rpc/add_vat", `[{"amount":1}]`, "", "FBRST102"},                     // array without the preference
		{"/rpc/add_vat", `{"amount":1}`, "params=single-object", "FBRST129"},   // two parameters
		{"/rpc/log_request", `{"broken":`, "params=single-object", "FBRST102"}, // invalid JSON
	} {
		h := []string{"Authorization", user}
		if c.prefer != "" {
			h = append(h, "Prefer", c.prefer)
		}
		r := do(t, ts, "POST", c.path, c.body, h...)
		if r.status/100 != 4 || !strings.Contains(r.body, `"code":"`+c.code+`"`) {
			t.Errorf("%s %s: %d %s", c.prefer, c.path, r.status, r.body)
		}
	}
}

func TestCountPreferences(t *testing.T) {
	ts := testServer(t)
	check := func(path, prefer string, status int, contentRange, applied string) {
		t.Helper()
		r := do(t, ts, "GET", path, "", "Prefer", prefer)
		if r.status != status || r.header.Get("Content-Range") != contentRange ||
			!strings.Contains(r.header.Get("Preference-Applied"), applied) {
			t.Errorf("%s %s: %d %q %q", prefer, path, r.status, r.header.Get("Content-Range"), r.header.Get("Preference-Applied"))
		}
	}
	// planned: from the index statistics of BOOKS (5 rows when they were computed)
	check("/books?select=id", "count=planned", 200, "0-4/5", "count=planned")
	check("/books?select=id&limit=2", "count=planned", 206, "0-1/5", "count=planned")
	// filtered, a view, aggregates: no estimate applies, the count is exact
	check("/books?select=id&id=gt.2", "count=planned", 200, "0-2/3", "count=exact")
	check("/books_with_authors?select=id", "count=planned", 200, "0-4/5", "count=exact")
	check("/rpc/books_by_country?country=UK", "count=planned", 200, "0-1/2", "count=exact")
	// estimated: exact within the threshold (db-max-rows, or 1000)
	check("/books?select=id", "count=estimated", 200, "0-4/5", "count=estimated")
	check("/books?select=id&id=gt.2", "count=estimated", 200, "0-2/3", "count=estimated")
	// unknown values are ignored
	check("/books?select=id", "count=approximate", 200, "0-4/*", "")

	// with db-max-rows = 2, estimated switches to the estimate above 2 rows
	small := testServerWith(t, func(c *config.Config) { c.DBMaxRows = 2 })
	for _, c := range []struct{ path, cr, applied string }{
		{"/books?select=id", "0-1/5", "count=estimated"},         // more than 2: the estimate
		{"/books?select=id&id=gt.1", "0-1/4", "count=exact"},     // more than 2 but filtered: exact
		{"/books?select=id&id=gt.3", "0-1/2", "count=estimated"}, // within 2: exact count
	} {
		r := do(t, small, "GET", c.path, "", "Prefer", "count=estimated")
		if r.header.Get("Content-Range") != c.cr || !strings.Contains(r.header.Get("Preference-Applied"), c.applied) {
			t.Errorf("max-rows 2 %s: %q %q", c.path, r.header.Get("Content-Range"), r.header.Get("Preference-Applied"))
		}
	}
}

func TestPreferStrict(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	strict := "handling=strict"

	// max-affected: over the limit fails and changes nothing (author 1 has 2 books)
	r := do(t, ts, "PATCH", "/books?author_id=eq.1", `{"price":1}`, "Authorization", user, "Prefer", strict+", max-affected=1")
	if r.status != 400 || !strings.Contains(r.body, `"code":"FBRST131"`) {
		t.Errorf("over max-affected: %d %s", r.status, r.body)
	}
	jsonEq(t, do(t, ts, "GET", "/books?select=price&author_id=eq.1&order=id", "").body, `[{"price":9.99},{"price":8.50}]`)
	r = do(t, ts, "DELETE", "/book_tags?book_id=eq.1", "", "Authorization", user, "Prefer", strict+", max-affected=1")
	if r.status != 400 || !strings.Contains(r.body, `"code":"FBRST131"`) {
		t.Errorf("delete over max-affected: %d %s", r.status, r.body)
	}
	jsonEq(t, do(t, ts, "GET", "/book_tags?select=tag_id&book_id=eq.1&order=tag_id", "").body, `[{"tag_id":1},{"tag_id":3}]`)

	// within the limit, and lenient mode, pass
	r = do(t, ts, "PATCH", "/books?author_id=eq.1", `{"price":1}`, "Authorization", user,
		"Prefer", strict+", max-affected=2", "Prefer", "tx=rollback")
	if r.status != 204 || !strings.Contains(r.header.Get("Preference-Applied"), "max-affected=2") {
		t.Errorf("within max-affected: %d %s %q", r.status, r.body, r.header.Get("Preference-Applied"))
	}
	r = do(t, ts, "PATCH", "/books?author_id=eq.1", `{"price":1}`, "Authorization", user,
		"Prefer", "max-affected=1", "Prefer", "tx=rollback")
	if r.status != 204 {
		t.Errorf("lenient max-affected: %d %s", r.status, r.body)
	}

	// strict refuses preferences it doesn't understand; lenient ignores them
	if r = do(t, ts, "GET", "/books?select=id&id=eq.1", "", "Prefer", strict+", count=sometimes"); r.status != 400 ||
		!strings.Contains(r.body, `"code":"FBRST130"`) || !strings.Contains(r.body, "count=sometimes") {
		t.Errorf("strict invalid: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/books?select=id&id=eq.1", "", "Prefer", "count=sometimes"); r.status != 200 {
		t.Errorf("lenient invalid: %d %s", r.status, r.body)
	}
}

func TestQueryPlans(t *testing.T) {
	plain := testServer(t)
	if r := do(t, plain, "GET", "/books", "", "Accept", "application/vnd.pgrst.plan"); r.status != 406 {
		t.Errorf("plans disabled: %d %s", r.status, r.body)
	}

	ts := testServerWith(t, func(c *config.Config) { c.DBPlan = true })
	// one statement per embed level, plus the count
	r := do(t, ts, "GET", "/books?select=title,authors(name)&price=lt.10&order=price", "",
		"Accept", "application/vnd.pgrst.plan", "Prefer", "count=exact")
	if r.status != 200 || !strings.HasPrefix(r.header.Get("Content-Type"), "application/vnd.pgrst.plan+text") ||
		strings.Count(r.body, "-- statement ") != 3 || strings.Count(r.body, "Select Expression") != 3 ||
		!strings.Contains(r.body, `"AUTHORS" t0`) || !strings.Contains(r.body, "SELECT COUNT(*)") {
		t.Errorf("text plan: %d %s", r.status, r.body)
	}

	// postgrest-js sends an empty options= parameter, which strict parsers reject
	r = do(t, ts, "GET", "/books?select=id&id=eq.1", "", "Accept", `application/vnd.pgrst.plan+text; for="application/json"; options=;`)
	if r.status != 200 || !strings.Contains(r.body, "BOOKS") {
		t.Errorf("postgrest-js plan header: %d %s", r.status, r.body)
	}
	r = do(t, ts, "GET", "/books?select=id&id=eq.1", "", "Accept", `application/vnd.pgrst.plan+json; for="text/csv"`)
	var plans []struct {
		Query, Plan string
		Tree        []struct {
			Node     string
			Children []json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(r.body), &plans); err != nil || len(plans) != 1 ||
		!strings.Contains(plans[0].Plan, "Unique Scan") || len(plans[0].Tree) != 1 ||
		plans[0].Tree[0].Node != "Select Expression" || len(plans[0].Tree[0].Children) != 1 {
		t.Errorf("json plan: %d %s", r.status, r.body)
	}
	// a selectable procedure
	r = do(t, ts, "GET", "/rpc/books_by_country?country=UK", "", "Accept", "application/vnd.pgrst.plan")
	if r.status != 200 || !strings.Contains(r.body, "BOOKS_BY_COUNTRY") {
		t.Errorf("rpc plan: %d %s", r.status, r.body)
	}
	// errors of the request itself still come back as errors
	if r = do(t, ts, "GET", "/books?select=nope", "", "Accept", "application/vnd.pgrst.plan"); r.status != 400 {
		t.Errorf("bad request plan: %d %s", r.status, r.body)
	}
	// writes run in a transaction that is rolled back: the plan comes back, the data stays
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	r = do(t, ts, "PATCH", "/books?id=eq.1", `{"price":1}`, "Authorization", user, "Accept", "application/vnd.pgrst.plan")
	if r.status != 200 || !strings.Contains(r.body, `"BOOKS" t0 SET`) || !strings.Contains(r.body, "RDB$PRIMARY") {
		t.Errorf("PATCH plan: %d %s", r.status, r.body)
	}
	r = do(t, ts, "DELETE", "/tags?name=eq.fantasy", "", "Authorization", user, "Accept", "application/vnd.pgrst.plan+json")
	if r.status != 200 || !strings.Contains(r.body, `\"TAGS\" t0 WHERE`) || !strings.Contains(r.body, `"tree":[{"node":"Select Expression"`) {
		t.Errorf("DELETE plan: %d %s", r.status, r.body)
	}
	r = do(t, ts, "POST", "/tags", `[{"name":"plan-a"},{"name":"plan-b"},{"name":"plan-c"}]`, "Authorization", user, "Accept", "application/vnd.pgrst.plan")
	if r.status != 200 || !strings.Contains(r.body, "EXECUTE BLOCK") || !strings.Contains(r.body, `"TAGS" ("NAME")`) {
		t.Errorf("POST plan: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/books?select=price&id=eq.1", ""); r.body != `[{"price":9.99}]` {
		t.Errorf("the planned update was committed: %s", r.body)
	}
	if r = do(t, ts, "GET", "/tags?select=name&name=in.(fantasy,plan-a)&order=name", ""); r.body != `[{"name":"fantasy"}]` {
		t.Errorf("the planned delete or insert was committed: %s", r.body)
	}
	if r = do(t, ts, "OPTIONS", "/books", "", "Accept", "application/vnd.pgrst.plan"); r.status == 200 && strings.Contains(r.body, "Select Expression") {
		t.Errorf("OPTIONS plan: %d %s", r.status, r.body)
	}
}

func TestPlanTree(t *testing.T) {
	text := "Select Expression\n    -> Filter\n        -> Table \"BOOKS\" as \"T0\" Access By ID\n            -> Bitmap\n                -> Index \"RDB$PRIMARY2\" Unique Scan\n\nSub-query\n    -> Table \"AUTHORS\" Full Scan\n"
	roots := planTree(text)
	if len(roots) != 2 || roots[0].Node != "Select Expression" || roots[1].Node != "Sub-query" ||
		len(roots[0].Children) != 1 || roots[0].Children[0].Node != "Filter" ||
		roots[0].Children[0].Children[0].Children[0].Children[0].Node != `Index "RDB$PRIMARY2" Unique Scan` ||
		roots[1].Children[0].Node != `Table "AUTHORS" Full Scan` {
		t.Errorf("tree: %+v", roots)
	}
	if n := planTree(""); len(n) != 0 {
		t.Errorf("empty plan: %+v", n)
	}
}

func TestPreferTimezone(t *testing.T) {
	ts := testServer(t)
	if r := do(t, ts, "GET", "/meetings?select=id&limit=1", ""); r.status == 404 {
		// Firebird 3: no time zones; the preference must not get in the way
		r = do(t, ts, "GET", "/books?select=id&id=eq.1", "", "Prefer", "timezone=UTC")
		if r.status != 200 || strings.Contains(r.header.Get("Preference-Applied"), "timezone") {
			t.Errorf("Firebird 3 timezone: %d %q", r.status, r.header.Get("Preference-Applied"))
		}
		t.Skip("no time zone types (Firebird 3): examples/bookstore-fb4.sql not loaded")
	}
	get := func(path, tz string) resp {
		t.Helper()
		if tz == "" {
			return do(t, ts, "GET", path, "")
		}
		return do(t, ts, "GET", path, "", "Prefer", "timezone="+tz)
	}
	// values keep their instant and take the requested zone's offset
	jsonEq(t, get("/meetings?select=starts_at&order=id", "").body,
		`[{"starts_at":"2026-10-20T09:00:00+03:00"},{"starts_at":"2026-10-21T15:30:00-04:00"}]`)
	jsonEq(t, get("/meetings?select=starts_at&order=id", "UTC").body,
		`[{"starts_at":"2026-10-20T06:00:00Z"},{"starts_at":"2026-10-21T19:30:00Z"}]`)
	r := get("/meetings?select=starts_at&order=id", "Asia/Tokyo")
	jsonEq(t, r.body, `[{"starts_at":"2026-10-20T15:00:00+09:00"},{"starts_at":"2026-10-22T04:30:00+09:00"}]`)
	if pa := r.header.Get("Preference-Applied"); !strings.Contains(pa, "timezone=Asia/Tokyo") {
		t.Errorf("Preference-Applied %q", pa)
	}
	// a timestamp without offset in a filter is read in the session zone
	jsonEq(t, get("/meetings?select=id&starts_at=eq.2026-10-20T15:00:00", "Asia/Tokyo").body, `[{"id":1}]`)

	// the session zone is set for the request and reset afterwards
	if r := get("/rpc/session_zone", "Asia/Tokyo"); r.body != `"Asia/Tokyo"` {
		t.Errorf("session zone %s", r.body)
	}
	for i := 0; i < 8; i++ { // more requests than pooled connections
		if r := get("/rpc/session_zone", ""); r.body == `"Asia/Tokyo"` {
			t.Fatalf("SET TIME ZONE leaked to a pooled connection")
		}
	}

	// unknown zones: ignored, or refused with handling=strict
	r = get("/meetings?select=id&id=eq.1", "Mars/Olympus_Mons")
	if r.status != 200 || strings.Contains(r.header.Get("Preference-Applied"), "timezone") {
		t.Errorf("lenient unknown zone: %d %q", r.status, r.header.Get("Preference-Applied"))
	}
	r = do(t, ts, "GET", "/meetings?select=id&id=eq.1", "", "Prefer", "handling=strict, timezone=Mars/Olympus_Mons")
	if r.status != 400 || !strings.Contains(r.body, "FBRST130") {
		t.Errorf("strict unknown zone: %d %s", r.status, r.body)
	}
}

func TestSchemas(t *testing.T) {
	plain := testServer(t)
	uri := os.Getenv("FBREST_TEST_DB_URI")
	cfg := &config.Config{DBURI: uri, DBAnonRole: "web_anon", DBPool: 4, DBSafeUpdates: true, DBAggregates: true,
		DBPreparedStatements: 64, DBRequestContext: "auto", JWTSecret: testSecret, JWTRoleClaim: "role",
		DBSchemas: []string{"public", "inventory"}}
	ts, err := startServer(t, cfg)
	if err != nil {
		// Firebird 3-5: no schemas
		if !strings.Contains(err.Error(), "needs Firebird 6") {
			t.Fatalf("db-schemas before Firebird 6: %v", err)
		}
		if r := do(t, plain, "GET", "/books", "", "Accept-Profile", "public"); r.status != 406 || !strings.Contains(r.body, "FBRST106") {
			t.Errorf("Accept-Profile before Firebird 6: %d %s", r.status, r.body)
		}
		t.Skip("schemas need Firebird 6: " + err.Error())
	}
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	inv := "inventory"

	// the default schema is the first one; responses name the schema used
	r := do(t, ts, "GET", "/books?select=title&id=eq.1", "")
	jsonEq(t, r.body, `[{"title":"Guards! Guards!"}]`)
	if cp := r.header.Get("Content-Profile"); cp != "public" {
		t.Errorf("default Content-Profile %q", cp)
	}
	if r = do(t, ts, "GET", "/warehouses", ""); r.status != 404 {
		t.Errorf("inventory table visible in public: %d", r.status)
	}

	// Accept-Profile selects the schema of a read; same-named tables stay apart
	r = do(t, ts, "GET", "/books?select=sku,title", "", "Accept-Profile", inv)
	jsonEq(t, r.body, `[{"sku":"SKU-1","title":"Guards! Guards! (paperback)"}]`)
	if cp := r.header.Get("Content-Profile"); cp != inv {
		t.Errorf("Content-Profile %q", cp)
	}
	jsonEq(t, do(t, ts, "GET", "/warehouses?select=name,stock(book_id,qty)&stock.order=book_id&order=id", "", "Accept-Profile", "INVENTORY").body,
		`[{"name":"Bucharest","stock":[{"book_id":1,"qty":12},{"book_id":2,"qty":3}]},{"name":"Cluj","stock":[{"book_id":1,"qty":0}]}]`)
	// a foreign key into another exposed schema is an embeddable relationship, both ways:
	// inventory.stock.book_id references public.books, while inventory has a books table of its own
	jsonEq(t, do(t, ts, "GET", "/stock?select=qty,books(title)&order=id", "", "Accept-Profile", inv).body,
		`[{"qty":12,"books":{"title":"Guards! Guards!"}},{"qty":3,"books":{"title":"Small Gods"}},{"qty":0,"books":{"title":"Guards! Guards!"}}]`)
	jsonEq(t, do(t, ts, "GET", "/books?select=title,stock(qty,warehouses(name))&id=eq.1&stock.order=id", "").body,
		`[{"title":"Guards! Guards!","stock":[{"qty":12,"warehouses":{"name":"Bucharest"}},{"qty":0,"warehouses":{"name":"Cluj"}}]}]`)
	// by the FK column name, with a hint, and filtered through the embed
	jsonEq(t, do(t, ts, "GET", "/stock?select=qty,book_id(title)&id=eq.1", "", "Accept-Profile", inv).body,
		`[{"qty":12,"book_id":{"title":"Guards! Guards!"}}]`)
	jsonEq(t, do(t, ts, "GET", "/books?select=title,stock!inner(qty)&stock.qty=gt.5", "").body,
		`[{"title":"Guards! Guards!","stock":[{"qty":12}]}]`)
	// the other schema's table is still not a resource of this one
	if r = do(t, ts, "GET", "/stock", ""); r.status != 404 {
		t.Errorf("inventory.stock as a public resource: %d", r.status)
	}
	// the referenced schema must be exposed
	only, err := startServer(t, &config.Config{DBURI: uri, DBAnonRole: "web_anon", DBPool: 2, DBPreparedStatements: 64,
		DBRequestContext: "auto", JWTSecret: testSecret, JWTRoleClaim: "role", DBSchemas: []string{"inventory"}})
	if err != nil {
		t.Fatal(err)
	}
	if r = do(t, only, "GET", "/stock?select=qty,books(title)", ""); r.status != 400 || !strings.Contains(r.body, "FBRST200") {
		t.Errorf("embed into a schema that isn't exposed: %d %s", r.status, r.body)
	}
	// counts and estimates use the schema's own table
	r = do(t, ts, "GET", "/stock?select=id&limit=1", "", "Accept-Profile", inv, "Prefer", "count=exact")
	if cr := r.header.Get("Content-Range"); cr != "0-0/3" {
		t.Errorf("count in inventory: %q", cr)
	}

	// routines: selectable procedure and packaged function of the schema
	jsonEq(t, do(t, ts, "GET", "/rpc/low_stock?below=5", "", "Accept-Profile", inv).body,
		`[{"warehouse":"Bucharest","book_id":2,"qty":3},{"warehouse":"Cluj","book_id":1,"qty":0}]`)
	if r = do(t, ts, "GET", "/rpc/reports.total_qty?book_id=1", "", "Accept-Profile", inv); r.body != "12" {
		t.Errorf("packaged function in inventory: %s", r.body)
	}
	if r = do(t, ts, "GET", "/rpc/low_stock?below=5", ""); r.status != 404 {
		t.Errorf("inventory routine visible in public: %d", r.status)
	}

	// writes take Content-Profile
	r = do(t, ts, "POST", "/warehouses?select=name", `{"name":"Iasi"}`, "Authorization", user,
		"Content-Profile", inv, "Prefer", "return=representation", "Prefer", "tx=rollback")
	if r.status != 201 {
		t.Fatalf("insert in inventory: %d %s", r.status, r.body)
	}
	jsonEq(t, r.body, `[{"name":"Iasi"}]`)
	r = do(t, ts, "PATCH", "/stock?id=eq.3&select=qty", `{"qty":7}`, "Authorization", user,
		"Content-Profile", inv, "Prefer", "return=representation", "Prefer", "tx=rollback")
	jsonEq(t, r.body, `[{"qty":7}]`)
	r = do(t, ts, "POST", "/rpc/low_stock?select=qty", `{"below":1}`, "Content-Profile", inv)
	jsonEq(t, r.body, `[{"qty":0}]`)
	// Accept-Profile does not apply to writes, nor Content-Profile to reads
	if r = do(t, ts, "POST", "/warehouses", `{"name":"x"}`, "Authorization", user, "Accept-Profile", inv,
		"Prefer", "tx=rollback"); r.status != 404 {
		t.Errorf("POST with Accept-Profile used inventory: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/warehouses", "", "Content-Profile", inv); r.status != 404 {
		t.Errorf("GET with Content-Profile used inventory: %d", r.status)
	}

	// unknown or unexposed schemas
	for _, name := range []string{"nope", "SYSTEM"} {
		r = do(t, ts, "GET", "/books", "", "Accept-Profile", name)
		if r.status != 406 || !strings.Contains(r.body, "FBRST106") || !strings.Contains(r.body, "public, inventory") {
			t.Errorf("Accept-Profile %s: %d %s", name, r.status, r.body)
		}
	}

	// OpenAPI describes the selected schema
	r = do(t, ts, "GET", "/", "", "Accept-Profile", inv)
	if !strings.Contains(r.body, `"/warehouses"`) || strings.Contains(r.body, `"/authors"`) {
		t.Errorf("OpenAPI for inventory: %.300s", r.body)
	}

	// configuration errors
	for _, bad := range [][]string{{"nope"}, {"system"}} {
		c := *cfg
		c.DBSchemas = bad
		if _, err := startServer(t, &c); err == nil {
			t.Errorf("db-schemas %v accepted", bad)
		}
	}
}

// TestRawMedia covers single values sent as is: Accept with a concrete
// media type on reads, writes and RPC, and raw request bodies for RPC.
func TestRawMedia(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	png, _ := hex.DecodeString("89504E470D0A1A0A0000000D4948445200000001000000010802000000907753DE0000000C49444154789C63F8CFC000000301010018DD8DB00000000049454E44AE426082")

	// a text column as text/plain
	r := do(t, ts, "GET", "/books?select=title&id=eq.1", "", "Accept", "text/plain")
	if r.status != 200 || r.body != "Guards! Guards!" || r.header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("text/plain: %d %q %q", r.status, r.body, r.header.Get("Content-Type"))
	}
	// a binary BLOB with the image's own type, and as application/octet-stream
	for _, mt := range []string{"image/png", "application/octet-stream"} {
		r = do(t, ts, "GET", "/covers?select=image&book_id=eq.1", "", "Accept", mt)
		if r.status != 200 || r.body != string(png) || r.header.Get("Content-Type") != mt {
			t.Errorf("%s: %d %d bytes, Content-Type %q", mt, r.status, len(r.body), r.header.Get("Content-Type"))
		}
	}
	// HEAD gives the headers only
	if r = do(t, ts, "HEAD", "/covers?select=image&book_id=eq.1", "", "Accept", "image/png"); r.status != 200 || r.body != "" {
		t.Errorf("HEAD: %d %d bytes", r.status, len(r.body))
	}
	// numbers and aggregates come as their JSON text
	if r = do(t, ts, "GET", "/books?select=price&id=eq.1", "", "Accept", "text/plain"); r.body != "9.99" {
		t.Errorf("number: %q", r.body)
	}
	if r = do(t, ts, "GET", "/books?select=count()", "", "Accept", "text/plain"); r.body != "5" {
		t.Errorf("count: %q", r.body)
	}
	// NULL is an empty 204
	if r = do(t, ts, "GET", "/books?select=summary&id=eq.5", "", "Accept", "text/plain"); r.status != 204 || r.body != "" {
		t.Errorf("null: %d %q", r.status, r.body)
	}
	// a built-in type anywhere in Accept wins, as does a wildcard
	if r = do(t, ts, "GET", "/books?select=title&id=eq.1", "", "Accept", "text/plain, application/json"); r.body != `[{"title":"Guards! Guards!"}]` {
		t.Errorf("mixed Accept: %q", r.body)
	}
	if r = do(t, ts, "GET", "/books?select=title&id=eq.1", "", "Accept", "text/html, */*"); !strings.HasPrefix(r.body, "[{") {
		t.Errorf("wildcard: %q", r.body)
	}
	// the shape must be one row of one column
	if r = do(t, ts, "GET", "/books?select=title", "", "Accept", "text/plain"); r.status != 406 || !strings.Contains(r.body, "FBRST116") {
		t.Errorf("many rows: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/books?select=id,title&id=eq.1", "", "Accept", "text/plain"); r.status != 406 || !strings.Contains(r.body, "FBRST107") {
		t.Errorf("two columns: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/books?select=title,authors(name)&id=eq.1", "", "Accept", "text/plain"); r.status != 406 {
		t.Errorf("embed: %d %s", r.status, r.body)
	}

	// RPC: functions and procedures with one output
	if r = do(t, ts, "GET", "/rpc/add_vat?amount=100&rate=10", "", "Accept", "text/plain"); r.body != "110.00" && r.body != "110" {
		t.Errorf("function text/plain: %q", r.body)
	}
	r = do(t, ts, "GET", "/rpc/books_by_country?country=Poland&select=title", "", "Accept", "text/plain")
	if r.status != 200 || r.body != "Solaris" {
		t.Errorf("selectable procedure: %d %q", r.status, r.body)
	}
	// a function returning JSON text, served as its own media type
	r = do(t, ts, "GET", "/rpc/whoami", "", "Accept", "application/geo+json")
	if r.status != 200 || !strings.HasPrefix(r.body, "anonymous") || r.header.Get("Content-Type") != "application/geo+json; charset=utf-8" {
		t.Errorf("geo+json: %d %q %q", r.status, r.body, r.header.Get("Content-Type"))
	}

	// raw request bodies: text to a text parameter, bytes to a binary one
	r = do(t, ts, "POST", "/rpc/log_request", "plain text, not JSON", "Authorization", user, "Content-Type", "text/plain")
	if r.status != 200 || !strings.Contains(r.body, `"id"`) {
		t.Errorf("text body: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/api_log?select=payload&order=id.desc&limit=1", "", "Authorization", user, "Accept", "text/plain"); r.body != "plain text, not JSON" {
		t.Errorf("stored text: %q", r.body)
	}
	r = do(t, ts, "POST", "/rpc/blob_size", string(png), "Authorization", user, "Content-Type", "image/png", "Accept", "text/plain")
	if r.status == 400 && strings.Contains(r.body, "Malformed string") && strings.Contains(do(t, ts, "GET", "/", "").body, "Firebird 6") {
		// the driver sends short binary values as charset-less text, which
		// Firebird 6 validates as UTF8 (driver branch fix-fb6-octets)
		t.Skip("binary writes need the driver fix on Firebird 6")
	}
	if r.status != 200 || r.body != strconv.Itoa(len(png)) {
		t.Errorf("binary body: %d %q", r.status, r.body)
	}
	r = do(t, ts, "POST", "/rpc/add_vat", "100", "Authorization", user, "Content-Type", "text/plain")
	if r.status != 400 || !strings.Contains(r.body, "FBRST133") {
		t.Errorf("two parameters: %d %s", r.status, r.body)
	}
	// JSON bodies are unchanged
	r = do(t, ts, "POST", "/rpc/add_vat", `{"amount":100,"rate":10}`, "Authorization", user, "Content-Type", "application/json")
	if r.status != 200 {
		t.Errorf("json body: %d %s", r.status, r.body)
	}

	// a write returning one value: upload a cover, get its size back
	r = do(t, ts, "POST", "/covers?select=book_id", `{"book_id":2,"image":"`+base64.StdEncoding.EncodeToString(png)+`"}`,
		"Authorization", user, "Prefer", "return=representation", "Accept", "text/plain")
	if r.status != 201 || r.body != "2" {
		t.Errorf("insert raw: %d %q", r.status, r.body)
	}
	r = do(t, ts, "GET", "/covers?select=image&book_id=eq.2", "", "Accept", "image/png")
	if r.body != string(png) {
		t.Errorf("uploaded cover: %d bytes", len(r.body))
	}
	if r = do(t, ts, "DELETE", "/covers?book_id=eq.2", "", "Authorization", user); r.status != 204 {
		t.Errorf("cleanup: %d %s", r.status, r.body)
	}
}
