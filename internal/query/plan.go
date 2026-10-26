package query

import (
	"fmt"
	"strings"

	"github.com/mariuz/firebirdrest/internal/schema"
)

// Join kinds between a parent node and an embedded child.
const (
	ManyToOne  = iota // parent has the foreign key -> child is an object
	OneToMany         // child has the foreign key -> child is an array
	ManyToMany        // linked through a junction table -> array
	Computed          // a procedure whose inputs are TYPE OF COLUMN the parent
)

// Field is one output column.
type Field struct {
	Col  *schema.Column  // result metadata (countColumn for count())
	Fn   *schema.Routine // computed field: a function of the row's columns
	Key  string
	Cast string // Firebird cast target, already validated
	Agg  string // count, sum, avg, min, max
}

// countColumn describes the result of count().
var countColumn = &schema.Column{Name: "count", FieldType: schema.TBigint}

// Join describes how a child node relates to its parent.
type Join struct {
	Kind       int
	FK         *schema.ForeignKey // the FK used (for M2M: junction -> parent)
	ParentCols []string           // parent columns matched (DB names)
	ChildCols  []string           // child (or junction for M2M) columns matched
	Junction   *schema.Relation
	JChildCols []string        // M2M: junction columns pointing to the child
	ChildRef   []string        // M2M: child columns referenced by the junction
	Routine    *schema.Routine // Computed: the procedure called per parent key
	Single     bool
}

// ReadNode is a resolved read of one relation plus its embedded children.
type ReadNode struct {
	Rel      *schema.Relation
	From     string // SQL source; defaults to the quoted relation name
	FromArgs []any
	Key      string // JSON key in the parent
	Path     string // embed path ("" for root)
	Fields   []Field
	Where    []Node
	Order    []OrderTerm
	Limit    int // -1 = none
	Offset   int
	Children []*ReadNode
	Join     *Join
	Inner    bool
	Spread   bool     // merge this (to-one) child's keys into the parent object
	Layout   []int    // output order: i >= 0 is Fields[i], i < 0 is Children[-i-1]
	outKeys  []string // OutKeys(), computed once
}

// HasAggregates reports whether any selected field is an aggregate.
func (n *ReadNode) HasAggregates() bool {
	for _, f := range n.Fields {
		if f.Agg != "" {
			return true
		}
	}
	return false
}

// OutKeys lists the JSON keys this node produces, in output order.
// A spread child contributes its own keys.
func (n *ReadNode) OutKeys() []string {
	if n.outKeys == nil {
		n.outKeys = n.computeOutKeys()
	}
	// full slice expression: an append by a caller copies instead of
	// writing into the shared array
	return n.outKeys[:len(n.outKeys):len(n.outKeys)]
}

func (n *ReadNode) computeOutKeys() []string {
	var out []string
	for _, i := range n.Layout {
		if i >= 0 {
			out = append(out, n.Fields[i].Key)
		} else if c := n.Children[-i-1]; c.Spread {
			out = append(out, c.OutKeys()...)
		} else {
			out = append(out, c.Key)
		}
	}
	return out
}

// castTypes maps the cast names accepted in select=col::type to Firebird types.
var castTypes = map[string]string{
	"text": "VARCHAR(8191)", "varchar": "VARCHAR(8191)", "string": "VARCHAR(8191)",
	"int": "INTEGER", "integer": "INTEGER", "smallint": "SMALLINT", "bigint": "BIGINT", "int128": "INT128",
	"float": "DOUBLE PRECISION", "double": "DOUBLE PRECISION", "numeric": "NUMERIC(18,4)",
	"decfloat": "DECFLOAT(34)", "boolean": "BOOLEAN", "bool": "BOOLEAN",
	"date": "DATE", "time": "TIME", "timestamp": "TIMESTAMP",
}

// castColumn returns the column metadata describing a cast result.
func castColumn(c *schema.Column, cast string) *schema.Column {
	n := *c
	n.Scale, n.SubType = 0, 0
	switch castTypes[cast] {
	case "INTEGER":
		n.FieldType = schema.TInteger
	case "SMALLINT":
		n.FieldType = schema.TSmallint
	case "BIGINT":
		n.FieldType = schema.TBigint
	case "INT128":
		n.FieldType = schema.TInt128
	case "DOUBLE PRECISION":
		n.FieldType = schema.TDouble
	case "NUMERIC(18,4)":
		n.FieldType, n.Scale, n.SubType, n.Precision = schema.TBigint, -4, 1, 18
	case "DECFLOAT(34)":
		n.FieldType = schema.TDecfloat34
	case "BOOLEAN":
		n.FieldType = schema.TBoolean
	case "DATE":
		n.FieldType = schema.TDate
	case "TIME":
		n.FieldType = schema.TTime
	case "TIMESTAMP":
		n.FieldType = schema.TTimestamp
	default:
		n.FieldType = schema.TVarchar
	}
	return &n
}

// fieldFor resolves a column name, or the name of a computed field (a
// function whose arguments are TYPE OF COLUMN this relation).
func fieldFor(rel *schema.Relation, name string) (Field, bool) {
	if c := rel.Column(name); c != nil {
		return Field{Col: c, Key: c.Name}, true
	}
	if rt := rel.ComputedField(name); rt != nil {
		c := *rt.Out[0]
		c.Name, c.DBName = rt.Name, ""
		return Field{Col: &c, Fn: rt, Key: rt.Name}, true
	}
	return Field{}, false
}

// BuildRead resolves a read request against the schema.
func BuildRead(s *schema.Schema, rel *schema.Relation, p *Params, maxRows int) (*ReadNode, error) {
	root := &ReadNode{Rel: rel, Limit: -1}
	sel := p.Select
	if sel == nil {
		sel = []*SelectItem{{Name: "*"}}
	}
	if err := resolveNode(s, root, sel, p, ""); err != nil {
		return nil, err
	}
	if maxRows > 0 && (root.Limit < 0 || root.Limit > maxRows) {
		root.Limit = maxRows
	}
	// Every params path must be an embed that exists.
	for path := range p.Where {
		if path != "" && findPath(root, path) == nil {
			return nil, badRequest("FBRST108", "filter references %q which is not embedded in select", path)
		}
	}
	for path := range p.Order {
		if path != "" && findPath(root, path) == nil {
			return nil, badRequest("FBRST108", "order references %q which is not embedded in select", path)
		}
	}
	return root, nil
}

func findPath(n *ReadNode, path string) *ReadNode {
	if n.Path == path {
		return n
	}
	for _, c := range n.Children {
		if f := findPath(c, path); f != nil {
			return f
		}
	}
	return nil
}

func resolveNode(s *schema.Schema, n *ReadNode, sel []*SelectItem, p *Params, path string) error {
	n.Path = path
	n.Where = p.Where[path]
	n.Order = p.Order[path]
	if l, ok := p.Limit[path]; ok {
		n.Limit = l
	} else {
		n.Limit = -1
	}
	n.Offset = p.Offset[path]

	addField := func(f Field) {
		n.Layout = append(n.Layout, len(n.Fields))
		n.Fields = append(n.Fields, f)
	}
	for _, item := range sel {
		if item.Embed {
			child, err := resolveEmbed(s, n, item)
			if err != nil {
				return err
			}
			childPath := item.Key()
			if path != "" {
				childPath = path + "." + childPath
			}
			if err := resolveNode(s, child, item.Children, p, childPath); err != nil {
				return err
			}
			child.Spread = item.Spread
			n.Children = append(n.Children, child)
			n.Layout = append(n.Layout, -len(n.Children))
			continue
		}
		if item.Name == "*" {
			for _, c := range n.Rel.Columns {
				addField(Field{Col: c, Key: c.Name})
			}
			continue
		}
		// "count" inside an embed, as in authors?select=name,books(count)
		if item.Agg == "" && item.Name == "count" && item.Cast == "" && n.Rel.Column("count") == nil {
			item = &SelectItem{Agg: "count", Alias: item.Alias}
		}
		if item.Agg != "" {
			if p.NoAggregates {
				return &Error{Status: 400, Code: "FBRST123", Message: "use of aggregate functions is disabled",
					Hint: "set db-aggregates-enabled = true"}
			}
			f := Field{Agg: item.Agg, Key: item.Key(), Col: countColumn}
			if item.Name != "" {
				base, ok := fieldFor(n.Rel, item.Name)
				if !ok {
					return &Error{Status: 400, Code: "FBRST204",
						Message: fmt.Sprintf("column %q does not exist in %q", item.Name, n.Rel.Name)}
				}
				f.Fn = base.Fn
				if item.Agg != "count" {
					f.Col = base.Col
				} else {
					f.Col = &schema.Column{Name: base.Col.Name, DBName: base.Col.DBName, FieldType: schema.TBigint}
				}
			}
			if item.Cast != "" {
				if _, ok := castTypes[item.Cast]; !ok {
					return badRequest("FBRST104", "unsupported cast %q", item.Cast)
				}
				f.Cast = item.Cast
			}
			addField(f)
			continue
		}
		f, ok := fieldFor(n.Rel, item.Name)
		if !ok {
			return &Error{Status: 400, Code: "FBRST204",
				Message: fmt.Sprintf("column %q does not exist in %q", item.Name, n.Rel.Name)}
		}
		if item.Alias != "" {
			f.Key = item.Alias
		}
		if item.Cast != "" {
			if _, ok := castTypes[item.Cast]; !ok {
				return badRequest("FBRST104", "unsupported cast %q", item.Cast)
			}
			f.Cast = item.Cast
		}
		addField(f)
	}
	seen := map[string]bool{}
	for _, k := range n.OutKeys() {
		if seen[k] {
			return &Error{Status: 400, Code: "FBRST121",
				Message: fmt.Sprintf("the key %q appears more than once in the result", k),
				Hint:    "rename one of them with an alias, e.g. ...tags(tag:name)"}
		}
		seen[k] = true
	}
	if n.Join != nil && n.Join.Kind == Computed && n.Join.Single &&
		(len(n.Where) > 0 || len(n.Order) > 0 || n.Limit >= 0 || n.Offset > 0 || n.HasAggregates()) {
		return badRequest("FBRST120", "%q returns a single row: filters, order, paging and aggregates are not supported on it", n.Key)
	}
	if len(n.Order) > 0 {
		// copy: the terms come from Params and get resolved per node
		n.Order = append([]OrderTerm(nil), n.Order...)
	}
	for i := range n.Order {
		o := &n.Order[i]
		if o.Embed != "" {
			if err := resolveRelatedOrder(n, o); err != nil {
				return err
			}
			continue
		}
		if _, ok := fieldFor(n.Rel, o.Field); !ok {
			return badRequest("FBRST204", "order column %q does not exist in %q", o.Field, n.Rel.Name)
		}
	}
	return nil
}

// resolveRelatedOrder links order=embed(column) to the embedded child. As in
// PostgREST, the embed must be in select and be to-one (many-to-one), so it
// yields at most one value per row.
func resolveRelatedOrder(n *ReadNode, o *OrderTerm) error {
	for _, c := range n.Children {
		if c.Key != o.Embed && c.Rel.Name != o.Embed {
			continue
		}
		if c.Join == nil || c.Join.Kind != ManyToOne {
			return &Error{Status: 400, Code: "FBRST126",
				Message: fmt.Sprintf("a related order on %q is not possible", o.Embed),
				Details: fmt.Sprintf("%q is a to-many relationship of %q", o.Embed, n.Rel.Name),
				Hint:    "order by a column of a many-to-one embed, or use " + o.Embed + ".order= to order the embedded rows"}
		}
		if n.HasAggregates() {
			return badRequest("FBRST126", "a related order on %q is not possible together with aggregates", o.Embed)
		}
		if _, ok := fieldFor(c.Rel, o.Field); !ok {
			return badRequest("FBRST204", "order column %q does not exist in %q", o.Field, c.Rel.Name)
		}
		o.child = c
		return nil
	}
	return &Error{Status: 400, Code: "FBRST108",
		Message: fmt.Sprintf("order references %q which is not embedded in select", o.Embed),
		Hint:    "add " + o.Embed + "(...) to select"}
}

func sameCols(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fkMatchesHint(fk *schema.ForeignKey, hint string) bool {
	if hint == "" {
		return true
	}
	if fk.Name == hint || fk.DBName == hint {
		return true
	}
	if len(fk.FromCols) == 1 && (schema.APIName(fk.FromCols[0]) == hint || fk.FromCols[0] == hint) {
		return true
	}
	return false
}

type candidate struct {
	child *schema.Relation
	join  *Join
	desc  string
}

// resolveEmbed finds the relationship between parent and the embedded item.
func resolveEmbed(s *schema.Schema, parent *ReadNode, item *SelectItem) (*ReadNode, error) {
	pr := parent.Rel
	var cands []candidate
	// A relationship's other side is matched by name, not looked up in the
	// request's schema: a foreign key may lead into another exposed schema
	// (Firebird 6), whose table is then embeddable although it isn't a
	// resource of this schema.
	named := func(r *schema.Relation) bool {
		return r.Name == item.Name || r.DBName == item.Name
	}
	var target *schema.Relation
	// the keys of the parent's own schema: a nested embed may already be in
	// another schema than the request's
	keys := pr.Keys()
	if keys == nil {
		keys = s.ForeignKeys
	}

	for _, fk := range keys {
		// many-to-one: parent holds the FK. Embed by table name or by FK column name.
		if fk.From == pr {
			byTable := named(fk.To)
			byCol := len(fk.FromCols) == 1 && schema.APIName(fk.FromCols[0]) == item.Name
			if (byTable && fkMatchesHint(fk, item.Hint)) || (byCol && item.Hint == "") {
				cands = append(cands, candidate{fk.To, &Join{Kind: ManyToOne, FK: fk,
					ParentCols: fk.FromCols, ChildCols: fk.ToCols, Single: true}, "many-to-one via " + fk.Name})
			}
		}
		// one-to-many: child holds an FK to the parent.
		if fk.To == pr && named(fk.From) && fkMatchesHint(fk, item.Hint) {
			target = fk.From
			single := len(target.PrimaryKey) > 0 && sameCols(fk.FromCols, target.PrimaryKey)
			cands = append(cands, candidate{target, &Join{Kind: OneToMany, FK: fk,
				ParentCols: fk.ToCols, ChildCols: fk.FromCols, Single: single}, "one-to-many via " + fk.Name})
		}
	}
	// many-to-many through a junction table whose FKs are part of its primary key.
	if len(cands) == 0 {
		for _, toParent := range keys {
			if toParent.To != pr {
				continue
			}
			j := toParent.From
			// the junction's keys are in its own schema's list (the
			// parent's, for relations not loaded from a database)
			jkeys := j.Keys()
			if jkeys == nil {
				jkeys = keys
			}
			for _, toChild := range jkeys {
				if toChild.From != j || !named(toChild.To) || toChild == toParent {
					continue
				}
				target = toChild.To
				if !inPK(j, toParent.FromCols) || !inPK(j, toChild.FromCols) {
					continue
				}
				if item.Hint != "" && item.Hint != j.Name && item.Hint != j.DBName {
					continue
				}
				cands = append(cands, candidate{target, &Join{Kind: ManyToMany, FK: toParent,
					ParentCols: toParent.ToCols, ChildCols: toParent.FromCols, Junction: j,
					JChildCols: toChild.FromCols, ChildRef: toChild.ToCols}, "many-to-many via " + j.Name})
			}
		}
	}
	// computed relationships: procedures taking TYPE OF COLUMN <parent>.<col> arguments
	for _, rt := range pr.Computed {
		if rt.Kind == schema.Function || (rt.Name != item.Name && rt.DBName != item.Name) {
			continue
		}
		cands = append(cands, candidate{rt.AsRelation(), &Join{Kind: Computed, Routine: rt,
			ParentCols: rt.ParentCols, ChildCols: rt.ParentCols, Single: rt.Kind == schema.Executable},
			"computed relationship via " + rt.Name})
	}
	if len(cands) == 0 {
		return nil, &Error{Status: 400, Code: "FBRST200",
			Message: fmt.Sprintf("could not find a relationship between %q and %q", pr.Name, item.Name),
			Hint:    "check that a foreign key exists between the two tables"}
	}
	if len(cands) > 1 {
		var d []string
		for _, c := range cands {
			d = append(d, c.desc)
		}
		return nil, &Error{Status: 300, Code: "FBRST201",
			Message: fmt.Sprintf("more than one relationship found between %q and %q", pr.Name, item.Name),
			Details: strings.Join(d, "; "),
			Hint:    fmt.Sprintf("disambiguate with %s!<constraint or column>(...)", item.Name)}
	}
	c := cands[0]
	return &ReadNode{Rel: c.child, Key: item.Key(), Join: c.join, Inner: item.Inner}, nil
}

func inPK(r *schema.Relation, cols []string) bool {
	for _, c := range cols {
		found := false
		for _, pk := range r.PrimaryKey {
			if pk == c {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return len(cols) > 0
}
