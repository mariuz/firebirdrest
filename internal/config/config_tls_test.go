package config

import (
	"strings"
	"testing"
)

func TestTLSSettings(t *testing.T) {
	t.Setenv("FBREST_DB_URI", "u:p@localhost/db.fdb")
	t.Setenv("FBREST_SERVER_TLS_CERT", "/etc/ssl/api.pem")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "together") {
		t.Errorf("cert without key: %v", err)
	}
	t.Setenv("FBREST_SERVER_TLS_KEY", "/etc/ssl/api.key")
	c, err := Load("")
	if err != nil || c.TLSCert != "/etc/ssl/api.pem" || c.TLSKey != "/etc/ssl/api.key" {
		t.Errorf("cert and key: %+v %v", c, err)
	}
}
