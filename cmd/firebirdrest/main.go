// Command firebirdrest serves a RESTful API from an existing Firebird database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // Prefer: timezone= works without a system time zone database

	"github.com/nakagami/firebirdsql"

	"github.com/mariuz/firebirdrest/internal/config"
	"github.com/mariuz/firebirdrest/internal/server"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	checkOnly := flag.Bool("check", false, "load the configuration, report problems and exit without connecting")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: firebirdrest [config-file]\n\nSettings can also be given as FBREST_* environment variables.\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if server.Version == "dev" {
		// go install github.com/mariuz/firebirdrest/cmd/firebirdrest@v1.2.3
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			server.Version = bi.Main.Version
		}
	}
	if *showVersion {
		fmt.Println("firebirdrest", server.Version)
		return
	}
	cfg, err := config.Load(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	if *checkOnly {
		if cfg.TLSCert != "" {
			certs, err := newCertStore(cfg.TLSCert, cfg.TLSKey)
			if err != nil {
				fmt.Fprintln(os.Stderr, "config error: server-tls-cert:", err)
				os.Exit(2)
			}
			if c := certs.cert.Load(); c.Leaf != nil {
				fmt.Printf("TLS certificate %q expires %s\n", c.Leaf.Subject.CommonName, c.Leaf.NotAfter.UTC().Format(time.RFC3339))
			}
		}
		for _, w := range cfg.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		fmt.Printf("configuration ok: firebirdrest %s, db-uri %s, %d warning(s)\n", server.Version, config.RedactURI(cfg.DBURI), len(cfg.Warnings))
		return
	}

	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error", "crit":
		level = slog.LevelError
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	log.Info("starting firebirdrest", "version", server.Version)
	for _, w := range cfg.Warnings {
		log.Warn("configuration", "warning", w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var srv *server.Server
	for attempt := 1; ; attempt++ {
		srv, err = server.New(ctx, cfg, log)
		if err == nil {
			break
		}
		wait := time.Duration(min(attempt, 10)) * time.Second
		log.Error("database not ready, retrying", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
	defer srv.Close()

	var certs *certStore
	if cfg.TLSCert != "" {
		if certs, err = newCertStore(cfg.TLSCert, cfg.TLSKey); err != nil {
			log.Error("TLS", "err", err)
			os.Exit(2)
		}
		log.Info("TLS certificate loaded", certs.describe()...)
		certs.warnExpiry(log)
	}

	// SIGUSR1 reloads the schema cache, SIGHUP the TLS certificate and the
	// schema cache (not available on Windows).
	reloadOnSignal(srv, certs, log)

	// POST_EVENT '<db-reload-event>' from the database also reloads it.
	if cfg.DBReloadEvent != "" {
		go listenReload(ctx, cfg, srv, log)
	}

	if cfg.AdminPort > 0 {
		admin := adminHandler(srv, cfg.AdminToken, cfg.AdminPprof)
		go func() {
			addr := net.JoinHostPort(cfg.AdminHost, strconv.Itoa(cfg.AdminPort))
			log.Info("admin server listening", "addr", addr, "token", cfg.AdminToken != "")
			as := &http.Server{Addr: addr, Handler: admin, ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
			if err := as.ListenAndServe(); err != nil {
				log.Error("admin server stopped", "err", err)
			}
		}()
	}

	addr := net.JoinHostPort(cfg.ServerHost, strconv.Itoa(cfg.ServerPort))
	timeout := time.Duration(cfg.ServerTimeout) * time.Second
	hs := &http.Server{Addr: addr, Handler: srv,
		ReadHeaderTimeout: 10 * time.Second,
		// a request may take server-request-timeout to arrive and as long again to
		// be answered; the handler also cancels its work at that deadline
		ReadTimeout:    timeout,
		WriteTimeout:   2 * timeout,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}
	if certs != nil {
		hs.TLSConfig = certs.tlsConfig()
	}
	// the server's own errors (mostly failed TLS handshakes from scanners and
	// plain-HTTP clients) go to the structured log, at debug level
	hs.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelDebug)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		log.Info("shutting down: finishing requests in flight")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := hs.Shutdown(sctx); err != nil {
			log.Warn("requests still running after 10s, closing anyway", "err", err)
		}
	}()
	log.Info("listening", "addr", addr, "tls", certs != nil)
	serve := hs.ListenAndServe
	if certs != nil {
		serve = func() error { return hs.ListenAndServeTLS("", "") } // the certificate comes from TLSConfig
	}
	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
	// ListenAndServe returns as soon as Shutdown starts: wait for the requests
	// in flight before the deferred srv.Close() closes their connections
	<-stopped
}

func listenReload(ctx context.Context, cfg *config.Config, srv *server.Server, log *slog.Logger) {
	for ctx.Err() == nil {
		ev, err := firebirdsql.NewFBEvent(cfg.DBURI)
		if err == nil {
			_, err = ev.Subscribe([]string{cfg.DBReloadEvent}, func(e firebirdsql.Event) {
				log.Info("reload event received", "event", e.Name)
				if err := srv.Reload(context.Background()); err != nil {
					log.Error("schema reload failed", "err", err)
				}
			})
		}
		if err == nil {
			log.Info("listening for reload event", "event", cfg.DBReloadEvent)
			<-ctx.Done()
			ev.Close()
			return
		}
		log.Warn("cannot subscribe to reload event, retrying", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}
