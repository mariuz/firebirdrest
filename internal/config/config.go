// Package config loads settings from a config file and FBREST_* environment variables.
package config

import (
	"bufio"
	"fmt"
	"net"
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
	// DBEvents lists the Firebird events clients may listen for at
	// GET /events/<name>; empty disables the endpoint. DBEventsRoles limits
	// listening to these roles (empty: any authenticated role).
	DBEvents      []string
	DBEventsRoles []string
	// OTelEndpoint is the OTLP/HTTP URL traces are posted to ("" disables
	// tracing); OTelHeaders are sent with each export.
	OTelEndpoint    string
	OTelHeaders     string
	OTelServiceName string
	OTelSampleRatio float64
	// DBReadTxFresh gives every read its own transaction instead of reusing
	// a read-only one per connection (db-read-tx-reuse = false).
	DBReadTxFresh      bool
	JWTSecret          string // HMAC secret, PEM public key, JWK or JWKS; "@file" reads it from a file
	JWTSecretIsBase64  bool   // the HMAC secret is base64 encoded
	JWTJWKSURL         string // JWKS fetched over HTTP(S) and refreshed
	JWTJWKSRefresh     int    // seconds between fetches of JWTJWKSURL
	JWTCacheMaxEntries int    // accepted tokens remembered (0 disables the cache)
	JWTAudience        string
	JWTRoleClaim       string // dotted path, e.g. "role" or "app_metadata.role"
	ServerHost         string
	ServerPort         int
	TLSCert            string // PEM certificate (chain) file; with TLSKey, serve HTTPS
	TLSKey             string // PEM private key file
	ServerTimeout      int    // seconds a request may take, including reading its body (0 = no limit)
	ServerMaxBody      int    // largest request body, in MB
	AdminHost          string // listen address of the admin server (default 127.0.0.1)
	AdminToken         string // bearer token for /reload, /metrics and /debug/pprof/ ("" = none)
	AdminPort          int    // 0 = disabled; serves /live and /ready
	AdminPprof         bool   // also serve Go profiling endpoints at /debug/pprof/ on the admin port
	ServerTiming       bool   // add a Server-Timing header to responses
	CORSOrigins        string
	OpenAPIProxyURI    string
	OpenAPIMode        string // follow-privileges, ignore-privileges or disabled
	LogLevel           string
	// Warnings are deprecation notices and advice found while loading; the
	// server logs them at startup and -check prints them.
	Warnings []string
}

// deprecated maps old setting keys to their replacements. An old key keeps
// working for at least one minor version, with a warning.
var deprecated = map[string]string{}

// RedactURI hides the password of a db-uri for logs and messages.
func RedactURI(uri string) string {
	at := strings.LastIndex(uri, "@")
	if at < 0 {
		return uri
	}
	cred := uri[:at]
	if i := strings.Index(cred, ":"); i >= 0 {
		return cred[:i] + ":***" + uri[at:]
	}
	return uri
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
	"db-events":                   "FBREST_DB_EVENTS",
	"db-events-roles":             "FBREST_DB_EVENTS_ROLES",
	"server-tls-cert":             "FBREST_SERVER_TLS_CERT",
	"server-tls-key":              "FBREST_SERVER_TLS_KEY",
	"otel-traces-endpoint":        "FBREST_OTEL_TRACES_ENDPOINT",
	"otel-traces-sample-ratio":    "FBREST_OTEL_TRACES_SAMPLE_RATIO",
	"otel-service-name":           "FBREST_OTEL_SERVICE_NAME",
	"db-read-tx-reuse":            "FBREST_DB_READ_TX_REUSE",
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
	"admin-server-token":          "FBREST_ADMIN_SERVER_TOKEN",
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
	var warnings []string
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
			if repl, old := deprecated[k]; old {
				warnings = append(warnings, fmt.Sprintf("%s:%d: %s is deprecated, use %s", path, line, k, repl))
				k = repl
			}
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
		AdminHost:       get("FBREST_ADMIN_SERVER_HOST", "127.0.0.1"),
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
	for _, name := range strings.Split(get("FBREST_DB_EVENTS", ""), ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.DBEvents = append(c.DBEvents, name)
		}
	}
	for _, role := range strings.Split(get("FBREST_DB_EVENTS_ROLES", ""), ",") {
		if role = strings.TrimSpace(role); role != "" {
			c.DBEventsRoles = append(c.DBEventsRoles, role)
		}
	}
	for _, name := range c.DBEvents {
		// the stream's own message types
		if strings.EqualFold(name, "error") || strings.EqualFold(name, "reset") {
			return nil, fmt.Errorf("db-events: %q is reserved for the stream's own messages; post another name", name)
		}
	}
	if c.AdminHost == "" {
		c.AdminHost = "127.0.0.1"
	}
	c.AdminToken = strings.TrimSpace(get("FBREST_ADMIN_SERVER_TOKEN", ""))
	if strings.HasPrefix(c.AdminToken, "@") {
		b, err := os.ReadFile(c.AdminToken[1:])
		if err != nil {
			return nil, fmt.Errorf("admin-server-token: %w", err)
		}
		c.AdminToken = strings.TrimSpace(string(b))
	}
	if c.AdminToken != "" && len(c.AdminToken) < 16 {
		return nil, fmt.Errorf("admin-server-token must be at least 16 characters")
	}
	if c.AdminPort > 0 && c.AdminToken == "" && !loopbackHost(c.AdminHost) {
		c.Warnings = append(c.Warnings, fmt.Sprintf("the admin server listens on %s without admin-server-token: anyone who can reach port %d can reload the schema cache and read the metrics", c.AdminHost, c.AdminPort))
	}
	if len(c.DBEventsRoles) > 0 && len(c.DBEvents) == 0 {
		c.Warnings = append(c.Warnings, "db-events-roles is set but db-events is empty: no event is exposed")
	}
	c.TLSCert = strings.TrimSpace(get("FBREST_SERVER_TLS_CERT", ""))
	c.TLSKey = strings.TrimSpace(get("FBREST_SERVER_TLS_KEY", ""))
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("server-tls-cert and server-tls-key must be set together")
	}
	// traces: our settings first, then the standard OpenTelemetry variables
	c.OTelEndpoint = strings.TrimSpace(get("FBREST_OTEL_TRACES_ENDPOINT", ""))
	if c.OTelEndpoint == "" {
		c.OTelEndpoint = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	}
	if c.OTelEndpoint == "" {
		if base := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")); base != "" {
			c.OTelEndpoint = strings.TrimRight(base, "/") + "/v1/traces"
		}
	}
	if c.OTelEndpoint != "" && !strings.HasPrefix(c.OTelEndpoint, "http://") && !strings.HasPrefix(c.OTelEndpoint, "https://") {
		return nil, fmt.Errorf("otel-traces-endpoint must be an http:// or https:// URL, got %q", c.OTelEndpoint)
	}
	c.OTelHeaders = os.Getenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS")
	if c.OTelHeaders == "" {
		c.OTelHeaders = os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")
	}
	c.OTelServiceName = get("FBREST_OTEL_SERVICE_NAME", "")
	if c.OTelServiceName == "" {
		c.OTelServiceName = os.Getenv("OTEL_SERVICE_NAME")
	}
	if c.OTelServiceName == "" {
		c.OTelServiceName = "firebirdrest"
	}
	c.OTelSampleRatio = 1
	if s := strings.TrimSpace(get("FBREST_OTEL_TRACES_SAMPLE_RATIO", "")); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f < 0 || f > 1 {
			return nil, fmt.Errorf("otel-traces-sample-ratio must be a number from 0 to 1, got %q", s)
		}
		c.OTelSampleRatio = f
	}
	c.JWTSecretIsBase64 = getBool("FBREST_JWT_SECRET_IS_BASE64", false)
	c.ServerTiming = getBool("FBREST_SERVER_TIMING_ENABLED", false)
	c.AdminPprof = getBool("FBREST_ADMIN_PPROF", false)
	c.DBReadTxFresh = !getBool("FBREST_DB_READ_TX_REUSE", true)
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
	if c.JWTSecret == "" && c.JWTJWKSURL == "" && c.DBAnonRole == "" {
		warnings = append(warnings, "neither jwt-secret, jwt-jwks-url nor db-anon-role is set: every request will be refused")
	}
	if c.JWTSecret != "" && !strings.HasPrefix(c.JWTSecret, "@") && !strings.HasPrefix(strings.TrimSpace(c.JWTSecret), "-----BEGIN") &&
		!strings.HasPrefix(strings.TrimSpace(c.JWTSecret), "{") && len(c.JWTSecret) < 32 {
		warnings = append(warnings, "jwt-secret is shorter than 32 characters: use a longer random secret")
	}
	if c.CORSOrigins == "*" {
		warnings = append(warnings, "server-cors-allowed-origins is \"*\": any website may call the API from a browser (without credentials)")
	}
	// warnings collected while reading the file come first, then those
	// added while checking the settings
	c.Warnings = append(warnings, c.Warnings...)
	return c, nil
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "false", "0", "off", "no":
		return false
	}
	return true
}

// loopbackHost reports whether a listen address only accepts local connections.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
