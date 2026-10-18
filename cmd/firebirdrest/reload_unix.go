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

// reloadOnSignal reloads the schema cache on SIGUSR1.
func reloadOnSignal(srv *server.Server, log *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			if err := srv.Reload(context.Background()); err != nil {
				log.Error("schema reload failed", "err", err)
			}
		}
	}()
}
