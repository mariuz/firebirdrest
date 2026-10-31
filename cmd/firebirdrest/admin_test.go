package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeServer struct {
	ready    error
	reloaded int
}

func (f *fakeServer) Ready(context.Context) error { return f.ready }
func (f *fakeServer) Reload(context.Context) error {
	f.reloaded++
	return nil
}
func (f *fakeServer) Metrics() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("firebirdrest_up 1\n")) })
}

func call(h http.Handler, method, path, auth string) int {
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestAdminToken(t *testing.T) {
	const tok = "an-admin-token-of-some-length"
	f := &fakeServer{}
	h := adminHandler(f, tok, true)
	for _, c := range []struct {
		method, path, auth string
		want               int
	}{
		// probes never need the token
		{"GET", "/live", "", 200},
		{"GET", "/ready", "", 200},
		// the rest does
		{"GET", "/metrics", "", 401},
		{"GET", "/metrics", "Bearer wrong", 401},
		{"GET", "/metrics", tok, 401},
		{"GET", "/metrics", "Bearer " + tok, 200},
		{"POST", "/reload", "", 401},
		{"POST", "/reload", "Bearer " + tok, 204},
		{"GET", "/reload", "Bearer " + tok, 405},
		{"GET", "/debug/pprof/", "", 401},
		{"GET", "/debug/pprof/", "Bearer " + tok, 200},
	} {
		if got := call(h, c.method, c.path, c.auth); got != c.want {
			t.Errorf("%s %s (%q): %d, want %d", c.method, c.path, c.auth, got, c.want)
		}
	}
	if f.reloaded != 1 {
		t.Errorf("reloads: %d, want 1 (only the authorized one)", f.reloaded)
	}
	f.ready = errors.New("down")
	if got := call(h, "GET", "/ready", ""); got != 503 {
		t.Errorf("ready while the database is down: %d", got)
	}
	// without a token everything is open, as before; pprof only when enabled
	open := adminHandler(&fakeServer{}, "", false)
	if call(open, "GET", "/metrics", "") != 200 || call(open, "POST", "/reload", "") != 204 || call(open, "GET", "/debug/pprof/", "") != 404 {
		t.Error("admin server without a token")
	}
}
