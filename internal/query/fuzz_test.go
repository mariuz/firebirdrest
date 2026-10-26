package query

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/mariuz/firebirdrest/internal/schema"
)

// The URL grammar is the main input an attacker controls. FuzzReadSQL runs
// the whole path from a query string to the SQL of every statement a read
// would execute (the root, each embedded level, the count), against an
// in-memory schema, and checks what must hold for any input:
//
//   - nothing panics;
//   - a refusal is a *query.Error with a 4xx status, never an internal error;
//   - every value reaches the database as a parameter: outside double-quoted
//     identifiers the SQL has no string literal, comment or statement
//     separator, and as many placeholders as arguments;
//   - every quoted identifier is a name from the schema, so a request can't
//     introduce its own.
//
// Run it longer locally with:
//
//	go test ./internal/query -run '^$' -fuzz FuzzReadSQL -fuzztime 5m

func fuzzSchema() *schema.Schema {
	authors := &schema.Relation{Name: "authors", DBName: "AUTHORS", PrimaryKey: []string{"ID"}, Columns: []*schema.Column{
		{Name: "id", DBName: "ID", FieldType: schema.TInteger},
		{Name: "name", DBName: "NAME", FieldType: schema.TVarchar, CharLength: 100},
		{Name: "born", DBName: "BORN", FieldType: schema.TDate, Nullable: true},
	}}
	books := &schema.Relation{Name: "books", DBName: "BOOKS", PrimaryKey: []string{"ID"}, Columns: []*schema.Column{
		{Name: "id", DBName: "ID", FieldType: schema.TInteger},
		{Name: "title", DBName: "TITLE", FieldType: schema.TVarchar, CharLength: 200},
		{Name: "author_id", DBName: "AUTHOR_ID", FieldType: schema.TInteger},
		{Name: "price", DBName: "PRICE", FieldType: schema.TBigint, Scale: -2, Precision: 10, SubType: 1},
		{Name: "in_stock", DBName: "IN_STOCK", FieldType: schema.TBoolean},
		{Name: "added_at", DBName: "ADDED_AT", FieldType: schema.TTimestamp, Nullable: true},
		{Name: "summary", DBName: "SUMMARY", FieldType: schema.TBlob, SubType: 1, Nullable: true},
		{Name: "Mixed Case", DBName: "Mixed Case", FieldType: schema.TVarchar, CharLength: 10, Nullable: true},
	}}
	tags := &schema.Relation{Name: "tags", DBName: "TAGS", PrimaryKey: []string{"ID"}, Columns: []*schema.Column{
		{Name: "id", DBName: "ID", FieldType: schema.TInteger},
		{Name: "name", DBName: "NAME", FieldType: schema.TVarchar, CharLength: 50},
	}}
	bookTags := &schema.Relation{Name: "book_tags", DBName: "BOOK_TAGS", PrimaryKey: []string{"BOOK_ID", "TAG_ID"}, Columns: []*schema.Column{
		{Name: "book_id", DBName: "BOOK_ID", FieldType: schema.TInteger},
		{Name: "tag_id", DBName: "TAG_ID", FieldType: schema.TInteger},
	}}
	for _, r := range []*schema.Relation{authors, books, tags, bookTags} {
		r.Index()
	}
	return &schema.Schema{
		Relations: map[string]*schema.Relation{"authors": authors, "books": books, "tags": tags, "book_tags": bookTags},
		ForeignKeys: []*schema.ForeignKey{
			{Name: "fk_book_author", DBName: "FK_BOOK_AUTHOR", From: books, FromCols: []string{"AUTHOR_ID"}, To: authors, ToCols: []string{"ID"}},
			{Name: "fk_bt_book", DBName: "FK_BT_BOOK", From: bookTags, FromCols: []string{"BOOK_ID"}, To: books, ToCols: []string{"ID"}},
			{Name: "fk_bt_tag", DBName: "FK_BT_TAG", From: bookTags, FromCols: []string{"TAG_ID"}, To: tags, ToCols: []string{"ID"}},
		},
	}
}

// recorder is a Querier that keeps the statements it is given and runs none.
type recorder struct {
	stmts []recorded
}

type recorded struct {
	sql  string
	args []any
}

var errRecorded = errors.New("recorded, not run")

func (r *recorder) QueryContext(_ context.Context, q string, args ...any) (*sql.Rows, error) {
	r.stmts = append(r.stmts, recorded{q, append([]any(nil), args...)})
	return nil, errRecorded
}

func (r *recorder) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	r.stmts = append(r.stmts, recorded{q, append([]any(nil), args...)})
	return nil, errRecorded
}

// record collects the statements of n and of every embedded level, each
// level as attachChildren would run it for one parent key.
func record(ctx context.Context, rec *recorder, n *ReadNode, root bool) error {
	var err error
	if root {
		err = queryNodeKeys(ctx, rec, n, "t0", nil, nil, true, func(*row) error { return nil })
	} else {
		key := make([]any, len(n.Join.ChildCols))
		for i := range key {
			key[i] = int64(1)
		}
		err = queryNodeKeys(ctx, rec, n, "t0", n.Join.ChildCols, [][]any{key}, false, func(*row) error { return nil })
	}
	if err != nil && !errors.Is(err, errRecorded) {
		return err
	}
	for _, c := range n.Children {
		if err := record(ctx, rec, c, false); err != nil {
			return err
		}
	}
	return nil
}

// checkSQL verifies one generated statement; names are the quoted
// identifiers the schema allows.
func checkSQL(t *testing.T, qs string, st recorded, names map[string]bool) {
	t.Helper()
	var rest strings.Builder
	s := st.sql
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			rest.WriteByte(s[i])
			continue
		}
		// a quoted identifier: "" is an escaped quote inside it
		j := i + 1
		var id strings.Builder
		for ; j < len(s); j++ {
			if s[j] == '"' {
				if j+1 < len(s) && s[j+1] == '"' {
					id.WriteByte('"')
					j++
					continue
				}
				break
			}
			id.WriteByte(s[j])
		}
		if j >= len(s) {
			t.Fatalf("query %q: unterminated identifier in %s", qs, s)
		}
		if !names[id.String()] {
			t.Fatalf("query %q: identifier %q is not from the schema in %s", qs, id.String(), s)
		}
		rest.WriteString(" ID ")
		i = j
	}
	r := rest.String()
	for _, bad := range []string{"'", ";", "--", "/*", "*/"} {
		if strings.Contains(r, bad) {
			t.Fatalf("query %q: %q outside identifiers in %s", qs, bad, s)
		}
	}
	if n := strings.Count(r, "?"); n != len(st.args) {
		t.Fatalf("query %q: %d placeholders, %d arguments in %s", qs, n, len(st.args), s)
	}
}

// refusal checks that err is a client error.
func refusal(t *testing.T, qs, stage string, err error) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("query %q: %s returned a %T, not a *query.Error: %v", qs, stage, err, err)
	}
	if e.Status < 400 || e.Status >= 500 {
		t.Fatalf("query %q: %s refused with status %d: %v", qs, stage, e.Status, err)
	}
}

func FuzzReadSQL(f *testing.F) {
	for _, seed := range []string{
		"",
		"select=id,title&id=eq.1",
		"select=title,authors(name)&title=ilike.*god*&order=title.desc.nullslast&limit=5&offset=2",
		"select=*,authors!inner(*)&authors.name=eq.x&in_stock=is.true",
		"select=title,tags(name)&tags.order=name&tags.limit=2",
		"or=(id.eq.1,and(title.like.*a*,not.price.gt.3))&not.and=(id.lt.2,id.gt.9)",
		"id=in.(1,2,3)&title=not.in.(\"a,b\",c)&summary=containing.x&title=starting.The&title=similar.(a|b)%",
		"select=id::text,price::numeric,t:title,total:price.sum(),count()&order=title",
		"select=title,author:authors(name)&order=authors(name).desc",
		"select=...authors(name),id",
		"added_at=gte.2020-01-01T00:00:00Z&born=lt.1970-01-01&price=isdistinct.3",
		"\"Mixed Case\"=eq.x&select=\"Mixed Case\"",
		"title=eq.x'); DROP TABLE books;--",
		"title=eq.\"\"\"\"&select=id::\"bigint\"",
		"order=title.desc --x",
		"select=id,title/*&id=eq.*/1",
		"or=(id.eq.1)),title.eq.x)",
		"limit=99999999999999999999&offset=-1",
		"books.limit=1&authors.order=nope",
	} {
		f.Add(uint8(0), seed)
		f.Add(uint8(1), seed)
	}
	sc := fuzzSchema()
	names := map[string]bool{}
	for _, r := range sc.Relations {
		names[r.DBName] = true
		for _, c := range r.Columns {
			names[c.DBName] = true
		}
	}
	rels := []string{"books", "authors", "tags", "book_tags"}
	f.Fuzz(func(t *testing.T, which uint8, qs string) {
		v, err := url.ParseQuery(qs)
		if err != nil {
			return
		}
		p, err := ParseQuery(v, nil)
		if err != nil {
			refusal(t, qs, "ParseQuery", err)
			return
		}
		rel := sc.Relations[rels[int(which)%len(rels)]]
		n, err := BuildRead(sc, rel, p, 1000)
		if err != nil {
			refusal(t, qs, "BuildRead", err)
			return
		}
		ctx := context.Background()
		rec := &recorder{}
		if err := record(ctx, rec, n, true); err != nil {
			refusal(t, qs, "building the statements", err)
			return
		}
		if _, err := Count(ctx, rec, n); err != nil && !errors.Is(err, errRecorded) {
			refusal(t, qs, "Count", err)
		}
		for _, st := range rec.stmts {
			checkSQL(t, qs, st, names)
		}
	})
}

// TestFuzzChecks makes sure the fuzz target's checks would catch something:
// a seed produces the statements of every level, and a statement with a
// literal or a foreign identifier is rejected.
func TestFuzzChecks(t *testing.T) {
	sc := fuzzSchema()
	v, _ := url.ParseQuery("select=title,authors(name),tags(name)&title=eq.x'y&order=id")
	p, err := ParseQuery(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := BuildRead(sc, sc.Relations["books"], p, 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if err := record(context.Background(), rec, n, true); err != nil {
		t.Fatal(err)
	}
	if len(rec.stmts) != 3 {
		t.Fatalf("statements: %d %+v", len(rec.stmts), rec.stmts)
	}
	names := map[string]bool{}
	for _, r := range sc.Relations {
		names[r.DBName] = true
		for _, c := range r.Columns {
			names[c.DBName] = true
		}
	}
	for _, st := range rec.stmts {
		checkSQL(t, "seed", st, names)
	}
	if rec.stmts[0].args[0] != "x'y" {
		t.Errorf("the value with a quote should be an argument: %+v", rec.stmts[0])
	}
	for _, bad := range []recorded{
		{sql: `SELECT t0."TITLE" FROM "BOOKS" t0 WHERE t0."TITLE" = 'x'`},
		{sql: `SELECT t0."PASSWORD" FROM "BOOKS" t0`},
		{sql: `SELECT t0."TITLE" FROM "BOOKS" t0 WHERE t0."ID" = ?`},
		{sql: `SELECT 1 FROM "BOOKS" t0; DELETE FROM "BOOKS"`},
	} {
		ft := &testing.T{}
		done := make(chan bool)
		go func() {
			defer func() { done <- ft.Failed() }()
			checkSQL(ft, "bad", bad, names)
		}()
		if !<-done {
			t.Errorf("not rejected: %s", bad.sql)
		}
	}
}
