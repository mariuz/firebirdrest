package server

import (
	"context"
	"database/sql"
	"github.com/mariuz/firebirdrest/internal/query"
	"net/http"
	"strings"

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
				ref := fk.To.Name
				if fk.To.Schema != r.Schema {
					ref = schema.APIName(fk.To.Schema) + "." + ref
				}
				cs["description"] = cs["description"].(string) + " (references " + ref + "." + schema.APIName(fk.ToCols[0]) + ")"
			}
		}
		props[c.Name] = cs
		if !c.Nullable && !c.HasDefault && !c.Identity && !c.Computed {
			required = append(required, c.Name)
		}
	}
	for _, rt := range r.Computed {
		switch rt.Kind {
		case schema.Function:
			cs := columnSchema(rt.Out[0])
			cs["readOnly"] = true
			cs["description"] = cs["description"].(string) + " (computed by function " + rt.Name + ")"
			props[rt.Name] = cs
		default:
			props[rt.Name] = m{"readOnly": true, "description": "computed relationship (" + rt.Kind +
				" procedure " + rt.Name + "); embed with select=" + rt.Name + "(...)"}
		}
	}
	out := m{"type": "object", "properties": props, "x-firebirdrest": relationExt(r, sc)}
	if len(required) > 0 {
		out["required"] = required
	}
	if r.Description != "" {
		out["description"] = r.Description
	}
	return out
}

// relationExt is the x-firebirdrest extension of a table or view: what a
// client generator needs beyond the JSON schema, in API names. foreignKeys
// lists the keys this relation holds, as PostgREST's Relationships do; the
// reverse direction is read from the other relation's keys.
func relationExt(r *schema.Relation, sc *schema.Schema) m {
	kind := "table"
	if r.IsView {
		kind = "view"
	}
	apiNames := func(db []string) []string {
		out := make([]string, len(db))
		for i, n := range db {
			out[i] = schema.APIName(n)
		}
		return out
	}
	unique := func(cols []string) bool {
		same := func(a, b []string) bool {
			if len(a) != len(b) {
				return false
			}
			seen := map[string]bool{}
			for _, x := range a {
				seen[x] = true
			}
			for _, x := range b {
				if !seen[x] {
					return false
				}
			}
			return true
		}
		if len(r.PrimaryKey) > 0 && same(cols, r.PrimaryKey) {
			return true
		}
		for _, u := range r.Uniques {
			if same(cols, u) {
				return true
			}
		}
		return false
	}
	fks := []any{}
	for _, fk := range sc.ForeignKeys {
		if fk.From != r {
			continue
		}
		k := m{"name": fk.Name, "columns": apiNames(fk.FromCols), "referencedRelation": fk.To.Name,
			"referencedColumns": apiNames(fk.ToCols), "isOneToOne": unique(fk.FromCols)}
		if fk.To.Schema != r.Schema {
			k["referencedSchema"] = schema.APIName(fk.To.Schema)
		}
		fks = append(fks, k)
	}
	computed := []string{}
	for _, rt := range r.Computed {
		if rt.Kind == schema.Function {
			computed = append(computed, rt.Name)
		}
	}
	return m{"kind": kind, "primaryKey": apiNames(r.PrimaryKey), "foreignKeys": fks, "computedFields": computed}
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

// serveOpenAPI answers GET / according to openapi-mode: the document lists
// what the caller's role may use (follow-privileges), everything in the
// schema cache (ignore-privileges), or nothing (disabled).
func (s *Server) serveOpenAPI(w http.ResponseWriter, r *http.Request, sc *schema.Schema) error {
	mode := s.cfg.OpenAPIMode
	if mode == "" {
		mode = "follow-privileges"
	}
	if mode == "disabled" {
		return &query.Error{Status: 404, Code: "FBRST125", Message: "the OpenAPI document is disabled (openapi-mode)"}
	}
	var allow *privileges
	if mode == "follow-privileges" {
		rc := &reqCtx{w: w, r: r, sc: sc}
		if err := s.authenticate(rc); err != nil {
			return err
		}
		p, err := s.pool(r.Context(), rc.role)
		if err != nil {
			return err
		}
		if allow, err = loadPrivileges(r.Context(), p.db, rc.role); err != nil {
			return err
		}
	}
	return s.openAPI(w, r, sc, allow)
}

// privileges holds what a role may do, from RDB$USER_PRIVILEGES: the
// privilege letters (S, I, U, D) per table or view, and the procedures,
// functions and packages it may execute. Grants are matched by object name
// only; on Firebird 6 a same-named object in another schema would count too,
// which can only over-list, never grant anything.
type privileges struct {
	relations map[string]string
	routines  map[string]bool
}

func loadPrivileges(ctx context.Context, db *sql.DB, role string) (*privileges, error) {
	rows, err := db.QueryContext(ctx, `SELECT RDB$PRIVILEGE, RDB$OBJECT_TYPE, TRIM(RDB$RELATION_NAME)
		FROM RDB$USER_PRIVILEGES
		WHERE RDB$FIELD_NAME IS NULL AND (UPPER(RDB$USER) = UPPER(?) OR RDB$USER = 'PUBLIC' OR RDB$USER = CURRENT_USER)`, role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p := &privileges{relations: map[string]string{}, routines: map[string]bool{}}
	for rows.Next() {
		var priv, name string
		var typ int
		if err := rows.Scan(&priv, &typ, &name); err != nil {
			return nil, err
		}
		priv = strings.TrimSpace(priv)
		switch typ {
		case 0: // table or view
			if strings.ContainsAny(priv, "SIUD") {
				p.relations[name] += priv
			}
		case 5, 15, 18: // procedure, function, package
			if priv == "X" {
				p.routines[name] = true
			}
		}
	}
	return p, rows.Err()
}

func (p *privileges) relation(rel *schema.Relation) string {
	if p == nil {
		return "SIUD"
	}
	return p.relations[rel.DBName]
}

func (p *privileges) routine(rt *schema.Routine) bool {
	if p == nil {
		return true
	}
	if rt.Package != "" {
		return p.routines[rt.Package]
	}
	return p.routines[rt.DBName]
}

func (s *Server) openAPI(w http.ResponseWriter, r *http.Request, sc *schema.Schema, allow *privileges) error {
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
		privs := allow.relation(rel)
		if privs == "" {
			continue
		}
		schemas[name] = relationSchema(rel, sc)
		filters := filterParams(rel)
		item := m{}
		if strings.Contains(privs, "S") {
			item["get"] = m{"tags": []string{name}, "summary": "Read " + name,
				"parameters": append(append([]any{}, filters...), ref("select"), ref("order"), ref("limit"), ref("offset"), ref("range"), ref("prefer")),
				"responses":  m{"200": okArray(name), "206": m{"description": "Partial Content"}}}
		}
		if strings.Contains(privs, "I") {
			item["post"] = m{"tags": []string{name}, "summary": "Insert into " + name,
				"parameters":  []any{ref("select"), ref("columns"), ref("on_conflict"), ref("prefer")},
				"requestBody": jsonBody(name), "responses": m{"201": m{"description": "Created"}}}
		}
		if strings.Contains(privs, "U") {
			item["patch"] = m{"tags": []string{name}, "summary": "Update " + name,
				"parameters":  append(append([]any{}, filters...), ref("select"), ref("prefer")),
				"requestBody": m{"content": m{"application/json": m{"schema": m{"$ref": "#/components/schemas/" + name}}}},
				"responses":   m{"200": okArray(name), "204": m{"description": "No Content"}}}
		}
		if strings.Contains(privs, "D") {
			item["delete"] = m{"tags": []string{name}, "summary": "Delete from " + name,
				"parameters": append(append([]any{}, filters...), ref("select"), ref("prefer")),
				"responses":  m{"200": okArray(name), "204": m{"description": "No Content"}}}
		}
		{
			if len(rel.PrimaryKey) > 0 && strings.Contains(privs, "I") && strings.Contains(privs, "U") {
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
		if !allow.routine(rt) {
			continue
		}
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
			desc = "function"
			if rt.Kind != schema.Function {
				desc = rt.Kind + " procedure"
			}
		}
		args := []any{}
		for _, a := range rt.In {
			args = append(args, m{"name": a.Name, "hasDefault": a.HasDefault})
		}
		responses := m{"200": m{"description": "OK", "content": m{"application/json": m{"schema": resp}}}}
		paths["/rpc/"+name] = m{
			"x-firebirdrest": m{"kind": rt.Kind, "args": args},
			"get":            m{"tags": []string{"(rpc) " + name}, "summary": desc, "parameters": qp, "responses": responses},
			"post": m{"tags": []string{"(rpc) " + name}, "summary": desc,
				"requestBody": m{"content": m{"application/json": m{"schema": m{"type": "object", "properties": props}}}},
				"responses":   responses},
		}
	}
	exposed := []string{}
	if cat := s.catalog.Load(); cat != nil && sc.Name != "" {
		exposed = cat.Names()
	}
	doc := m{
		"openapi": "3.0.3",
		"info": m{"title": "FirebirdREST API", "version": Version,
			"description": "REST API generated from a Firebird " + sc.Version + " database by FirebirdREST",
			// for client generators: the schema this document describes ("" before
			// Firebird 6) and the exposed schemas, each read with Accept-Profile
			"x-firebirdrest": m{"schema": sc.Name, "schemas": exposed, "firebird": sc.Version}},
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
