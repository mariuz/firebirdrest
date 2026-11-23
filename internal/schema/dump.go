package schema

import (
	"encoding/json"
	"sort"
)

// Dump returns the schema cache as indented JSON, as -dump-schema prints
// it: per schema its relations with their columns and keys, the foreign keys
// (inferred view keys marked viaView) and the routines. The layout is meant
// for people and diffs, not for programs.
func (c *Catalog) Dump() ([]byte, error) {
	type col struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Nullable bool   `json:"nullable"`
		Default  bool   `json:"hasDefault,omitempty"`
		Identity bool   `json:"identity,omitempty"`
		Computed bool   `json:"computed,omitempty"`
	}
	type rel struct {
		Name       string     `json:"name"`
		Kind       string     `json:"kind"`
		Columns    []col      `json:"columns"`
		PrimaryKey []string   `json:"primaryKey,omitempty"`
		Uniques    [][]string `json:"uniques,omitempty"`
		Computed   []string   `json:"computed,omitempty"`
		Response   bool       `json:"setsResponse,omitempty"`
	}
	type fk struct {
		Name     string   `json:"name"`
		From     string   `json:"from"`
		FromCols []string `json:"fromColumns"`
		To       string   `json:"to"`
		ToCols   []string `json:"toColumns"`
		ViaView  bool     `json:"viaView,omitempty"`
	}
	type routine struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		In       []col  `json:"in"`
		Out      []col  `json:"out"`
		Parent   string `json:"computedFor,omitempty"`
		Response bool   `json:"setsResponse,omitempty"`
	}
	type sch struct {
		Name               string    `json:"name,omitempty"`
		Relations          []rel     `json:"relations"`
		ForeignKeys        []fk      `json:"foreignKeys"`
		Routines           []routine `json:"routines"`
		UsesRequestContext bool      `json:"usesRequestContext"`
		SetsResponse       bool      `json:"setsResponse"`
	}
	cols := func(cs []*Column) []col {
		out := make([]col, 0, len(cs))
		for _, c := range cs {
			out = append(out, col{c.Name, c.SQLType(), c.Nullable, c.HasDefault, c.Identity, c.Computed})
		}
		return out
	}
	qualified := func(r *Relation) string {
		if r == nil {
			return ""
		}
		if r.Schema != "" {
			return APIName(r.Schema) + "." + r.Name
		}
		return r.Name
	}
	out := struct {
		Version string `json:"version"`
		Schemas []sch  `json:"schemas"`
	}{Version: c.Version}
	for _, s := range c.Schemas {
		d := sch{Name: s.Name, UsesRequestContext: s.UsesRequestContext, SetsResponse: s.SetsResponse,
			Relations: []rel{}, ForeignKeys: []fk{}, Routines: []routine{}}
		for _, r := range s.Relations {
			kind := "table"
			if r.IsView {
				kind = "view"
			}
			x := rel{Name: r.Name, Kind: kind, Columns: cols(r.Columns), PrimaryKey: r.PrimaryKey, Uniques: r.Uniques, Response: r.SetsResponse}
			for _, rt := range r.Computed {
				x.Computed = append(x.Computed, rt.Name)
			}
			d.Relations = append(d.Relations, x)
		}
		sort.Slice(d.Relations, func(i, j int) bool { return d.Relations[i].Name < d.Relations[j].Name })
		for _, k := range s.ForeignKeys {
			d.ForeignKeys = append(d.ForeignKeys, fk{k.Name, qualified(k.From), k.FromCols, qualified(k.To), k.ToCols, k.ViaView})
		}
		sort.SliceStable(d.ForeignKeys, func(i, j int) bool {
			a, b := d.ForeignKeys[i], d.ForeignKeys[j]
			if a.From != b.From {
				return a.From < b.From
			}
			if a.To != b.To {
				return a.To < b.To
			}
			return a.Name < b.Name
		})
		for _, rt := range s.Routines {
			x := routine{Name: rt.Name, Kind: rt.Kind, In: cols(rt.In), Out: cols(rt.Out), Response: rt.SetsResponse}
			if rt.Parent != nil {
				x.Parent = rt.Parent.Name
			}
			d.Routines = append(d.Routines, x)
		}
		sort.Slice(d.Routines, func(i, j int) bool { return d.Routines[i].Name < d.Routines[j].Name })
		out.Schemas = append(out.Schemas, d)
	}
	return json.MarshalIndent(out, "", "  ")
}
