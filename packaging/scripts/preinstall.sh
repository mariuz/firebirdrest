#!/bin/sh
# Create the system user the service runs as.
set -e
if ! getent group firebirdrest >/dev/null; then
  groupadd --system firebirdrest
fi
if ! getent passwd firebirdrest >/dev/null; then
  useradd --system --gid firebirdrest --no-create-home --home-dir /nonexistent \
    --shell /usr/sbin/nologin --comment "FirebirdREST" firebirdrest
fi
