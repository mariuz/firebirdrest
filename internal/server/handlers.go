package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mariuz/firebirdrest/internal/query"
	"github.com/mariuz/firebirdrest/internal/schema"
)

var urlParseQuery = url.ParseQuery

func (s *Server) params(rc *reqCtx, isArg func(string) bool) (*query.Params, error) {
	p, err := query.ParseQuery(rc.r.URL.Query(), isArg)
	if err != nil {
		return nil, err
	}
	p.NoAggregates = !s.cfg.DBAggregates
	return p, nil
}

// applyRange applies a "Range: 0-24" header to the root node.
func applyRange(r *http.Request, n *query.ReadNode, p *query.Params) error {
	h := r.Header.Get("Range")
	if h == "" {
		return nil
	}
	h = strings.TrimPrefix(strings.TrimSpace(h), "items=")
	from, to, ok := strings.Cut(h, "-")
	start, err := strconv.Atoi(strings.TrimSpace(from))
	if !ok || err != nil || start < 0 {
		return &query.Error{Status: 416, Code: "FBRST103", Message: "invalid Range header", Hint: "use Range: 0-24"}
	}
	if _, set := p.Offset[""]; !set {
		n.Offset = start
	}
	if strings.TrimSpace(to) != "" {
		end, err := strconv.Atoi(strings.TrimSpace(to))
		if err != nil || end < start {
			return &query.Error{Status: 416, Code: "FBRST103", Message: "invalid Range header"}
		}
		if l := end - start + 1; n.Limit < 0 || l < n.Limit {
			n.Limit = l
		}
	}
	return nil
}

func contentRange(offset, n int, total int64) string {
	t := "*"
	if total >= 0 {
		t = strconv.FormatInt(total, 10)
	}
	if n == 0 {
		return "*/" + t
	}
	return fmt.Sprintf("%d-%d/%s", offset, offset+n-1, t)
}

// readNode builds the read tree, failing on root-only parameters misuse.
func (s *Server) readNode(rc *reqCtx, rel *schema.Relation, p *query.Params) (*query.ReadNode, error) {
	return query.BuildRead(rc.sc, rel, p, s.cfg.DBMaxRows)
}

func (s *Server) read(rc *reqCtx, rel *schema.Relation) error {
	p, err := s.params(rc, nil)
	if err != nil {
		return err
	}
	n, err := s.readNode(rc, rel, p)
	if err != nil {
		return err
	}
	if err := applyRange(rc.r, n, p); err != nil {
		return err
	}
	return s.respondRead(rc, n)
}

// respondRead runs a read node and writes rows, Content-Range and status.
func (s *Server) respondRead(rc *reqCtx, n *query.ReadNode) error {
	ctx := rc.r.Context()
	rows, err := query.Read(ctx, rc.tx, n, nil)
	if err != nil {
		return err
	}
	total := int64(-1)
	if rc.prefer.count != "" {
		if total, err = query.Count(ctx, rc.tx, n); err != nil {
			return err
		}
	}
	rc.w.Header().Set("Content-Range", contentRange(n.Offset, len(rows), total))
	rc.w.Header().Set("Range-Unit", "items")
	status := http.StatusOK
	if total >= 0 && int64(n.Offset+len(rows)) < total || (total >= 0 && n.Offset > 0) {
		status = http.StatusPartialContent
	}
	if total >= 0 && n.Offset > 0 && int64(n.Offset) >= total && total > 0 {
		return &query.Error{Status: 416, Code: "FBRST103", Message: "requested range not satisfiable",
			Details: fmt.Sprintf("offset %d is beyond the %d available rows", n.Offset, total)}
	}
	return writeRows(rc, status, rows)
}

// rowValues validates body keys against the relation and coerces values.
func rowValues(rel *schema.Relation, row map[string]any, only []string, missingDefault bool) ([]*schema.Column, []any, error) {
	var cols []*schema.Column
	var vals []any
	add := func(name string, v any, present bool) error {
		c := rel.Column(name)
		if c == nil {
			return &query.Error{Status: 400, Code: "FBRST204",
				Message: fmt.Sprintf("could not find the %q column of %q", name, rel.Name)}
		}
		if !present {
			if missingDefault {
				return nil
			}
			v = nil
		}
		cv, err := query.CoerceJSON(c, v)
		if err != nil {
			return err
		}
		cols = append(cols, c)
		vals = append(vals, cv)
		return nil
	}
	if only != nil {
		for _, name := range only {
			v, ok := row[name]
			if err := add(name, v, ok); err != nil {
				return nil, nil, err
			}
		}
		return cols, vals, nil
	}
	// keep column order of the table for stable SQL
	seen := 0
	for _, c := range rel.Columns {
		if v, ok := row[c.Name]; ok {
			if err := add(c.Name, v, true); err != nil {
				return nil, nil, err
			}
			seen++
		} else if v, ok := row[c.DBName]; ok && c.DBName != c.Name {
			if err := add(c.DBName, v, true); err != nil {
				return nil, nil, err
			}
			seen++
		}
	}
	if seen != len(row) {
		for k := range row {
			if rel.Column(k) == nil {
				return nil, nil, &query.Error{Status: 400, Code: "FBRST204",
					Message: fmt.Sprintf("could not find the %q column of %q", k, rel.Name)}
			}
		}
	}
	return cols, vals, nil
}

func quoteCols(cols []*schema.Column) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = schema.Quote(c.DBName)
	}
	return strings.Join(q, ", ")
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// returningCols picks the columns RETURNING gives back: the primary key, or all columns.
func returningCols(rel *schema.Relation) []*schema.Column {
	if len(rel.PrimaryKey) > 0 {
		out := make([]*schema.Column, len(rel.PrimaryKey))
		for i, pk := range rel.PrimaryKey {
			out[i] = rel.Column(pk)
		}
		return out
	}
	return rel.Columns
}

func (s *Server) insert(rc *reqCtx, rel *schema.Relation) error {
	p, err := s.params(rc, nil)
	if err != nil {
		return err
	}
	rows, isArray, err := readBody(rc.r)
	if err != nil {
		return err
	}
	ctx := rc.r.Context()
	retCols := returningCols(rel)
	var keys [][]any
	var returned []*query.Obj

	var conflict []*schema.Column
	if rc.prefer.resolution != "" {
		names := p.OnConflict
		if names == nil {
			names = rel.PrimaryKey
		}
		if len(names) == 0 {
			return &query.Error{Status: 400, Code: "FBRST124",
				Message: "upsert needs a primary key or on_conflict=<columns>"}
		}
		for _, n := range names {
			c := rel.Column(n)
			if c == nil {
				return &query.Error{Status: 400, Code: "FBRST204", Message: fmt.Sprintf("on_conflict column %q does not exist", n)}
			}
			conflict = append(conflict, c)
		}
	}

	for _, row := range rows {
		cols, vals, err := rowValues(rel, row, p.Columns, p.Columns == nil || rc.prefer.missing == "default")
		if err != nil {
			return err
		}
		b := &query.Builder{}
		switch rc.prefer.resolution {
		case "merge-duplicates":
			if len(cols) == 0 {
				return &query.Error{Status: 400, Code: "FBRST102", Message: "upsert requires at least one column"}
			}
			b.W("UPDATE OR INSERT INTO ", schema.Quote(rel.DBName), " (", quoteCols(cols), ") VALUES (")
			for i, v := range vals {
				if i > 0 {
					b.W(", ")
				}
				b.Arg(v)
			}
			b.W(") MATCHING (", quoteCols(conflict), ")")
		case "ignore-duplicates":
			b.W("MERGE INTO ", schema.Quote(rel.DBName), " t0 USING RDB$DATABASE ON ")
			for i, c := range conflict {
				if i > 0 {
					b.W(" AND ")
				}
				idx := indexOf(cols, c)
				if idx < 0 {
					return &query.Error{Status: 400, Code: "FBRST102",
						Message: fmt.Sprintf("upsert row is missing conflict column %q", c.Name)}
				}
				b.W("t0.", schema.Quote(c.DBName), " = ")
				b.Arg(vals[idx])
			}
			b.W(" WHEN NOT MATCHED THEN INSERT (", quoteCols(cols), ") VALUES (")
			for i, v := range vals {
				if i > 0 {
					b.W(", ")
				}
				b.Arg(v)
			}
			b.W(")")
		default:
			if len(cols) == 0 {
				b.W("INSERT INTO ", schema.Quote(rel.DBName), " DEFAULT VALUES")
			} else {
				b.W("INSERT INTO ", schema.Quote(rel.DBName), " (", quoteCols(cols), ") VALUES (")
				for i, v := range vals {
					if i > 0 {
						b.W(", ")
					}
					b.Arg(v)
				}
				b.W(")")
			}
		}
		b.W(" RETURNING ", quoteCols(retCols))
		res, err := rc.tx.QueryContext(ctx, b.String(), b.Args...)
		if err != nil {
			return err
		}
		for res.Next() {
			v := make([]any, len(retCols))
			ptrs := make([]any, len(retCols))
			for i := range v {
				ptrs[i] = &v[i]
			}
			if err := res.Scan(ptrs...); err != nil {
				res.Close()
				return err
			}
			keys = append(keys, v)
			o := &query.Obj{}
			for i, c := range retCols {
				o.Set(c.Name, query.Output(v[i], c))
			}
			returned = append(returned, o)
		}
		res.Close()
		if err := res.Err(); err != nil {
			return err
		}
	}

	h := rc.w.Header()
	if len(keys) == 1 && len(rel.PrimaryKey) > 0 {
		var loc []string
		for i, pk := range rel.PrimaryKey {
			loc = append(loc, url.QueryEscape(schema.APIName(pk))+"=eq."+url.QueryEscape(fmt.Sprint(query.Output(keys[0][i], rel.Column(pk)))))
		}
		h.Set("Location", "/"+rel.Name+"?"+strings.Join(loc, "&"))
	}
	if rc.prefer.count != "" {
		h.Set("Content-Range", "*/"+strconv.Itoa(len(keys)))
	}
	if rc.prefer.ret != "representation" {
		writeBody(rc, http.StatusCreated, "", nil)
		return nil
	}
	out, err := s.representation(rc, rel, p, keys, returned)
	if err != nil {
		return err
	}
	_ = isArray
	return writeRows(rc, http.StatusCreated, out)
}

func indexOf(cols []*schema.Column, c *schema.Column) int {
	for i, x := range cols {
		if x == c {
			return i
		}
	}
	return -1
}

// representation re-reads written rows (by primary key) so select= and embeds apply.
func (s *Server) representation(rc *reqCtx, rel *schema.Relation, p *query.Params, keys [][]any, returned []*query.Obj) ([]*query.Obj, error) {
	readParams := &query.Params{Select: p.Select, Where: map[string][]query.Node{}, Order: map[string][]query.OrderTerm{},
		Limit: map[string]int{}, Offset: map[string]int{}}
	for path, w := range p.Where {
		if path != "" {
			readParams.Where[path] = w
		}
	}
	for path, o := range p.Order {
		if path != "" {
			readParams.Order[path] = o
		}
	}
	n, err := query.BuildRead(rc.sc, rel, readParams, 0)
	if err != nil {
		return nil, err
	}
	if len(rel.PrimaryKey) == 0 {
		if len(n.Children) > 0 {
			return nil, &query.Error{Status: 400, Code: "FBRST200",
				Message: "embedding in the response requires a primary key on " + rel.Name}
		}
		out := make([]*query.Obj, len(returned))
		for i, r := range returned {
			o := &query.Obj{}
			for _, f := range n.Fields {
				for j, k := range r.Keys {
					if k == f.Col.Name {
						o.Set(f.Key, r.Vals[j])
					}
				}
			}
			out[i] = o
		}
		return out, nil
	}
	return query.Read(rc.r.Context(), rc.tx, n, &query.KeyFilter{Cols: rel.PrimaryKey, Keys: keys})
}

func (s *Server) update(rc *reqCtx, rel *schema.Relation) error {
	p, err := s.params(rc, nil)
	if err != nil {
		return err
	}
	rows, isArray, err := readBody(rc.r)
	if err != nil {
		return err
	}
	if isArray || len(rows) != 1 {
		return &query.Error{Status: 400, Code: "FBRST102", Message: "PATCH expects a single JSON object"}
	}
	cols, vals, err := rowValues(rel, rows[0], p.Columns, true)
	if err != nil {
		return err
	}
	n, err := query.BuildRead(rc.sc, rel, p, 0)
	if err != nil {
		return err
	}
	if s.cfg.DBSafeUpdates && len(n.Where) == 0 {
		return &query.Error{Status: 400, Code: "FBRST109",
			Message: "PATCH without filters is not allowed", Hint: "add a filter such as ?id=eq.1, or set db-safe-updates = false"}
	}
	ctx := rc.r.Context()
	wantRep := rc.prefer.ret == "representation"
	var keys [][]any
	if wantRep && len(rel.PrimaryKey) > 0 {
		if keys, err = query.Keys(ctx, rc.tx, n, rel.PrimaryKey); err != nil {
			return err
		}
	}
	affected := 0
	if len(cols) > 0 {
		b := &query.Builder{}
		b.W("UPDATE ", schema.Quote(rel.DBName), " t0 SET ")
		for i, c := range cols {
			if i > 0 {
				b.W(", ")
			}
			b.W(schema.Quote(c.DBName), " = ")
			b.Arg(vals[i])
		}
		b.W(" WHERE ")
		ok, err := b.WriteWhere(n, "t0", nil)
		if err != nil {
			return err
		}
		sqlText := b.String()
		if !ok {
			sqlText = strings.TrimSuffix(sqlText, " WHERE ")
		}
		res, err := rc.tx.ExecContext(ctx, sqlText, b.Args...)
		if err != nil {
			return err
		}
		a, _ := res.RowsAffected()
		affected = int(a)
	}
	if rc.prefer.count != "" {
		rc.w.Header().Set("Content-Range", contentRange(0, affected, int64(affected)))
	}
	if !wantRep {
		writeBody(rc, http.StatusNoContent, "", nil)
		return nil
	}
	if len(rel.PrimaryKey) == 0 {
		return &query.Error{Status: 400, Code: "FBRST200",
			Message: "return=representation on PATCH requires a primary key on " + rel.Name}
	}
	// primary key values may have been changed by the update
	for i, pk := range rel.PrimaryKey {
		for j, c := range cols {
			if c.DBName == pk {
				for _, k := range keys {
					k[i] = vals[j]
				}
			}
		}
	}
	out, err := s.representation(rc, rel, p, keys, nil)
	if err != nil {
		return err
	}
	return writeRows(rc, http.StatusOK, out)
}

// put upserts exactly one row identified by eq filters on every primary key column.
func (s *Server) put(rc *reqCtx, rel *schema.Relation) error {
	p, err := s.params(rc, nil)
	if err != nil {
		return err
	}
	if len(rel.PrimaryKey) == 0 {
		return &query.Error{Status: 405, Code: "FBRST105", Message: "PUT requires a primary key on " + rel.Name}
	}
	pkVals := map[string]string{}
	for _, w := range p.Where[""] {
		if w.Filter == nil || w.Filter.Op != "eq" || w.Filter.Negate {
			return &query.Error{Status: 405, Code: "FBRST105", Message: "PUT only accepts eq filters on the primary key"}
		}
		c := rel.Column(w.Filter.Field)
		if c == nil {
			return &query.Error{Status: 400, Code: "FBRST204", Message: fmt.Sprintf("column %q does not exist", w.Filter.Field)}
		}
		pkVals[c.DBName] = w.Filter.Value
	}
	for _, pk := range rel.PrimaryKey {
		if _, ok := pkVals[pk]; !ok || len(pkVals) != len(rel.PrimaryKey) {
			return &query.Error{Status: 405, Code: "FBRST105", Message: "PUT requires eq filters on all primary key columns"}
		}
	}
	rows, isArray, err := readBody(rc.r)
	if err != nil {
		return err
	}
	if isArray || len(rows) != 1 {
		return &query.Error{Status: 400, Code: "FBRST114", Message: "PUT expects a single JSON object"}
	}
	cols, vals, err := rowValues(rel, rows[0], nil, true)
	if err != nil {
		return err
	}
	var key []any
	for _, pk := range rel.PrimaryKey {
		c := rel.Column(pk)
		want, _ := query.Coerce(c, pkVals[pk])
		idx := indexOf(cols, c)
		if idx < 0 {
			return &query.Error{Status: 400, Code: "FBRST115", Message: fmt.Sprintf("body must contain primary key column %q", c.Name)}
		}
		if fmt.Sprint(vals[idx]) != fmt.Sprint(want) {
			return &query.Error{Status: 400, Code: "FBRST115", Message: "payload values do not match URL in primary key column(s)"}
		}
		key = append(key, vals[idx])
	}
	b := &query.Builder{}
	b.W("UPDATE OR INSERT INTO ", schema.Quote(rel.DBName), " (", quoteCols(cols), ") VALUES (")
	for i, v := range vals {
		if i > 0 {
			b.W(", ")
		}
		b.Arg(v)
	}
	b.W(") MATCHING (", quoteCols(returningCols(rel)), ")")
	if _, err := rc.tx.ExecContext(rc.r.Context(), b.String(), b.Args...); err != nil {
		return err
	}
	if rc.prefer.ret != "representation" {
		writeBody(rc, http.StatusNoContent, "", nil)
		return nil
	}
	out, err := s.representation(rc, rel, p, [][]any{key}, nil)
	if err != nil {
		return err
	}
	return writeRows(rc, http.StatusOK, out)
}

func (s *Server) delete(rc *reqCtx, rel *schema.Relation) error {
	p, err := s.params(rc, nil)
	if err != nil {
		return err
	}
	n, err := query.BuildRead(rc.sc, rel, p, 0)
	if err != nil {
		return err
	}
	if s.cfg.DBSafeUpdates && len(n.Where) == 0 {
		return &query.Error{Status: 400, Code: "FBRST109",
			Message: "DELETE without filters is not allowed", Hint: "add a filter such as ?id=eq.1, or set db-safe-updates = false"}
	}
	ctx := rc.r.Context()
	var out []*query.Obj
	if rc.prefer.ret == "representation" {
		n.Limit, n.Offset = -1, 0
		if out, err = query.Read(ctx, rc.tx, n, nil); err != nil {
			return err
		}
	}
	b := &query.Builder{}
	b.W("DELETE FROM ", schema.Quote(rel.DBName), " t0 WHERE ")
	ok, err := b.WriteWhere(n, "t0", nil)
	if err != nil {
		return err
	}
	sqlText := b.String()
	if !ok {
		sqlText = strings.TrimSuffix(sqlText, " WHERE ")
	}
	res, err := rc.tx.ExecContext(ctx, sqlText, b.Args...)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if rc.prefer.count != "" {
		rc.w.Header().Set("Content-Range", contentRange(0, int(affected), affected))
	}
	if rc.prefer.ret != "representation" {
		writeBody(rc, http.StatusNoContent, "", nil)
		return nil
	}
	return writeRows(rc, http.StatusOK, out)
}
