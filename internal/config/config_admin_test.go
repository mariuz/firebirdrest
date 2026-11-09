package config

import (
	"net/url"
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

func TestDBPasswordFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o600)
		return p
	}
	// a password with every character that means something in a URL
	odd := `p@ss:w/rd?#%&x y`
	t.Setenv("FBREST_DB_URI", "authenticator@db.internal:3050/data/app.fdb?wire_crypt=false")
	t.Setenv("FBREST_DB_PASSWORD_FILE", write("pw", odd+"\n"))
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse("firebird://" + c.DBURI) // as the driver reads it
	if err != nil {
		t.Fatalf("%q: %v", c.DBURI, err)
	}
	if pw, _ := u.User.Password(); pw != odd || u.User.Username() != "authenticator" || u.Host != "db.internal:3050" ||
		u.Path != "/data/app.fdb" || u.Query().Get("wire_crypt") != "false" {
		t.Errorf("DSN %q read as user %q password %q host %q path %q", c.DBURI, u.User.Username(), pw, u.Host, u.Path)
	}
	if strings.Contains(RedactURI(c.DBURI), "rd") {
		t.Errorf("password visible in %s", RedactURI(c.DBURI))
	}
	for _, bad := range []struct{ uri, file, want string }{
		{"authenticator:inline@db/app.fdb", write("pw2", "x"), "has a password too"},
		{"db/app.fdb", write("pw3", "x"), "must start with the user name"},
		{"authenticator@db/app.fdb", filepath.Join(dir, "missing"), "db-password-file"},
		{"authenticator@db/app.fdb", write("empty", "\n"), "is empty"},
	} {
		t.Setenv("FBREST_DB_URI", bad.uri)
		t.Setenv("FBREST_DB_PASSWORD_FILE", bad.file)
		if _, err := Load(""); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s with %s: %v, want %q", bad.uri, bad.file, err, bad.want)
		}
	}
}

func TestJWTIssuerAndLeewaySettings(t *testing.T) {
	t.Setenv("FBREST_DB_URI", "u:p@localhost/db.fdb")
	t.Setenv("FBREST_JWT_ISS", " https://id.example.com/realms/a , https://id.example.com/realms/b ")
	t.Setenv("FBREST_JWT_LEEWAY", "30")
	c, err := Load("")
	if err != nil || len(c.JWTIssuers) != 2 || c.JWTIssuers[1] != "https://id.example.com/realms/b" || c.JWTLeeway != 30 {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("FBREST_JWT_LEEWAY", "3600")
	if _, err := Load(""); err == nil {
		t.Error("an hour of leeway accepted")
	}
	t.Setenv("FBREST_JWT_LEEWAY", "")
	t.Setenv("FBREST_JWT_JWKS_URL", "https://id.example.com/certs")
	c, _ = Load("")
	if !strings.Contains(strings.Join(c.Warnings, "|"), "without jwt-aud") {
		t.Errorf("JWKS without audience: no warning: %v", c.Warnings)
	}
}
