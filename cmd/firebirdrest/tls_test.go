package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCert writes a self-signed certificate and key for localhost with the
// given serial number.
func writeCert(t *testing.T, dir string, serial int64) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// servedSerial connects with a fresh TLS connection and returns the serial
// of the certificate the server presents.
func servedSerial(t *testing.T, url string) int64 {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives: true,
	}}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.TLS.PeerCertificates[0].SerialNumber.Int64()
}

func TestCertReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, 1)
	store, err := newCertStore(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	// served as main serves it: no Certificates, only GetCertificate
	// (httptest's StartTLS would add a certificate of its own)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), TLSConfig: store.tlsConfig()}
	go hs.ServeTLS(ln, "", "")
	defer hs.Close()
	ts := struct{ URL string }{"https://" + ln.Addr().String()}

	if s := servedSerial(t, ts.URL); s != 1 {
		t.Fatalf("serial %d, want 1", s)
	}
	// a renewal: new files, then reload; new connections get the new certificate
	writeCert(t, dir, 2)
	if s := servedSerial(t, ts.URL); s != 1 {
		t.Fatalf("before reload: serial %d, want 1", s)
	}
	if err := store.reload(); err != nil {
		t.Fatal(err)
	}
	if s := servedSerial(t, ts.URL); s != 2 {
		t.Fatalf("after reload: serial %d, want 2", s)
	}
	// a broken renewal keeps the certificate in use
	os.WriteFile(keyFile, []byte("not a key"), 0o600)
	if err := store.reload(); err == nil {
		t.Fatal("reload of a broken key succeeded")
	}
	if s := servedSerial(t, ts.URL); s != 2 {
		t.Fatalf("after a failed reload: serial %d, want 2", s)
	}
	if attrs := store.describe(); len(attrs) < 4 || attrs[1] != "localhost" {
		t.Errorf("describe: %v", attrs)
	}
	// TLS 1.1 is refused
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS11}}}
	if _, err := c.Get(ts.URL); err == nil {
		t.Error("TLS 1.1 accepted")
	}
}

func TestCertStoreErrors(t *testing.T) {
	if _, err := newCertStore("/nonexistent/cert.pem", "/nonexistent/key.pem"); err == nil {
		t.Error("missing files accepted")
	}
}
