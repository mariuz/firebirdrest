package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mariuz/firebirdrest/internal/query"
	"github.com/mariuz/firebirdrest/internal/schema"
)

// acquire takes a connection from the pool, waiting at most timeout seconds
// (db-pool-acquisition-timeout; 0 waits as long as the request may take).
func acquire(ctx context.Context, db *sql.DB, timeout int) (*sql.Conn, error) {
	if timeout <= 0 {
		return db.Conn(ctx)
	}
	actx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	conn, err := db.Conn(actx)
	if err != nil && ctx.Err() == nil && actx.Err() != nil {
		return nil, &query.Error{Status: 504, Code: "FBRST003",
			Message: fmt.Sprintf("timed out after %d s waiting for a database connection (db-pool-acquisition-timeout)", timeout),
			Hint:    "every connection of the pool is busy: raise db-pool, or look for slow requests"}
	}
	return conn, err
}

// Response status and headers set from SQL, as PostgREST's response.status
// and response.headers settings: a procedure, function or trigger sets the
// USER_TRANSACTION context variables
//
//	response.status   '201'
//	response.headers  '[{"Location": "/books?id=eq.7"}, {"Cache-Control": "no-cache"}]'
//
// A context variable holds 255 characters, so longer headers continue in
// response.headers.2, response.headers.3 ... up to respChunks pieces, which
// are joined before parsing.
const respChunks = 16

// responseVarsSQL reads the variables and clears them, so a reused read
// transaction doesn't hand them to the next request.
var responseVarsSQL = strings.Join(strings.Fields(fmt.Sprintf(`EXECUTE BLOCK RETURNS (st VARCHAR(255), h VARCHAR(%d)) AS
DECLARE r INTEGER; DECLARE i INTEGER = 1; DECLARE n VARCHAR(30); DECLARE v VARCHAR(255);
BEGIN
  st = RDB$GET_CONTEXT('USER_TRANSACTION', 'response.status');
  IF (st IS NOT NULL) THEN r = RDB$SET_CONTEXT('USER_TRANSACTION', 'response.status', NULL);
  WHILE (i <= %d) DO BEGIN
    n = IIF(i = 1, 'response.headers', 'response.headers.' || i);
    v = RDB$GET_CONTEXT('USER_TRANSACTION', n);
    IF (v IS NULL) THEN LEAVE;
    h = COALESCE(h, '') || v;
    r = RDB$SET_CONTEXT('USER_TRANSACTION', n, NULL);
    i = i + 1;
  END
  SUSPEND;
END`, 255*respChunks, respChunks)), " ")

// ignoredHeaders are managed by the HTTP server and can't be set from SQL.
var ignoredHeaders = map[string]bool{"Content-Length": true, "Transfer-Encoding": true, "Connection": true}

// applyResponseVars reads response.status and response.headers after the
// request's statements ran, and applies them to the held response. Invalid
// values fail the request (and roll the transaction back).
func (s *Server) applyResponseVars(rc *reqCtx, held *txWriter) error {
	rows, err := rc.tx.QueryContext(rc.r.Context(), responseVarsSQL)
	if err != nil {
		return err
	}
	defer rows.Close()
	var st, hdr sql.NullString
	if rows.Next() {
		if err := rows.Scan(&st, &hdr); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if held == nil {
		return nil
	}
	status := 0
	if st.Valid {
		n, err := strconv.Atoi(strings.TrimSpace(st.String))
		if err != nil || n < 200 || n > 599 {
			return &query.Error{Status: 500, Code: "FBRST140",
				Message: "response.status must be an HTTP status code from 200 to 599",
				Details: fmt.Sprintf("response.status is %q", st.String)}
		}
		status = n
	}
	var pairs [][2]string
	if hdr.Valid && hdr.String != "" {
		if pairs, err = parseResponseHeaders(hdr.String); err != nil {
			return &query.Error{Status: 500, Code: "FBRST139",
				Message: "response.headers must be a JSON array of objects with string values, e.g. [{\"Cache-Control\": \"no-cache\"}]",
				Details: err.Error()}
		}
	}
	if status != 0 {
		held.status = status
	}
	seen := map[string]bool{}
	for _, p := range pairs {
		name := http.CanonicalHeaderKey(p[0])
		if ignoredHeaders[name] {
			continue
		}
		if seen[name] {
			held.h.Add(name, p[1]) // several Set-Cookie headers
		} else {
			held.h.Set(name, p[1])
			seen[name] = true
		}
	}
	return nil
}

// parseResponseHeaders reads [{"Name": "value"}, ...] keeping the order.
func parseResponseHeaders(s string) ([][2]string, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	expect := func(want json.Delim) error {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); !ok || d != want {
			return fmt.Errorf("expected %q, got %v", want, t)
		}
		return nil
	}
	if err := expect('['); err != nil {
		return nil, err
	}
	var out [][2]string
	for dec.More() {
		if err := expect('{'); err != nil {
			return nil, err
		}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			vt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			v, ok := vt.(string)
			if !ok {
				return nil, fmt.Errorf("the value of %q is not a string", k)
			}
			if !validHeaderName(k) {
				return nil, fmt.Errorf("%q is not a header name", k)
			}
			if strings.ContainsAny(v, "\r\n\x00") {
				return nil, fmt.Errorf("the value of %q contains a line break", k)
			}
			out = append(out, [2]string{k, v})
		}
		if err := expect('}'); err != nil {
			return nil, err
		}
	}
	if err := expect(']'); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after the array")
	}
	return out, nil
}

// validHeaderName reports whether s is an HTTP header field name (a token).
func validHeaderName(s string) bool {
	for _, r := range s {
		if r > 126 || r <= 32 || strings.ContainsRune("\"(),/:;<=>?@[\\]{}", r) {
			return false
		}
	}
	return s != ""
}

// rootSpec answers GET / with the result of the db-root-spec routine, sent
// as is (a function's return value, or the first output of a procedure)
// with Content-Type application/openapi+json, unless the routine sets
// another through response.headers.
func (s *Server) rootSpec(w http.ResponseWriter, r *http.Request, sc *schema.Schema) error {
	rt := sc.Routine(s.cfg.DBRootSpec)
	if rt == nil {
		return &query.Error{Status: 404, Code: "FBRST202",
			Message: fmt.Sprintf("could not find the procedure or function %q (db-root-spec) in the schema cache", s.cfg.DBRootSpec)}
	}
	if len(rt.Out) == 0 {
		return &query.Error{Status: 500, Code: "FBRST141",
			Message: fmt.Sprintf("db-root-spec %q returns nothing", s.cfg.DBRootSpec),
			Hint:    "use a function, or a procedure with an output parameter"}
	}
	rc := &reqCtx{w: w, r: r, sc: sc, traceHeader: s.cfg.TraceHeader,
		respond: rt.SetsResponse || s.preRequestResponds(sc)}
	if err := rc.prefer.txEnd(s.cfg.DBTxEnd); err != nil {
		return err
	}
	if err := s.authenticate(rc); err != nil {
		return err
	}
	err := s.withTx(rc, true, func() error {
		call, vals := callSQL(rt, nil)
		var q string
		switch rt.Kind {
		case schema.Function:
			q = "SELECT " + call + " FROM RDB$DATABASE"
		case schema.Selectable:
			q = "SELECT FIRST 1 * FROM " + call
		default:
			q = "EXECUTE PROCEDURE " + call
		}
		rows, err := rc.tx.QueryContext(rc.r.Context(), q, vals...)
		if err != nil {
			return err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		var v any
		if rows.Next() {
			out := make([]any, len(cols))
			ptrs := make([]any, len(out))
			for i := range out {
				ptrs[i] = &out[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			v = out[0]
		}
		if err := rows.Err(); err != nil {
			return err
		}
		var body []byte
		switch x := v.(type) {
		case []byte:
			body = x
		case string:
			body = []byte(x)
		case nil:
			body = []byte("null")
		default:
			if body, err = json.Marshal(x); err != nil {
				return err
			}
		}
		rc.w.Header().Set("Content-Type", mimeOpenAPI+"; charset=utf-8")
		rc.w.WriteHeader(http.StatusOK)
		_, err = io.Copy(rc.w, bytes.NewReader(body))
		return err
	})
	if err != nil && rc.anon {
		if e := toAPIError(err); e.Status == 403 {
			e.Status = 401
			return e
		}
	}
	return err
}
