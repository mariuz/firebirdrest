package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdminSettings(t *testing.T) {
	t.Setenv("FBREST_DB_URI", "u:p@localhost/db.fdb")
	t.Setenv("FBREST_ADMIN_SERVER_PORT", "3001")
	c, err := Load("")
	if err != nil || c.AdminHost != "127.0.0.1" || c.AdminToken != "" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for _, w := range c.Warnings {
		if strings.Contains(w, "admin server") {
			t.Errorf("warning on loopback: %s", w)
		}
	}
	warned := func(c *Config) bool {
		for _, w := range c.Warnings {
			if strings.Contains(w, "without admin-server-token") {
				return true
			}
		}
		return false
	}
	t.Setenv("FBREST_ADMIN_SERVER_HOST", "0.0.0.0")
	if c, err = Load(""); err != nil || !warned(c) {
		t.Errorf("exposed without a token: no warning: %v %v", c.Warnings, err)
	}
	t.Setenv("FBREST_ADMIN_SERVER_TOKEN", "short")
	if _, err = Load(""); err == nil || !strings.Contains(err.Error(), "16 characters") {
		t.Errorf("short token: %v", err)
	}
	f := filepath.Join(t.TempDir(), "admin-token")
	os.WriteFile(f, []byte("a-token-read-from-a-file\n"), 0o600)
	t.Setenv("FBREST_ADMIN_SERVER_TOKEN", "@"+f)
	if c, err = Load(""); err != nil || c.AdminToken != "a-token-read-from-a-file" || warned(c) {
		t.Errorf("token from a file: %q %v %v", c.AdminToken, c.Warnings, err)
	}
	t.Setenv("FBREST_ADMIN_SERVER_HOST", "::1")
	t.Setenv("FBREST_ADMIN_SERVER_TOKEN", "")
	if c, err = Load(""); err != nil || warned(c) {
		t.Errorf("IPv6 loopback: %v %v", c.Warnings, err)
	}
}

func TestEventsRolesWarning(t *testing.T) {
	t.Setenv("FBREST_DB_URI", "u:p@localhost/db.fdb")
	t.Setenv("FBREST_DB_EVENTS_ROLES", "web_user")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(c.Warnings, "|"), "db-events is empty") {
		t.Errorf("warnings: %v", c.Warnings)
	}
}
