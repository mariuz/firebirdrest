package query

import (
	"net/url"
	"reflect"
	"testing"
)

func TestParseSelect(t *testing.T) {
	items, err := ParseSelect(`id,full:name,price::text,author:authors!fk_author!inner(name,books(title)),"Odd Col"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 5 {
		t.Fatalf("got %d items", len(items))
	}
	if items[1].Alias != "full" || items[1].Name != "name" {
		t.Errorf("alias: %+v", items[1])
	}
	if items[2].Cast != "text" || items[2].Name != "price" {
		t.Errorf("cast: %+v", items[2])
	}
	e := items[3]
	if !e.Embed || e.Alias != "author" || e.Name != "authors" || e.Hint != "fk_author" || !e.Inner {
		t.Errorf("embed: %+v", e)
	}
	if len(e.Children) != 2 || !e.Children[1].Embed || e.Children[1].Children[0].Name != "title" {
		t.Errorf("nested: %+v", e.Children)
	}
	if items[4].Name != "Odd Col" {
		t.Errorf("quoted: %+v", items[4])
	}
}

func TestParseSelectErrors(t *testing.T) {
	for _, s := range []string{"a,(b)", "x!inner", "a(b"} {
		if _, err := ParseSelect(s); err == nil {
			t.Errorf("%q: expected error", s)
		}
	}
}

func TestParseFilter(t *testing.T) {
	cases := []struct {
		raw  string
		want Filter
	}{
		{"eq.5", Filter{Field: "f", Op: "eq", Value: "5"}},
		{"not.ilike.*bird*", Filter{Field: "f", Negate: true, Op: "ilike", Value: "*bird*"}},
		{"in.(1,2,\"a,b\")", Filter{Field: "f", Op: "in", Value: "(1,2,\"a,b\")", List: []string{"1", "2", "a,b"}}},
		{"is.NULL", Filter{Field: "f", Op: "is", Value: "null"}},
		{"eq.a.b.c", Filter{Field: "f", Op: "eq", Value: "a.b.c"}},
	}
	for _, c := range cases {
		got, err := ParseFilter("f", c.raw)
		if err != nil {
			t.Errorf("%s: %v", c.raw, err)
			continue
		}
		if !reflect.DeepEqual(*got, c.want) {
			t.Errorf("%s: got %+v want %+v", c.raw, *got, c.want)
		}
	}
	for _, bad := range []string{"5", "foo.5", "in.1,2", "is.maybe"} {
		if _, err := ParseFilter("f", bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestParseLogic(t *testing.T) {
	n, err := ParseLogic("or", false, "(age.lt.18,and(name.eq.\"a,b\",active.not.is.true),not.or(x.eq.1,y.eq.2))")
	if err != nil {
		t.Fatal(err)
	}
	if n.Op != "or" || len(n.Children) != 3 {
		t.Fatalf("got %+v", n)
	}
	if f := n.Children[0].Filter; f.Field != "age" || f.Op != "lt" || f.Value != "18" {
		t.Errorf("first: %+v", f)
	}
	and := n.Children[1]
	if and.Op != "and" || and.Children[0].Filter.Value != "a,b" || !and.Children[1].Filter.Negate {
		t.Errorf("and: %+v", and)
	}
	if !n.Children[2].Negate || n.Children[2].Op != "or" {
		t.Errorf("not.or: %+v", n.Children[2])
	}
}

func TestParseOrder(t *testing.T) {
	got, err := ParseOrder("name,price.desc.nullslast,id.asc.nullsfirst")
	if err != nil {
		t.Fatal(err)
	}
	want := []OrderTerm{{Field: "name"}, {Field: "price", Desc: true, Nulls: "last"}, {Field: "id", Nulls: "first"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
	if _, err := ParseOrder("id.sideways"); err == nil {
		t.Error("expected error")
	}
}

func TestParseQuery(t *testing.T) {
	v, _ := url.ParseQuery("select=id,orders(id)&id=gt.1&id=lt.9&orders.status=eq.new&orders.order=id.desc&orders.limit=2&limit=10&offset=5&not.or=(a.eq.1,b.eq.2)&country=UK")
	p, err := ParseQuery(v, func(k string) bool { return k == "country" })
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Where[""]) != 3 { // id twice + not.or
		t.Errorf("root where: %+v", p.Where[""])
	}
	if len(p.Where["orders"]) != 1 || p.Order["orders"][0].Field != "id" || p.Limit["orders"] != 2 {
		t.Errorf("embedded params: %+v", p)
	}
	if p.Limit[""] != 10 || p.Offset[""] != 5 {
		t.Errorf("paging: %+v %+v", p.Limit, p.Offset)
	}
	if p.Args["country"] != "UK" {
		t.Errorf("args: %+v", p.Args)
	}
	var neg bool
	for _, n := range p.Where[""] {
		if n.Filter == nil && n.Op == "or" && n.Negate {
			neg = true
		}
	}
	if !neg {
		t.Error("not.or was not parsed")
	}
	if _, err := ParseQuery(url.Values{"limit": {"-1"}}, nil); err == nil {
		t.Error("negative limit accepted")
	}
}
