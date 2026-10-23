// Package config loads settings from a config file and FBREST_* environment variables.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime settings.
type Config struct {
	DBURI         string // nakagami/firebirdsql DSN: user:password@host:port/path/to/db.fdb
	DBAnonRole    string // role for requests without a JWT ("" = reject them)
	DBPool        int
	DBMaxRows     int
	DBPreRequest  string // executable procedure run before each request
	DBSafeUpdates bool   // reject PATCH/DELETE without filters
	DBAggregates  bool   // allow count()/sum()/avg()/min()/max() in select
	DBPlan        bool   // allow Accept: application/vnd.pgrst.plan (query plans)
	// DBSchemas lists the SQL schemas to expose (Firebird 6+); the first is the
	// default. Empty means PUBLIC.
	DBSchemas []string
	// DBRequestContext: "auto" sets the request.* context variables only when
	// some database object reads them; "always" or "never" force it.
	DBRequestContext string
	// DBPreparedStatements is the number of prepared statements cached per
	// role (0 disables the cache).
	DBPreparedStatements int
	// DBPreparedStatementsIdle closes cached statements unused for this many
	// seconds, releasing their metadata locks (0 = never).
	DBPreparedStatementsIdle int
	DBReloadEvent            string // POST_EVENT name that triggers a schema reload
	JWTSecret                string // HMAC secret, PEM public key, JWK or JWKS; "@file" reads it from a file
	JWTSecretIsBase64        bool   // the HMAC secret is base64 encoded
	JWTJWKSURL               string // JWKS fetched over HTTP(S) and refreshed
	JWTJWKSRefresh           int    // seconds between fetches of JWTJWKSURL
	JWTCacheMaxEntries       int    // accepted tokens remembered (0 disables the cache)
	JWTAudience              string
	JWTRoleClaim             string // dotted path, e.g. "role" or "app_metadata.role"
	ServerHost               string
	ServerPort               int
	ServerTimeout            int    // seconds a request may take, including reading its body (0 = no limit)
	ServerMaxBody            int    // largest request body, in MB
	AdminHost                string // listen address of the admin server ("" = server-host)
	AdminPort                int    // 0 = disabled; serves /live and /ready
	AdminPprof               bool   // also serve Go profiling endpoints at /debug/pprof/ on the admin port
	ServerTiming             bool   // add a Server-Timing header to responses
	CORSOrigins              string
	OpenAPIProxyURI          string
	OpenAPIMode              string // follow-privileges, ignore-privileges or disabled
	LogLevel                 string
}

// keys maps config-file keys to environment variable names.
var keys = map[string]string{
	"db-uri":                      "FBREST_DB_URI",
	"db-anon-role":                "FBREST_DB_ANON_ROLE",
	"db-pool":                     "FBREST_DB_POOL",
	"db-max-rows":                 "FBREST_DB_MAX_ROWS",
	"db-pre-request":              "FBREST_DB_PRE_REQUEST",
	"db-safe-updates":             "FBREST_DB_SAFE_UPDATES",
	"db-aggregates-enabled":       "FBREST_DB_AGGREGATES_ENABLED",
	"db-plan-enabled":             "FBREST_DB_PLAN_ENABLED",
	"db-schemas":                  "FBREST_DB_SCHEMAS",
	"db-request-context":          "FBREST_DB_REQUEST_CONTEXT",
	"db-prepared-statements":      "FBREST_DB_PREPARED_STATEMENTS",
	"db-prepared-statements-idle": "FBREST_DB_PREPARED_STATEMENTS_IDLE",
	"db-reload-event":             "FBREST_DB_RELOAD_EVENT",
	"jwt-secret":                  "FBREST_JWT_SECRET",
	"jwt-secret-is-base64":        "FBREST_JWT_SECRET_IS_BASE64",
	"jwt-jwks-url":                "FBREST_JWT_JWKS_URL",
	"jwt-jwks-refresh":            "FBREST_JWT_JWKS_REFRESH",
	"jwt-cache-max-entries":       "FBREST_JWT_CACHE_MAX_ENTRIES",
	"jwt-aud":                     "FBREST_JWT_AUD",
	"jwt-role-claim-key":          "FBREST_JWT_ROLE_CLAIM_KEY",
	"server-host":                 "FBREST_SERVER_HOST",
	"server-port":                 "FBREST_SERVER_PORT",
	"server-request-timeout":      "FBREST_SERVER_REQUEST_TIMEOUT",
	"server-max-body-size":        "FBREST_SERVER_MAX_BODY_SIZE",
	"admin-server-host":           "FBREST_ADMIN_SERVER_HOST",
	"server-timing-enabled":       "FBREST_SERVER_TIMING_ENABLED",
	"admin-server-port":           "FBREST_ADMIN_SERVER_PORT",
	"admin-pprof":                 "FBREST_ADMIN_PPROF",
	"server-cors-allowed-origins": "FBREST_SERVER_CORS_ALLOWED_ORIGINS",
	"openapi-server-proxy-uri":    "FBREST_OPENAPI_SERVER_PROXY_URI",
	"openapi-mode":                "FBREST_OPENAPI_MODE",
	"log-level":                   "FBREST_LOG_LEVEL",
}

// Load reads the optional config file, then lets environment variables override it.
func Load(path string) (*Config, error) {
	vals := map[string]string{}
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		line := 0
		for sc.Scan() {
			line++
			t := strings.TrimSpace(sc.Text())
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			k, v, ok := strings.Cut(t, "=")
			if !ok {
				return nil, fmt.Errorf("%s:%d: expected key = value", path, line)
			}
			k = strings.TrimSpace(k)
			env, known := keys[k]
			if !known {
				return nil, fmt.Errorf("%s:%d: unknown setting %q", path, line, k)
			}
			v = strings.TrimSpace(v)
			if uq, err := strconv.Unquote(v); err == nil {
				v = uq
			} else if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
				v = v[1 : len(v)-1]
			}
			vals[env] = v
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	get := func(env, def string) string {
		if v, ok := os.LookupEnv(env); ok {
			return v
		}
		if v, ok := vals[env]; ok {
			return v
		}
		return def
	}
	// an empty value keeps the default: "admin-pprof =" must not enable pprof
	getBool := func(env string, def bool) bool {
		s := strings.TrimSpace(get(env, ""))
		if s == "" {
			return def
		}
		return isTrue(s)
	}
	geti := func(env string, def int) (int, error) {
		s := get(env, "")
		if s == "" {
			return def, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer: %w", env, err)
		}
		return n, nil
	}
	c := &Config{
		DBURI:           get("FBREST_DB_URI", ""),
		DBAnonRole:      get("FBREST_DB_ANON_ROLE", ""),
		DBPreRequest:    get("FBREST_DB_PRE_REQUEST", ""),
		DBReloadEvent:   get("FBREST_DB_RELOAD_EVENT", "fbrest_reload"),
		JWTSecret:       get("FBREST_JWT_SECRET", ""),
		JWTAudience:     get("FBREST_JWT_AUD", ""),
		JWTJWKSURL:      get("FBREST_JWT_JWKS_URL", ""),
		JWTRoleClaim:    get("FBREST_JWT_ROLE_CLAIM_KEY", "role"),
		ServerHost:      get("FBREST_SERVER_HOST", "0.0.0.0"),
		AdminHost:       get("FBREST_ADMIN_SERVER_HOST", ""),
		CORSOrigins:     get("FBREST_SERVER_CORS_ALLOWED_ORIGINS", "*"),
		OpenAPIProxyURI: get("FBREST_OPENAPI_SERVER_PROXY_URI", ""),
		LogLevel:        get("FBREST_LOG_LEVEL", "info"),
	}
	var err error
	if c.DBPool, err = geti("FBREST_DB_POOL", 10); err != nil {
		return nil, err
	}
	if c.DBMaxRows, err = geti("FBREST_DB_MAX_ROWS", 0); err != nil {
		return nil, err
	}
	if c.DBPreparedStatements, err = geti("FBREST_DB_PREPARED_STATEMENTS", 256); err != nil {
		return nil, err
	}
	if c.DBPreparedStatementsIdle, err = geti("FBREST_DB_PREPARED_STATEMENTS_IDLE", 30); err != nil {
		return nil, err
	}
	if c.JWTJWKSRefresh, err = geti("FBREST_JWT_JWKS_REFRESH", 3600); err != nil {
		return nil, err
	}
	if c.JWTJWKSRefresh < 30 {
		return nil, fmt.Errorf("jwt-jwks-refresh must be at least 30 seconds, got %d", c.JWTJWKSRefresh)
	}
	if c.JWTCacheMaxEntries, err = geti("FBREST_JWT_CACHE_MAX_ENTRIES", 1000); err != nil {
		return nil, err
	}
	if c.JWTCacheMaxEntries < 0 {
		return nil, fmt.Errorf("jwt-cache-max-entries must not be negative, got %d", c.JWTCacheMaxEntries)
	}
	if c.JWTJWKSURL != "" && !strings.HasPrefix(c.JWTJWKSURL, "https://") && !strings.HasPrefix(c.JWTJWKSURL, "http://") {
		return nil, fmt.Errorf("jwt-jwks-url must be an http:// or https:// URL, got %q", c.JWTJWKSURL)
	}
	if c.ServerPort, err = geti("FBREST_SERVER_PORT", 3000); err != nil {
		return nil, err
	}
	if c.AdminPort, err = geti("FBREST_ADMIN_SERVER_PORT", 0); err != nil {
		return nil, err
	}
	if c.ServerTimeout, err = geti("FBREST_SERVER_REQUEST_TIMEOUT", 60); err != nil {
		return nil, err
	}
	if c.ServerMaxBody, err = geti("FBREST_SERVER_MAX_BODY_SIZE", 16); err != nil {
		return nil, err
	}
	if c.ServerMaxBody < 1 {
		return nil, fmt.Errorf("server-max-body-size must be at least 1 (MB), got %d", c.ServerMaxBody)
	}
	c.DBSafeUpdates = getBool("FBREST_DB_SAFE_UPDATES", true)
	c.DBAggregates = getBool("FBREST_DB_AGGREGATES_ENABLED", true)
	c.DBPlan = getBool("FBREST_DB_PLAN_ENABLED", false)
	for _, name := range strings.Split(get("FBREST_DB_SCHEMAS", ""), ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.DBSchemas = append(c.DBSchemas, name)
		}
	}
	c.JWTSecretIsBase64 = getBool("FBREST_JWT_SECRET_IS_BASE64", false)
	c.ServerTiming = getBool("FBREST_SERVER_TIMING_ENABLED", false)
	c.AdminPprof = getBool("FBREST_ADMIN_PPROF", false)
	c.OpenAPIMode = strings.ToLower(get("FBREST_OPENAPI_MODE", "follow-privileges"))
	switch c.OpenAPIMode {
	case "follow-privileges", "ignore-privileges", "disabled":
	default:
		return nil, fmt.Errorf("openapi-mode must be follow-privileges, ignore-privileges or disabled, got %q", c.OpenAPIMode)
	}
	c.DBRequestContext = strings.ToLower(get("FBREST_DB_REQUEST_CONTEXT", "auto"))
	switch c.DBRequestContext {
	case "auto", "always", "never":
	default:
		return nil, fmt.Errorf("db-request-context must be auto, always or never, got %q", c.DBRequestContext)
	}
	if c.DBURI == "" {
		return nil, fmt.Errorf("db-uri (FBREST_DB_URI) is required, e.g. user:password@localhost:3050/var/lib/firebird/data/app.fdb")
	}
	return c, nil
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "false", "0", "off", "no":
		return false
	}
	return true
}
