#!/usr/bin/env bash
# Start (or stop) the local Prometheus + Grafana stack for ethereum-node.
#
# Usage: scripts/observability-up.sh [down]
set -euo pipefail
cd "$(dirname "$0")/../observability"

if [ "${1:-}" = "down" ]; then
    docker compose down
    exit 0
fi

docker compose up -d
echo
echo ">> grafana:    http://127.0.0.1:3001  (no login, dashboard: 'ethereum-node')"
echo ">> prometheus: http://127.0.0.1:9090"
echo ">> scraping:   geth 127.0.0.1:6060, beacon :8080, validator :8081"
echo ">> stop with:  scripts/observability-up.sh down"
