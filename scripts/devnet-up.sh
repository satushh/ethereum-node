#!/usr/bin/env bash
# Boot a fresh single-node devnet with the combined ethereum-node binary, a
# separate Prysm validator client (64 interop validators), and a transaction
# spammer so blocks are never empty (NO_SPAM=1 disables).
#
# Usage: scripts/devnet-up.sh [genesis-delay-seconds]
#        scripts/devnet-up.sh down     # stop node + validator + spammer + dashboards
#
# Env: NO_SPAM=1            skip the tx spammer
#      NO_OBSERVABILITY=1   skip prometheus/grafana
#      WAIT_FOR_BLOCKS=1    block until the chain actually produces (CI mode)
#
# Lifecycle: every launched process is tracked by pid file under run/, `down`
# signals exactly those pids (TERM, wait, KILL), and startup gates on real
# readiness — the script fails, with cleanup, rather than reporting success
# over dead processes.
#
# macOS note: system sleep freezes the devnet; keep the machine awake
# (`caffeinate -is`) while it runs.
set -euo pipefail
cd "$(dirname "$0")/.."

PIDFILES="run/node.pid run/validator.pid run/spam-launcher.pid"

alive() { kill -0 "$1" 2>/dev/null; }

# stop_pidfile <file> <name>: TERM, wait up to 10s, KILL, wait up to 3s.
# Returns nonzero only if the process survives SIGKILL.
stop_pidfile() {
    local file="$1" name="$2" pid
    [ -f "$file" ] || return 0
    pid=$(cat "$file")
    rm -f "$file"
    alive "$pid" || return 0
    kill -TERM "$pid" 2>/dev/null || true
    for _ in $(seq 1 10); do alive "$pid" || return 0; sleep 1; done
    echo ">> $name (pid $pid) ignored SIGTERM; escalating to SIGKILL"
    kill -KILL "$pid" 2>/dev/null || true
    for _ in $(seq 1 3); do alive "$pid" || return 0; sleep 1; done
    echo ">> ERROR: $name (pid $pid) survived SIGKILL" >&2
    return 1
}

devnet_down() {
    local fail=0
    stop_pidfile run/spam-launcher.pid "spam launcher" || fail=1
    pkill -TERM -f "$PWD/bin/devnet-spam" 2>/dev/null || true
    stop_pidfile run/node.pid "ethereum-node" || fail=1
    stop_pidfile run/validator.pid "validator" || fail=1
    # fallback for processes started before pid tracking — anchored to THIS
    # checkout's binaries so other projects' processes are never touched
    pkill -TERM -f "$PWD/bin/ethereum-node run" 2>/dev/null || true
    pkill -TERM -f "$PWD/bin/validator" 2>/dev/null || true
    for _ in $(seq 1 10); do
        pgrep -f "$PWD/bin/(ethereum-node run|validator|devnet-spam)" >/dev/null 2>&1 || break
        sleep 1
    done
    if [ "${NO_OBSERVABILITY:-0}" != "1" ] && docker info >/dev/null 2>&1; then
        "$(dirname "$0")/observability-up.sh" down
    fi
    if [ "$fail" -ne 0 ]; then
        echo ">> devnet stop INCOMPLETE — processes survived, datadir untouched" >&2
        return 1
    fi
    echo ">> devnet stopped"
}

if [ "${1:-}" = "down" ]; then
    devnet_down
    exit $?
fi

DELAY="${1:-45}"
FEE_RECIPIENT=0x878705ba3f8bc32fcf7f4caa1a35e72af65cf766
SPAM_ACCOUNTS=3
SPAM_RATE=30

# a running devnet must be stopped — verifiably — before its datadir is
# deleted underneath it
if pgrep -f "$PWD/bin/(ethereum-node run|validator)" >/dev/null 2>&1 || [ -f run/node.pid ]; then
    echo ">> devnet already running; stopping it first"
    NO_OBSERVABILITY=1 devnet_down || { echo ">> refusing to continue" >&2; exit 1; }
fi

echo ">> cleaning previous devnet state"
rm -rf run/data run/genesis.json run/genesis.ssz
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

# From here on, a failed start must not leave half a devnet running.
STARTED_OK=0
cleanup_on_failure() {
    if [ "$STARTED_OK" -ne 1 ]; then
        echo ">> startup failed; cleaning up started processes" >&2
        NO_OBSERVABILITY=1 devnet_down || true
    fi
}
trap cleanup_on_failure EXIT

echo ">> starting ethereum-node (geth + prysm beacon, one process; config: configs/devnet.yaml)"
./bin/ethereum-node run --config configs/devnet.yaml > run/logs/node.log 2>&1 &
echo $! > run/node.pid
echo "   pid $(cat run/node.pid) (logs: run/logs/node.log)"

# readiness: the execution RPC answering proves geth is up and the process
# alive; the beacon HTTP API answering proves the consensus module started
wait_http() { # url, name, pidfile, seconds
    local url="$1" name="$2" pidfile="$3" secs="$4"
    for _ in $(seq 1 "$secs"); do
        if ! alive "$(cat "$pidfile")"; then
            echo ">> ERROR: node process exited while waiting for $name; last log lines:" >&2
            tail -5 run/logs/node.log >&2 || true
            return 1
        fi
        curl -sf -o /dev/null --max-time 2 "$url" && return 0
        sleep 1
    done
    echo ">> ERROR: $name not ready after ${secs}s" >&2
    return 1
}
wait_http "http://127.0.0.1:8545" "execution RPC" run/node.pid 60
wait_http "http://127.0.0.1:3500/eth/v1/node/version" "beacon API" run/node.pid 90
echo ">> node ready (EL RPC + CL API answering)"

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
echo $! > run/validator.pid
echo "   pid $(cat run/validator.pid) (logs: run/logs/validator.log)"
sleep 3
if ! alive "$(cat run/validator.pid)"; then
    echo ">> ERROR: validator exited immediately; last log lines:" >&2
    tail -5 run/logs/validator.log >&2 || true
    exit 1
fi

if [ "${NO_SPAM:-0}" != "1" ]; then
    echo ">> starting tx spammer (${SPAM_RATE} tx/s from ${SPAM_ACCOUNTS} accounts; NO_SPAM=1 disables)"
    (sleep $((DELAY + 10)) && ./bin/devnet-spam --rate="${SPAM_RATE}" --num-accounts="${SPAM_ACCOUNTS}"; rm -f run/spam-launcher.pid) \
        > run/logs/spam.log 2>&1 &
    echo $! > run/spam-launcher.pid
    echo "   pid $(cat run/spam-launcher.pid) (logs: run/logs/spam.log)"
fi

if [ "${NO_OBSERVABILITY:-0}" != "1" ]; then
    if docker info >/dev/null 2>&1; then
        echo ">> starting prometheus + grafana"
        "$(dirname "$0")/observability-up.sh"
    else
        echo ">> docker not available; skipping prometheus/grafana (NO_OBSERVABILITY=1 silences this)"
    fi
fi

if [ "${WAIT_FOR_BLOCKS:-0}" = "1" ]; then
    echo ">> waiting for the chain to produce (genesis in ${DELAY}s)"
    DEADLINE=$((DELAY + 120))
    for _ in $(seq 1 "$DEADLINE"); do
        alive "$(cat run/node.pid)" || { echo ">> ERROR: node died while waiting for blocks" >&2; exit 1; }
        BLOCK=$(curl -s --max-time 2 -X POST -H 'Content-Type: application/json' \
            -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
            http://127.0.0.1:8545 | python3 -c 'import json,sys; print(int(json.load(sys.stdin)["result"],16))' 2>/dev/null || echo 0)
        [ "$BLOCK" -gt 0 ] && { echo ">> chain producing: block $BLOCK"; break; }
        sleep 1
    done
    [ "${BLOCK:-0}" -gt 0 ] || { echo ">> ERROR: no blocks after ${DEADLINE}s" >&2; exit 1; }
fi

STARTED_OK=1
echo ">> devnet up; genesis in ${DELAY}s. Watch it with:"
echo "   grafana:  http://127.0.0.1:3001  (dashboards: ethereum-node, Beacon node, Geth node)"
echo "   tail -f run/logs/node.log"
echo "   curl -s -X POST -H 'Content-Type: application/json' \\"
echo "     -d '{\"jsonrpc\":\"2.0\",\"method\":\"eth_blockNumber\",\"params\":[],\"id\":1}' http://127.0.0.1:8545"
echo "   stop:     scripts/devnet-up.sh down"
