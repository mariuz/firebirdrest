package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

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
	// calls holds the arguments of each call, by parameter DB name. There is
	// one call, except with Prefer: params=multiple-objects.
	var calls []map[string]any
	multi := false
	switch rc.r.Method {
	case http.MethodGet, http.MethodHead:
		args := map[string]any{}
		for k, v := range p.Args {
			c := rt.Arg(k)
			cv, err := query.Coerce(c, v)
			if err != nil {
				return err
			}
			args[c.DBName] = cv
		}
		calls = append(calls, args)
	case http.MethodPost:
		switch rc.prefer.params {
		case "single-object":
			args, err := rc.singleObjectArgs(rt)
			if err != nil {
				return err
			}
			calls = append(calls, args)
		case "multiple-objects":
			multi = true
			rows, _, err := rc.readBody()
			if err != nil {
				return err
			}
			for _, row := range rows {
				args, err := jsonArgs(rt, row)
				if err != nil {
					return err
				}
				calls = append(calls, args)
			}
		default:
			args := map[string]any{}
			if raw := rawBodyType(rc.r); raw != "" {
				if args, err = rc.rawArgs(rt, raw); err != nil {
					return err
				}
			} else if rc.r.ContentLength != 0 {
				rows, isArray, err := rc.readBody()
				if err != nil {
					return err
				}
				if isArray || len(rows) != 1 {
					return &query.Error{Status: 400, Code: "FBRST102", Message: "RPC expects a JSON object of arguments",
						Hint: "to call the routine once per object of an array, send Prefer: params=multiple-objects"}
				}
				if args, err = jsonArgs(rt, rows[0]); err != nil {
					return err
				}
			}
			calls = append(calls, args)
		}
	default:
		return &query.Error{Status: 405, Code: "FBRST101", Message: "RPC accepts GET, HEAD and POST"}
	}
	switch rt.Kind {
	case schema.Selectable:
		n, err := s.buildRead(rc, rt.AsRelation(), p, s.cfg.DBMaxRows)
		if err != nil {
			return err
		}
		if len(n.Children) > 0 {
			return &query.Error{Status: 400, Code: "FBRST200", Message: "procedure results cannot embed other resources"}
		}
		if len(calls) == 0 {
			if rc.prefer.count != "" {
				rc.w.Header().Set("Content-Range", "*/0")
			}
			return writeRows(rc, http.StatusOK, nil)
		}
		if len(calls) > maxUnionCalls {
			return &query.Error{Status: 400, Code: "FBRST128",
				Message: fmt.Sprintf("too many calls: %d objects, at most %d for a selectable procedure", len(calls), maxUnionCalls),
				Hint:    "split the array over several requests"}
		}
		if len(calls) == 1 {
			n.From, n.FromArgs = callSQL(rt, calls[0])
		} else {
			// one query over all calls, so select=, filters, order and limit
			// apply to the combined result
			parts := make([]string, len(calls))
			for i, args := range calls {
				call, vals := callSQL(rt, args)
				parts[i] = "SELECT * FROM " + call
				n.FromArgs = append(n.FromArgs, vals...)
			}
			n.From = "(" + strings.Join(parts, " UNION ALL ") + ")"
		}
		if err := applyRange(rc.r, n, p); err != nil {
			return err
		}
		return s.respondRead(rc, n)

	case schema.Executable:
		var out []*query.Obj
		for _, args := range calls {
			call, vals := callSQL(rt, args)
			obj, err := executeProcedure(rc, rt, call, vals)
			if err != nil {
				return err
			}
			out = append(out, obj)
		}
		if len(rt.Out) == 0 {
			writeBody(rc, http.StatusNoContent, "", nil)
			return nil
		}
		if multi {
			return writeRows(rc, http.StatusOK, out)
		}
		var col *schema.Column
		if len(rt.Out) == 1 {
			col = rt.Out[0]
		}
		return s.writeValue(rc, out[0], col)

	default: // function
		var col *schema.Column
		if len(rt.Out) > 0 {
			col = rt.Out[0]
		}
		vals := make([]any, 0, len(calls))
		for _, args := range calls {
			call, args := callSQL(rt, args)
			v, err := callFunction(rc, "SELECT "+call+" FROM RDB$DATABASE", args)
			if err != nil {
				return err
			}
			vals = append(vals, query.OutputIn(v, col, query.TimeZoneOf(rc.r.Context())))
		}
		if multi {
			return s.writeValue(rc, vals, col)
		}
		return s.writeValue(rc, vals[0], col)
	}
}

// maxUnionCalls caps a multiple-objects call of a selectable procedure:
// Firebird allows 256 procedure/table contexts per statement, and each
// call is one branch of a UNION ALL.
const maxUnionCalls = 200

// jsonArgs maps one JSON object of arguments to parameter DB names.
func jsonArgs(rt *schema.Routine, row map[string]any) (map[string]any, error) {
	args := map[string]any{}
	for k, v := range row {
		c := rt.Arg(k)
		if c == nil {
			return nil, &query.Error{Status: 404, Code: "FBRST202",
				Message: fmt.Sprintf("%q has no argument named %q", rt.Name, k),
				Hint:    "arguments: " + argList(rt)}
		}
		cv, err := query.CoerceJSON(c, v)
		if err != nil {
			return nil, err
		}
		args[c.DBName] = cv
	}
	return args, nil
}

// singleObjectArgs passes the raw request body to the routine's only
// parameter, which must be text (Firebird has no JSON type, so the routine
// receives the JSON document as text).
func singleObjectArgs(r *http.Request, rt *schema.Routine) (map[string]any, error) {
	if len(rt.In) != 1 || !isText(rt.In[0]) {
		return nil, &query.Error{Status: 400, Code: "FBRST129",
			Message: fmt.Sprintf("Prefer: params=single-object needs a routine with a single text parameter; %q has (%s)", rt.Name, argList(rt)),
			Hint:    "declare one VARCHAR or BLOB SUB_TYPE TEXT parameter to receive the JSON body"}
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if !json.Valid(data) {
		return nil, &query.Error{Status: 400, Code: "FBRST102", Message: "invalid json body"}
	}
	c := rt.In[0]
	if c.FieldType != schema.TBlob && c.CharLength > 0 && utf8.RuneCount(data) > c.CharLength {
		return nil, &query.Error{Status: 400, Code: "FBRST102",
			Message: fmt.Sprintf("the body is longer than parameter %q (%d characters)", c.Name, c.CharLength),
			Hint:    "use a BLOB SUB_TYPE TEXT parameter for large documents"}
	}
	return map[string]any{c.DBName: string(data)}, nil
}

// rawBodyType returns the request's media type when the body is sent as is
// (text/plain, application/octet-stream, image/png...) instead of as JSON or
// a form; "" otherwise.
func rawBodyType(r *http.Request) string {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt == mimeJSON || mt == "application/x-www-form-urlencoded" ||
		strings.HasSuffix(mt, "+json") || r.ContentLength == 0 {
		return ""
	}
	return mt
}

// rawArgs passes a raw body to the routine's only parameter: as text for a
// text/* body, as bytes otherwise.
func (rc *reqCtx) rawArgs(rt *schema.Routine, mt string) (map[string]any, error) {
	defer since(&rc.timing.parse, time.Now())
	if len(rt.In) != 1 {
		return nil, &query.Error{Status: 400, Code: "FBRST133",
			Message: fmt.Sprintf("a %s body goes to a single parameter, but %q has (%s)", mt, rt.Name, argList(rt)),
			Hint:    "send a JSON object of arguments, or declare one parameter to receive the body"}
	}
	data, err := io.ReadAll(io.LimitReader(rc.r.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	c := rt.In[0]
	if strings.HasPrefix(mt, "text/") || isText(c) {
		if c.FieldType != schema.TBlob && c.CharLength > 0 && utf8.RuneCount(data) > c.CharLength {
			return nil, &query.Error{Status: 400, Code: "FBRST102",
				Message: fmt.Sprintf("the body is longer than parameter %q (%d characters)", c.Name, c.CharLength),
				Hint:    "use a BLOB parameter for large bodies"}
		}
		return map[string]any{c.DBName: string(data)}, nil
	}
	return map[string]any{c.DBName: data}, nil
}

func isText(c *schema.Column) bool {
	switch c.FieldType {
	case schema.TVarchar, schema.TChar:
		return c.SubType != 1 // not OCTETS
	case schema.TBlob:
		return c.SubType == 1
	}
	return false
}

// callSQL returns the call expression NAME(?, ...) and its arguments.
// Trailing arguments that are missing are omitted so their defaults apply;
// other missing arguments are NULL.
func callSQL(rt *schema.Routine, args map[string]any) (string, []any) {
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
	call := rt.SQLName()
	if len(vals) > 0 || rt.Kind == schema.Function {
		call += "(" + placeholders(len(vals)) + ")"
	}
	return call, vals
}

// callFunction runs a function call and returns its scalar result.
func callFunction(rc *reqCtx, sqlText string, args []any) (any, error) {
	rows, err := rc.tx.QueryContext(rc.r.Context(), sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var v any
	if rows.Next() {
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
	}
	return v, rows.Err()
}

// executeProcedure runs EXECUTE PROCEDURE and returns its output parameters.
func executeProcedure(rc *reqCtx, rt *schema.Routine, call string, vals []any) (*query.Obj, error) {
	ctx := rc.r.Context()
	sqlText := "EXECUTE PROCEDURE " + call
	if len(rt.Out) == 0 {
		_, err := rc.tx.ExecContext(ctx, sqlText, vals...)
		return nil, err
	}
	rows, err := rc.tx.QueryContext(ctx, sqlText, vals...)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		for i, c := range rt.Out {
			obj.Set(c.Name, query.OutputIn(v[i], c, query.TimeZoneOf(rc.r.Context())))
		}
	}
	return obj, rows.Err()
}

// writeValue writes the result of a function or an executable procedure:
// JSON, or with a concrete Accept type (text/plain, image/png...) the bare
// value. col describes the value when it is one output parameter.
func (s *Server) writeValue(rc *reqCtx, v any, col *schema.Column) error {
	defer since(&rc.timing.response, time.Now())
	if mt := rawType(rc.r, mimeJSON, mimeObject); mt != "" {
		var rows []*query.Obj
		switch x := v.(type) {
		case *query.Obj:
			rows = []*query.Obj{x}
		case []any: // Prefer: params=multiple-objects
			for _, e := range x {
				rows = append(rows, &query.Obj{Keys: []string{"value"}, Vals: []any{e}})
			}
		default:
			rows = []*query.Obj{{Keys: []string{"value"}, Vals: []any{v}}}
		}
		return writeRaw(rc, http.StatusOK, mt, rows, col)
	}
	if _, err := negotiate(rc.r, mimeJSON, mimeObject); err != nil {
		return err
	}
	writeBody(rc, http.StatusOK, mimeJSON, query.AppendJSON(nil, v))
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
