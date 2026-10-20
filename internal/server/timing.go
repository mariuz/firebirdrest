package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mariuz/firebirdrest/internal/query"
	"github.com/mariuz/firebirdrest/internal/schema"
)

// timing records where a request spends its time, for the Server-Timing
// response header (server-timing-enabled) and the request duration metrics.
// The names follow PostgREST:
//
//	jwt          verifying the bearer token
//	parse        parsing the URL and the body, and building the SQL
//	transaction  time in the database: from BEGIN to COMMIT, less parse and response
//	response     rendering the result as JSON or CSV
type timing struct {
	jwt, parse, response time.Duration
	txStart              time.Time
	tx                   time.Duration // BEGIN to COMMIT, including parse and response
}

// since adds the time elapsed since start to *d.
func since(d *time.Duration, start time.Time) { *d += time.Since(start) }

// header returns the Server-Timing value.
func (t *timing) header() string {
	var sb strings.Builder
	add := func(name string, d time.Duration) {
		if sb.Len() > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(name)
		sb.WriteString(";dur=")
		sb.WriteString(strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 1, 64))
	}
	add("jwt", t.jwt)
	add("parse", t.parse)
	if t.tx > 0 {
		add("transaction", max(t.tx-t.parse-t.response, 0))
	}
	add("response", t.response)
	return sb.String()
}

// parse-phase wrappers

func (s *Server) buildRead(rc *reqCtx, rel *schema.Relation, p *query.Params, maxRows int) (*query.ReadNode, error) {
	defer since(&rc.timing.parse, time.Now())
	return query.BuildRead(rc.sc, rel, p, maxRows)
}

func (rc *reqCtx) readBody() ([]map[string]any, bool, error) {
	defer since(&rc.timing.parse, time.Now())
	return readBody(rc.r)
}

// txWriter holds the response written inside a transaction until the
// transaction commits. A failed commit (or any error after the handler wrote
// its response) then turns into a proper error response instead of a success
// already on its way to the client, and the headers set on the way (such as
// Content-Range) don't leak into that error response.
type txWriter struct {
	h      http.Header
	status int
	body   [][]byte
}

func newTxWriter() *txWriter { return &txWriter{h: http.Header{}} }

func (b *txWriter) Header() http.Header { return b.h }

func (b *txWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

// Write keeps p without copying it: the handlers hand over freshly built
// buffers they don't touch again.
func (b *txWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	b.body = append(b.body, p)
	return len(p), nil
}

// flush sends the held response, with extra headers added first.
func (b *txWriter) flush(w http.ResponseWriter, extra func(http.Header)) {
	h := w.Header()
	for k, v := range b.h {
		h[k] = v
	}
	if extra != nil {
		extra(h)
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	w.WriteHeader(b.status)
	for _, p := range b.body {
		w.Write(p)
	}
}

func (rc *reqCtx) singleObjectArgs(rt *schema.Routine) (map[string]any, error) {
	defer since(&rc.timing.parse, time.Now())
	return singleObjectArgs(rc.r, rt)
}
