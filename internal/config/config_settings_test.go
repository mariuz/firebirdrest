package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostgRESTSettings(t *testing.T) {
	t.Setenv("FBREST_DB_URI", "authenticator:s3cret@localhost/db.fdb")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.DBPoolAcquireTimeout != 10 || c.DBPoolMaxLifetime != 1800 || c.DBPoolMaxIdle != 300 ||
		c.DBTxEnd != "commit-allow-override" || c.UnixSocketMode != 0o660 || c.LogQuery || c.TraceHeader != "" {
		t.Errorf("defaults: %+v", c)
	}
	bad := map[string]string{
		"FBREST_DB_TX_END":                   "abort",
		"FBREST_SERVER_UNIX_SOCKET_MODE":     "999",
		"FBREST_SERVER_TRACE_HEADER":         "X Request",
		"FBREST_LOG_QUERY":                   "some",
		"FBREST_DB_POOL_ACQUISITION_TIMEOUT": "-1",
	}
	for env, v := range bad {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, v)
			if _, err := Load(""); err == nil {
				t.Errorf("%s=%s accepted", env, v)
			}
		})
	}
	t.Setenv("FBREST_SERVER_UNIX_SOCKET_MODE", "400")
	if _, err := Load(""); err == nil {
		t.Error("a socket the owner can't write accepted")
	}
	t.Setenv("FBREST_SERVER_UNIX_SOCKET_MODE", "0666")
	t.Setenv("FBREST_DB_TX_END", "rollback")
	t.Setenv("FBREST_LOG_QUERY", "main-query")
	t.Setenv("FBREST_SERVER_TRACE_HEADER", "X-Request-Id")
	if c, err = Load(""); err != nil {
		t.Fatal(err)
	}
	if c.UnixSocketMode != 0o666 || c.DBTxEnd != "rollback" || !c.LogQuery || c.TraceHeader != "X-Request-Id" {
		t.Errorf("set: %+v", c)
	}
	if !strings.Contains(strings.Join(c.Warnings, "\n"), "rolled back") {
		t.Errorf("no warning for db-tx-end = rollback: %q", c.Warnings)
	}
}

func TestDumpConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "api.conf")
	os.WriteFile(file, []byte("db-uri = \"authenticator:s3cret@localhost/db.fdb\"\njwt-secret = \"averyveryverylongsecretofthirtytwochars\"\ndb-pool = 4\n"), 0o600)
	t.Setenv("FBREST_ADMIN_SERVER_TOKEN", "abcdefghijklmnopqrstu")
	t.Setenv("FBREST_DB_ANON_ROLE", "web_anon")
	c, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	d := c.Dump()
	for _, want := range []string{
		`db-uri = "authenticator:***@localhost/db.fdb"`,
		`jwt-secret = "***"`,
		`admin-server-token = "***"`,
		`db-pool = "4"`,
		`db-anon-role = "web_anon"`,
		`db-safe-updates = "true"`,
		`db-pool-acquisition-timeout = "10"`,
		`db-tx-end = "commit-allow-override"`,
		`server-port = "3000"`,
	} {
		if !strings.Contains(d, want+"\n") {
			t.Errorf("dump lacks %s:\n%s", want, d)
		}
	}
	if strings.Contains(d, "s3cret") || strings.Contains(d, "averyvery") || strings.Contains(d, "abcdefgh") {
		t.Errorf("dump shows a secret:\n%s", d)
	}
	// every setting is listed once, and the dump loads back
	if n := strings.Count(d, "\n"); n != len(keys) {
		t.Errorf("dump has %d lines for %d settings", n, len(keys))
	}
	again := filepath.Join(dir, "dump.conf")
	os.WriteFile(again, []byte(strings.ReplaceAll(strings.ReplaceAll(d, `"***"`, `""`), "***", "x")), 0o600)
	t.Setenv("FBREST_ADMIN_SERVER_TOKEN", "")
	if _, err := Load(again); err != nil {
		t.Errorf("the dump doesn't load: %v", err)
	}
}
