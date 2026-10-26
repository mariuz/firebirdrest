#!/bin/sh
# The configuration holds the database password: readable by the service only.
set -e
chown root:firebirdrest /etc/firebirdrest /etc/firebirdrest/firebirdrest.conf
chmod 0750 /etc/firebirdrest
chmod 0640 /etc/firebirdrest/firebirdrest.conf
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || true
  # an upgrade restarts a running service; a first install leaves it off
  # until the configuration is filled in
  if systemctl is-active --quiet firebirdrest; then
    systemctl try-restart firebirdrest >/dev/null 2>&1 || true
  else
    echo "FirebirdREST installed. Edit /etc/firebirdrest/firebirdrest.conf, then: systemctl enable --now firebirdrest"
  fi
fi
