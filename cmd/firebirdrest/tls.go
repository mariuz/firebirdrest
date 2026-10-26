package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// certStore holds the server certificate. Connections ask it for the
// certificate during the handshake (tls.Config.GetCertificate), so reload
// can swap in a renewed one without a restart: new connections get it,
// open ones keep theirs.
type certStore struct {
	certFile, keyFile string
	cert              atomic.Pointer[tls.Certificate]
}

func newCertStore(certFile, keyFile string) (*certStore, error) {
	s := &certStore{certFile: certFile, keyFile: keyFile}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// reload reads the certificate and key again. On error the current
// certificate stays in use.
func (s *certStore) reload() error {
	c, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return fmt.Errorf("loading %s and %s: %w", s.certFile, s.keyFile, err)
	}
	if c.Leaf == nil && len(c.Certificate) > 0 {
		c.Leaf, _ = x509.ParseCertificate(c.Certificate[0])
	}
	s.cert.Store(&c)
	return nil
}

func (s *certStore) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return s.cert.Load(), nil
}

// describe returns log attributes for the loaded certificate.
func (s *certStore) describe() []any {
	c := s.cert.Load()
	if c == nil || c.Leaf == nil {
		return nil
	}
	attrs := []any{"subject", c.Leaf.Subject.CommonName, "expires", c.Leaf.NotAfter.UTC().Format(time.RFC3339)}
	if len(c.Leaf.DNSNames) > 0 {
		attrs = append(attrs, "names", c.Leaf.DNSNames)
	}
	return attrs
}

// tlsConfig serves the store's certificate with TLS 1.2 or newer.
func (s *certStore) tlsConfig() *tls.Config {
	return &tls.Config{GetCertificate: s.get, MinVersion: tls.VersionTLS12}
}

// warnExpiry logs when the certificate expires within 14 days.
func (s *certStore) warnExpiry(log *slog.Logger) {
	c := s.cert.Load()
	if c == nil || c.Leaf == nil {
		return
	}
	if left := time.Until(c.Leaf.NotAfter); left < 14*24*time.Hour {
		log.Warn("TLS certificate expires soon", "expires", c.Leaf.NotAfter.UTC().Format(time.RFC3339), "in", left.Round(time.Hour))
	}
}
