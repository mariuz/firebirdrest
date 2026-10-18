package server

import (
	"net/http"

	"github.com/mariuz/firebirdrest/internal/schema"
)

// Version is set at build time with -ldflags "-X .../server.Version=v1.2.3".
var Version = "dev"

type m = map[string]any

func columnSchema(c *schema.Column) m {
	typ, format := c.OpenAPIType()
	out := m{"type": typ, "description": c.SQLType()}
	if format != "" {
		out["format"] = format
	}
	if c.Nullable {
		out["nullable"] = true
	}
	if c.Identity || c.Computed {
		out["readOnly"] = c.Computed
	}
	if (c.FieldType == schema.TVarchar || c.FieldType == schema.TChar) && c.CharLength > 0 {
		out["maxLength"] = c.CharLength
	}
	return out
}

func relationSchema(r *schema.Relation, sc *schema.Schema) m {
	props := m{}
	var required []string
	for _, c := range r.Columns {
		cs := columnSchema(c)
		for _, pk := range r.PrimaryKey {
			if pk == c.DBName {
				cs["description"] = cs["description"].(string) + " (primary key)"
			}
		}
		for _, fk := range sc.ForeignKeys {
			if fk.From == r && len(fk.FromCols) == 1 && fk.FromCols[0] == c.DBName {
				cs["description"] = cs["description"].(string) + " (references " + fk.To.Name + "." + schema.APIName(fk.ToCols[0]) + ")"
			}
		}
		props[c.Name] = cs
		if !c.Nullable && !c.HasDefault && !c.Identity && !c.Computed {
			required = append(required, c.Name)
		}
	}
	out := m{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	if r.Description != "" {
		out["description"] = r.Description
	}
	return out
}

var commonParams = m{
	"select": m{"name": "select", "in": "query", "schema": m{"type": "string"},
		"description": "Columns and embedded resources, e.g. id,name,orders(id,total)"},
	"order": m{"name": "order", "in": "query", "schema": m{"type": "string"},
		"description": "Ordering, e.g. name.asc,created.desc.nullslast"},
	"limit":  m{"name": "limit", "in": "query", "schema": m{"type": "integer"}},
	"offset": m{"name": "offset", "in": "query", "schema": m{"type": "integer"}},
	"range":  m{"name": "Range", "in": "header", "schema": m{"type": "string"}, "description": "e.g. 0-24"},
	"prefer": m{"name": "Prefer", "in": "header", "schema": m{"type": "string"},
		"description": "return=representation|minimal, count=exact, resolution=merge-duplicates|ignore-duplicates, tx=rollback"},
	"on_conflict": m{"name": "on_conflict", "in": "query", "schema": m{"type": "string"}},
	"columns":     m{"name": "columns", "in": "query", "schema": m{"type": "string"}},
}

func ref(name string) m { return m{"$ref": "#/components/parameters/" + name} }

func filterParams(r *schema.Relation) []any {
	out := []any{}
	for _, c := range r.Columns {
		out = append(out, m{"name": c.Name, "in": "query", "required": false,
			"schema": m{"type": "string"}, "description": "Filter, e.g. eq.1, gt.5, in.(1,2), ilike.*abc*, is.null"})
	}
	return out
}

func (s *Server) openAPI(w http.ResponseWriter, r *http.Request, sc *schema.Schema) error {
	rc := &reqCtx{w: w, r: r}
	if _, err := negotiate(r, mimeOpenAPI, mimeJSON); err != nil {
		return err
	}
	server := s.cfg.OpenAPIProxyURI
	if server == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		server = scheme + "://" + r.Host
	}
	schemas := m{}
	paths := m{}
	jsonBody := func(name string) m {
		return m{"content": m{"application/json": m{"schema": m{"oneOf": []any{
			m{"$ref": "#/components/schemas/" + name},
			m{"type": "array", "items": m{"$ref": "#/components/schemas/" + name}},
		}}}}}
	}
	okArray := func(name string) m {
		return m{"description": "OK", "content": m{"application/json": m{"schema": m{
			"type": "array", "items": m{"$ref": "#/components/schemas/" + name}}}}}
	}
	for _, name := range sc.RelationNames() {
		rel := sc.Relations[name]
		schemas[name] = relationSchema(rel, sc)
		filters := filterParams(rel)
		get := m{"tags": []string{name}, "summary": "Read " + name,
			"parameters": append(append([]any{}, filters...), ref("select"), ref("order"), ref("limit"), ref("offset"), ref("range"), ref("prefer")),
			"responses":  m{"200": okArray(name), "206": m{"description": "Partial Content"}}}
		item := m{"get": get}
		if !rel.IsView || true {
			item["post"] = m{"tags": []string{name}, "summary": "Insert into " + name,
				"parameters":  []any{ref("select"), ref("columns"), ref("on_conflict"), ref("prefer")},
				"requestBody": jsonBody(name), "responses": m{"201": m{"description": "Created"}}}
			item["patch"] = m{"tags": []string{name}, "summary": "Update " + name,
				"parameters":  append(append([]any{}, filters...), ref("select"), ref("prefer")),
				"requestBody": m{"content": m{"application/json": m{"schema": m{"$ref": "#/components/schemas/" + name}}}},
				"responses":   m{"200": okArray(name), "204": m{"description": "No Content"}}}
			item["delete"] = m{"tags": []string{name}, "summary": "Delete from " + name,
				"parameters": append(append([]any{}, filters...), ref("select"), ref("prefer")),
				"responses":  m{"200": okArray(name), "204": m{"description": "No Content"}}}
			if len(rel.PrimaryKey) > 0 {
				item["put"] = m{"tags": []string{name}, "summary": "Upsert one row of " + name + " by primary key",
					"parameters":  append(append([]any{}, filters...), ref("prefer")),
					"requestBody": m{"content": m{"application/json": m{"schema": m{"$ref": "#/components/schemas/" + name}}}},
					"responses":   m{"200": okArray(name), "204": m{"description": "No Content"}}}
			}
		}
		paths["/"+name] = item
	}
	for _, name := range sc.RoutineNames() {
		rt := sc.Routines[name]
		props := m{}
		var qp []any
		for _, a := range rt.In {
			props[a.Name] = columnSchema(a)
			qp = append(qp, m{"name": a.Name, "in": "query", "schema": m{"type": "string"}, "description": a.SQLType()})
		}
		outProps := m{}
		for _, o := range rt.Out {
			outProps[o.Name] = columnSchema(o)
		}
		var resp m
		switch rt.Kind {
		case schema.Selectable:
			resp = m{"type": "array", "items": m{"type": "object", "properties": outProps}}
		case schema.Function:
			if len(rt.Out) > 0 {
				resp = columnSchema(rt.Out[0])
			}
		default:
			resp = m{"type": "object", "properties": outProps}
		}
		desc := rt.Description
		if desc == "" {
			desc = rt.Kind + " " + map[bool]string{true: "function", false: "procedure"}[rt.Kind == schema.Function]
		}
		responses := m{"200": m{"description": "OK", "content": m{"application/json": m{"schema": resp}}}}
		paths["/rpc/"+name] = m{
			"get": m{"tags": []string{"(rpc) " + name}, "summary": desc, "parameters": qp, "responses": responses},
			"post": m{"tags": []string{"(rpc) " + name}, "summary": desc,
				"requestBody": m{"content": m{"application/json": m{"schema": m{"type": "object", "properties": props}}}},
				"responses":   responses},
		}
	}
	doc := m{
		"openapi": "3.0.3",
		"info": m{"title": "FirebirdREST API", "version": Version,
			"description": "REST API generated from a Firebird " + sc.Version + " database by FirebirdREST"},
		"servers":    []any{m{"url": server}},
		"paths":      paths,
		"components": m{"schemas": schemas, "parameters": commonParams},
	}
	body, err := jsonMarshal(doc)
	if err != nil {
		return err
	}
	writeBody(rc, http.StatusOK, mimeOpenAPI, body)
	return nil
}
