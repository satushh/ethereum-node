#!/usr/bin/env bash
# Boot a fresh single-node devnet with the combined ethereum-node binary, a
# separate Prysm validator client (64 interop validators), and a transaction
# spammer so blocks are never empty (NO_SPAM=1 disables).
#
# Usage: scripts/devnet-up.sh [genesis-delay-seconds]
#        scripts/devnet-up.sh down     # stop node + validator + spammer
#
# macOS note: system sleep freezes the devnet (slots get skipped, finality
# stalls until ~2 full epochs after wake). Keep the machine awake while the
# devnet runs, e.g. `caffeinate -is` in another terminal.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ "${1:-}" = "down" ]; then
    # the delayed spam launcher (sleep && devnet-spam) doesn't match the
    # devnet-spam pattern while still sleeping — kill it by recorded pid
    if [ -f run/spam-launcher.pid ]; then
        kill "$(cat run/spam-launcher.pid)" 2>/dev/null || true
        rm -f run/spam-launcher.pid
    fi
    pkill -TERM -f "ethereum-node run" 2>/dev/null || true
    pkill -TERM -f "bin/validator" 2>/dev/null || true
    pkill -TERM -f "bin/devnet-spam" 2>/dev/null || true
    for _i in 1 2 3 4 5 6 7 8 9 10; do
        pgrep -f "ethereum-node run|bin/validator|bin/devnet-spam" >/dev/null 2>&1 || break
        sleep 1
    done
    if [ "${NO_OBSERVABILITY:-0}" != "1" ] && docker info >/dev/null 2>&1; then
        "$(dirname "$0")/observability-up.sh" down
    fi
    echo ">> devnet stopped"
    exit 0
fi

DELAY="${1:-45}"
FEE_RECIPIENT=0x878705ba3f8bc32fcf7f4caa1a35e72af65cf766
SPAM_ACCOUNTS=3
SPAM_RATE=30

# a running devnet must be stopped before its datadir is deleted underneath it
if pgrep -f "ethereum-node run" >/dev/null 2>&1 || pgrep -f "bin/validator" >/dev/null 2>&1; then
    echo ">> devnet already running; stopping it first"
    NO_OBSERVABILITY=1 "$0" down
    sleep 2
fi

echo ">> cleaning previous devnet state"
rm -rf run/data run/genesis.json run/genesis.ssz run/spam-launcher.pid
mkdir -p run/logs

# unconditional builds — Go's cache makes these near-instant, and stale
# binaries silently running old code is worse than a moment of compiling
echo ">> building binaries (cached)"
go build -o bin/ethereum-node ./cmd/ethereum-node
go build -o bin/devnet-spam ./cmd/devnet-spam
# built from this module's context: exact pinned prysm + mirrored replaces
# (`go install pkg@version` rejects modules with replace directives)
go build -o bin/validator github.com/OffchainLabs/prysm/v7/cmd/validator

if [ ! -d run/wallet ]; then
    echo ">> creating devnet validator wallet"
    ./bin/ethereum-node devnet-wallet --wallet-dir=run/wallet --num-validators=64
fi

# Two-pass genesis: the spam accounts must be funded BEFORE the consensus
# genesis state is computed — the state embeds the execution genesis hash,
# so mutating genesis.json afterwards desyncs the two halves.
echo ">> generating genesis (delay ${DELAY}s, ${SPAM_ACCOUNTS} funded spam accounts)"
./bin/ethereum-node testnet generate-genesis \
    --fork=fulu --num-validators=64 \
    --chain-config-file=devnet/chain-config.yml \
    --geth-genesis-json-out=run/genesis-template.json \
    --output-ssz=run/genesis-discard.ssz >/dev/null 2>&1
SPAM_ADDRS=$(./bin/devnet-spam --print-addrs --num-accounts "${SPAM_ACCOUNTS}") python3 - <<'EOF'
import json, os
g = json.load(open("run/genesis-template.json"))
for addr in os.environ["SPAM_ADDRS"].split():
    g.setdefault("alloc", {})[addr] = {"balance": "0xd3c21bcecceda1000000"}
g["baseFeePerGas"] = "0x3b9aca00"
json.dump(g, open("run/genesis-template.json", "w"), indent=1)
EOF
./bin/ethereum-node testnet generate-genesis \
    --fork=fulu --num-validators=64 \
    --genesis-time-delay="${DELAY}" \
    --chain-config-file=devnet/chain-config.yml \
    --geth-genesis-json-in=run/genesis-template.json \
    --geth-genesis-json-out=run/genesis.json \
    --output-ssz=run/genesis.ssz
rm -f run/genesis-template.json run/genesis-discard.ssz

echo ">> starting ethereum-node (geth + prysm beacon, one process; config: configs/devnet.yaml)"
./bin/ethereum-node run --config configs/devnet.yaml > run/logs/node.log 2>&1 &
echo "   pid $! (logs: run/logs/node.log)"

echo ">> starting prysm validator (separate process)"
./bin/validator \
    --accept-terms-of-use \
    --datadir=run/data/validator \
    --beacon-rpc-provider=127.0.0.1:4000 \
    --chain-config-file=devnet/chain-config.yml \
    --wallet-dir=run/wallet \
    --wallet-password-file=run/wallet/password.txt \
    --suggested-fee-recipient="${FEE_RECIPIENT}" \
    --monitoring-host=127.0.0.1 \
    --monitoring-port=8081 \
    > run/logs/validator.log 2>&1 &
echo "   pid $! (logs: run/logs/validator.log)"

if [ "${NO_SPAM:-0}" != "1" ]; then
    echo ">> starting tx spammer (${SPAM_RATE} tx/s from ${SPAM_ACCOUNTS} accounts; NO_SPAM=1 disables)"
    (sleep $((DELAY + 10)) && ./bin/devnet-spam --rate="${SPAM_RATE}" --num-accounts="${SPAM_ACCOUNTS}"; rm -f run/spam-launcher.pid) \
        > run/logs/spam.log 2>&1 &
    echo $! > run/spam-launcher.pid
    echo "   pid $! (logs: run/logs/spam.log)"
fi

if [ "${NO_OBSERVABILITY:-0}" != "1" ]; then
    if docker info >/dev/null 2>&1; then
        echo ">> starting prometheus + grafana"
        "$(dirname "$0")/observability-up.sh"
    else
        echo ">> docker not available; skipping prometheus/grafana (NO_OBSERVABILITY=1 silences this)"
    fi
fi

echo ">> devnet coming up; genesis in ${DELAY}s. Watch it with:"
echo "   grafana:  http://127.0.0.1:3001  (dashboards: ethereum-node, Beacon node)"
echo "   tail -f run/logs/node.log"
echo "   curl -s -X POST -H 'Content-Type: application/json' \\"
echo "     -d '{\"jsonrpc\":\"2.0\",\"method\":\"eth_blockNumber\",\"params\":[],\"id\":1}' http://127.0.0.1:8545"
echo "   curl -s http://127.0.0.1:3500/eth/v1/beacon/headers/head"
