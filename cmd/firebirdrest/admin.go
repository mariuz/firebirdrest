package main

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/http/pprof"
	"time"
)

// adminServer is what the admin endpoints need from the API server.
type adminServer interface {
	Ready(context.Context) error
	Reload(context.Context) error
	Metrics() http.Handler
}

// adminHandler serves the admin port. /live and /ready are always open, so
// health checks and Kubernetes probes need no credentials; with a token,
// /reload, /metrics and /debug/pprof/ need "Authorization: Bearer <token>".
func adminHandler(srv adminServer, token string, withPprof bool) http.Handler {
	admin := http.NewServeMux()
	admin.HandleFunc("/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	admin.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := srv.Ready(c); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	guard := func(h http.Handler) http.Handler {
		if token == "" {
			return h
		}
		want := []byte("Bearer " + token)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="firebirdrest admin"`)
				http.Error(w, "admin-server-token required", http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	admin.Handle("/reload", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := srv.Reload(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	admin.Handle("/metrics", guard(srv.Metrics()))
	if withPprof {
		admin.Handle("/debug/pprof/", guard(http.HandlerFunc(pprof.Index)))
		admin.Handle("/debug/pprof/cmdline", guard(http.HandlerFunc(pprof.Cmdline)))
		admin.Handle("/debug/pprof/profile", guard(http.HandlerFunc(pprof.Profile)))
		admin.Handle("/debug/pprof/symbol", guard(http.HandlerFunc(pprof.Symbol)))
		admin.Handle("/debug/pprof/trace", guard(http.HandlerFunc(pprof.Trace)))
	}
	return admin
}
