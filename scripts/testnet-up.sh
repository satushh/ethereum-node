#!/usr/bin/env bash
# One command to follow a public network with the combined binary, with the
# Grafana dashboard alongside.
#
# Usage: scripts/testnet-up.sh [hoodi|sepolia|mainnet]   (default: hoodi)
#
# Expectations on hoodi: beacon at the network head within minutes
# (checkpoint sync); geth snap sync then needs hours and tens of GB.
# Laptop note: system sleep pauses the node — keep the machine awake
# (`caffeinate -is` in another terminal).
set -euo pipefail
cd "$(dirname "$0")/.."

NETWORK="${1:-hoodi}"
DATADIR="run/${NETWORK}-data"
mkdir -p run/logs

if [ ! -x bin/ethereum-node ]; then
    echo ">> building ethereum-node"
    go build -o bin/ethereum-node ./cmd/ethereum-node
fi

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
    ./bin/ethereum-node run --config "configs/${NETWORK}.yaml" \
        > "run/logs/${NETWORK}.log" 2>&1 &
else
    echo ">> starting ethereum-node on ${NETWORK} (no configs/${NETWORK}.yaml; using flags)"
    ./bin/ethereum-node run --network="${NETWORK}" --datadir="${DATADIR}" \
        > "run/logs/${NETWORK}.log" 2>&1 &
fi
echo "   pid $! (logs: run/logs/${NETWORK}.log)"

echo ">> watch it:"
echo "   grafana:  http://127.0.0.1:3001  (dashboards: ethereum-node, Beacon node)"
echo "   tail -f run/logs/${NETWORK}.log"
echo "   curl -s http://127.0.0.1:3500/eth/v1/node/syncing"
echo "   stop:     pkill -TERM -f 'ethereum-node run'"
