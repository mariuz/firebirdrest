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
