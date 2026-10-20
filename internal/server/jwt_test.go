package server

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
)

var b64 = base64.RawURLEncoding

func rsaJWK(kid string, k *rsa.PublicKey) map[string]any {
	return map[string]any{"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": b64.EncodeToString(k.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(k.E)).Bytes())}
}

func ecJWK(kid string, k *ecdsa.PublicKey) map[string]any {
	size := (k.Curve.Params().BitSize + 7) / 8
	return map[string]any{"kty": "EC", "kid": kid, "crv": k.Curve.Params().Name,
		"x": b64.EncodeToString(k.X.FillBytes(make([]byte, size))),
		"y": b64.EncodeToString(k.Y.FillBytes(make([]byte, size)))}
}

func mustJSON(t *testing.T, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sign(t *testing.T, m jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(m, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func verifier(t *testing.T, change func(*config.Config)) (*jwtVerifier, *metrics) {
	t.Helper()
	cfg := &config.Config{JWTCacheMaxEntries: 100, JWTJWKSRefresh: 3600}
	change(cfg)
	m := newMetrics()
	v, err := newJWTVerifier(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.close)
	return v, m
}

func TestJWTSecretFormats(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	hmac := []byte("reallyreallyreallyreallyverysafe1234")
	claims := jwt.MapClaims{"role": "web_user"}

	pkix1, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	rsaPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pkix1}))
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&rsaKey.PublicKey)}))
	edDER, _ := x509.MarshalPKIXPublicKey(edPub)
	edPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: edDER}))
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "idp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &ecKey.PublicKey, ecKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	file := filepath.Join(t.TempDir(), "jwks.json")
	os.WriteFile(file, []byte(mustJSON(t, map[string]any{"keys": []any{rsaJWK("r1", &rsaKey.PublicKey)}})), 0o600)

	cases := []struct {
		name   string
		secret string
		base64 bool
		token  string
	}{
		{"HMAC", string(hmac), false, sign(t, jwt.SigningMethodHS256, hmac, "", claims)},
		{"HMAC base64", base64.StdEncoding.EncodeToString(hmac), true, sign(t, jwt.SigningMethodHS512, hmac, "", claims)},
		{"RSA PEM", rsaPEM, false, sign(t, jwt.SigningMethodRS256, rsaKey, "", claims)},
		{"RSA PEM, PSS token", rsaPEM, false, sign(t, jwt.SigningMethodPS256, rsaKey, "", claims)},
		{"RSA PKCS1 PEM", pkcs1, false, sign(t, jwt.SigningMethodRS384, rsaKey, "", claims)},
		{"Ed25519 PEM", edPEM, false, sign(t, jwt.SigningMethodEdDSA, edPriv, "", claims)},
		{"certificate", certPEM, false, sign(t, jwt.SigningMethodES256, ecKey, "", claims)},
		{"RSA JWK", mustJSON(t, rsaJWK("", &rsaKey.PublicKey)), false, sign(t, jwt.SigningMethodRS256, rsaKey, "", claims)},
		{"EC JWK", mustJSON(t, ecJWK("e1", &ecKey.PublicKey)), false, sign(t, jwt.SigningMethodES256, ecKey, "e1", claims)},
		{"OKP JWK", mustJSON(t, map[string]any{"kty": "OKP", "crv": "Ed25519", "x": b64.EncodeToString(edPub)}), false,
			sign(t, jwt.SigningMethodEdDSA, edPriv, "", claims)},
		{"oct JWK", mustJSON(t, map[string]any{"kty": "oct", "k": b64.EncodeToString(hmac)}), false,
			sign(t, jwt.SigningMethodHS256, hmac, "", claims)},
		{"JWKS file", "@" + file, false, sign(t, jwt.SigningMethodRS256, rsaKey, "r1", claims)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, _ := verifier(t, func(cfg *config.Config) { cfg.JWTSecret, cfg.JWTSecretIsBase64 = c.secret, c.base64 })
			got, err := v.verify(c.token)
			if err != nil {
				t.Fatal(err)
			}
			if got["role"] != "web_user" {
				t.Errorf("claims %v", got)
			}
		})
	}

	// a key of one type never verifies a token of another: an HMAC token
	// signed with the PEM text as its secret is the classic confusion attack
	v, _ := verifier(t, func(cfg *config.Config) { cfg.JWTSecret = rsaPEM })
	if _, err := v.verify(sign(t, jwt.SigningMethodHS256, []byte(rsaPEM), "", claims)); err == nil {
		t.Error("HS256 token accepted with an RSA public key")
	}
	// alg in the JWK restricts the algorithm
	v, _ = verifier(t, func(cfg *config.Config) { cfg.JWTSecret = mustJSON(t, rsaJWK("", &rsaKey.PublicKey)) })
	if _, err := v.verify(sign(t, jwt.SigningMethodRS512, rsaKey, "", claims)); err == nil {
		t.Error("RS512 token accepted by an RS256 JWK")
	}

	for _, bad := range []struct{ secret, want string }{
		{"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----", "unsupported PEM block"},
		{`{"kty":"EC","crv":"P-256","x":"AAAA","y":"AAAA"}`, "invalid EC point"},
		{`{"keys":[{"kty":"RSA","use":"enc","n":"AQAB","e":"AQAB"}]}`, "no usable signing keys"},
		{`{"kty":`, "invalid JWK JSON"},
		{"@/nonexistent/secret", "no such file"},
	} {
		cfg := &config.Config{JWTSecret: bad.secret}
		if _, err := newJWTVerifier(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), newMetrics()); err == nil ||
			!strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q: got %v, want %q", bad.secret, err, bad.want)
		}
	}
	cfg := &config.Config{JWTSecret: "not base64!", JWTSecretIsBase64: true}
	if _, err := newJWTVerifier(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), newMetrics()); err == nil {
		t.Error("invalid base64 secret accepted")
	}
}

func TestJWKSKeySelection(t *testing.T) {
	k1, _ := rsa.GenerateKey(rand.Reader, 2048)
	k2, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := mustJSON(t, map[string]any{"keys": []any{rsaJWK("one", &k1.PublicKey), rsaJWK("two", &k2.PublicKey)}})
	v, _ := verifier(t, func(cfg *config.Config) { cfg.JWTSecret = jwks; cfg.JWTCacheMaxEntries = 0 })
	claims := jwt.MapClaims{"role": "web_user"}

	for _, c := range []struct {
		name  string
		token string
		ok    bool
	}{
		{"kid one", sign(t, jwt.SigningMethodRS256, k1, "one", claims), true},
		{"kid two", sign(t, jwt.SigningMethodRS256, k2, "two", claims), true},
		{"no kid: every key is tried", sign(t, jwt.SigningMethodRS256, k2, "", claims), true},
		{"wrong kid", sign(t, jwt.SigningMethodRS256, k1, "two", claims), false},
		{"unknown kid", sign(t, jwt.SigningMethodRS256, k1, "three", claims), false},
		{"unknown key", sign(t, jwt.SigningMethodRS256, other, "", claims), false},
	} {
		if _, err := v.verify(c.token); (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

func TestJWKSURL(t *testing.T) {
	k1, _ := rsa.GenerateKey(rand.Reader, 2048)
	k2, _ := rsa.GenerateKey(rand.Reader, 2048)
	var mu sync.Mutex
	keys := []any{rsaJWK("one", &k1.PublicKey)}
	var hits atomic.Int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	defer idp.Close()

	v, m := verifier(t, func(cfg *config.Config) { cfg.JWTJWKSURL = idp.URL })
	claims := jwt.MapClaims{"role": "web_user", "exp": time.Now().Add(time.Hour).Unix()}
	tok1 := sign(t, jwt.SigningMethodRS256, k1, "one", claims)
	if _, err := v.verify(tok1); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Errorf("fetched %d times at startup", hits.Load())
	}

	// cached: the second use skips the signature check
	if _, err := v.verify(tok1); err != nil {
		t.Fatal(err)
	}
	if m.jwtCacheHits.Load() != 1 || m.jwtCacheMisses.Load() != 1 {
		t.Errorf("cache hits %d, misses %d", m.jwtCacheHits.Load(), m.jwtCacheMisses.Load())
	}

	// the provider rotates to a new key: a token with its unknown kid makes
	// FirebirdREST fetch the JWKS again (allowed once the retry interval passed)
	mu.Lock()
	keys = []any{rsaJWK("two", &k2.PublicKey)}
	mu.Unlock()
	v.fetchMu.Lock()
	v.lastFetch = time.Now().Add(-time.Minute)
	v.fetchMu.Unlock()
	tok2 := sign(t, jwt.SigningMethodRS256, k2, "two", claims)
	if _, err := v.verify(tok2); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("fetched %d times after rotation", hits.Load())
	}
	// the removed key stops working at once, cache included
	if _, err := v.verify(tok1); err == nil {
		t.Error("token of a removed key still accepted")
	}
	// unknown kids don't make it fetch again within the retry interval
	for range 5 {
		v.verify(sign(t, jwt.SigningMethodRS256, k2, "nope", claims))
	}
	if hits.Load() != 2 {
		t.Errorf("fetched %d times for unknown kids", hits.Load())
	}
	if m.jwksFetches.Load() != 2 || m.jwksErrors.Load() != 0 {
		t.Errorf("jwks fetches %d, errors %d", m.jwksFetches.Load(), m.jwksErrors.Load())
	}

	// an unreachable URL at startup doesn't stop the server
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	v2, m2 := verifier(t, func(cfg *config.Config) { cfg.JWTJWKSURL = down.URL })
	if !v2.enabled() || m2.jwksErrors.Load() != 1 {
		t.Errorf("enabled %v, errors %d", v2.enabled(), m2.jwksErrors.Load())
	}
	if _, err := v2.verify(tok2); err == nil {
		t.Error("token accepted without keys")
	}
}

func TestJWTCache(t *testing.T) {
	secret := []byte("reallyreallyreallyreallyverysafe1234")
	v, m := verifier(t, func(cfg *config.Config) { cfg.JWTSecret = string(secret); cfg.JWTCacheMaxEntries = 3 })

	// an entry never outlives the token
	soon := sign(t, jwt.SigningMethodHS256, secret, "", jwt.MapClaims{"exp": time.Now().Add(time.Second).Unix()})
	if _, err := v.verify(soon); err != nil {
		t.Fatal(err)
	}
	v.cacheMu.Lock()
	until := v.cache[soon].until
	v.cacheMu.Unlock()
	if time.Until(until) > time.Second {
		t.Errorf("cached until %v, after exp", until)
	}
	v.cacheMu.Lock()
	v.cache[soon] = cachedToken{claims: v.cache[soon].claims, until: time.Now().Add(-time.Millisecond)}
	v.cacheMu.Unlock()
	hits := m.jwtCacheHits.Load()
	v.verify(soon)
	if m.jwtCacheHits.Load() != hits {
		t.Error("expired cache entry used")
	}

	// bounded size
	for i := range 10 {
		v.verify(sign(t, jwt.SigningMethodHS256, secret, "", jwt.MapClaims{"n": i}))
	}
	v.cacheMu.Lock()
	n := len(v.cache)
	v.cacheMu.Unlock()
	if n > 3 {
		t.Errorf("cache has %d entries, max 3", n)
	}
	// rejected tokens are not cached
	bad := sign(t, jwt.SigningMethodHS256, []byte("another secret, also long enough!!"), "", nil)
	for range 2 {
		if _, err := v.verify(bad); err == nil {
			t.Fatal("bad signature accepted")
		}
	}
}
