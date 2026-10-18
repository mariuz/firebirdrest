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
	"strconv"
	"syscall"
	"time"

	"github.com/nakagami/firebirdsql"

	"github.com/mariuz/firebirdrest/internal/config"
	"github.com/mariuz/firebirdrest/internal/server"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: firebirdrest [config-file]\n\nSettings can also be given as FBREST_* environment variables.\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("firebirdrest", server.Version)
		return
	}
	cfg, err := config.Load(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
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

	// SIGUSR1 reloads the schema cache.
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	go func() {
		for range usr1 {
			if err := srv.Reload(context.Background()); err != nil {
				log.Error("schema reload failed", "err", err)
			}
		}
	}()

	// POST_EVENT '<db-reload-event>' from the database also reloads it.
	if cfg.DBReloadEvent != "" {
		go listenReload(ctx, cfg, srv, log)
	}

	if cfg.AdminPort > 0 {
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
		admin.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if err := srv.Reload(r.Context()); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		go func() {
			addr := net.JoinHostPort(cfg.ServerHost, strconv.Itoa(cfg.AdminPort))
			log.Info("admin server listening", "addr", addr)
			if err := http.ListenAndServe(addr, admin); err != nil {
				log.Error("admin server stopped", "err", err)
			}
		}()
	}

	addr := net.JoinHostPort(cfg.ServerHost, strconv.Itoa(cfg.ServerPort))
	hs := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
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
