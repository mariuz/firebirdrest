package query

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mariuz/firebirdrest/internal/schema"
)

// Querier is satisfied by *sql.Tx and *sql.DB.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Obj is a JSON object that keeps its key order.
type Obj struct {
	Keys []string
	Vals []any
}

func (o *Obj) Set(k string, v any) {
	for i, key := range o.Keys {
		if key == k {
			o.Vals[i] = v
			return
		}
	}
	o.Keys = append(o.Keys, k)
	o.Vals = append(o.Vals, v)
}

// Get returns the value of key k, or nil.
func (o *Obj) Get(k string) any {
	for i, key := range o.Keys {
		if key == k {
			return o.Vals[i]
		}
	}
	return nil
}

// MarshalJSON writes the object with keys in order.
func (o *Obj) MarshalJSON() ([]byte, error) {
	return AppendJSON(nil, o), nil
}

func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// KeyFilter restricts a root read to specific key tuples and returns rows in that order.
type KeyFilter struct {
	Cols []string
	Keys [][]any
}

const chunkSize = 400

type row struct {
	obj    *Obj
	hidden []any // parent columns needed by each child, flattened
	match  []any
}

// Read runs the read tree and returns the root objects.
func Read(ctx context.Context, q Querier, n *ReadNode, kf *KeyFilter) ([]*Obj, error) {
	if n.Limit == 0 {
		return []*Obj{}, nil
	}
	var rows []*row
	var err error
	if kf != nil {
		if len(kf.Keys) == 0 {
			return []*Obj{}, nil
		}
		for _, chunk := range chunks(kf.Keys, chunkSize/len(kf.Cols)) {
			part, err := queryNode(ctx, q, n, "t0", kf.Cols, chunk, false)
			if err != nil {
				return nil, err
			}
			rows = append(rows, part...)
		}
		// keep the order of the keys
		pos := map[string]int{}
		for i, k := range kf.Keys {
			pos[keyString(k)] = i
		}
		ordered := make([]*row, len(kf.Keys))
		for _, r := range rows {
			if i, ok := pos[keyString(r.match)]; ok && ordered[i] == nil {
				ordered[i] = r
			}
		}
		rows = rows[:0]
		for _, r := range ordered {
			if r != nil {
				rows = append(rows, r)
			}
		}
	} else {
		rows, err = queryNode(ctx, q, n, "t0", nil, nil, true)
		if err != nil {
			return nil, err
		}
	}
	if err := attachChildren(ctx, q, n, rows); err != nil {
		return nil, err
	}
	out := make([]*Obj, len(rows))
	for i, r := range rows {
		out[i] = r.obj
	}
	return out, nil
}

// Count returns the number of rows matching the root conditions.
// With aggregates it counts the groups.
func Count(ctx context.Context, q Querier, n *ReadNode) (int64, error) {
	return CountUpTo(ctx, q, n, 0)
}

// CountUpTo counts the rows like Count, but stops after limit+1 rows when
// limit > 0: a result above limit means "more than limit".
func CountUpTo(ctx context.Context, q Querier, n *ReadNode, limit int) (int64, error) {
	var group []string
	if n.HasAggregates() {
		for _, f := range n.Fields {
			if f.Agg == "" {
				group = append(group, FieldExpr("t0", f))
			}
		}
		if len(group) == 0 {
			return 1, nil // a single summary row
		}
	}
	b := &Builder{}
	switch {
	case group != nil:
		named := make([]string, len(group))
		for i, g := range group {
			named[i] = fmt.Sprintf("%s AS c%d", g, i)
		}
		b.W("SELECT COUNT(*) FROM (SELECT ", strings.Join(named, ", "), " FROM ")
	case limit > 0:
		b.W("SELECT COUNT(*) FROM (SELECT 1 AS c0 FROM ")
	default:
		b.W("SELECT COUNT(*) FROM ")
	}
	b.WriteFrom(n, "t0")
	if err := b.WriteWhereClause(n, "t0"); err != nil {
		return 0, err
	}
	if group != nil {
		b.W(" GROUP BY ", strings.Join(group, ", "))
	}
	if limit > 0 {
		b.W(" ROWS ", strconv.Itoa(limit+1))
	}
	if group != nil || limit > 0 {
		b.W(") g")
	}
	var c int64
	rows, err := q.QueryContext(ctx, b.String(), b.Args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&c); err != nil {
			return 0, err
		}
	}
	return c, rows.Err()
}

// EstimateRows returns the number of rows of table rel according to
// Firebird's index statistics, without reading the table: the selectivity
// of a unique index (RDB$INDICES.RDB$STATISTICS) is 1 / row count as of the
// last time the statistics were computed (index creation or
// SET STATISTICS INDEX). ok is false for views and procedure results, and
// for tables whose unique indexes have no statistics yet.
func EstimateRows(ctx context.Context, q Querier, rel *schema.Relation) (est int64, ok bool, err error) {
	if rel.IsView {
		return 0, false, nil
	}
	sqlText := `SELECT MIN(i.RDB$STATISTICS) FROM RDB$INDICES i
WHERE i.RDB$RELATION_NAME = ? AND i.RDB$UNIQUE_FLAG = 1
  AND COALESCE(i.RDB$INDEX_INACTIVE, 0) = 0 AND i.RDB$STATISTICS > 0`
	args := []any{rel.DBName}
	if rel.Schema != "" {
		sqlText += " AND i.RDB$SCHEMA_NAME = ?"
		args = append(args, rel.Schema)
	}
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var sel sql.NullFloat64
	if rows.Next() {
		if err := rows.Scan(&sel); err != nil {
			return 0, false, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	if !sel.Valid || sel.Float64 <= 0 {
		return 0, false, nil
	}
	return int64(math.Round(1 / sel.Float64)), true, nil
}

// Keys returns the given columns of rows matching the root conditions.
func Keys(ctx context.Context, q Querier, n *ReadNode, cols []string) ([][]any, error) {
	b := &Builder{}
	b.W("SELECT ")
	for i, c := range cols {
		if i > 0 {
			b.W(", ")
		}
		b.W(col("t0", c))
	}
	b.W(" FROM ")
	b.WriteFrom(n, "t0")
	b.W(" WHERE ")
	ok, err := b.WriteWhere(n, "t0", nil)
	if err != nil {
		return nil, err
	}
	if !ok {
		sqlText := strings.TrimSuffix(b.String(), " WHERE ")
		b.sb.Reset()
		b.sb.WriteString(sqlText)
	}
	// a limited PATCH selects the same rows it then updates
	if err := b.WriteOrder(n, "t0"); err != nil {
		return nil, err
	}
	b.WritePaging(n.Offset, n.Limit)
	rows, err := q.QueryContext(ctx, b.String(), b.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

// queryNode runs one SELECT for node n. matchCols/keys restrict the rows
// (for embedded children, or for re-reading written rows).
func queryNode(ctx context.Context, q Querier, n *ReadNode, alias string, matchCols []string, keys [][]any, paging bool) ([]*row, error) {
	b := &Builder{}
	var sel []string
	for _, f := range n.Fields {
		sel = append(sel, FieldExpr(alias, f))
	}
	nHidden := 0
	for _, c := range n.Children {
		for _, pc := range c.Join.ParentCols {
			sel = append(sel, col(alias, pc))
			nHidden++
		}
	}
	matchAlias := alias
	if n.Join != nil && n.Join.Kind == ManyToMany && keys != nil && n.Path != "" {
		matchAlias = "j0"
	}
	for _, mc := range matchCols {
		sel = append(sel, col(matchAlias, mc))
	}
	if len(sel) == 0 {
		sel = []string{"1"}
	}
	computed := n.Join != nil && n.Join.Kind == Computed && keys != nil
	if computed {
		matchAlias = "k"
		sel = sel[:len(sel)-len(matchCols)]
		for _, mc := range matchCols {
			sel = append(sel, col("k", mc))
		}
	}
	b.W("SELECT ", strings.Join(sel, ", "), " FROM ")
	if computed {
		// distinct parent keys, each passed to the procedure (a lateral call):
		// (SELECT DISTINCT k.a FROM parent k WHERE k.a IN (...)) k CROSS JOIN proc(k.a) t0
		rt := n.Join.Routine
		kcols := make([]string, len(matchCols))
		for i, mc := range matchCols {
			kcols[i] = col("k", mc)
		}
		b.W("(SELECT DISTINCT ", strings.Join(kcols, ", "), " FROM ", rt.Parent.SQLName(), " k WHERE ")
		b.WriteKeyMatch("k", matchCols, keys)
		b.W(") k CROSS JOIN ", callExpr(rt, "k"), " ", alias)
		keys = nil // already applied
	} else {
		b.WriteFrom(n, alias)
	}
	if matchAlias == "j0" {
		j := n.Join
		b.W(" JOIN ", j.Junction.SQLName(), " j0 ON ")
		for i := range j.JChildCols {
			if i > 0 {
				b.W(" AND ")
			}
			b.W(col("j0", j.JChildCols[i]), " = ", col(alias, j.ChildRef[i]))
		}
	}
	var extra func()
	if keys != nil {
		extra = func() { b.WriteKeyMatch(matchAlias, matchCols, keys) }
	}
	b.W(" WHERE ")
	ok, err := b.WriteWhere(n, alias, extra)
	if err != nil {
		return nil, err
	}
	if !ok {
		sqlText := strings.TrimSuffix(b.String(), " WHERE ")
		b.sb.Reset()
		b.sb.WriteString(sqlText)
	}
	if n.HasAggregates() {
		// group by every selected plain column plus the join keys
		var group []string
		for i, f := range n.Fields {
			if f.Agg == "" {
				group = append(group, sel[i])
			}
		}
		group = append(group, sel[len(n.Fields):]...)
		if len(group) > 0 {
			b.W(" GROUP BY ", strings.Join(group, ", "))
		}
	}
	if err := b.WriteOrder(n, alias); err != nil {
		return nil, err
	}
	if paging {
		b.WritePaging(n.Offset, n.Limit)
	}

	rs, err := q.QueryContext(ctx, b.String(), b.Args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	nf := len(n.Fields)
	total := len(sel)
	fieldCols := make([]*schema.Column, nf)
	for i, f := range n.Fields {
		fieldCols[i] = FieldColumn(f)
	}
	var out []*row
	for rs.Next() {
		vals := make([]any, total)
		ptrs := make([]any, total)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := &row{obj: newObj(n)}
		for i, f := range n.Fields {
			r.obj.Set(f.Key, OutputCtx(ctx, vals[i], fieldCols[i]))
		}
		r.hidden = vals[nf : nf+nHidden]
		r.match = vals[nf+nHidden : nf+nHidden+len(matchCols)]
		out = append(out, r)
	}
	return out, rs.Err()
}

// executeComputed runs an executable procedure for one parent key and
// returns its output parameters as a row (to-one computed relationship).
func executeComputed(ctx context.Context, q Querier, c *ReadNode, key []any) (*row, error) {
	rt := c.Join.Routine
	rs, err := q.QueryContext(ctx, "EXECUTE PROCEDURE "+rt.SQLName()+"("+strings.TrimSuffix(strings.Repeat("?, ", len(key)), ", ")+")", key...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	if !rs.Next() {
		return nil, rs.Err()
	}
	vals := make([]any, len(rt.Out))
	ptrs := make([]any, len(vals))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rs.Scan(ptrs...); err != nil {
		return nil, err
	}
	r := &row{obj: newObj(c)}
	for _, f := range c.Fields {
		for i, out := range rt.Out {
			if out.DBName == f.Col.DBName {
				v := OutputCtx(ctx, vals[i], out)
				if f.Cast != "" {
					v = OutputCtx(ctx, vals[i], FieldColumn(f))
				}
				r.obj.Set(f.Key, v)
			}
		}
	}
	return r, rs.Err()
}

// newObj creates an object with every output key of n in select order, so
// fields and embeds keep the order they were requested in.
func newObj(n *ReadNode) *Obj {
	keys := n.OutKeys()
	return &Obj{Keys: keys, Vals: make([]any, len(keys))}
}

// isAggregateOnly is true when every field of c is an aggregate, so c yields
// one summary row per parent.
func isAggregateOnly(c *ReadNode) bool {
	if len(c.Fields) == 0 || len(c.Children) > 0 {
		return false
	}
	for _, f := range c.Fields {
		if f.Agg == "" {
			return false
		}
	}
	return true
}

// emptyAggregate is the value of an aggregate-only embed with no rows,
// e.g. {"count":0} for books(count) of an author without books.
func emptyAggregate(c *ReadNode) *Obj {
	for _, f := range c.Fields {
		if f.Agg == "" {
			return nil
		}
	}
	if len(c.Children) > 0 || len(c.Fields) == 0 {
		return nil
	}
	o := newObj(c)
	for _, f := range c.Fields {
		if f.Agg == "count" {
			o.Set(f.Key, int64(0))
		}
	}
	return o
}

// attachChildren fetches every embedded child of n for the given parent rows.
func attachChildren(ctx context.Context, q Querier, n *ReadNode, parents []*row) error {
	offset := 0
	for _, c := range n.Children {
		width := len(c.Join.ParentCols)
		// distinct parent keys
		seen := map[string]bool{}
		var keys [][]any
		for _, p := range parents {
			k := p.hidden[offset : offset+width]
			if hasNil(k) {
				continue
			}
			ks := keyString(k)
			if !seen[ks] {
				seen[ks] = true
				keys = append(keys, k)
			}
		}
		groups := map[string][]*row{}
		var all []*row
		if c.Join.Kind == Computed && c.Join.Single {
			for _, k := range keys {
				r, err := executeComputed(ctx, q, c, k)
				if err != nil {
					return err
				}
				if r != nil {
					groups[keyString(k)] = []*row{r}
				}
			}
			keys = nil
		}
		for _, chunk := range chunks(keys, chunkSize/width) {
			rows, err := queryNode(ctx, q, c, "t0", c.Join.ChildCols, chunk, false)
			if err != nil {
				return err
			}
			for _, r := range rows {
				ks := keyString(r.match)
				groups[ks] = append(groups[ks], r)
			}
			all = append(all, rows...)
		}
		if err := attachChildren(ctx, q, c, all); err != nil {
			return err
		}
		for _, p := range parents {
			k := p.hidden[offset : offset+width]
			g := groups[keyString(k)]
			if hasNil(k) {
				g = nil
			}
			if c.Offset > 0 {
				if c.Offset >= len(g) {
					g = nil
				} else {
					g = g[c.Offset:]
				}
			}
			if c.Limit >= 0 && len(g) > c.Limit {
				g = g[:c.Limit]
			}
			switch {
			case c.Spread && (c.Join.Single || isAggregateOnly(c)):
				// one row at most: merge its values (to-one, or an aggregate summary)
				src := (*Obj)(nil)
				if len(g) > 0 {
					src = g[0].obj
				} else if !c.Join.Single {
					src = emptyAggregate(c)
				}
				if src != nil {
					for i, k := range src.Keys {
						p.obj.Set(k, src.Vals[i])
					}
				} // otherwise the placeholders stay null
			case c.Spread:
				// to-many spread: every key becomes an array of the related rows' values
				for _, k := range c.OutKeys() {
					vals := make([]any, len(g))
					for i, r := range g {
						vals[i] = r.obj.Get(k)
					}
					p.obj.Set(k, vals)
				}
			case c.Join.Single:
				if len(g) == 0 {
					p.obj.Set(c.Key, nil)
				} else {
					p.obj.Set(c.Key, g[0].obj)
				}
			default:
				objs := make([]*Obj, len(g))
				for i, r := range g {
					objs[i] = r.obj
				}
				if len(objs) == 0 {
					if e := emptyAggregate(c); e != nil {
						objs = append(objs, e)
					}
				}
				p.obj.Set(c.Key, objs)
			}
		}
		offset += width
	}
	return nil
}

func hasNil(k []any) bool {
	for _, v := range k {
		if v == nil {
			return true
		}
	}
	return false
}

func keyString(k []any) string {
	var sb strings.Builder
	for i, v := range k {
		if i > 0 {
			sb.WriteByte(0)
		}
		switch x := v.(type) {
		case []byte:
			sb.Write(x)
		case time.Time:
			sb.WriteString(x.Format(time.RFC3339Nano))
		case int, int16, int32:
			fmt.Fprintf(&sb, "%d", x)
		default:
			fmt.Fprint(&sb, x)
		}
	}
	return sb.String()
}

func chunks(keys [][]any, size int) [][][]any {
	if size < 1 {
		size = 1
	}
	var out [][][]any
	for len(keys) > size {
		out = append(out, keys[:size])
		keys = keys[size:]
	}
	if len(keys) > 0 {
		out = append(out, keys)
	}
	return out
}

type timeZoneKey struct{}
type rawBinaryKey struct{}

// WithRawBinary returns a context whose reads keep binary values as bytes
// instead of base64 text: for a response that sends one value as it is.
func WithRawBinary(ctx context.Context) context.Context {
	return context.WithValue(ctx, rawBinaryKey{}, true)
}

// WithTimeZone returns a context whose reads render TIME/TIMESTAMP WITH TIME
// ZONE values in loc (Prefer: timezone=).
func WithTimeZone(ctx context.Context, loc *time.Location) context.Context {
	return context.WithValue(ctx, timeZoneKey{}, loc)
}

// TimeZoneOf returns the time zone set by WithTimeZone, or nil.
func TimeZoneOf(ctx context.Context) *time.Location {
	loc, _ := ctx.Value(timeZoneKey{}).(*time.Location)
	return loc
}

// OutputIn is Output, with TIME/TIMESTAMP WITH TIME ZONE values moved to
// loc first (the same instant, shown with loc's offset). A nil loc keeps the
// value's own zone.
func OutputIn(v any, c *schema.Column, loc *time.Location) any {
	if t, ok := v.(time.Time); ok && loc != nil && c != nil &&
		(c.FieldType == schema.TTimestampTZ || c.FieldType == schema.TTimeTZ) {
		v = t.In(loc)
	}
	return Output(v, c)
}

// OutputCtx is OutputIn with the context's time zone and binary mode.
func OutputCtx(ctx context.Context, v any, c *schema.Column) any {
	if b, ok := v.([]byte); ok && ctx.Value(rawBinaryKey{}) != nil &&
		((c != nil && c.IsBinary()) || !utf8.Valid(b)) {
		return b
	}
	return OutputIn(v, c, TimeZoneOf(ctx))
}

// Output converts a value from the driver into its JSON form.
func Output(v any, c *schema.Column) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		if (c != nil && c.IsBinary()) || !utf8.Valid(x) {
			return base64.StdEncoding.EncodeToString(x)
		}
		return string(x)
	case string:
		if c != nil && (c.IsExactNumeric() || c.FieldType == schema.TInt128 ||
			c.FieldType == schema.TDecfloat16 || c.FieldType == schema.TDecfloat34) {
			s := strings.TrimSpace(x)
			if json.Valid([]byte(s)) && s != "" && (s[0] == '-' || (s[0] >= '0' && s[0] <= '9')) {
				return json.Number(s)
			}
		}
		return x
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Sprint(x)
		}
		return x
	case float32:
		return float64(x)
	case time.Time:
		ft := schema.TTimestamp
		if c != nil {
			ft = c.FieldType
		}
		switch ft {
		case schema.TDate:
			return x.Format("2006-01-02")
		case schema.TTime:
			return x.Format("15:04:05.9999")
		case schema.TTimeTZ:
			return x.Format("15:04:05.9999Z07:00")
		case schema.TTimestampTZ:
			return x.Format("2006-01-02T15:04:05.9999Z07:00")
		}
		return x.Format("2006-01-02T15:04:05.9999")
	case int64, int32, int16, int, bool:
		return x
	}
	return fmt.Sprint(v)
}
