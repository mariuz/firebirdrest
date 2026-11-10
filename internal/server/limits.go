package server

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariuz/firebirdrest/internal/query"
)

// Rate and concurrency limits, per client: a token's role and sub claim,
// or for anonymous requests the client's IP address. Off unless configured.
//
//	server-rate-limit        requests per second for clients with a token
//	server-rate-limit-anon   requests per second for anonymous clients (default: server-rate-limit)
//	server-rate-burst        requests a client may make at once above its rate (default: 2× the rate, at least 1)
//	server-max-concurrent    requests a client may have in progress at once (event streams excepted)
//
// A refused request gets 429 with Retry-After. Clients idle for a few
// minutes are forgotten, so the table holds only recent clients.

const limiterIdle = 5 * time.Minute

type limiter struct {
	authRate, anonRate float64 // per second; 0 = no rate limit
	burst              float64 // 0 = derived from the rate
	maxConcurrent      int     // 0 = no concurrency limit
	ipHeader           string  // header holding the client IP behind a proxy

	mu      sync.Mutex
	clients map[string]*clientState
	now     func() time.Time // for tests

	refusedRate, refusedConcurrent atomic.Uint64
}

type clientState struct {
	tokens   float64
	last     time.Time
	inFlight int
}

func newLimiter(authRate, anonRate float64, burst, maxConcurrent int, ipHeader string) *limiter {
	if authRate <= 0 && anonRate <= 0 && maxConcurrent <= 0 {
		return nil
	}
	if anonRate <= 0 {
		anonRate = authRate
	}
	return &limiter{authRate: authRate, anonRate: anonRate, burst: float64(burst), maxConcurrent: maxConcurrent,
		ipHeader: ipHeader, clients: map[string]*clientState{}, now: time.Now}
}

// clientKey names the client a request is counted against.
func (l *limiter) clientKey(rc *reqCtx) (key string, anon bool) {
	if !rc.anon {
		sub, _ := rc.claims["sub"].(string)
		return "role:" + strings.ToUpper(rc.role) + "\x00sub:" + sub, false
	}
	return "ip:" + clientIP(rc.r, l.ipHeader), true
}

// clientIP is the address the request came from, or with header set (behind
// a proxy) the address the proxy reports: X-Real-IP as is, or the last entry
// of X-Forwarded-For, the one the proxy closest to this server added. The
// entries before it come from the client and can't be trusted.
func clientIP(r *http.Request, header string) string {
	if header != "" {
		if v := r.Header.Values(header); len(v) > 0 {
			parts := strings.Split(v[len(v)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// admit counts a request against its client. It returns a release function
// to call when the request is done (nil when it holds no concurrency slot),
// or a 429 error.
func (l *limiter) admit(rc *reqCtx, stream bool) (func(), error) {
	key, anon := l.clientKey(rc)
	rate := l.authRate
	if anon {
		rate = l.anonRate
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.clients[key]
	if c == nil {
		c = &clientState{tokens: l.burstFor(rate), last: now}
		l.clients[key] = c
	}
	if rate > 0 {
		c.tokens = math.Min(l.burstFor(rate), c.tokens+now.Sub(c.last).Seconds()*rate)
		c.last = now
		if c.tokens < 1 {
			l.refusedRate.Add(1)
			wait := math.Ceil((1 - c.tokens) / rate)
			return nil, tooMany(fmt.Sprintf("rate limit of %s requests per second exceeded", strconv.FormatFloat(rate, 'f', -1, 64)), int(wait))
		}
	} else {
		c.last = now
	}
	if l.maxConcurrent > 0 && !stream {
		if c.inFlight >= l.maxConcurrent {
			l.refusedConcurrent.Add(1)
			return nil, tooMany(fmt.Sprintf("%d requests already in progress for this client", c.inFlight), 1)
		}
		c.inFlight++
		if rate > 0 {
			c.tokens--
		}
		return func() {
			l.mu.Lock()
			c.inFlight--
			l.mu.Unlock()
		}, nil
	}
	if rate > 0 {
		c.tokens--
	}
	return nil, nil
}

func (l *limiter) burstFor(rate float64) float64 {
	if l.burst > 0 {
		return l.burst
	}
	return math.Max(1, math.Ceil(2*rate))
}

// prune forgets clients idle for limiterIdle with no request in progress.
func (l *limiter) prune() {
	cut := l.now().Add(-limiterIdle)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, c := range l.clients {
		if c.inFlight == 0 && c.last.Before(cut) {
			delete(l.clients, k)
		}
	}
}

func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.clients)
}

// limitError is the 429 answer; retry is the Retry-After value in seconds.
type limitError struct {
	qe    *query.Error
	retry int
}

func tooMany(msg string, retry int) error {
	return &limitError{qe: &query.Error{Status: http.StatusTooManyRequests, Code: "FBRST138", Message: msg,
		Hint: fmt.Sprintf("retry after %d second(s)", max(retry, 1))}, retry: max(retry, 1)}
}

func (e *limitError) Error() string { return e.qe.Error() }
func (e *limitError) Unwrap() error { return e.qe }
