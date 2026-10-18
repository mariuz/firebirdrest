package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/mariuz/firebirdrest/internal/query"
	"github.com/mariuz/firebirdrest/internal/schema"
)

// rpc calls a stored procedure or PSQL function.
//
//   - selectable procedures (with SUSPEND) are read like a table: SELECT ... FROM PROC(args),
//     so select=, filters, order, limit and Prefer: count work on their output;
//   - executable procedures run EXECUTE PROCEDURE and return their output parameters as an object;
//   - functions return their scalar result.
func (s *Server) rpc(rc *reqCtx, rt *schema.Routine) error {
	isArg := func(k string) bool { return rt.Arg(k) != nil }
	if rc.r.Method == http.MethodPost {
		isArg = nil // arguments come from the body; the query string only shapes the result
	}
	p, err := s.params(rc, isArg)
	if err != nil {
		return err
	}
	args := map[string]any{}
	switch rc.r.Method {
	case http.MethodGet, http.MethodHead:
		for k, v := range p.Args {
			c := rt.Arg(k)
			cv, err := query.Coerce(c, v)
			if err != nil {
				return err
			}
			args[c.DBName] = cv
		}
	case http.MethodPost:
		if rc.r.ContentLength != 0 {
			rows, isArray, err := readBody(rc.r)
			if err != nil {
				return err
			}
			if isArray || len(rows) != 1 {
				return &query.Error{Status: 400, Code: "FBRST102", Message: "RPC expects a JSON object of arguments"}
			}
			for k, v := range rows[0] {
				c := rt.Arg(k)
				if c == nil {
					return &query.Error{Status: 404, Code: "FBRST202",
						Message: fmt.Sprintf("%q has no argument named %q", rt.Name, k),
						Hint:    "arguments: " + argList(rt)}
				}
				cv, err := query.CoerceJSON(c, v)
				if err != nil {
					return err
				}
				args[c.DBName] = cv
			}
		}
	default:
		return &query.Error{Status: 405, Code: "FBRST101", Message: "RPC accepts GET, HEAD and POST"}
	}

	// positional arguments; trailing missing ones are omitted so their defaults apply
	var vals []any
	last := -1
	for i, c := range rt.In {
		if _, ok := args[c.DBName]; ok {
			last = i
		}
	}
	for i := 0; i <= last; i++ {
		vals = append(vals, args[rt.In[i].DBName])
	}
	for i := last + 1; i < len(rt.In); i++ {
		if !rt.In[i].HasDefault {
			vals = append(vals, nil)
		} else {
			break
		}
	}
	call := schema.Quote(rt.DBName)
	if len(vals) > 0 || rt.Kind == schema.Function {
		call += "(" + placeholders(len(vals)) + ")"
	}
	ctx := rc.r.Context()

	switch rt.Kind {
	case schema.Selectable:
		n, err := query.BuildRead(rc.sc, rt.AsRelation(), p, s.cfg.DBMaxRows)
		if err != nil {
			return err
		}
		if len(n.Children) > 0 {
			return &query.Error{Status: 400, Code: "FBRST200", Message: "procedure results cannot embed other resources"}
		}
		n.From, n.FromArgs = call, vals
		if err := applyRange(rc.r, n, p); err != nil {
			return err
		}
		return s.respondRead(rc, n)

	case schema.Executable:
		sqlText := "EXECUTE PROCEDURE " + call
		if len(rt.Out) == 0 {
			if _, err := rc.tx.ExecContext(ctx, sqlText, vals...); err != nil {
				return err
			}
			writeBody(rc, http.StatusNoContent, "", nil)
			return nil
		}
		rows, err := rc.tx.QueryContext(ctx, sqlText, vals...)
		if err != nil {
			return err
		}
		defer rows.Close()
		obj := &query.Obj{}
		if rows.Next() {
			v := make([]any, len(rt.Out))
			ptrs := make([]any, len(v))
			for i := range v {
				ptrs[i] = &v[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			for i, c := range rt.Out {
				obj.Set(c.Name, query.Output(v[i], c))
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		return s.writeValue(rc, obj)

	default: // function
		rows, err := rc.tx.QueryContext(ctx, "SELECT "+call+" FROM RDB$DATABASE", vals...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var v any
		if rows.Next() {
			if err := rows.Scan(&v); err != nil {
				return err
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		var col *schema.Column
		if len(rt.Out) > 0 {
			col = rt.Out[0]
		}
		return s.writeValue(rc, query.Output(v, col))
	}
}

func (s *Server) writeValue(rc *reqCtx, v any) error {
	mt, err := negotiate(rc.r, mimeJSON, mimeObject, "text/plain")
	if err != nil {
		return err
	}
	if mt == "text/plain" {
		writeBody(rc, http.StatusOK, mt, []byte(fmt.Sprint(v)))
		return nil
	}
	body, err := jsonMarshal(v)
	if err != nil {
		return err
	}
	writeBody(rc, http.StatusOK, mimeJSON, body)
	return nil
}

func argList(rt *schema.Routine) string {
	names := make([]string, len(rt.In))
	for i, c := range rt.In {
		names[i] = c.Name
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}
