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
)

// Field is one output column.
type Field struct {
	Col  *schema.Column // nil for count()
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
	JChildCols []string // M2M: junction columns pointing to the child
	ChildRef   []string // M2M: child columns referenced by the junction
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
	Spread   bool  // merge this (to-one) child's keys into the parent object
	Layout   []int // output order: i >= 0 is Fields[i], i < 0 is Children[-i-1]
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
			if item.Spread {
				if !child.Join.Single {
					return badRequest("FBRST119", "spread (...%s) only works on to-one relationships", item.Name)
				}
				child.Spread = true
			}
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
				c := n.Rel.Column(item.Name)
				if c == nil {
					return &Error{Status: 400, Code: "FBRST204",
						Message: fmt.Sprintf("column %q does not exist in %q", item.Name, n.Rel.Name)}
				}
				if item.Agg != "count" {
					f.Col = c
				} else {
					f.Col = &schema.Column{Name: c.Name, DBName: c.DBName, FieldType: schema.TBigint}
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
		c := n.Rel.Column(item.Name)
		if c == nil {
			return &Error{Status: 400, Code: "FBRST204",
				Message: fmt.Sprintf("column %q does not exist in %q", item.Name, n.Rel.Name)}
		}
		f := Field{Col: c, Key: item.Key()}
		if item.Alias == "" {
			f.Key = c.Name
		}
		if item.Cast != "" {
			if _, ok := castTypes[item.Cast]; !ok {
				return badRequest("FBRST104", "unsupported cast %q", item.Cast)
			}
			f.Cast = item.Cast
		}
		addField(f)
	}
	for _, o := range n.Order {
		if n.Rel.Column(o.Field) == nil {
			return badRequest("FBRST204", "order column %q does not exist in %q", o.Field, n.Rel.Name)
		}
	}
	return nil
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
	target := s.Relation(item.Name)

	for _, fk := range s.ForeignKeys {
		// many-to-one: parent holds the FK. Embed by table name or by FK column name.
		if fk.From == pr {
			byTable := target != nil && fk.To == target
			byCol := len(fk.FromCols) == 1 && schema.APIName(fk.FromCols[0]) == item.Name
			if (byTable && fkMatchesHint(fk, item.Hint)) || (byCol && item.Hint == "") {
				cands = append(cands, candidate{fk.To, &Join{Kind: ManyToOne, FK: fk,
					ParentCols: fk.FromCols, ChildCols: fk.ToCols, Single: true}, "many-to-one via " + fk.Name})
			}
		}
		// one-to-many: child holds an FK to the parent.
		if target != nil && fk.From == target && fk.To == pr && fkMatchesHint(fk, item.Hint) {
			single := len(target.PrimaryKey) > 0 && sameCols(fk.FromCols, target.PrimaryKey)
			cands = append(cands, candidate{target, &Join{Kind: OneToMany, FK: fk,
				ParentCols: fk.ToCols, ChildCols: fk.FromCols, Single: single}, "one-to-many via " + fk.Name})
		}
	}
	// many-to-many through a junction table whose FKs are part of its primary key.
	if target != nil && len(cands) == 0 {
		for _, toParent := range s.ForeignKeys {
			if toParent.To != pr {
				continue
			}
			j := toParent.From
			for _, toChild := range s.ForeignKeys {
				if toChild.From != j || toChild.To != target || toChild == toParent {
					continue
				}
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
