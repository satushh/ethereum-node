#!/bin/sh
# nfpm postinstall: create the service user. Never enables or starts the
# service; that is the operator's explicit step.
set -e

if ! getent passwd ethereum-node >/dev/null; then
    useradd --system --user-group --home-dir /var/lib/ethereum-node \
        --shell /usr/sbin/nologin ethereum-node
fi

# Refresh systemd only where one is actually running (not in containers).
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

echo "ethereum-node installed. Review /etc/ethereum-node/config.yaml, then:"
echo "  sudo systemctl enable --now ethereum-node"
