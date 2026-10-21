package server

import (
	"bytes"
	"database/sql/driver"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nakagami/firebirdsql"

	"github.com/mariuz/firebirdrest/internal/query"
	"github.com/mariuz/firebirdrest/internal/schema"
)

const (
	mimeJSON    = "application/json"
	mimeObject  = "application/vnd.pgrst.object+json"
	mimeCSV     = "text/csv"
	mimeOpenAPI = "application/openapi+json"
)

func jsonMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// prefer holds the parsed Prefer header.
type prefer struct {
	ret         string         // minimal, representation, headers-only
	count       string         // exact, planned, estimated
	resolution  string         // merge-duplicates, ignore-duplicates
	tx          string         // commit, rollback
	missing     string         // default, null
	params      string         // single-object, multiple-objects (RPC)
	handling    string         // lenient (default), strict
	maxAffected int            // max-affected=n, enforced with handling=strict; 0 = none
	timezone    string         // timezone=<IANA name>
	loc         *time.Location // the loaded timezone
	applied     []string
	invalid     []string // preferences that were not understood
}

// preferValues lists the accepted values of each preference.
var preferValues = map[string][]string{
	"return":     {"minimal", "representation", "headers-only"},
	"count":      {"exact", "planned", "estimated"},
	"resolution": {"merge-duplicates", "ignore-duplicates"},
	"tx":         {"commit", "rollback"},
	"missing":    {"default", "null"},
	"params":     {"single-object", "multiple-objects"},
	"handling":   {"lenient", "strict"},
}

func parsePrefer(values []string) prefer {
	var p prefer
	for _, v := range values {
		for _, item := range strings.Split(v, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			k, val, _ := strings.Cut(item, "=")
			if k == "timezone" {
				loc, err := time.LoadLocation(val)
				if err != nil || val == "" || val == "Local" {
					p.invalid = append(p.invalid, item)
					continue
				}
				p.timezone, p.loc = val, loc
				p.applied = append(p.applied, item)
				continue
			}
			if k == "max-affected" {
				n, err := strconv.Atoi(val)
				if err != nil || n < 0 {
					p.invalid = append(p.invalid, item)
					continue
				}
				p.maxAffected = n
				p.applied = append(p.applied, item)
				continue
			}
			if !slices.Contains(preferValues[k], val) {
				p.invalid = append(p.invalid, item)
				continue
			}
			switch k {
			case "return":
				p.ret = val
			case "count":
				p.count = val
			case "resolution":
				p.resolution = val
			case "tx":
				p.tx = val
			case "missing":
				p.missing = val
			case "params":
				p.params = val
			case "handling":
				p.handling = val
			}
			p.applied = append(p.applied, item)
		}
	}
	return p
}

// check enforces Prefer: handling=strict, which refuses preferences that
// would otherwise be ignored. max-affected only applies in strict mode, as
// in PostgREST.
func (p *prefer) check() error {
	if p.handling != "strict" {
		p.maxAffected = 0
		return nil
	}
	if len(p.invalid) > 0 {
		return &query.Error{Status: 400, Code: "FBRST130",
			Message: "invalid preferences given with handling=strict",
			Details: strings.Join(p.invalid, ", ")}
	}
	return nil
}

// checkAffected enforces Prefer: max-affected after a write; the error
// rolls the transaction back.
func (p *prefer) checkAffected(n int64) error {
	if p.maxAffected > 0 && n > int64(p.maxAffected) {
		return &query.Error{Status: 400, Code: "FBRST131",
			Message: "the query affects more rows than allowed by max-affected",
			Details: fmt.Sprintf("%d rows would be affected, max-affected is %d", n, p.maxAffected),
			Hint:    "nothing was changed; narrow the filters, add a limit, or raise max-affected"}
	}
	return nil
}

// dropApplied removes a preference from Preference-Applied, e.g. a
// timezone the database refused.
func (p *prefer) dropApplied(key string) {
	out := p.applied[:0]
	for _, a := range p.applied {
		if k, _, _ := strings.Cut(a, "="); k != key {
			out = append(out, a)
		}
	}
	p.applied = out
}

// setApplied replaces the applied value of a preference, e.g. when
// count=planned had to fall back to an exact count.
func (p *prefer) setApplied(key, val string) {
	for i, a := range p.applied {
		if k, _, _ := strings.Cut(a, "="); k == key {
			p.applied[i] = key + "=" + val
			return
		}
	}
	p.applied = append(p.applied, key+"="+val)
}

// negotiate picks the response media type from the Accept header.
func negotiate(r *http.Request, allowed ...string) (string, error) {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return allowed[0], nil
	}
	for _, part := range strings.Split(accept, ",") {
		mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		if mt == "*/*" || mt == "application/*" {
			return allowed[0], nil
		}
		if slices.Contains(allowed, mt) {
			return mt, nil
		}
	}
	return "", &query.Error{Status: 406, Code: "FBRST107",
		Message: fmt.Sprintf("none of these media types are available: %s", accept)}
}

// rawType returns the media type to send a single value as, when the Accept
// header asks for a concrete type that isn't one of the built-in formats:
// text/plain, application/octet-stream, image/png, application/geo+json...
// It is "" when the header is empty, a wildcard, or lists a built-in type.
func rawType(r *http.Request, builtin ...string) string {
	first := ""
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		if strings.Contains(mt, "*") || slices.Contains(builtin, mt) || strings.HasPrefix(mt, mimePlan) {
			return ""
		}
		if first == "" && strings.Contains(mt, "/") {
			first = mt
		}
	}
	return first
}

// writeRaw sends one value as the body, with the requested media type: the
// bytes of a binary column, the text of a text column, or the JSON of any
// other scalar. The result must be one row of one column.
func writeRaw(rc *reqCtx, status int, mt string, rows []*query.Obj, col *schema.Column) error {
	if len(rows) != 1 {
		return &query.Error{Status: 406, Code: "FBRST116",
			Message: fmt.Sprintf("%s requested, but the result has %d rows instead of one", mt, len(rows)),
			Hint:    "a single value needs filters that match exactly one row"}
	}
	if len(rows[0].Keys) != 1 {
		return &query.Error{Status: 406, Code: "FBRST107",
			Message: fmt.Sprintf("%s requested, but %d columns were selected instead of one", mt, len(rows[0].Keys)),
			Hint:    "select the one column to return, e.g. select=image"}
	}
	var body []byte
	switch v := rows[0].Vals[0].(type) {
	case nil:
		writeBody(rc, http.StatusNoContent, "", nil)
		return nil
	case []byte:
		body = v
	case string:
		if col != nil && col.IsBinary() {
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return err
			}
			body = b
		} else {
			body = []byte(v)
		}
	case json.Number:
		body = []byte(v)
	default:
		body = query.AppendJSON(nil, v)
	}
	if strings.HasPrefix(mt, "text/") || strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml") || mt == "application/xml" {
		mt += "; charset=utf-8"
	}
	rc.w.Header().Set("Content-Type", mt)
	writeBody(rc, status, "", body)
	return nil
}

// writeRows writes a result set in the negotiated format.
func writeRows(rc *reqCtx, status int, rows []*query.Obj) error {
	defer since(&rc.timing.response, time.Now())
	if mt := rawType(rc.r, mimeJSON, mimeObject, mimeCSV); mt != "" {
		return writeRaw(rc, status, mt, rows, rc.rawCol)
	}
	mt, err := negotiate(rc.r, mimeJSON, mimeObject, mimeCSV)
	if err != nil {
		return err
	}
	var body []byte
	switch mt {
	case mimeObject:
		if len(rows) != 1 {
			return &query.Error{Status: 406, Code: "FBRST116",
				Message: "JSON object requested, multiple (or no) rows returned",
				Details: fmt.Sprintf("the result contains %d rows", len(rows))}
		}
		body = query.AppendJSON(make([]byte, 0, 512), rows[0])
	case mimeCSV:
		body, err = toCSV(rows)
	default:
		if rows == nil {
			rows = []*query.Obj{}
		}
		body = query.AppendJSON(make([]byte, 0, 256*len(rows)+2), rows)
		mt = mimeJSON
	}
	if err != nil {
		return err
	}
	writeBody(rc, status, mt, body)
	return nil
}

func writeBody(rc *reqCtx, status int, mt string, body []byte) {
	h := rc.w.Header()
	if len(rc.prefer.applied) > 0 {
		h.Set("Preference-Applied", strings.Join(rc.prefer.applied, ", "))
	}
	if mt != "" {
		h.Set("Content-Type", mt+"; charset=utf-8")
	}
	rc.w.WriteHeader(status)
	if rc.r.Method != http.MethodHead && body != nil {
		rc.w.Write(body)
	}
}

func toCSV(rows []*query.Obj) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if len(rows) == 0 {
		w.Flush()
		return buf.Bytes(), nil
	}
	w.Write(rows[0].Keys)
	for _, r := range rows {
		rec := make([]string, len(r.Vals))
		for i, v := range r.Vals {
			switch x := v.(type) {
			case nil:
				rec[i] = ""
			case string:
				rec[i] = x
			default:
				rec[i] = string(query.AppendJSON(nil, x))
			}
		}
		w.Write(rec)
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// readBody decodes a JSON (array or object) or form-encoded request body.
func readBody(r *http.Request) ([]map[string]any, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		return nil, false, err
	}
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	trimmed := bytes.TrimSpace(data)
	looksJSON := len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
	if mt == "application/x-www-form-urlencoded" && !looksJSON {
		vals, err := urlParseQuery(string(data))
		if err != nil {
			return nil, false, &query.Error{Status: 400, Code: "FBRST102", Message: "invalid form body"}
		}
		m := map[string]any{}
		for k, v := range vals {
			m[k] = v[0]
		}
		return []map[string]any{m}, false, nil
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, false, &query.Error{Status: 400, Code: "FBRST102", Message: "empty or invalid json body"}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if data[0] == '[' {
		var arr []map[string]any
		if err := dec.Decode(&arr); err != nil {
			return nil, true, &query.Error{Status: 400, Code: "FBRST102", Message: "invalid json body: " + err.Error()}
		}
		return arr, true, nil
	}
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, false, &query.Error{Status: 400, Code: "FBRST102", Message: "invalid json body: " + err.Error()}
	}
	return []map[string]any{obj}, false, nil
}

// apiError is the JSON error body, compatible with PostgREST's format.
type apiError struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Details *string `json:"details"`
	Hint    *string `json:"hint"`
	status  int
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// toAPIError maps query errors and Firebird errors to HTTP responses.
func toAPIError(err error) *query.Error {
	var qe *query.Error
	if errors.As(err, &qe) {
		return qe
	}
	var fe *firebirdsql.FbError
	if errors.As(err, &fe) {
		msg := strings.TrimSpace(fe.Message)
		e := &query.Error{Status: 400, Code: fe.SQLState, Message: firstLine(msg), Details: rest(msg)}
		if e.Code == "" {
			e.Code = strconv.Itoa(int(fe.SQLCode))
		}
		switch {
		case slices.Contains(fe.GDSCodes, firebirdsql.ISCUniqueKeyViolation) || slices.Contains(fe.GDSCodes, firebirdsql.ISCForeignKey):
			e.Status = 409 // unique / foreign key violation
		case fe.SQLState == "28000" || strings.Contains(strings.ToLower(msg), "no permission"):
			e.Status = 403
		case strings.HasPrefix(fe.SQLState, "40"):
			e.Status = 409 // deadlock / update conflict
		case strings.HasPrefix(fe.SQLState, "08"):
			e.Status = 503
		case strings.HasPrefix(fe.SQLState, "HY") || strings.HasPrefix(fe.SQLState, "XX"):
			e.Status = 500
		}
		if slices.Contains(fe.GDSCodes, 335544517) { // isc_except: user-defined EXCEPTION
			// Firebird reports "exception <id>\n<NAME>\n<message>\n<where>...";
			// lead with the exception's own message.
			e.Status = 400
			lines := strings.Split(msg, "\n")
			if len(lines) >= 3 {
				e.Message = strings.TrimSpace(lines[2])
				e.Details = strings.TrimSpace(strings.Join(lines[3:], "\n"))
				e.Hint = "exception " + plainName(strings.TrimSpace(lines[1]))
			}
		}
		return e
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.EOF) {
		return &query.Error{Status: 503, Code: "FBRST001", Message: "the database is not reachable",
			Details: err.Error()}
	}
	return &query.Error{Status: 500, Code: "FBRST000", Message: err.Error()}
}

// plainName turns Firebird 6's schema-qualified "PUBLIC"."NAME" into NAME,
// the form earlier versions report.
func plainName(s string) string {
	s = strings.TrimPrefix(s, `"PUBLIC".`)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' && !strings.Contains(s[1:len(s)-1], `"`) {
		s = s[1 : len(s)-1]
	}
	return s
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(l)
}

func rest(s string) string {
	_, r, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(r)
}

func writeError(w http.ResponseWriter, e *query.Error) {
	status := e.Status
	if status == 0 {
		status = 500
	}
	if status == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer`)
	}
	body, _ := jsonMarshal(apiError{Code: e.Code, Message: e.Message, Details: strPtr(e.Details), Hint: strPtr(e.Hint)})
	w.Header().Set("Content-Type", mimeJSON+"; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}
