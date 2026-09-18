#!/bin/sh
# nfpm preremove: stop the service if a running systemd manages it.
set -e

if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now ethereum-node 2>/dev/null || true
fi
