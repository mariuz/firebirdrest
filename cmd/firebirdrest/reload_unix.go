//go:build !windows

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mariuz/firebirdrest/internal/server"
)

// reloadOnSignal reloads the schema cache on SIGUSR1, and on SIGHUP the TLS
// certificate (when one is configured) as well as the schema cache, so
// `systemctl reload` after a certificate renewal picks up both.
func reloadOnSignal(srv *server.Server, certs *certStore, log *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1, syscall.SIGHUP)
	go func() {
		for sig := range ch {
			if sig == syscall.SIGHUP && certs != nil {
				if err := certs.reload(); err != nil {
					log.Error("TLS certificate reload failed, keeping the current one", "err", err)
				} else {
					log.Info("TLS certificate reloaded", certs.describe()...)
					certs.warnExpiry(log)
				}
			}
			if err := srv.Reload(context.Background()); err != nil {
				log.Error("schema reload failed", "err", err)
			}
		}
	}()
}
