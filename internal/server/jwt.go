package server

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
)

// jwtVerifier checks bearer tokens against the keys of jwt-secret and
// jwt-jwks-url, and caches tokens it has accepted.
//
// jwt-secret is one of
//   - an HMAC secret (base64 encoded with jwt-secret-is-base64),
//   - a PEM public key or certificate (RSA, ECDSA or Ed25519),
//   - a JWK or a JWKS as JSON ({"kty": ...} or {"keys": [...]}),
//   - "@/path/to/file" to read any of the above from a file.
//
// jwt-jwks-url is fetched at startup, every jwt-jwks-refresh seconds, and
// again (at most every 30 seconds) when a token names a key id that isn't
// known, so keys rotated by the identity provider are picked up.
type jwtVerifier struct {
	cfg *config.Config
	log *slog.Logger
	m   *metrics

	static []verifyKey
	remote atomic.Pointer[[]verifyKey]

	client    *http.Client
	fetchMu   sync.Mutex
	lastFetch time.Time
	stop      chan struct{}

	cacheMu sync.Mutex
	cache   map[string]cachedToken
}

// verifyKey is one key a token may be signed with.
type verifyKey struct {
	kid string
	alg string // "" = any algorithm of the key's type
	key any    // []byte, *rsa.PublicKey, *ecdsa.PublicKey or ed25519.PublicKey
}

type cachedToken struct {
	claims jwt.MapClaims
	until  time.Time
}

// jwtCacheTTL bounds how long an accepted token is reused without checking
// its signature again, so a key removed from the JWKS stops working soon.
const jwtCacheTTL = 5 * time.Minute

// jwksRetry is the shortest interval between two fetches of jwt-jwks-url.
const jwksRetry = 30 * time.Second

var jwtMethods = []string{"HS256", "HS384", "HS512", "RS256", "RS384", "RS512",
	"PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}

func newJWTVerifier(cfg *config.Config, log *slog.Logger, m *metrics) (*jwtVerifier, error) {
	v := &jwtVerifier{cfg: cfg, log: log, m: m, cache: map[string]cachedToken{},
		client: &http.Client{Timeout: 10 * time.Second}, stop: make(chan struct{})}
	if cfg.JWTSecret != "" {
		secret := []byte(cfg.JWTSecret)
		if strings.HasPrefix(cfg.JWTSecret, "@") {
			b, err := os.ReadFile(strings.TrimPrefix(cfg.JWTSecret, "@"))
			if err != nil {
				return nil, fmt.Errorf("jwt-secret: %w", err)
			}
			secret = b
		}
		keys, err := parseSecret(secret, cfg.JWTSecretIsBase64)
		if err != nil {
			return nil, fmt.Errorf("jwt-secret: %w", err)
		}
		v.static = keys
	}
	if cfg.JWTJWKSURL != "" {
		if err := v.fetch(context.Background()); err != nil {
			// keep serving: anonymous requests work, and the refresh retries
			log.Error("cannot fetch jwt-jwks-url, retrying", "url", cfg.JWTJWKSURL, "err", err)
		}
		go v.refresher()
	}
	return v, nil
}

func (v *jwtVerifier) enabled() bool {
	return len(v.static) > 0 || v.cfg.JWTJWKSURL != ""
}

func (v *jwtVerifier) close() {
	select {
	case <-v.stop:
	default:
		close(v.stop)
	}
}

// verify returns the claims of a valid token.
func (v *jwtVerifier) verify(token string) (jwt.MapClaims, error) {
	if v.cfg.JWTCacheMaxEntries > 0 {
		v.cacheMu.Lock()
		c, ok := v.cache[token]
		v.cacheMu.Unlock()
		if ok && time.Now().Before(c.until) {
			v.m.jwtCacheHits.Add(1)
			return c.claims, nil
		}
		v.m.jwtCacheMisses.Add(1)
	}
	opts := []jwt.ParserOption{jwt.WithValidMethods(jwtMethods)}
	if v.cfg.JWTAudience != "" {
		opts = append(opts, jwt.WithAudience(v.cfg.JWTAudience))
	}
	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(token, claims, v.keyFunc, opts...); err != nil {
		return nil, err
	}
	if v.cfg.JWTCacheMaxEntries > 0 {
		v.store(token, claims)
	}
	return claims, nil
}

func (v *jwtVerifier) store(token string, claims jwt.MapClaims) {
	until := time.Now().Add(jwtCacheTTL)
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil && exp.Before(until) {
		until = exp.Time
	}
	v.cacheMu.Lock()
	defer v.cacheMu.Unlock()
	if len(v.cache) >= v.cfg.JWTCacheMaxEntries {
		now := time.Now()
		for k, c := range v.cache {
			if !now.Before(c.until) {
				delete(v.cache, k)
			}
		}
		// still full: drop arbitrary entries (map order is random)
		for k := range v.cache {
			if len(v.cache) < v.cfg.JWTCacheMaxEntries {
				break
			}
			delete(v.cache, k)
		}
	}
	v.cache[token] = cachedToken{claims: claims, until: until}
}

func (v *jwtVerifier) clearCache() {
	v.cacheMu.Lock()
	v.cache = map[string]cachedToken{}
	v.cacheMu.Unlock()
}

// keyFunc picks the keys that may have signed the token.
func (v *jwtVerifier) keyFunc(t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)
	keys := v.candidates(t.Method, kid)
	if len(keys) == 0 && kid != "" && v.cfg.JWTJWKSURL != "" && v.refetch() {
		keys = v.candidates(t.Method, kid) // the provider may have added a key
	}
	switch len(keys) {
	case 0:
		if kid != "" {
			return nil, fmt.Errorf("no %s key with kid %q", t.Method.Alg(), kid)
		}
		return nil, fmt.Errorf("no key for signing method %s", t.Method.Alg())
	case 1:
		return keys[0], nil
	}
	return jwt.VerificationKeySet{Keys: keys}, nil
}

func (v *jwtVerifier) candidates(m jwt.SigningMethod, kid string) []jwt.VerificationKey {
	all := v.static
	if r := v.remote.Load(); r != nil {
		all = append(all[:len(all):len(all)], *r...)
	}
	var out []jwt.VerificationKey
	for _, k := range all {
		if kid != "" && k.kid != "" && k.kid != kid {
			continue
		}
		if k.alg != "" && k.alg != m.Alg() {
			continue
		}
		if keyFits(m, k.key) {
			out = append(out, k.key)
		}
	}
	return out
}

func keyFits(m jwt.SigningMethod, key any) bool {
	switch m.(type) {
	case *jwt.SigningMethodHMAC:
		_, ok := key.([]byte)
		return ok
	case *jwt.SigningMethodRSA, *jwt.SigningMethodRSAPSS:
		_, ok := key.(*rsa.PublicKey)
		return ok
	case *jwt.SigningMethodECDSA:
		_, ok := key.(*ecdsa.PublicKey)
		return ok
	case *jwt.SigningMethodEd25519:
		_, ok := key.(ed25519.PublicKey)
		return ok
	}
	return false
}

// refresher re-fetches jwt-jwks-url: every jwt-jwks-refresh seconds, or every
// jwksRetry while fetching fails.
func (v *jwtVerifier) refresher() {
	every := max(time.Duration(v.cfg.JWTJWKSRefresh)*time.Second, jwksRetry)
	for {
		wait := every
		if v.remote.Load() == nil {
			wait = jwksRetry
		}
		select {
		case <-v.stop:
			return
		case <-time.After(wait):
		}
		if err := v.fetch(context.Background()); err != nil {
			v.log.Error("cannot fetch jwt-jwks-url", "url", v.cfg.JWTJWKSURL, "err", err)
		}
	}
}

// refetch fetches the JWKS now unless it was fetched recently. Concurrent
// callers wait for one fetch instead of each making their own.
func (v *jwtVerifier) refetch() bool {
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	if time.Since(v.lastFetch) < jwksRetry {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := v.fetchLocked(ctx); err != nil {
		v.log.Warn("cannot fetch jwt-jwks-url", "url", v.cfg.JWTJWKSURL, "err", err)
		return false
	}
	return true
}

func (v *jwtVerifier) fetch(ctx context.Context) error {
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	return v.fetchLocked(ctx)
}

func (v *jwtVerifier) fetchLocked(ctx context.Context) error {
	v.lastFetch = time.Now()
	v.m.jwksFetches.Add(1)
	keys, err := v.download(ctx)
	if err != nil {
		v.m.jwksErrors.Add(1)
		return err
	}
	old := v.remote.Swap(&keys)
	if old != nil && !sameKeys(*old, keys) {
		// a removed key must stop working now, not when cached tokens expire
		v.clearCache()
		v.log.Info("jwt-jwks-url keys changed", "keys", len(keys))
	} else if old == nil {
		v.log.Info("jwt-jwks-url loaded", "keys", len(keys))
	}
	return nil
}

func (v *jwtVerifier) download(ctx context.Context) ([]verifyKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWTJWKSURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	keys, err := parseJWKJSON(body)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, errors.New("the JWKS has no usable signing keys")
	}
	return keys, nil
}

func sameKeys(a, b []verifyKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].kid != b[i].kid || a[i].alg != b[i].alg || !keyEqual(a[i].key, b[i].key) {
			return false
		}
	}
	return true
}

func keyEqual(a, b any) bool {
	switch x := a.(type) {
	case []byte:
		y, ok := b.([]byte)
		return ok && string(x) == string(y)
	case interface{ Equal(crypto.PublicKey) bool }:
		return x.Equal(b)
	}
	return false
}

// parseSecret reads jwt-secret.
func parseSecret(secret []byte, isBase64 bool) ([]verifyKey, error) {
	trimmed := strings.TrimSpace(string(secret))
	switch {
	case strings.HasPrefix(trimmed, "{"):
		keys, err := parseJWKJSON([]byte(trimmed))
		if err != nil {
			return nil, err
		}
		if len(keys) == 0 {
			return nil, errors.New("the JWKS has no usable signing keys")
		}
		return keys, nil
	case strings.HasPrefix(trimmed, "-----BEGIN"):
		return parsePEM([]byte(trimmed))
	}
	if isBase64 {
		b, err := decodeBase64(trimmed)
		if err != nil {
			return nil, fmt.Errorf("jwt-secret-is-base64 is set but the secret is not base64: %w", err)
		}
		return []verifyKey{{key: b}}, nil
	}
	return []verifyKey{{key: secret}}, nil
}

func decodeBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	_, err := base64.StdEncoding.DecodeString(s)
	return nil, err
}

// parsePEM reads every public key or certificate in the PEM data.
func parsePEM(data []byte) ([]verifyKey, error) {
	var keys []verifyKey
	for {
		block, rest := pem.Decode(data)
		if block == nil {
			break
		}
		data = rest
		var key any
		var err error
		switch block.Type {
		case "PUBLIC KEY":
			key, err = x509.ParsePKIXPublicKey(block.Bytes)
		case "RSA PUBLIC KEY":
			key, err = x509.ParsePKCS1PublicKey(block.Bytes)
		case "CERTIFICATE":
			var cert *x509.Certificate
			if cert, err = x509.ParseCertificate(block.Bytes); err == nil {
				key = cert.PublicKey
			}
		default:
			return nil, fmt.Errorf("unsupported PEM block %q: give a public key or a certificate", block.Type)
		}
		if err != nil {
			return nil, err
		}
		switch key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
		default:
			return nil, fmt.Errorf("unsupported public key type %T", key)
		}
		keys = append(keys, verifyKey{key: key})
	}
	if len(keys) == 0 {
		return nil, errors.New("invalid PEM")
	}
	return keys, nil
}

type jwkJSON struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
	K   string `json:"k"`
}

// parseJWKJSON reads a JWK or a JWKS. In a JWKS, keys that aren't for
// signatures, or of an unsupported type, are skipped; a single JWK must be
// usable.
func parseJWKJSON(data []byte) ([]verifyKey, error) {
	var probe struct {
		Keys json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("invalid JWK JSON: %w", err)
	}
	if probe.Keys == nil {
		var k jwkJSON
		if err := json.Unmarshal(data, &k); err != nil {
			return nil, fmt.Errorf("invalid JWK JSON: %w", err)
		}
		vk, err := k.toKey()
		if err != nil {
			return nil, err
		}
		return []verifyKey{vk}, nil
	}
	var set []jwkJSON
	if err := json.Unmarshal(probe.Keys, &set); err != nil {
		return nil, fmt.Errorf("invalid JWKS: %w", err)
	}
	var keys []verifyKey
	for _, k := range set {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		vk, err := k.toKey()
		if err != nil {
			continue
		}
		keys = append(keys, vk)
	}
	return keys, nil
}

func (k jwkJSON) toKey() (verifyKey, error) {
	b64 := base64.RawURLEncoding
	field := func(name, v string) ([]byte, error) {
		b, err := b64.DecodeString(strings.TrimRight(v, "="))
		if err != nil || len(b) == 0 {
			return nil, fmt.Errorf("JWK %q: invalid or missing %q", k.Kid, name)
		}
		return b, nil
	}
	vk := verifyKey{kid: k.Kid, alg: k.Alg}
	switch k.Kty {
	case "oct":
		b, err := field("k", k.K)
		if err != nil {
			return vk, err
		}
		vk.key = b
	case "RSA":
		n, err := field("n", k.N)
		if err != nil {
			return vk, err
		}
		e, err := field("e", k.E)
		if err != nil {
			return vk, err
		}
		ev := new(big.Int).SetBytes(e)
		if !ev.IsInt64() || ev.Int64() > 1<<31-1 || ev.Int64() < 3 {
			return vk, fmt.Errorf("JWK %q: invalid RSA exponent", k.Kid)
		}
		vk.key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(ev.Int64())}
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return vk, fmt.Errorf("JWK %q: unsupported curve %q", k.Kid, k.Crv)
		}
		x, err := field("x", k.X)
		if err != nil {
			return vk, err
		}
		y, err := field("y", k.Y)
		if err != nil {
			return vk, err
		}
		pub := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if _, err := pub.ECDH(); err != nil { // rejects points not on the curve
			return vk, fmt.Errorf("JWK %q: invalid EC point", k.Kid)
		}
		vk.key = pub
	case "OKP":
		if k.Crv != "Ed25519" {
			return vk, fmt.Errorf("JWK %q: unsupported curve %q", k.Kid, k.Crv)
		}
		x, err := field("x", k.X)
		if err != nil {
			return vk, err
		}
		if len(x) != ed25519.PublicKeySize {
			return vk, fmt.Errorf("JWK %q: invalid Ed25519 key", k.Kid)
		}
		vk.key = ed25519.PublicKey(x)
	default:
		return vk, fmt.Errorf("JWK %q: unsupported key type %q", k.Kid, k.Kty)
	}
	return vk, nil
}
