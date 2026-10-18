package main

import (
	"log/slog"

	"github.com/mariuz/firebirdrest/internal/server"
)

// reloadOnSignal does nothing on Windows, which has no SIGUSR1. Use
// POST_EVENT 'fbrest_reload' or POST /reload on the admin server instead.
func reloadOnSignal(*server.Server, *slog.Logger) {}
