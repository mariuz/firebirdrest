package server

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
)

func fakeClock(l *limiter) *time.Time {
	t := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return t }
	return &t
}

func anonReq(ip string) *reqCtx {
	r := httptest.NewRequest("GET", "/books", nil)
	r.RemoteAddr = ip + ":40000"
	return &reqCtx{r: r, anon: true, role: "web_anon"}
}

func userReq(sub string) *reqCtx {
	return &reqCtx{r: httptest.NewRequest("GET", "/books", nil), role: "web_user", claims: map[string]any{"sub": sub}}
}

func retryOf(t *testing.T, err error) int {
	t.Helper()
	var le *limitError
	if !errors.As(err, &le) {
		t.Fatalf("not a 429: %v", err)
	}
	if e := toAPIError(err); e.Status != 429 || e.Code != "FBRST138" {
		t.Fatalf("api error: %+v", e)
	}
	return le.retry
}

func TestRateLimit(t *testing.T) {
	l := newLimiter(2, 0.5, 0, 0, "")
	now := fakeClock(l)
	// a client with a token: burst of 2× the rate, then refused until refilled
	for i := 0; i < 4; i++ {
		if _, err := l.admit(userReq("ana"), false); err != nil {
			t.Fatalf("request %d within the burst: %v", i+1, err)
		}
	}
	_, err := l.admit(userReq("ana"), false)
	if retryOf(t, err) != 1 {
		t.Errorf("Retry-After for 2/s: %d", retryOf(t, err))
	}
	// another sub of the same role has its own bucket
	if _, err := l.admit(userReq("bob"), false); err != nil {
		t.Errorf("bob limited by ana's requests: %v", err)
	}
	*now = now.Add(500 * time.Millisecond) // one token back at 2/s
	if _, err := l.admit(userReq("ana"), false); err != nil {
		t.Errorf("after refilling: %v", err)
	}
	if _, err := l.admit(userReq("ana"), false); err == nil {
		t.Error("the refill was one token")
	}
	// anonymous clients have their own rate, per IP: 0.5/s, burst 1
	if _, err := l.admit(anonReq("198.51.100.7"), false); err != nil {
		t.Fatal(err)
	}
	_, err = l.admit(anonReq("198.51.100.7"), false)
	if retryOf(t, err) != 2 {
		t.Errorf("Retry-After for 0.5/s: %d", retryOf(t, err))
	}
	if _, err := l.admit(anonReq("198.51.100.8"), false); err != nil {
		t.Errorf("another IP: %v", err)
	}
	if l.refusedRate.Load() != 3 {
		t.Errorf("refused: %d", l.refusedRate.Load())
	}
	// idle clients are forgotten
	*now = now.Add(limiterIdle + time.Second)
	l.prune()
	if n := l.size(); n != 0 {
		t.Errorf("clients after pruning: %d", n)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	l := newLimiter(0, 0, 0, 2, "")
	fakeClock(l)
	r1, err1 := l.admit(userReq("ana"), false)
	r2, err2 := l.admit(userReq("ana"), false)
	if err1 != nil || err2 != nil || r1 == nil || r2 == nil {
		t.Fatalf("two in flight: %v %v", err1, err2)
	}
	if _, err := l.admit(userReq("ana"), false); err == nil {
		t.Fatal("a third request in flight was admitted")
	}
	// event streams don't take a slot
	if rel, err := l.admit(userReq("ana"), true); err != nil || rel != nil {
		t.Errorf("stream: release %v, %v", rel != nil, err)
	}
	r1()
	r3, err := l.admit(userReq("ana"), false)
	if err != nil {
		t.Fatalf("after one finished: %v", err)
	}
	r2()
	r3()
	// a client in flight is never pruned
	rel, _ := l.admit(userReq("carol"), false)
	*fakeClock(l) = time.Unix(1_700_000_000, 0).Add(time.Hour)
	l.prune()
	if l.size() == 0 {
		t.Error("pruned a client with a request in progress")
	}
	rel()
	if l.refusedConcurrent.Load() != 1 {
		t.Errorf("refused: %d", l.refusedConcurrent.Load())
	}
	if newLimiter(0, 0, 0, 0, "") != nil {
		t.Error("a limiter without limits")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:51000"
	r.Header.Add("X-Forwarded-For", "203.0.113.9, 198.51.100.1")
	r.Header.Set("X-Real-IP", "198.51.100.2")
	for header, want := range map[string]string{
		"":                "10.0.0.5",     // no proxy configured: headers are ignored
		"X-Forwarded-For": "198.51.100.1", // the entry the closest proxy added, not the client's
		"X-Real-IP":       "198.51.100.2",
		"X-Missing":       "10.0.0.5",
	} {
		if got := clientIP(r, header); got != want {
			t.Errorf("%q: %s, want %s", header, got, want)
		}
	}
	r.Header.Add("X-Forwarded-For", "192.0.2.4") // a second header line: the last wins
	if got := clientIP(r, "X-Forwarded-For"); got != "192.0.2.4" {
		t.Errorf("two header lines: %s", got)
	}
}

func TestRateLimitServer(t *testing.T) {
	ts := testServerWith(t, func(c *config.Config) { c.RateLimit = 1; c.RateBurst = 2 })
	ana := "Bearer " + token(t, jwt.MapClaims{"role": "web_user", "sub": "ana"})
	bob := "Bearer " + token(t, jwt.MapClaims{"role": "web_user", "sub": "bob"})
	for i := 0; i < 2; i++ {
		if r := do(t, ts, "GET", "/books?select=id&limit=1", "", "Authorization", ana); r.status != 200 {
			t.Fatalf("within the burst: %d %s", r.status, r.body)
		}
	}
	r := do(t, ts, "GET", "/books?select=id&limit=1", "", "Authorization", ana)
	if r.status != 429 || r.header.Get("Retry-After") != "1" || !strings.Contains(r.body, "FBRST138") {
		t.Errorf("over the rate: %d %q %s", r.status, r.header.Get("Retry-After"), r.body)
	}
	if r := do(t, ts, "GET", "/books?select=id&limit=1", "", "Authorization", bob); r.status != 200 {
		t.Errorf("another client: %d %s", r.status, r.body)
	}
	time.Sleep(1100 * time.Millisecond)
	if r := do(t, ts, "GET", "/books?select=id&limit=1", "", "Authorization", ana); r.status != 200 {
		t.Errorf("after a second: %d %s", r.status, r.body)
	}
	if m := metricsOf(serverOf(t, ts)); !strings.Contains(m, `firebirdrest_rate_limited_total{limit="rate"} 1`) {
		t.Errorf("metrics: %s", m)
	}
}
