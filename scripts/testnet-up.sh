#!/usr/bin/env bash
# One command to follow a public network with the combined binary, with the
# Grafana dashboard alongside.
#
# Usage: scripts/testnet-up.sh [hoodi|sepolia|mainnet]   (default: hoodi)
#        scripts/testnet-up.sh down                      # stop the node
#
# Expectations on hoodi: beacon at the network head within minutes
# (checkpoint sync); geth snap sync then needs hours and ~100 GB.
# Laptop note: system sleep pauses the node — keep the machine awake
# (`caffeinate -is` in another terminal).
set -euo pipefail
cd "$(dirname "$0")/.."

PIDFILE="run/testnet-node.pid"

alive() { kill -0 "$1" 2>/dev/null; }
ours() { ps -p "$1" -o args= 2>/dev/null | grep -qF "$PWD/bin/"; }

if [ "${1:-}" = "down" ]; then
    if [ -f "$PIDFILE" ]; then
        pid=$(cat "$PIDFILE")
        if printf '%s' "$pid" | grep -qE '^[1-9][0-9]*$' && alive "$pid" && ours "$pid"; then
            kill -TERM "$pid" 2>/dev/null || true
            for _ in $(seq 1 15); do alive "$pid" || break; sleep 1; done
            alive "$pid" && { kill -KILL "$pid" 2>/dev/null || true; sleep 2; }
        fi
        alive "${pid:-0}" 2>/dev/null && { echo ">> ERROR: node survived SIGKILL" >&2; exit 1; }
        rm -f "$PIDFILE"
    fi
    if [ "${NO_OBSERVABILITY:-0}" != "1" ] && docker info >/dev/null 2>&1; then
        "$(dirname "$0")/observability-up.sh" down
    fi
    echo ">> testnet node stopped"
    exit 0
fi

NETWORK="${1:-hoodi}"
DATADIR="run/${NETWORK}-data"
mkdir -p run/logs

if [ -f "$PIDFILE" ] && alive "$(cat "$PIDFILE")" 2>/dev/null; then
    echo ">> a testnet node is already running (pid $(cat "$PIDFILE")); stop it first: scripts/testnet-up.sh down" >&2
    exit 1
fi

echo ">> building ethereum-node (cached)"
go build -o bin/ethereum-node ./cmd/ethereum-node

if [ "${NO_OBSERVABILITY:-0}" != "1" ]; then
    if docker info >/dev/null 2>&1; then
        echo ">> starting prometheus + grafana"
        scripts/observability-up.sh
    else
        echo ">> docker not available; skipping prometheus/grafana (NO_OBSERVABILITY=1 silences this)"
    fi
fi

if [ -f "configs/${NETWORK}.yaml" ]; then
    echo ">> starting ethereum-node on ${NETWORK} (config: configs/${NETWORK}.yaml)"
    "$PWD/bin/ethereum-node" run --config "configs/${NETWORK}.yaml" \
        > "run/logs/${NETWORK}.log" 2>&1 &
else
    echo ">> starting ethereum-node on ${NETWORK} (no configs/${NETWORK}.yaml; using flags)"
    "$PWD/bin/ethereum-node" run --network="${NETWORK}" --datadir="${DATADIR}" \
        > "run/logs/${NETWORK}.log" 2>&1 &
fi
echo $! > "$PIDFILE"
echo "   pid $(cat "$PIDFILE") (logs: run/logs/${NETWORK}.log)"

echo ">> watch it:"
echo "   grafana:  http://127.0.0.1:3001  (dashboards: ethereum-node, Beacon node, Geth node)"
echo "   tail -f run/logs/${NETWORK}.log"
echo "   curl -s http://127.0.0.1:3500/eth/v1/node/syncing"
echo "   stop:     scripts/testnet-up.sh down"
