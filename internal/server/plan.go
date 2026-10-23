package server

import (
	"context"
	"database/sql"
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/mariuz/firebirdrest/internal/query"
)

// Query plans: Accept: application/vnd.pgrst.plan[+text|+json] returns the
// execution plans of the statements a request runs, as Firebird explains them
// (MON$STATEMENTS.MON$EXPLAINED_PLAN), instead of the data. Like PostgREST it
// is off unless db-plan-enabled is set, since plans reveal tables and indexes.
//
// Embedded resources are read with one statement per level, built from the
// keys of the level above, so the request is run (in its read-only
// transaction, output discarded) to learn every statement; each one is then
// prepared again and its plan read from the monitoring tables. Only GET and
// HEAD are allowed, so nothing is written.

const mimePlan = "application/vnd.pgrst.plan"

type planCapture struct {
	format  string // "text" or "json"
	queries []string
	plans   []statementPlan
}

type statementPlan struct {
	Query string `json:"query"`
	Plan  string `json:"plan"`
}

// planFormat returns "text" or "json" when the Accept header asks for a plan.
func planFormat(r *http.Request) string {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, _ := parseAccept(part)
		switch mt {
		case mimePlan, mimePlan + "+text":
			return "text"
		case mimePlan + "+json":
			return "json"
		}
	}
	return ""
}

// startPlan switches the request to plan mode: the handlers run as for the
// media type in the for= parameter (default JSON) and write to a discarded
// response.
func (s *Server) startPlan(rc *reqCtx, format string) error {
	if !s.cfg.DBPlan {
		return &query.Error{Status: 406, Code: "FBRST107", Message: "query plans are disabled",
			Hint: "set db-plan-enabled = true to allow Accept: " + mimePlan}
	}
	if rc.r.Method != http.MethodGet && rc.r.Method != http.MethodHead {
		return &query.Error{Status: 405, Code: "FBRST132",
			Message: "query plans are available for GET and HEAD requests only",
			Hint:    "a plan of a write would require running it"}
	}
	target := mimeJSON
	for _, part := range strings.Split(rc.r.Header.Get("Accept"), ",") {
		if mt, params := parseAccept(part); strings.HasPrefix(mt, mimePlan) && params["for"] != "" {
			target = params["for"]
		}
	}
	r := rc.r.Clone(rc.r.Context())
	r.Header.Set("Accept", target)
	rc.r = r
	rc.w = &discardWriter{h: http.Header{}}
	rc.plan = &planCapture{format: format}
	return nil
}

// explain prepares every recorded statement again and reads its plan. All
// statements are prepared before MON$STATEMENTS is read, because the
// monitoring tables are a snapshot taken at their first use in a transaction.
func (p *planCapture) explain(rc *reqCtx) error {
	ctx := rc.r.Context()
	var order []string
	seen := map[string]bool{}
	for _, q := range p.queries {
		if !seen[q] {
			seen[q] = true
			order = append(order, q)
		}
	}
	var stmts []*sql.Stmt
	defer func() {
		for _, st := range stmts {
			st.Close()
		}
	}()
	for _, q := range order {
		st, err := rc.tx.tx.PrepareContext(ctx, q)
		if err != nil {
			return err
		}
		stmts = append(stmts, st)
	}
	plans, err := monitoredPlans(ctx, rc.tx.tx)
	if err != nil {
		return err
	}
	for _, q := range order {
		p.plans = append(p.plans, statementPlan{Query: q, Plan: strings.TrimSpace(plans[q])})
	}
	return nil
}

func monitoredPlans(ctx context.Context, tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT MON$SQL_TEXT, MON$EXPLAINED_PLAN FROM MON$STATEMENTS
WHERE MON$ATTACHMENT_ID = CURRENT_CONNECTION AND MON$EXPLAINED_PLAN IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var text, plan any
		if err := rows.Scan(&text, &plan); err != nil {
			return nil, err
		}
		out[blobString(text)] = blobString(plan)
	}
	return out, rows.Err()
}

func blobString(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func (p *planCapture) write(w http.ResponseWriter) error {
	if p.format == "json" {
		w.Header().Set("Content-Type", mimePlan+"+json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		var b []byte
		b = append(b, '[')
		for i, sp := range p.plans {
			if i > 0 {
				b = append(b, ',')
			}
			o := &query.Obj{}
			o.Set("query", sp.Query)
			o.Set("plan", sp.Plan)
			b = query.AppendJSON(b, o)
		}
		b = append(b, ']')
		_, err := w.Write(b)
		return err
	}
	var sb strings.Builder
	for i, sp := range p.plans {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "-- statement %d\n%s\n\n%s", i+1, sp.Query, sp.Plan)
	}
	sb.WriteString("\n")
	w.Header().Set("Content-Type", mimePlan+"+text; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(sb.String()))
	return err
}

// parseAccept parses one entry of an Accept header. Unlike
// mime.ParseMediaType it tolerates parameters without a value, which
// postgrest-js sends for plans ("...plan+text; for=\"application/json\";
// options=;"), and returns the media type even when a parameter is malformed.
func parseAccept(part string) (string, map[string]string) {
	part = strings.TrimSpace(part)
	if mt, params, err := mime.ParseMediaType(part); err == nil {
		return mt, params
	}
	mt, rest, _ := strings.Cut(part, ";")
	params := map[string]string{}
	for _, p := range strings.Split(rest, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && v != "" {
			params[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return strings.ToLower(strings.TrimSpace(mt)), params
}

// discardWriter swallows the response of a request run for its plan.
type discardWriter struct{ h http.Header }

func (d *discardWriter) Header() http.Header         { return d.h }
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}
