package main

import (
	"log/slog"

	"github.com/mariuz/firebirdrest/internal/server"
)

// reloadOnSignal does nothing on Windows, which has no SIGUSR1 or SIGHUP.
// Use POST_EVENT 'fbrest_reload' or POST /reload on the admin server to
// reload the schema cache; a renewed TLS certificate needs a restart.
func reloadOnSignal(*server.Server, *certStore, *slog.Logger) {}
