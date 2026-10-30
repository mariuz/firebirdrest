package server

import (
	"encoding/json"
	"testing"

	"github.com/mariuz/firebirdrest/internal/config"
)

// TestOpenAPIExtension checks the x-firebirdrest data client generators
// (firebirdrest-js gen types) read: relations' kinds, keys and computed
// fields, routines' kinds and arguments, and the exposed schemas.
func TestOpenAPIExtension(t *testing.T) {
	ts := testServerWith(t, func(c *config.Config) { c.OpenAPIMode = "ignore-privileges" })
	r := do(t, ts, "GET", "/", "")
	var doc struct {
		Info struct {
			Ext struct {
				Schema  string   `json:"schema"`
				Schemas []string `json:"schemas"`
			} `json:"x-firebirdrest"`
		} `json:"info"`
		Paths      map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Ext struct {
					Kind        string   `json:"kind"`
					PrimaryKey  []string `json:"primaryKey"`
					Computed    []string `json:"computedFields"`
					ForeignKeys []struct {
						Name               string   `json:"name"`
						Columns            []string `json:"columns"`
						ReferencedRelation string   `json:"referencedRelation"`
						ReferencedColumns  []string `json:"referencedColumns"`
						IsOneToOne         bool     `json:"isOneToOne"`
					} `json:"foreignKeys"`
				} `json:"x-firebirdrest"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(r.body), &doc); err != nil {
		t.Fatal(err)
	}
	books := doc.Components.Schemas["books"].Ext
	if books.Kind != "table" || len(books.PrimaryKey) != 1 || books.PrimaryKey[0] != "id" {
		t.Errorf("books: %+v", books)
	}
	if len(books.ForeignKeys) != 1 || books.ForeignKeys[0].ReferencedRelation != "authors" ||
		books.ForeignKeys[0].Columns[0] != "author_id" || books.ForeignKeys[0].ReferencedColumns[0] != "id" || books.ForeignKeys[0].IsOneToOne {
		t.Errorf("books keys: %+v", books.ForeignKeys)
	}
	found := map[string]bool{}
	for _, c := range books.Computed {
		found[c] = true
	}
	if !found["label"] || !found["price_with_vat"] || found["similar_books"] {
		t.Errorf("books computed fields: %v", books.Computed)
	}
	if bt := doc.Components.Schemas["book_tags"].Ext; len(bt.ForeignKeys) != 2 || len(bt.PrimaryKey) != 2 {
		t.Errorf("book_tags: %+v", bt)
	}
	if v := doc.Components.Schemas["books_with_authors"].Ext; v.Kind != "view" {
		t.Errorf("view kind: %+v", v)
	}
	var po struct {
		Ext struct {
			Kind string `json:"kind"`
			Args []struct {
				Name       string `json:"name"`
				HasDefault bool   `json:"hasDefault"`
			} `json:"args"`
		} `json:"x-firebirdrest"`
	}
	json.Unmarshal(doc.Paths["/rpc/place_order"], &po)
	if po.Ext.Kind != "executable" || len(po.Ext.Args) != 3 || po.Ext.Args[0].HasDefault || !po.Ext.Args[2].HasDefault || po.Ext.Args[2].Name != "qty" {
		t.Errorf("place_order: %+v", po.Ext)
	}
	json.Unmarshal(doc.Paths["/rpc/books_by_country"], &po)
	if po.Ext.Kind != "selectable" {
		t.Errorf("books_by_country: %+v", po.Ext)
	}
	if doc.Info.Ext.Schemas == nil {
		t.Errorf("info extension: %+v", doc.Info.Ext)
	}
}
