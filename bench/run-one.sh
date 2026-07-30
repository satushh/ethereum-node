#!/usr/bin/env bash
# Run one benchmark leg: fresh 6s-slot devnet in the given isolation mode,
# with tx spam, for N slots. Results land in bench/out/<mode>/.
#
# Usage: bench/run-one.sh <single|process> [slots=200] [txrate=50]
set -euo pipefail
cd "$(dirname "$0")/.."

MODE="${1:?usage: run-one.sh <single|process> [slots] [txrate]}"
SLOTS="${2:-200}"
TXRATE="${3:-50}"
OUT="bench/out/${MODE}"
FUNDED=0x123463a4B065722E99115D6c222f267d9cABb524

mkdir -p "$OUT" run/logs
rm -rf run/data run/genesis.json run/genesis.ssz

./bin/ethereum-node testnet generate-genesis --fork=fulu --num-validators=64 \
    --genesis-time-delay=30 --chain-config-file=bench/config-6s.yml \
    --geth-genesis-json-out=run/genesis.json --output-ssz=run/genesis.ssz >/dev/null 2>&1

# prefund the spammer account
python3 - <<EOF
import json
g = json.load(open("run/genesis.json"))
g["alloc"]["$FUNDED"] = {"balance": "0xd3c21bcecceda1000000"}
json.dump(g, open("run/genesis.json", "w"), indent=1)
EOF

./bin/ethereum-node run \
    --datadir=run/data \
    --el-genesis=run/genesis.json \
    --cl-genesis-state=run/genesis.ssz \
    --cl-chain-config=bench/config-6s.yml \
    --isolation="$MODE" \
    --beacon-flag no-discovery --beacon-flag supernode \
    --beacon-flag min-sync-peers=0 --beacon-flag minimum-peers-per-subnet=0 \
    --beacon-flag contract-deployment-block=0 \
    > "$OUT/node.log" 2>&1 &
NODE_PID=$!

./bin/validator --accept-terms-of-use --datadir=run/data/validator \
    --beacon-rpc-provider=127.0.0.1:4000 --chain-config-file=bench/config-6s.yml \
    --wallet-dir=run/wallet --wallet-password-file=run/wallet/password.txt \
    --suggested-fee-recipient="$FUNDED" --monitoring-port=8081 \
    > "$OUT/validator.log" 2>&1 &
VAL_PID=$!

sleep 35  # genesis delay
./bin/devnet-spam --rate="$TXRATE" > "$OUT/spam.log" 2>&1 &
SPAM_PID=$!

# RSS sampler: sum across every process of the node (1 in single mode,
# supervisor+2 children in process mode); validator/spammer excluded.
(
  while :; do
    ps ax -o rss=,args= | awk '/ethereum-node (run|el-child|beacon)/ && !/awk/ {s+=$1} END {print int(s)}'
    sleep 5
  done > "$OUT/rss.samples"
) &
RSS_PID=$!

DURATION=$((SLOTS * 6 + 40))
echo ">> bench[$MODE]: ${SLOTS} slots (~$((DURATION / 60)) min), spam ${TXRATE} tx/s"
caffeinate -is sleep "$DURATION"

kill "$SPAM_PID" "$RSS_PID" 2>/dev/null || true
kill -TERM "$NODE_PID" 2>/dev/null || true
kill -TERM "$VAL_PID" 2>/dev/null || true
wait "$NODE_PID" 2>/dev/null || true
wait "$VAL_PID" 2>/dev/null || true

python3 bench/collect.py "$OUT/node.log" > "$OUT/latency.json"
python3 - <<EOF > "$OUT/rss.json"
import json, statistics
vals = [int(x) for x in open("$OUT/rss.samples") if x.strip() and int(x) > 0]
vals = vals[3:]  # settle
print(json.dumps({"rss_mib_mean": round(statistics.mean(vals)/1024), "rss_mib_max": round(max(vals)/1024), "samples": len(vals)}) if vals else "{}")
EOF
echo ">> bench[$MODE] done:"
cat "$OUT/latency.json" "$OUT/rss.json"
