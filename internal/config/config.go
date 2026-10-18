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
	DBURI           string // nakagami/firebirdsql DSN: user:password@host:port/path/to/db.fdb
	DBAnonRole      string // role for requests without a JWT ("" = reject them)
	DBPool          int
	DBMaxRows       int
	DBPreRequest    string // executable procedure run before each request
	DBSafeUpdates   bool   // reject PATCH/DELETE without filters
	DBReloadEvent   string // POST_EVENT name that triggers a schema reload
	JWTSecret       string // HMAC secret, or PEM encoded RSA/ECDSA public key
	JWTAudience     string
	JWTRoleClaim    string // dotted path, e.g. "role" or "app_metadata.role"
	ServerHost      string
	ServerPort      int
	AdminPort       int // 0 = disabled; serves /live and /ready
	CORSOrigins     string
	OpenAPIProxyURI string
	LogLevel        string
}

// keys maps config-file keys to environment variable names.
var keys = map[string]string{
	"db-uri":                      "FBREST_DB_URI",
	"db-anon-role":                "FBREST_DB_ANON_ROLE",
	"db-pool":                     "FBREST_DB_POOL",
	"db-max-rows":                 "FBREST_DB_MAX_ROWS",
	"db-pre-request":              "FBREST_DB_PRE_REQUEST",
	"db-safe-updates":             "FBREST_DB_SAFE_UPDATES",
	"db-reload-event":             "FBREST_DB_RELOAD_EVENT",
	"jwt-secret":                  "FBREST_JWT_SECRET",
	"jwt-aud":                     "FBREST_JWT_AUD",
	"jwt-role-claim-key":          "FBREST_JWT_ROLE_CLAIM_KEY",
	"server-host":                 "FBREST_SERVER_HOST",
	"server-port":                 "FBREST_SERVER_PORT",
	"admin-server-port":           "FBREST_ADMIN_SERVER_PORT",
	"server-cors-allowed-origins": "FBREST_SERVER_CORS_ALLOWED_ORIGINS",
	"openapi-server-proxy-uri":    "FBREST_OPENAPI_SERVER_PROXY_URI",
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
		JWTRoleClaim:    get("FBREST_JWT_ROLE_CLAIM_KEY", "role"),
		ServerHost:      get("FBREST_SERVER_HOST", "0.0.0.0"),
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
	if c.ServerPort, err = geti("FBREST_SERVER_PORT", 3000); err != nil {
		return nil, err
	}
	if c.AdminPort, err = geti("FBREST_ADMIN_SERVER_PORT", 0); err != nil {
		return nil, err
	}
	safe := strings.ToLower(get("FBREST_DB_SAFE_UPDATES", "true"))
	c.DBSafeUpdates = safe != "false" && safe != "0" && safe != "off"
	if c.DBURI == "" {
		return nil, fmt.Errorf("db-uri (FBREST_DB_URI) is required, e.g. user:password@localhost:3050/var/lib/firebird/data/app.fdb")
	}
	return c, nil
}
