// Package server implements the HTTP API.
package server

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	_ "github.com/nakagami/firebirdsql"

	"github.com/mariuz/firebirdrest/internal/config"
	"github.com/mariuz/firebirdrest/internal/query"
	"github.com/mariuz/firebirdrest/internal/schema"
)

// Server serves the REST API for one Firebird database.
type Server struct {
	cfg     *config.Config
	base    *sql.DB // connection without a role, used to read the schema
	catalog atomic.Pointer[schema.Catalog]
	log     *slog.Logger
	m       *metrics
	jwt     *jwtVerifier

	mu    sync.Mutex
	pools map[string]*rolePool
	basep *rolePool
}

// rolePool is the connection pool of one Firebird role plus its statement cache.
type rolePool struct {
	db    *sql.DB
	cache *stmtCache // nil when db-prepared-statements = false
}

func (s *Server) newRolePool(db *sql.DB) *rolePool {
	rp := &rolePool{db: db}
	if s.cfg.DBPreparedStatements > 0 {
		rp.cache = newStmtCache(db, s.cfg.DBPreparedStatements, time.Duration(s.cfg.DBPreparedStatementsIdle)*time.Second)
	}
	return rp
}

// New opens the database and loads the schema cache.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Server, error) {
	db, err := openPool(cfg.DBURI, "", cfg.DBPool)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, base: db, log: log, pools: map[string]*rolePool{}, m: newMetrics()}
	jv, err := newJWTVerifier(cfg, log, s.m)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.jwt = jv
	s.basep = s.newRolePool(db)
	if err := s.Reload(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Reload re-reads the schema cache.
func (s *Server) Reload(ctx context.Context) error {
	start := time.Now()
	cat, err := schema.LoadCatalog(ctx, s.base, s.cfg.DBSchemas)
	s.m.reloads.Add(1)
	if err != nil {
		s.m.reloadErrors.Add(1)
		return fmt.Errorf("loading schema: %w", err)
	}
	s.catalog.Store(cat)
	s.m.lastLoadNanos.Store(int64(time.Since(start)))
	s.m.lastLoadAt.Store(time.Now().Unix())
	s.jwt.clearCache()
	// cached statements may refer to objects that were just altered
	s.mu.Lock()
	for _, rp := range append(s.poolList(), s.basep) {
		if rp != nil && rp.cache != nil {
			rp.cache.clear()
		}
	}
	s.mu.Unlock()
	for _, sc := range cat.Schemas {
		s.log.Info("schema cache loaded", "firebird", cat.Version, "schema", sc.Name, "relations", len(sc.Relations),
			"routines", len(sc.Routines), "foreign_keys", len(sc.ForeignKeys),
			"request_context", s.cfg.DBRequestContext == "always" || s.cfg.DBRequestContext != "never" && sc.UsesRequestContext,
			"took", time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// Close releases all connections.
func (s *Server) Close() {
	s.jwt.close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range append(s.poolList(), s.basep) {
		if p.cache != nil {
			p.cache.close()
		}
		if p.db != s.base {
			p.db.Close()
		}
	}
	s.base.Close()
}

// Ready reports whether the database answers.
func (s *Server) Ready(ctx context.Context) error {
	return s.base.PingContext(ctx)
}

func withParam(dsn, key, value string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + key + "=" + url.QueryEscape(value)
}

func openPool(dsn, role string, size int) (*sql.DB, error) {
	if role != "" {
		dsn = withParam(dsn, "role", role)
	}
	db, err := sql.Open("firebirdsql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(size)
	db.SetMaxIdleConns(size)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}

var roleName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)

// pool returns the connection pool for a Firebird role, creating it on first use.
// Firebird silently falls back to no role when the user is not granted the
// requested one, so the effective CURRENT_ROLE is checked before use.
func (s *Server) poolList() []*rolePool {
	out := make([]*rolePool, 0, len(s.pools))
	for _, p := range s.pools {
		out = append(out, p)
	}
	return out
}

func (s *Server) pool(ctx context.Context, role string) (*rolePool, error) {
	if role == "" {
		return s.basep, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pools[role]; ok {
		return p, nil
	}
	p, err := openPool(s.cfg.DBURI, role, s.cfg.DBPool)
	if err != nil {
		return nil, err
	}
	var current string
	if err := p.QueryRowContext(ctx, `SELECT CURRENT_ROLE FROM RDB$DATABASE`).Scan(&current); err != nil {
		p.Close()
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(current), role) {
		p.Close()
		return nil, &query.Error{Status: 403, Code: "FBRST300",
			Message: fmt.Sprintf("role %q is not granted to the database user", role),
			Hint:    fmt.Sprintf("GRANT %s TO USER <authenticator>", role)}
	}
	rp := s.newRolePool(p)
	s.pools[role] = rp
	return rp, nil
}

// reqCtx carries per-request state.
type reqCtx struct {
	w      http.ResponseWriter
	r      *http.Request
	sc     *schema.Schema
	role   string
	anon   bool
	claims map[string]any
	prefer prefer
	tx     *cachedTx
	status int
	plan   *planCapture // set for Accept: application/vnd.pgrst.plan
	timing timing
	rawCol *schema.Column // the single selected column, for raw responses
}

// ServeHTTP routes requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rw := &statusWriter{ResponseWriter: w, status: 200}
	errCode := ""
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("panic", "err", rec, "path", r.URL.Path)
			errCode = "FBRST000"
			writeError(rw, &query.Error{Status: 500, Code: errCode, Message: "internal server error"})
		}
		s.m.request(r.Method, rw.status, time.Since(start), errCode)
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rw.status,
			"took", time.Since(start).Round(time.Microsecond))
	}()
	s.cors(rw, r)
	if r.Method == http.MethodOptions {
		rw.Header().Set("Allow", "OPTIONS,GET,HEAD,POST,PUT,PATCH,DELETE")
		rw.WriteHeader(http.StatusOK)
		return
	}
	if err := s.handle(rw, r); err != nil {
		e := toAPIError(err)
		errCode = e.Code
		writeError(rw, e)
	}
}

func (s *Server) cors(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || s.cfg.CORSOrigins == "" {
		return
	}
	allowed := s.cfg.CORSOrigins == "*"
	for _, o := range strings.Split(s.cfg.CORSOrigins, ",") {
		if strings.TrimSpace(o) == origin {
			allowed = true
		}
	}
	if !allowed {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Expose-Headers", "Content-Encoding, Content-Location, Content-Range, Content-Type, Date, Location, Server, Transfer-Encoding, Range-Unit, Preference-Applied, Content-Profile")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS, HEAD")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Accept-Language, Content-Language, Prefer, Range, Accept-Profile, Content-Profile")
		h.Set("Access-Control-Max-Age", "86400")
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) error {
	cat := s.catalog.Load()
	sc, err := pickSchema(cat, r)
	if err != nil {
		return err
	}
	if len(cat.Schemas) > 1 {
		w.Header().Set("Content-Profile", sc.Name)
	}
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	for i := range parts {
		if u, err := url.PathUnescape(parts[i]); err == nil && utf8.ValidString(u) {
			parts[i] = u
		}
	}

	if path == "" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return &query.Error{Status: 405, Code: "FBRST117", Message: "only GET is allowed on /"}
		}
		return s.openAPI(w, r, sc)
	}

	rc := &reqCtx{w: w, r: r, sc: sc, prefer: parsePrefer(r.Header.Values("Prefer"))}
	if err := rc.prefer.check(); err != nil {
		return err
	}
	jwtStart := time.Now()
	err = s.authenticate(rc)
	since(&rc.timing.jwt, jwtStart)
	if err != nil {
		return err
	}
	if rc.prefer.loc != nil {
		if !hasTimeZones(sc.Version) {
			// Firebird 3 has no time zone support: nothing to apply
			rc.prefer.dropApplied("timezone")
			rc.prefer.timezone, rc.prefer.loc = "", nil
		} else {
			rc.r = rc.r.WithContext(query.WithTimeZone(rc.r.Context(), rc.prefer.loc))
		}
	}
	if rawType(r, mimeJSON, mimeObject, mimeCSV) != "" {
		// one value sent as is: keep binary values as bytes, not base64
		rc.r = rc.r.WithContext(query.WithRawBinary(rc.r.Context()))
	}
	if format := planFormat(r); format != "" {
		if err := s.startPlan(rc, format); err != nil {
			return err
		}
	}
	err = s.route(rc, parts)
	if err == nil && rc.plan != nil {
		if f := s.timingHeader(rc); f != nil {
			f(w.Header())
		}
		return rc.plan.write(w)
	}
	if err != nil && rc.anon {
		// like PostgREST: permission errors for anonymous requests ask for credentials
		if e := toAPIError(err); e.Status == 403 {
			e.Status = 401
			return e
		}
	}
	return err
}

func (s *Server) route(rc *reqCtx, parts []string) error {
	sc, r := rc.sc, rc.r

	switch {
	case len(parts) == 2 && parts[0] == "rpc":
		rt := sc.Routine(parts[1])
		if rt == nil {
			return &query.Error{Status: 404, Code: "FBRST202",
				Message: fmt.Sprintf("could not find the procedure or function %q in the schema cache", parts[1])}
		}
		return s.withTx(rc, readOnly(r.Method), func() error { return s.rpc(rc, rt) })
	case len(parts) == 1:
		rel := sc.Relation(parts[0])
		if rel == nil {
			return &query.Error{Status: 404, Code: "FBRST205",
				Message: fmt.Sprintf("could not find the table %q in the schema cache", parts[0])}
		}
		var fn func() error
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			fn = func() error { return s.read(rc, rel) }
		case http.MethodPost:
			fn = func() error { return s.insert(rc, rel) }
		case http.MethodPatch:
			fn = func() error { return s.update(rc, rel) }
		case http.MethodPut:
			fn = func() error { return s.put(rc, rel) }
		case http.MethodDelete:
			fn = func() error { return s.delete(rc, rel) }
		default:
			return &query.Error{Status: 405, Code: "FBRST117", Message: "method not allowed"}
		}
		return s.withTx(rc, readOnly(r.Method), fn)
	}
	return &query.Error{Status: 404, Code: "FBRST125", Message: "invalid path"}
}

func readOnly(method string) bool { return method == http.MethodGet || method == http.MethodHead }

// withTx runs fn inside a transaction for the request's role, after setting
// the request context variables and running the optional pre-request procedure.
func (s *Server) withTx(rc *reqCtx, ro bool, fn func() error) error {
	ctx := rc.r.Context()
	p, err := s.pool(ctx, rc.role)
	if err != nil {
		return err
	}
	rc.timing.txStart = time.Now()
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: ro})
	if err != nil {
		return err
	}
	rc.tx = &cachedTx{tx: tx, cache: p.cache}
	// hold the response until COMMIT succeeds (plan mode discards it anyway)
	out := rc.w
	var held *txWriter
	if rc.plan == nil {
		held = newTxWriter()
		rc.w = held
		defer func() { rc.w = out }()
	}
	done := false
	zoned := false
	// the connection goes back to the pool: undo SET TIME ZONE first
	resetZone := func() {
		if zoned {
			zoned = false
			tx.ExecContext(context.Background(), "SET TIME ZONE LOCAL")
		}
	}
	defer func() {
		if !done {
			resetZone()
			tx.Rollback()
		}
	}()
	if rc.prefer.timezone != "" {
		if _, err := tx.ExecContext(ctx, "SET TIME ZONE '"+strings.ReplaceAll(rc.prefer.timezone, "'", "''")+"'"); err != nil {
			if rc.prefer.handling == "strict" {
				return &query.Error{Status: 400, Code: "FBRST130", Message: "invalid preferences given with handling=strict",
					Details: "timezone=" + rc.prefer.timezone + ": " + firstLine(err.Error())}
			}
			// Firebird doesn't know this zone: ignore the preference
			rc.prefer.dropApplied("timezone")
			rc.r = rc.r.WithContext(query.WithTimeZone(rc.r.Context(), nil))
		} else {
			zoned = true
		}
	}
	if err := s.setContext(rc); err != nil {
		return err
	}
	if rc.plan != nil {
		rc.tx.record = &rc.plan.queries
	}
	if err := fn(); err != nil {
		return err
	}
	if rc.plan != nil {
		rc.tx.record = nil
		if err := rc.plan.explain(rc); err != nil {
			return err
		}
	}
	resetZone()
	done = true
	if rc.prefer.tx == "rollback" {
		err = tx.Rollback()
	} else {
		err = tx.Commit()
	}
	rc.timing.tx = time.Since(rc.timing.txStart)
	if err != nil || held == nil {
		return err
	}
	held.flush(out, s.timingHeader(rc))
	return nil
}

// timingHeader adds Server-Timing when server-timing-enabled is set.
func (s *Server) timingHeader(rc *reqCtx) func(http.Header) {
	if !s.cfg.ServerTiming {
		return nil
	}
	return func(h http.Header) { h.Set("Server-Timing", rc.timing.header()) }
}

// setContext exposes request data to SQL via RDB$GET_CONTEXT('USER_TRANSACTION', ...).
func (s *Server) setContext(rc *reqCtx) error {
	withVars := s.cfg.DBRequestContext == "always" ||
		s.cfg.DBRequestContext != "never" && rc.sc.UsesRequestContext
	pre := s.cfg.DBPreRequest
	if !withVars && pre == "" {
		return nil // nothing in the database reads the variables: save a round trip
	}
	var vars [][2]string
	if withVars {
		vars = requestVars(rc)
	}
	// One EXECUTE BLOCK sets every variable and runs the pre-request procedure,
	// in a single round trip and without a result set to fetch.
	var sb strings.Builder
	sb.WriteString("EXECUTE BLOCK")
	args := make([]any, 0, len(vars)*2)
	if len(vars) > 0 {
		sb.WriteString(" (")
		for i, v := range vars {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "k%d VARCHAR(80) = ?, v%d VARCHAR(255) = ?", i, i)
			args = append(args, truncate(v[0], 80), truncate(v[1], 255))
		}
		sb.WriteString(")")
	}
	sb.WriteString(" AS DECLARE r INTEGER; BEGIN ")
	for i := range vars {
		fmt.Fprintf(&sb, "r = RDB$SET_CONTEXT('USER_TRANSACTION', :k%d, :v%d); ", i, i)
	}
	if pre != "" {
		sb.WriteString("EXECUTE PROCEDURE " + quoteDotted(pre) + "; ")
	}
	sb.WriteString("END")
	_, err := rc.tx.ExecContext(rc.r.Context(), sb.String(), args...)
	return err
}

// requestVars lists the context variables for a request, in a stable order.
func requestVars(rc *reqCtx) [][2]string {
	vars := [][2]string{
		{"request.method", rc.r.Method},
		{"request.path", rc.r.URL.Path},
		{"request.role", rc.role},
	}
	if rc.claims != nil {
		if b, err := jsonMarshal(rc.claims); err == nil && len(b) <= 255 {
			vars = append(vars, [2]string{"request.jwt.claims", string(b)})
		}
		keys := make([]string, 0, len(rc.claims))
		for k := range rc.claims {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch x := rc.claims[k].(type) {
			case string:
				vars = append(vars, [2]string{"request.jwt.claim." + k, x})
			case float64, bool:
				vars = append(vars, [2]string{"request.jwt.claim." + k, fmt.Sprint(x)})
			}
		}
	}
	for _, h := range []string{"Origin", "User-Agent", "X-Forwarded-For"} {
		if v := rc.r.Header.Get(h); v != "" {
			vars = append(vars, [2]string{"request.header." + strings.ToLower(h), v})
		}
	}
	return vars
}

// truncate cuts s to at most n characters (not bytes).
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// hasTimeZones reports whether the engine version (e.g. "5.0.4") supports
// time zones (Firebird 4+).
func hasTimeZones(version string) bool {
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= 4
}

// quoteDotted quotes a possibly schema-qualified name such as
// "inventory.check_request": each part is upper-cased (an unquoted
// identifier) and quoted.
func quoteDotted(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = schema.Quote(strings.ToUpper(strings.TrimSpace(p)))
	}
	return strings.Join(parts, ".")
}

// pickSchema returns the schema a request asks for: Accept-Profile for GET
// and HEAD, Content-Profile for the other methods (as in PostgREST), or the
// first exposed schema.
func pickSchema(cat *schema.Catalog, r *http.Request) (*schema.Schema, error) {
	header := "Content-Profile"
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		header = "Accept-Profile"
	}
	name := strings.TrimSpace(r.Header.Get(header))
	if name == "" {
		return cat.Default(), nil
	}
	if cat.Default().Name == "" {
		return nil, &query.Error{Status: 406, Code: "FBRST106",
			Message: fmt.Sprintf("%s is not supported: Firebird %s has no schemas", header, cat.Version),
			Hint:    "schemas need Firebird 6 or later"}
	}
	sc := cat.Lookup(name)
	if sc == nil {
		return nil, &query.Error{Status: 406, Code: "FBRST106",
			Message: "the schema must be one of the following: " + strings.Join(cat.Names(), ", "),
			Details: fmt.Sprintf("%s: %s", header, name),
			Hint:    "expose more schemas with db-schemas"}
	}
	return sc, nil
}
