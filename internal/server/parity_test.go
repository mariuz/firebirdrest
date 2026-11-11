package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
	"github.com/mariuz/firebirdrest/internal/query"
)

func TestClaimPath(t *testing.T) {
	var claims map[string]any
	json.Unmarshal([]byte(`{
		"role": "web_user",
		"https://example.com/role": "editor",
		"app_metadata": {"role": "admin", "roles": ["first", "second"]},
		"realm_access": {"roles": ["web_user", "offline_access"]},
		"https://example.com/claims": {"role": "nested"},
		"matrix": [["a", "b"], ["c"]]
	}`), &claims)
	for path, want := range map[string]string{
		"role":                               "web_user",
		".role":                              "web_user",
		"app_metadata.role":                  "admin",
		".app_metadata.roles[1]":             "second",
		"app_metadata.roles[-1]":             "second",
		"realm_access.roles[0]":              "web_user",
		"https://example.com/role":           "editor", // a claim named with dots
		`."https://example.com/role"`:        "editor",
		`."https://example.com/claims".role`: "nested",
		"matrix[0][1]":                       "b",
		"matrix[1][0]":                       "c",
		"realm_access.roles[5]":              "",
		"realm_access.roles[x]":              "",
		"realm_access.roles":                 "", // a list, not a string
		"app_metadata.role[0]":               "",
		"missing.role":                       "",
		`."unterminated`:                     "",
	} {
		if got := claimPath(claims, path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

func TestCSVRows(t *testing.T) {
	rows, err := csvRows([]byte("\xef\xbb\xbfname,country,born\nAna,\"Romania, EU\",1990-01-02\nBob,,\n\"Quote \"\"Q\"\"\",UK,\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0]["country"] != "Romania, EU" || rows[1]["country"] != nil || rows[1]["born"] != nil ||
		rows[2]["name"] != `Quote "Q"` || rows[0]["born"] != "1990-01-02" {
		t.Errorf("rows: %#v", rows)
	}
	for body, want := range map[string]string{
		"":                  "header row",
		"name\n":            "no rows",
		"name,\nA,B\n":      "has no name",
		"name,country\nA\n": "wrong number of fields",
	} {
		if _, err := csvRows([]byte(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
}

func TestStripNulls(t *testing.T) {
	inner := &query.Obj{Keys: []string{"name", "born"}, Vals: []any{"Ana", nil}}
	o := &query.Obj{Keys: []string{"id", "summary", "author", "tags", "none"}, Vals: []any{int64(1), nil, inner, []any{nil, "x"}, (*query.Obj)(nil)}}
	got := string(query.AppendJSON(nil, query.StripNulls(o)))
	if got != `{"id":1,"author":{"name":"Ana"},"tags":[null,"x"]}` {
		t.Errorf("stripped: %s", got)
	}
	if string(query.AppendJSON(nil, o)) == got {
		t.Error("the original was changed")
	}
}

// TestPostgRESTParity covers PostgREST features added for compatibility:
// the array media type and nulls=stripped, any/all modifiers, CSV bodies,
// and role claims with array indexes.
func TestPostgRESTParity(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})

	// application/vnd.pgrst.array+json is JSON, not a raw value
	r := do(t, ts, "GET", "/books?select=id&order=id&limit=2", "", "Accept", "application/vnd.pgrst.array+json")
	if r.status != 200 || r.body != `[{"id":1},{"id":2}]` {
		t.Errorf("array+json: %d %s", r.status, r.body)
	}
	// nulls=stripped, as supabase-js .stripNulls() sends it
	r = do(t, ts, "POST", "/authors?select=id,name,country,born", `{"name":"No Country"}`, "Authorization", user,
		"Prefer", "return=representation", "Prefer", "tx=rollback", "Accept", "application/vnd.pgrst.array+json;nulls=stripped")
	if r.status != 201 || strings.Contains(r.body, "null") || !strings.Contains(r.body, `"name":"No Country"`) {
		t.Errorf("nulls=stripped on an insert: %d %s", r.status, r.body)
	}
	r = do(t, ts, "GET", "/books?select=title,summary,authors(name,born)&id=eq.1", "", "Accept", "application/vnd.pgrst.object+json; nulls=stripped")
	if r.status != 200 || strings.Contains(r.body, "null") || !strings.HasPrefix(r.body, "{") {
		t.Errorf("object+json;nulls=stripped: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/books?select=title,summary&id=eq.1", ""); !strings.Contains(r.body, "summary") {
		t.Errorf("without the parameter nothing is stripped: %s", r.body)
	}

	// any / all
	titles := func(path string) string {
		t.Helper()
		r := do(t, ts, "GET", path, "")
		if r.status != 200 {
			t.Fatalf("%s: %d %s", path, r.status, r.body)
		}
		var rows []struct{ Title string }
		json.Unmarshal([]byte(r.body), &rows)
		var out []string
		for _, x := range rows {
			out = append(out, x.Title)
		}
		return strings.Join(out, "|")
	}
	if got := titles("/books?select=title&title=like(any).{*Gods*,*Solaris*}&order=title"); got != "Small Gods|Solaris" {
		t.Errorf("like(any): %s", got)
	}
	if got := titles("/books?select=title&title=ilike(all).{*s*,*o*}&id=lte.4&order=id"); got != "Small Gods|The Dispossessed|A Wizard of Earthsea" {
		t.Errorf("ilike(all): %s", got)
	}
	if got := titles("/books?select=title&id=eq(any).{1,3}&order=id"); got != "Guards! Guards!|The Dispossessed" {
		t.Errorf("eq(any): %s", got)
	}
	if got := titles("/books?select=title&id=not.eq(any).{1,2,3}&id=lte.4"); got != "A Wizard of Earthsea" {
		t.Errorf("not.eq(any): %s", got)
	}
	if got := titles(`/books?select=title&title=eq(any).{%22Small%20Gods%22,%22a,%20b%22}`); got != "Small Gods" {
		t.Errorf("quoted values: %s", got)
	}
	if got := titles("/books?select=title&title=like(any).{}"); got != "" {
		t.Errorf("empty any: %s", got)
	}
	if got := titles("/books?select=title&or=(title.like(any).{*Gods*},id.eq.1)&order=id"); got != "Guards! Guards!|Small Gods" {
		t.Errorf("any inside or: %s", got)
	}
	// a value of the wrong type is refused as for price=gt.cheap: by the
	// conversion or by Firebird, whose error code differs between versions
	for path, code := range map[string]string{
		"/books?title=in(any).{a}":     "FBRST100",
		"/books?title=like(any).*a*":   "FBRST100",
		"/books?price=gt(any).{cheap}": `"code"`,
	} {
		if r := do(t, ts, "GET", path, ""); r.status != 400 || !strings.Contains(r.body, code) {
			t.Errorf("%s: %d %s", path, r.status, r.body)
		}
	}

	// CSV bodies
	csvBody := "name,country,born\nCSV One,\"Romania, EU\",1990-01-02\nCSV Two,,\n"
	r = do(t, ts, "POST", "/authors?select=name,country,born", csvBody, "Authorization", user, "Content-Type", "text/csv",
		"Prefer", "return=representation", "Prefer", "tx=rollback")
	if r.status != 201 {
		t.Fatalf("csv insert: %d %s", r.status, r.body)
	}
	jsonEq(t, r.body, `[{"name":"CSV One","country":"Romania, EU","born":"1990-01-02"},{"name":"CSV Two","country":null,"born":null}]`)
	if r = do(t, ts, "POST", "/authors", "name,nope\nx,y\n", "Authorization", user, "Content-Type", "text/csv", "Prefer", "tx=rollback"); r.status != 400 {
		t.Errorf("csv with an unknown column: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "POST", "/authors", "name\n", "Authorization", user, "Content-Type", "text/csv"); r.status != 400 || !strings.Contains(r.body, "no rows") {
		t.Errorf("csv without rows: %d %s", r.status, r.body)
	}

	// a role taken from a list, as Keycloak's realm_access.roles
	kc := testServerWith(t, func(c *config.Config) { c.JWTRoleClaim = "realm_access.roles[0]" })
	tok := "Bearer " + token(t, jwt.MapClaims{"sub": "ana", "realm_access": map[string]any{"roles": []string{"web_user", "offline_access"}}})
	if r = do(t, kc, "GET", "/rpc/whoami", "", "Authorization", tok); !strings.Contains(r.body, "as WEB_USER") {
		t.Errorf("role from realm_access.roles[0]: %d %s", r.status, r.body)
	}
}

func TestLogicTreesAcrossEmbeds(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	get := func(path string, auth ...string) resp {
		t.Helper()
		h := []string{}
		if len(auth) > 0 {
			h = []string{"Authorization", auth[0]}
		}
		return do(t, ts, "GET", path, "", h...)
	}
	// to-one: a book whose author matches, or whose title does
	jsonEq(t, get("/books?select=title,authors(name)&or=(authors.name.eq.Stanislaw%20Lem,title.eq.Small%20Gods)&order=id").body,
		`[{"title":"Small Gods","authors":{"name":"Terry Pratchett"}},{"title":"Solaris","authors":{"name":"Stanislaw Lem"}}]`)
	// to-many: an author with some matching book; the embed still shows all their books
	jsonEq(t, get("/authors?select=name,books(title)&or=(books.title.eq.Solaris,name.eq.Terry%20Pratchett)&order=id").body,
		`[{"name":"Terry Pratchett","books":[{"title":"Guards! Guards!"},{"title":"Small Gods"}]},{"name":"Stanislaw Lem","books":[{"title":"Solaris"}]}]`)
	// many-to-many through the junction, and an and= tree
	if r := get("/books?select=title,tags(name)&or=(tags.name.eq.humour)&order=id"); !strings.Contains(r.body, "Guards! Guards!") || strings.Contains(r.body, "Solaris") {
		t.Errorf("many-to-many: %s", r.body)
	}
	if r := get("/books?select=title,authors(country)&and=(authors.country.eq.UK,price.lt.9)"); r.body != `[{"title":"Small Gods","authors":{"country":"UK"}}]` {
		t.Errorf("and=: %s", r.body)
	}
	// negation inside the embed's condition, and the modifiers
	if r := get("/books?select=title,authors(name)&or=(authors.name.not.eq.Terry%20Pratchett)&order=id"); strings.Contains(r.body, "Pratchett") || !strings.Contains(r.body, "Solaris") {
		t.Errorf("not inside: %s", r.body)
	}
	if r := get("/books?select=title,authors(name)&or=(authors.name.like(any).{*Lem*,*Guin*})&order=id"); !strings.Contains(r.body, "Solaris") || !strings.Contains(r.body, "Dispossessed") || strings.Contains(r.body, "Pratchett") {
		t.Errorf("like(any) in an embed: %s", r.body)
	}
	// two levels: customers with an order line of more than 2 copies
	r := get("/customers?select=email,orders(order_items(qty))&or=(orders.order_items.qty.gt.2)", user)
	if r.status != 200 {
		t.Fatalf("nested path: %d %s", r.status, r.body)
	}
	var custs []struct {
		Orders []struct {
			OrderItems []struct{ Qty int } `json:"order_items"`
		}
	}
	json.Unmarshal([]byte(r.body), &custs)
	for _, c := range custs {
		big := false
		for _, o := range c.Orders {
			for _, it := range o.OrderItems {
				big = big || it.Qty > 2
			}
		}
		if !big {
			t.Errorf("nested path let a customer through without such a line: %s", r.body)
		}
	}
	// the embed must be selected; the column must exist
	if r := get("/books?select=title&or=(authors.name.eq.x)"); r.status != 400 || !strings.Contains(r.body, "embed it in select") {
		t.Errorf("embed not selected: %d %s", r.status, r.body)
	}
	if r := get("/books?select=title,authors(name)&or=(authors.nope.eq.x)"); r.status != 400 || !strings.Contains(r.body, "FBRST204") {
		t.Errorf("unknown column of the embed: %d %s", r.status, r.body)
	}
}

func TestViewEmbedding(t *testing.T) {
	ts := testServer(t)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	// books_with_authors exposes books.id: it inherits the keys into books
	jsonEq(t, do(t, ts, "GET", "/books_with_authors?select=title,tags(name),covers(mime)&id=eq.1", "").body,
		`[{"title":"Guards! Guards!","tags":[{"name":"fantasy"},{"name":"humour"}],"covers":{"mime":"image/png"}}]`)
	// and the other direction: from a referencing table to the view
	r := do(t, ts, "GET", "/order_items?select=qty,books_with_authors(title)&order=order_id&limit=1", "", "Authorization", user)
	if r.status != 200 || !strings.Contains(r.body, `"books_with_authors":{"title":`) {
		t.Errorf("to the view: %d %s", r.status, r.body)
	}
	// filters and logic trees work through the inferred key too
	if r := do(t, ts, "GET", "/books_with_authors?select=title,tags!inner(name)&tags.name=eq.humour&order=id", ""); r.status != 200 || strings.Contains(r.body, "Solaris") || !strings.Contains(r.body, "Small Gods") {
		t.Errorf("!inner through the view: %d %s", r.status, r.body)
	}
	// it doesn't expose authors.id or books.author_id: no relationship to authors
	if r := do(t, ts, "GET", "/books_with_authors?select=title,authors(name)", ""); r.status != 400 || !strings.Contains(r.body, "FBRST200") {
		t.Errorf("no key, no relationship: %d %s", r.status, r.body)
	}
	// a key column keeps naming the base table, not the view as well
	if r := do(t, ts, "GET", "/book_tags?select=book_id(title)&book_id=eq.1&limit=1", "", "Authorization", user); r.status != 200 || !strings.Contains(r.body, "Guards") {
		t.Errorf("embedding by key column after inference: %d %s", r.status, r.body)
	}
}
