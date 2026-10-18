// Package query parses PostgREST-style URL parameters and turns them into
// Firebird SQL.
package query

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// SelectItem is one entry of the select= parameter.
//
//	select=id,full_name:name,price::text,author(name),orders!fk_orders_customer!inner(id,total)
type SelectItem struct {
	Name     string
	Alias    string
	Cast     string
	Embed    bool
	Hint     string
	Inner    bool
	Children []*SelectItem
}

// Key is the JSON key used in the output.
func (s *SelectItem) Key() string {
	if s.Alias != "" {
		return s.Alias
	}
	return s.Name
}

// Filter is a single condition such as id=gt.5 or name=not.ilike.*bird*.
type Filter struct {
	Field  string
	Negate bool
	Op     string
	Value  string
	List   []string
}

// Node is an element of a condition tree: either a Filter or an and/or group.
type Node struct {
	Filter   *Filter
	Op       string // "and" or "or" when Filter is nil
	Negate   bool
	Children []Node
}

// OrderTerm is one entry of order=.
type OrderTerm struct {
	Field string
	Desc  bool
	Nulls string // "", "first", "last"
}

// Params holds everything parsed from the query string.
// Maps are keyed by embed path: "" for the root, "orders" or "orders.items" for embeds.
type Params struct {
	Select     []*SelectItem
	Where      map[string][]Node
	Order      map[string][]OrderTerm
	Limit      map[string]int
	Offset     map[string]int
	Columns    []string
	OnConflict []string
	Args       map[string]string // RPC arguments taken from the query string
}

// Error is a request error with an HTTP status.
type Error struct {
	Status  int
	Code    string
	Message string
	Details string
	Hint    string
}

func (e *Error) Error() string { return e.Message }

func badRequest(code, format string, a ...any) *Error {
	return &Error{Status: 400, Code: code, Message: fmt.Sprintf(format, a...)}
}

// Operators accepted in filters.
var operators = map[string]bool{
	"eq": true, "neq": true, "gt": true, "gte": true, "lt": true, "lte": true,
	"like": true, "ilike": true, "similar": true, "in": true, "is": true,
	"isdistinct": true, "containing": true, "starting": true,
}

var reserved = map[string]bool{"select": true, "columns": true, "on_conflict": true}

// ParseQuery parses url.Values. isArg reports whether a key is an RPC
// argument name (nil when not an RPC call).
func ParseQuery(v url.Values, isArg func(string) bool) (*Params, error) {
	p := &Params{
		Where:  map[string][]Node{},
		Order:  map[string][]OrderTerm{},
		Limit:  map[string]int{},
		Offset: map[string]int{},
		Args:   map[string]string{},
	}
	var err error
	if s := v.Get("select"); s != "" {
		if p.Select, err = ParseSelect(s); err != nil {
			return nil, err
		}
	}
	if s := v.Get("columns"); s != "" {
		for _, c := range splitTop(s, ',') {
			if c = strings.TrimSpace(unquote(c)); c != "" {
				p.Columns = append(p.Columns, c)
			}
		}
	}
	if s := v.Get("on_conflict"); s != "" {
		for _, c := range splitTop(s, ',') {
			if c = strings.TrimSpace(unquote(c)); c != "" {
				p.OnConflict = append(p.OnConflict, c)
			}
		}
	}

	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if reserved[key] {
			continue
		}
		if isArg != nil && isArg(key) {
			p.Args[key] = v.Get(key)
			continue
		}
		path, last := splitPath(key)
		for _, val := range v[key] {
			switch {
			case last == "order":
				terms, err := ParseOrder(val)
				if err != nil {
					return nil, err
				}
				p.Order[path] = append(p.Order[path], terms...)
			case last == "limit" || last == "offset":
				n, err := strconv.Atoi(val)
				if err != nil || n < 0 {
					return nil, badRequest("FBRST102", "%q must be a non-negative integer", key)
				}
				if last == "limit" {
					p.Limit[path] = n
				} else {
					p.Offset[path] = n
				}
			case last == "or" || last == "and":
				neg := false
				if strings.HasSuffix(path, ".not") || path == "not" {
					neg = true
					path = strings.TrimSuffix(strings.TrimSuffix(path, "not"), ".")
				}
				n, err := ParseLogic(last, neg, val)
				if err != nil {
					return nil, err
				}
				p.Where[path] = append(p.Where[path], n)
			default:
				f, err := ParseFilter(last, val)
				if err != nil {
					return nil, err
				}
				p.Where[path] = append(p.Where[path], Node{Filter: f})
			}
		}
	}
	return p, nil
}

// splitPath splits "orders.items.price" into ("orders.items", "price").
// "not.or" / "orders.not.and" keep "not" in the path for the caller.
func splitPath(key string) (string, string) {
	parts := splitTop(key, '.')
	if len(parts) == 1 {
		return "", unquote(parts[0])
	}
	for i := range parts {
		parts[i] = unquote(parts[i])
	}
	return strings.Join(parts[:len(parts)-1], "."), parts[len(parts)-1]
}

// ParseFilter parses the value part of field=op.value.
func ParseFilter(field, raw string) (*Filter, error) {
	f := &Filter{Field: field}
	rest := raw
	if strings.HasPrefix(rest, "not.") {
		f.Negate = true
		rest = rest[4:]
	}
	dot := strings.IndexByte(rest, '.')
	if dot < 0 {
		return nil, badRequest("FBRST100", "failed to parse filter %s=%s (expected operator.value)", field, raw)
	}
	f.Op, f.Value = rest[:dot], rest[dot+1:]
	if !operators[f.Op] {
		return nil, &Error{Status: 400, Code: "FBRST100", Message: fmt.Sprintf("unknown operator %q in filter on %q", f.Op, field),
			Hint: "valid operators: eq, neq, gt, gte, lt, lte, like, ilike, similar, in, is, isdistinct, containing, starting"}
	}
	switch f.Op {
	case "in":
		v := strings.TrimSpace(f.Value)
		if !strings.HasPrefix(v, "(") || !strings.HasSuffix(v, ")") {
			return nil, badRequest("FBRST100", "in filter on %q must look like in.(a,b,c)", field)
		}
		inner := v[1 : len(v)-1]
		if strings.TrimSpace(inner) != "" {
			for _, item := range splitTop(inner, ',') {
				f.List = append(f.List, unquote(strings.TrimSpace(item)))
			}
		}
	case "is":
		switch strings.ToLower(f.Value) {
		case "null", "true", "false", "unknown", "not_null":
			f.Value = strings.ToLower(f.Value)
		default:
			return nil, badRequest("FBRST100", "is filter accepts null, not_null, true, false or unknown, got %q", f.Value)
		}
	default:
		f.Value = unquote(f.Value)
	}
	return f, nil
}

// ParseLogic parses or=(a.eq.1,and(b.gt.2,c.lt.3)).
func ParseLogic(op string, negate bool, raw string) (Node, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "(") || !strings.HasSuffix(raw, ")") {
		return Node{}, badRequest("FBRST100", "%s= value must be wrapped in parentheses", op)
	}
	n := Node{Op: op, Negate: negate}
	for _, part := range splitTop(raw[1:len(raw)-1], ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		neg := false
		body := part
		if strings.HasPrefix(body, "not.") {
			neg, body = true, body[4:]
		}
		if strings.HasPrefix(body, "and(") || strings.HasPrefix(body, "or(") {
			i := strings.IndexByte(body, '(')
			child, err := ParseLogic(body[:i], neg, body[i:])
			if err != nil {
				return Node{}, err
			}
			n.Children = append(n.Children, child)
			continue
		}
		pieces := splitTop(part, '.')
		if len(pieces) < 3 {
			return Node{}, badRequest("FBRST100", "failed to parse logic tree element %q", part)
		}
		field := unquote(pieces[0])
		f, err := ParseFilter(field, part[len(pieces[0])+1:])
		if err != nil {
			return Node{}, err
		}
		n.Children = append(n.Children, Node{Filter: f})
	}
	if len(n.Children) == 0 {
		return Node{}, badRequest("FBRST100", "empty %s= condition", op)
	}
	return n, nil
}

// ParseOrder parses order=col.desc.nullslast,other.
func ParseOrder(s string) ([]OrderTerm, error) {
	var out []OrderTerm
	for _, item := range splitTop(s, ',') {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := splitTop(item, '.')
		t := OrderTerm{Field: unquote(parts[0])}
		for _, mod := range parts[1:] {
			switch mod {
			case "asc":
				t.Desc = false
			case "desc":
				t.Desc = true
			case "nullsfirst":
				t.Nulls = "first"
			case "nullslast":
				t.Nulls = "last"
			default:
				return nil, badRequest("FBRST101", "unknown order modifier %q", mod)
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// ParseSelect parses the select= parameter.
func ParseSelect(s string) ([]*SelectItem, error) {
	var out []*SelectItem
	for _, raw := range splitTop(s, ',') {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		item, err := parseSelectItem(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil, badRequest("FBRST103", "select= is empty")
	}
	return out, nil
}

func parseSelectItem(raw string) (*SelectItem, error) {
	item := &SelectItem{}
	head := raw
	if i := indexTop(raw, '('); i >= 0 {
		if !strings.HasSuffix(raw, ")") {
			return nil, badRequest("FBRST103", "unbalanced parentheses in select item %q", raw)
		}
		head = raw[:i]
		children, err := ParseSelect(raw[i+1 : len(raw)-1])
		if err != nil {
			return nil, err
		}
		item.Embed = true
		item.Children = children
	}
	// alias:name (single colon, not part of ::cast)
	if i := indexAlias(head); i >= 0 {
		item.Alias = unquote(head[:i])
		head = head[i+1:]
	}
	if i := strings.Index(head, "::"); i >= 0 {
		item.Cast = strings.ToLower(strings.TrimSpace(head[i+2:]))
		head = head[:i]
	}
	parts := strings.Split(head, "!")
	item.Name = unquote(strings.TrimSpace(parts[0]))
	for _, h := range parts[1:] {
		switch h {
		case "inner":
			item.Inner = true
		case "left":
		default:
			item.Hint = h
		}
	}
	if item.Name == "" {
		return nil, badRequest("FBRST103", "empty name in select item %q", raw)
	}
	if !item.Embed && (item.Hint != "" || item.Inner) {
		return nil, badRequest("FBRST103", "hints are only allowed on embedded resources: %q", raw)
	}
	return item, nil
}

func indexAlias(s string) int {
	inQ := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQ = !inQ
		case ':':
			if inQ {
				continue
			}
			if i+1 < len(s) && s[i+1] == ':' {
				return -1
			}
			return i
		}
	}
	return -1
}

func indexTop(s string, ch byte) int {
	inQ := false
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			inQ = !inQ
		} else if s[i] == ch && !inQ {
			return i
		}
	}
	return -1
}

// splitTop splits on sep, ignoring separators inside parentheses or double quotes.
func splitTop(s string, sep byte) []string {
	var out []string
	depth, start, inQ := 0, 0, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && inQ && i+1 < len(s):
			i++
		case c == '"':
			inQ = !inQ
		case inQ:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, `\"`, `"`)
		s = strings.ReplaceAll(s, `\\`, `\`)
	}
	return s
}
