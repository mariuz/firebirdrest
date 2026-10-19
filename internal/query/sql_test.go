package query

import (
	"net/url"
	"strings"
	"testing"

	"github.com/mariuz/firebirdrest/internal/schema"
)

func testSchema() *schema.Schema {
	authors := &schema.Relation{Name: "authors", DBName: "AUTHORS", PrimaryKey: []string{"ID"}, Columns: []*schema.Column{
		{Name: "id", DBName: "ID", FieldType: schema.TInteger},
		{Name: "name", DBName: "NAME", FieldType: schema.TVarchar},
	}}
	books := &schema.Relation{Name: "books", DBName: "BOOKS", PrimaryKey: []string{"ID"}, Columns: []*schema.Column{
		{Name: "id", DBName: "ID", FieldType: schema.TInteger},
		{Name: "title", DBName: "TITLE", FieldType: schema.TVarchar},
		{Name: "author_id", DBName: "AUTHOR_ID", FieldType: schema.TInteger},
		{Name: "in_stock", DBName: "IN_STOCK", FieldType: schema.TBoolean},
	}}
	authors.Index()
	books.Index()
	return &schema.Schema{
		Relations: map[string]*schema.Relation{"authors": authors, "books": books},
		ForeignKeys: []*schema.ForeignKey{{Name: "fk_book_author", DBName: "FK_BOOK_AUTHOR",
			From: books, FromCols: []string{"AUTHOR_ID"}, To: authors, ToCols: []string{"ID"}}},
	}
}

func build(t *testing.T, rel, qs string) *ReadNode {
	t.Helper()
	sc := testSchema()
	v, _ := url.ParseQuery(qs)
	p, err := ParseQuery(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := BuildRead(sc, sc.Relations[rel], p, 0)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func where(t *testing.T, n *ReadNode) (string, []any) {
	t.Helper()
	b := &Builder{}
	if _, err := b.WriteWhere(n, "t0", nil); err != nil {
		t.Fatal(err)
	}
	return b.String(), b.Args
}

func TestWhereSQL(t *testing.T) {
	n := build(t, "books", "title=ilike.*god*&in_stock=is.true&or=(id.eq.1,id.in.(2,3))")
	sql, args := where(t, n)
	for _, want := range []string{`UPPER(t0."TITLE") LIKE UPPER(?)`, `t0."IN_STOCK" IS TRUE`, `t0."ID" = ?`, `t0."ID" IN (?, ?)`, " OR "} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in %s", want, sql)
		}
	}
	// keys are processed alphabetically: in_stock, or, title
	if len(args) != 4 || args[0] != int64(1) || args[3] != "%god%" {
		t.Errorf("args: %#v", args)
	}
}

func TestEmbedResolution(t *testing.T) {
	n := build(t, "books", "select=title,authors(name)")
	if len(n.Children) != 1 || n.Children[0].Join.Kind != ManyToOne || !n.Children[0].Join.Single {
		t.Fatalf("many-to-one: %+v", n.Children)
	}
	n = build(t, "authors", "select=name,books(title)")
	if len(n.Children) != 1 || n.Children[0].Join.Kind != OneToMany || n.Children[0].Join.Single {
		t.Fatalf("one-to-many: %+v", n.Children)
	}
	// embed by FK column name
	n = build(t, "books", "select=title,author_id(name)")
	if n.Children[0].Rel.Name != "authors" {
		t.Fatalf("by column: %+v", n.Children[0])
	}
}

func TestInnerEmbedUsesExists(t *testing.T) {
	n := build(t, "books", "select=title,authors!inner(name)&authors.name=eq.Lem")
	sql, args := where(t, n)
	if !strings.Contains(sql, `EXISTS (SELECT 1 FROM "AUTHORS" t1 WHERE ((t1."NAME" = ?)) AND (t1."ID" = t0."AUTHOR_ID"))`) {
		t.Errorf("got %s", sql)
	}
	if len(args) != 1 || args[0] != "Lem" {
		t.Errorf("args %#v", args)
	}
}

func TestUnknownColumn(t *testing.T) {
	sc := testSchema()
	v, _ := url.ParseQuery("select=nope")
	p, _ := ParseQuery(v, nil)
	if _, err := BuildRead(sc, sc.Relations["books"], p, 0); err == nil {
		t.Error("expected error for unknown column")
	}
	v, _ = url.ParseQuery("select=title,customers(id)")
	p, _ = ParseQuery(v, nil)
	if _, err := BuildRead(sc, sc.Relations["books"], p, 0); err == nil {
		t.Error("expected error for unknown relationship")
	}
}

func TestCoerce(t *testing.T) {
	ts := &schema.Column{Name: "ts", FieldType: schema.TTimestamp}
	if v, _ := Coerce(ts, "2024-05-06T07:08:09Z"); v != "2024-05-06 07:08:09.0000" {
		t.Errorf("timestamp: %v", v)
	}
	b := &schema.Column{Name: "b", FieldType: schema.TBoolean}
	if v, _ := Coerce(b, "true"); v != true {
		t.Errorf("bool: %v", v)
	}
	if _, err := Coerce(b, "maybe"); err == nil {
		t.Error("bad bool accepted")
	}
}

func TestMaxRows(t *testing.T) {
	sc := testSchema()
	p, _ := ParseQuery(url.Values{"limit": {"500"}}, nil)
	n, _ := BuildRead(sc, sc.Relations["books"], p, 100)
	if n.Limit != 100 {
		t.Errorf("limit %d", n.Limit)
	}
}

func TestSpreadAndLayout(t *testing.T) {
	n := build(t, "books", "select=authors(name),title,...authors!fk_book_author(id)")
	if got := n.OutKeys(); strings.Join(got, ",") != "authors,title,id" {
		t.Errorf("keys %v", got)
	}
	n = build(t, "authors", "select=name,...books(title)")
	if c := n.Children[0]; !c.Spread || c.Join.Single {
		t.Errorf("to-many spread: %+v", c)
	}
	sc := testSchema()
	v, _ := url.ParseQuery("select=name,...books(name:title)")
	p, _ := ParseQuery(v, nil)
	if _, err := BuildRead(sc, sc.Relations["authors"], p, 0); err == nil {
		t.Error("duplicate key accepted")
	}
}

func TestAggregatesDisabled(t *testing.T) {
	sc := testSchema()
	p, _ := ParseQuery(url.Values{"select": {"count()"}}, nil)
	p.NoAggregates = true
	if _, err := BuildRead(sc, sc.Relations["books"], p, 0); err == nil {
		t.Error("aggregate accepted while disabled")
	}
}

func computedSchema() *schema.Schema {
	sc := testSchema()
	books := sc.Relations["books"]
	similar := &schema.Routine{Name: "similar", DBName: "SIMILAR", Kind: schema.Selectable,
		In:     []*schema.Column{{Name: "author_id", DBName: "AUTHOR_ID", FieldType: schema.TInteger}},
		Out:    []*schema.Column{{Name: "title", DBName: "TITLE", FieldType: schema.TVarchar}},
		Parent: books, ParentCols: []string{"AUTHOR_ID"}}
	vat := &schema.Routine{Name: "vat", DBName: "VAT", Kind: schema.Function,
		In:     []*schema.Column{{Name: "id", DBName: "ID", FieldType: schema.TInteger}},
		Out:    []*schema.Column{{Name: "vat", DBName: "RESULT", FieldType: schema.TDouble}},
		Parent: books, ParentCols: []string{"ID"}}
	books.Computed = []*schema.Routine{similar, vat}
	sc.Routines = map[string]*schema.Routine{"similar": similar, "vat": vat}
	return sc
}

func TestComputedFieldSQL(t *testing.T) {
	sc := computedSchema()
	v, _ := url.ParseQuery("select=id,vat&vat=gt.1&order=vat.desc")
	p, _ := ParseQuery(v, nil)
	n, err := BuildRead(sc, sc.Relations["books"], p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := FieldExpr("t0", n.Fields[1]); got != `"VAT"(t0."ID")` {
		t.Errorf("field expr %s", got)
	}
	b := &Builder{}
	b.WriteWhere(n, "t0", nil)
	if err := b.WriteOrder(n, "t0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"VAT"(t0."ID") > ?`) || !strings.Contains(b.String(), `ORDER BY "VAT"(t0."ID") DESC`) {
		t.Errorf("sql %s", b.String())
	}
}

func TestComputedRelationship(t *testing.T) {
	sc := computedSchema()
	v, _ := url.ParseQuery("select=id,similar!inner(title)")
	p, _ := ParseQuery(v, nil)
	n, err := BuildRead(sc, sc.Relations["books"], p, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := n.Children[0]
	if c.Join.Kind != Computed || c.Join.Single || c.Join.Routine.Name != "similar" {
		t.Fatalf("join %+v", c.Join)
	}
	sql, _ := where(t, n)
	if !strings.Contains(sql, `EXISTS (SELECT 1 FROM "SIMILAR"(t0."AUTHOR_ID") t1`) {
		t.Errorf("exists %s", sql)
	}
}

func orderSQL(t *testing.T, n *ReadNode) string {
	t.Helper()
	b := &Builder{}
	if err := b.WriteOrder(n, "t0"); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestRelatedOrderSQL(t *testing.T) {
	n := build(t, "books", "select=title,authors(name)&order=authors(name).desc.nullslast,id")
	want := ` ORDER BY (SELECT t1."NAME" FROM "AUTHORS" t1 WHERE (t1."ID" = t0."AUTHOR_ID")) DESC NULLS LAST, t0."ID"`
	if got := orderSQL(t, n); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	// by alias, and with the embed's own filter, which nulls the value as it nulls the embed
	n = build(t, "books", "select=title,writer:authors(name)&writer.name=like.T*&order=writer(name)")
	b := &Builder{}
	if err := b.WriteOrder(n, "t0"); err != nil {
		t.Fatal(err)
	}
	want = ` ORDER BY (SELECT t1."NAME" FROM "AUTHORS" t1 WHERE ((t1."NAME" LIKE ?)) AND (t1."ID" = t0."AUTHOR_ID"))`
	if b.String() != want || len(b.Args) != 1 || b.Args[0] != "T%" {
		t.Errorf("got %s %v", b.String(), b.Args)
	}

	// a spread to-one embed works too
	n = build(t, "books", "select=title,...authors(name)&order=authors(name)")
	if got := orderSQL(t, n); !strings.Contains(got, `(SELECT t1."NAME" FROM "AUTHORS" t1`) {
		t.Errorf("spread: %s", got)
	}

	// the embedded rows of a to-many embed can be ordered by their own to-one embed
	n = build(t, "authors", "select=name,books(title,authors(name))&books.order=authors(name)")
	if got := orderSQL(t, n.Children[0]); !strings.Contains(got, `(SELECT t1."NAME" FROM "AUTHORS" t1 WHERE (t1."ID" = t0."AUTHOR_ID"))`) {
		t.Errorf("nested: %s", got)
	}
}

func TestRelatedOrderErrors(t *testing.T) {
	sc := testSchema()
	for qs, code := range map[string]string{
		"select=title&order=authors(name)":                       "FBRST108", // not embedded
		"select=title,authors(name)&order=authors(nope)":         "FBRST204", // unknown column
		"select=title,authors(name),count()&order=authors(name)": "FBRST126", // with aggregates
	} {
		v, _ := url.ParseQuery(qs)
		p, err := ParseQuery(v, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = BuildRead(sc, sc.Relations["books"], p, 0)
		if e, ok := err.(*Error); !ok || e.Code != code {
			t.Errorf("%s: got %v, want %s", qs, err, code)
		}
	}
	// to-many: an author has many books
	v, _ := url.ParseQuery("select=name,books(title)&order=books(title)")
	p, _ := ParseQuery(v, nil)
	_, err := BuildRead(sc, sc.Relations["authors"], p, 0)
	if e, ok := err.(*Error); !ok || e.Code != "FBRST126" {
		t.Errorf("to-many: got %v", err)
	}
}

func TestPrepareLimitedWrite(t *testing.T) {
	// not limited: order is dropped, nothing else changes
	n := build(t, "books", "order=title")
	if err := PrepareLimitedWrite(n); err != nil || n.Order != nil {
		t.Errorf("unlimited: %v %+v", err, n.Order)
	}

	// limited: the primary key is appended as a tie-breaker, once
	n = build(t, "books", "order=author_id.desc&limit=2")
	if err := PrepareLimitedWrite(n); err != nil {
		t.Fatal(err)
	}
	b := &Builder{}
	if err := b.WriteOrder(n, "t0"); err != nil {
		t.Fatal(err)
	}
	b.WriteRows(n)
	if got := b.String(); got != ` ORDER BY t0."AUTHOR_ID" DESC, t0."ID" ROWS 2` {
		t.Errorf("got %s", got)
	}
	n = build(t, "books", "order=id.desc&offset=3&limit=2")
	if err := PrepareLimitedWrite(n); err != nil {
		t.Fatal(err)
	}
	b = &Builder{}
	_ = b.WriteOrder(n, "t0")
	b.WriteRows(n)
	if got := b.String(); got != ` ORDER BY t0."ID" DESC ROWS 4 TO 5` {
		t.Errorf("got %s", got)
	}

	for qs, msg := range map[string]string{
		"limit=2":           "needs an explicit order",
		"order=id&offset=2": "needs a limit",
		"select=id,authors(name)&order=authors(name)&limit=1": "its own columns",
	} {
		n := build(t, "books", qs)
		err := PrepareLimitedWrite(n)
		if e, ok := err.(*Error); !ok || e.Code != "FBRST127" || !strings.Contains(e.Message, msg) {
			t.Errorf("%s: got %v", qs, err)
		}
	}
}
