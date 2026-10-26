#!/bin/sh
# Stop the service when the package is removed, not when it is upgraded.
# (deb passes "remove", rpm passes 0 packages left)
set -e
case "$1" in
  remove|0)
    if [ -d /run/systemd/system ]; then
      systemctl disable --now firebirdrest >/dev/null 2>&1 || true
    fi
    ;;
esac
