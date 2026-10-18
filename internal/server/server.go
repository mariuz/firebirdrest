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
	cfg    *config.Config
	base   *sql.DB // connection without a role, used to read the schema
	schema atomic.Pointer[schema.Schema]
	log    *slog.Logger

	mu    sync.Mutex
	pools map[string]*sql.DB
}

// New opens the database and loads the schema cache.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Server, error) {
	db, err := openPool(cfg.DBURI, "", cfg.DBPool)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, base: db, log: log, pools: map[string]*sql.DB{}}
	if err := s.Reload(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Reload re-reads the schema cache.
func (s *Server) Reload(ctx context.Context) error {
	start := time.Now()
	sc, err := schema.Load(ctx, s.base)
	if err != nil {
		return fmt.Errorf("loading schema: %w", err)
	}
	s.schema.Store(sc)
	s.log.Info("schema cache loaded", "firebird", sc.Version, "relations", len(sc.Relations),
		"routines", len(sc.Routines), "foreign_keys", len(sc.ForeignKeys), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// Close releases all connections.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pools {
		p.Close()
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
func (s *Server) pool(ctx context.Context, role string) (*sql.DB, error) {
	if role == "" {
		return s.base, nil
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
	s.pools[role] = p
	return p, nil
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
	tx     *sql.Tx
	status int
}

// ServeHTTP routes requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rw := &statusWriter{ResponseWriter: w, status: 200}
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("panic", "err", rec, "path", r.URL.Path)
			writeError(rw, &query.Error{Status: 500, Code: "FBRST000", Message: "internal server error"})
		}
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
		writeError(rw, toAPIError(err))
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
	h.Set("Access-Control-Expose-Headers", "Content-Encoding, Content-Location, Content-Range, Content-Type, Date, Location, Server, Transfer-Encoding, Range-Unit, Preference-Applied")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS, HEAD")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Accept-Language, Content-Language, Prefer, Range")
		h.Set("Access-Control-Max-Age", "86400")
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) error {
	sc := s.schema.Load()
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
	if err := s.authenticate(rc); err != nil {
		return err
	}
	err := s.route(rc, parts)
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
	tx, err := p.BeginTx(ctx, &sql.TxOptions{ReadOnly: ro})
	if err != nil {
		return err
	}
	rc.tx = tx
	done := false
	defer func() {
		if !done {
			tx.Rollback()
		}
	}()
	if err := s.setContext(rc); err != nil {
		return err
	}
	if s.cfg.DBPreRequest != "" {
		if _, err := tx.ExecContext(ctx, "EXECUTE PROCEDURE "+schema.Quote(strings.ToUpper(s.cfg.DBPreRequest))); err != nil {
			return err
		}
	}
	if err := fn(); err != nil {
		return err
	}
	done = true
	if rc.prefer.tx == "rollback" {
		return tx.Rollback()
	}
	return tx.Commit()
}

// setContext exposes request data to SQL via RDB$GET_CONTEXT('USER_TRANSACTION', ...).
func (s *Server) setContext(rc *reqCtx) error {
	vars := [][2]string{
		{"request.method", rc.r.Method},
		{"request.path", rc.r.URL.Path},
		{"request.role", rc.role},
	}
	if rc.claims != nil {
		if b, err := jsonMarshal(rc.claims); err == nil && len(b) <= 255 {
			vars = append(vars, [2]string{"request.jwt.claims", string(b)})
		}
		for k, v := range rc.claims {
			switch x := v.(type) {
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
	var sb strings.Builder
	sb.WriteString("SELECT ")
	args := make([]any, 0, len(vars)*2)
	for i, v := range vars {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("RDB$SET_CONTEXT('USER_TRANSACTION', CAST(? AS VARCHAR(80)), CAST(? AS VARCHAR(255)))")
		key, val := v[0], v[1]
		if len(key) > 80 {
			key = key[:80]
		}
		if len(val) > 255 {
			val = val[:255]
		}
		args = append(args, key, val)
	}
	sb.WriteString(" FROM RDB$DATABASE")
	rows, err := rc.tx.QueryContext(rc.r.Context(), sb.String(), args...)
	if err != nil {
		return err
	}
	// the context functions run when the row is fetched
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
