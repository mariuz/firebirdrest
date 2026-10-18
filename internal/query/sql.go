package query

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mariuz/firebirdrest/internal/schema"
)

// Builder accumulates SQL text and positional arguments.
type Builder struct {
	sb    strings.Builder
	Args  []any
	alias int
}

func (b *Builder) W(s ...string) {
	for _, x := range s {
		b.sb.WriteString(x)
	}
}

func (b *Builder) Arg(v any) {
	b.sb.WriteByte('?')
	b.Args = append(b.Args, v)
}

func (b *Builder) String() string { return b.sb.String() }

func (b *Builder) nextAlias() string {
	b.alias++
	return fmt.Sprintf("t%d", b.alias)
}

func col(alias, dbName string) string { return alias + "." + schema.Quote(dbName) }

// FieldExpr returns the SQL for a selected field.
func FieldExpr(alias string, f Field) string {
	var e string
	switch {
	case f.Agg == "count" && f.Col.DBName == "":
		e = "COUNT(*)"
	case f.Agg != "":
		e = strings.ToUpper(f.Agg) + "(" + col(alias, f.Col.DBName) + ")"
	default:
		e = col(alias, f.Col.DBName)
	}
	if f.Cast != "" {
		e = "CAST(" + e + " AS " + castTypes[f.Cast] + ")"
	}
	return e
}

// FieldColumn returns metadata describing the produced value (after casts).
func FieldColumn(f Field) *schema.Column {
	if f.Cast != "" {
		return castColumn(f.Col, f.Cast)
	}
	return f.Col
}

// WriteFrom writes "<source> alias".
func (b *Builder) WriteFrom(n *ReadNode, alias string) {
	if n.From != "" {
		b.W(n.From)
		b.Args = append(b.Args, n.FromArgs...)
	} else {
		b.W(schema.Quote(n.Rel.DBName))
	}
	b.W(" ", alias)
}

// WriteWhere writes the conditions of n (filters plus !inner/is.null embeds).
// It writes nothing and returns false when there is no condition.
func (b *Builder) WriteWhere(n *ReadNode, alias string, extra func()) (bool, error) {
	var parts []func() error
	for _, w := range n.Where {
		w := w
		parts = append(parts, func() error { return b.writeNode(n, alias, w) })
	}
	for _, c := range n.Children {
		if c.Inner {
			c := c
			parts = append(parts, func() error { return b.writeExists(n, alias, c, false) })
		}
	}
	if extra != nil {
		parts = append(parts, func() error { extra(); return nil })
	}
	if len(parts) == 0 {
		return false, nil
	}
	for i, p := range parts {
		if i > 0 {
			b.W(" AND ")
		}
		b.W("(")
		if err := p(); err != nil {
			return false, err
		}
		b.W(")")
	}
	return true, nil
}

func (b *Builder) writeNode(n *ReadNode, alias string, w Node) error {
	if w.Filter != nil {
		return b.writeFilter(n, alias, w.Filter)
	}
	if w.Negate {
		b.W("NOT ")
	}
	b.W("(")
	for i, c := range w.Children {
		if i > 0 {
			b.W(" ", strings.ToUpper(w.Op), " ")
		}
		b.W("(")
		if err := b.writeNode(n, alias, c); err != nil {
			return err
		}
		b.W(")")
	}
	b.W(")")
	return nil
}

func (b *Builder) writeFilter(n *ReadNode, alias string, f *Filter) error {
	c := n.Rel.Column(f.Field)
	if c == nil {
		// embed=is.null / embed=not.is.null filters on embedded resources
		for _, ch := range n.Children {
			if ch.Key == f.Field && f.Op == "is" && (f.Value == "null" || f.Value == "not_null") {
				exists := (f.Value == "null") == f.Negate
				return b.writeExists(n, alias, ch, !exists)
			}
		}
		return &Error{Status: 400, Code: "FBRST204",
			Message: fmt.Sprintf("column %q does not exist in %q", f.Field, n.Rel.Name)}
	}
	if f.Negate {
		b.W("NOT ")
	}
	e := col(alias, c.DBName)
	b.W("(")
	defer b.W(")")
	switch f.Op {
	case "eq", "neq", "gt", "gte", "lt", "lte":
		op := map[string]string{"eq": "=", "neq": "<>", "gt": ">", "gte": ">=", "lt": "<", "lte": "<="}[f.Op]
		v, err := Coerce(c, f.Value)
		if err != nil {
			return err
		}
		b.W(e, " ", op, " ")
		b.Arg(v)
	case "like":
		b.W(e, " LIKE ")
		b.Arg(strings.ReplaceAll(f.Value, "*", "%"))
	case "ilike":
		b.W("UPPER(", e, ") LIKE UPPER(")
		b.Arg(strings.ReplaceAll(f.Value, "*", "%"))
		b.W(")")
	case "similar":
		b.W(e, " SIMILAR TO ")
		b.Arg(f.Value)
	case "containing":
		b.W(e, " CONTAINING ")
		b.Arg(f.Value)
	case "starting":
		b.W(e, " STARTING WITH ")
		b.Arg(f.Value)
	case "isdistinct":
		v, err := Coerce(c, f.Value)
		if err != nil {
			return err
		}
		b.W(e, " IS DISTINCT FROM ")
		b.Arg(v)
	case "in":
		if len(f.List) == 0 {
			b.W("1 = 0")
			return nil
		}
		b.W(e, " IN (")
		for i, item := range f.List {
			if i > 0 {
				b.W(", ")
			}
			v, err := Coerce(c, item)
			if err != nil {
				return err
			}
			b.Arg(v)
		}
		b.W(")")
	case "is":
		switch f.Value {
		case "null":
			b.W(e, " IS NULL")
		case "not_null":
			b.W(e, " IS NOT NULL")
		case "true":
			b.W(e, " IS TRUE")
		case "false":
			b.W(e, " IS FALSE")
		case "unknown":
			b.W(e, " IS UNKNOWN")
		}
	}
	return nil
}

// writeExists writes EXISTS (...) linking child to parent alias.
func (b *Builder) writeExists(parent *ReadNode, palias string, c *ReadNode, negate bool) error {
	if negate {
		b.W("NOT ")
	}
	ca := b.nextAlias()
	b.W("EXISTS (SELECT 1 FROM ")
	b.WriteFrom(c, ca)
	j := c.Join
	matchAlias := ca
	if j.Kind == ManyToMany {
		ja := b.nextAlias()
		b.W(" JOIN ", schema.Quote(j.Junction.DBName), " ", ja, " ON ")
		for i := range j.JChildCols {
			if i > 0 {
				b.W(" AND ")
			}
			b.W(col(ja, j.JChildCols[i]), " = ", col(ca, j.ChildRef[i]))
		}
		matchAlias = ja
	}
	b.W(" WHERE ")
	_, err := b.WriteWhere(c, ca, func() {
		for i := range j.ParentCols {
			if i > 0 {
				b.W(" AND ")
			}
			b.W(col(matchAlias, j.ChildCols[i]), " = ", col(palias, j.ParentCols[i]))
		}
	})
	b.W(")")
	return err
}

// WriteOrder writes ORDER BY when terms are present.
func (b *Builder) WriteOrder(n *ReadNode, alias string) {
	if len(n.Order) == 0 {
		return
	}
	b.W(" ORDER BY ")
	for i, o := range n.Order {
		if i > 0 {
			b.W(", ")
		}
		b.W(col(alias, n.Rel.Column(o.Field).DBName))
		if o.Desc {
			b.W(" DESC")
		}
		switch o.Nulls {
		case "first":
			b.W(" NULLS FIRST")
		case "last":
			b.W(" NULLS LAST")
		}
	}
}

// WritePaging writes OFFSET/FETCH (Firebird 3+ syntax). A zero limit is
// handled by the caller, which skips the query.
func (b *Builder) WritePaging(offset, limit int) {
	if offset > 0 {
		b.W(" OFFSET ", strconv.Itoa(offset), " ROWS")
	}
	if limit > 0 {
		b.W(" FETCH NEXT ", strconv.Itoa(limit), " ROWS ONLY")
	}
}

// WriteKeyMatch writes a condition matching cols against a set of key tuples.
func (b *Builder) WriteKeyMatch(alias string, cols []string, keys [][]any) {
	if len(keys) == 0 {
		b.W("1 = 0")
		return
	}
	if len(cols) == 1 {
		b.W(col(alias, cols[0]), " IN (")
		for i, k := range keys {
			if i > 0 {
				b.W(", ")
			}
			b.Arg(k[0])
		}
		b.W(")")
		return
	}
	for i, k := range keys {
		if i > 0 {
			b.W(" OR ")
		}
		b.W("(")
		for j, c := range cols {
			if j > 0 {
				b.W(" AND ")
			}
			b.W(col(alias, c), " = ")
			b.Arg(k[j])
		}
		b.W(")")
	}
}

// Coerce converts a string from the URL into a value suitable for column c.
func Coerce(c *schema.Column, s string) (any, error) {
	switch {
	case c.FieldType == schema.TBoolean:
		switch strings.ToLower(s) {
		case "true", "t", "1", "yes", "on":
			return true, nil
		case "false", "f", "0", "no", "off":
			return false, nil
		}
		return nil, badRequest("22018", "invalid boolean value %q for %q", s, c.Name)
	case c.IsInteger() && c.FieldType != schema.TInt128:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, badRequest("22018", "invalid integer value %q for %q", s, c.Name)
		}
		return n, nil
	case isTemporal(c):
		return normalizeTemporal(c, s), nil
	}
	return s, nil
}

func isTemporal(c *schema.Column) bool {
	switch c.FieldType {
	case schema.TDate, schema.TTime, schema.TTimestamp, schema.TTimeTZ, schema.TTimestampTZ:
		return true
	}
	return false
}

// normalizeTemporal rewrites ISO-8601 strings into the literal form Firebird accepts.
func normalizeTemporal(c *schema.Column, s string) string {
	layouts := []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04", "2006-01-02 15:04:05.999999999Z07:00"}
	for _, l := range layouts {
		t, err := time.Parse(l, s)
		if err != nil {
			continue
		}
		switch c.FieldType {
		case schema.TDate:
			return t.Format("2006-01-02")
		case schema.TTime:
			return t.Format("15:04:05.0000")
		case schema.TTimeTZ:
			return t.Format("15:04:05.0000 -07:00")
		case schema.TTimestampTZ:
			if strings.HasSuffix(l, "07:00") || strings.HasSuffix(s, "Z") {
				return t.Format("2006-01-02 15:04:05.0000 -07:00")
			}
		}
		return t.Format("2006-01-02 15:04:05.0000")
	}
	return s
}

// CoerceJSON converts a decoded JSON body value into a parameter for column c.
func CoerceJSON(c *schema.Column, v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case bool:
		return x, nil
	case string:
		if c.IsBinary() {
			b, err := base64.StdEncoding.DecodeString(x)
			if err != nil {
				return nil, badRequest("22018", "column %q expects base64 data", c.Name)
			}
			return b, nil
		}
		if c.FieldType == schema.TBoolean || (c.IsInteger() && c.FieldType != schema.TInt128) || isTemporal(c) {
			return Coerce(c, x)
		}
		return x, nil
	case interface{ String() string }: // json.Number
		s := x.String()
		if c.IsInteger() && c.FieldType != schema.TInt128 {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n, nil
			}
		}
		if c.FieldType == schema.TDouble || c.FieldType == schema.TFloat {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f, nil
			}
		}
		return s, nil
	case float64:
		return x, nil
	default:
		// objects and arrays are stored as their JSON text
		return marshalJSON(v)
	}
}
