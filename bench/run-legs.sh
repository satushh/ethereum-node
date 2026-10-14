#!/usr/bin/env bash
# A/B/C legs for the typed engine transport measurement.
# Per leg: fresh devnet, warm up to slot WARMUP, snapshot beacon metrics,
# run MEASURE slots, snapshot again, sample RSS/CPU every 15s, tear down.
# Usage: run-legs.sh <outdir> [legs...]   (default legs: ipc inproc typed)
set -euo pipefail
REPO="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:?outdir}"; shift || true
LEGS=("${@:-ipc inproc typed}")
[ $# -gt 0 ] || LEGS=(ipc inproc typed)
WARMUP=10
MEASURE=90
mkdir -p "$OUT"

slot() { curl -s --max-time 2 http://127.0.0.1:3500/eth/v1/beacon/headers/head \
  | python3 -c 'import json,sys; print(int(json.load(sys.stdin)["data"]["header"]["message"]["slot"]))' 2>/dev/null || echo -1; }

wait_slot() { # target
  while true; do
    S=$(slot); [ "$S" -ge "$1" ] && { echo "$S"; return; }
    sleep 3
  done
}

for LEG in "${LEGS[@]}"; do
  echo "=== leg $LEG: starting devnet"
  cd "$REPO"
  ENGINE_TRANSPORT="$LEG" NO_OBSERVABILITY=1 WAIT_FOR_BLOCKS=1 scripts/devnet-up.sh 20 >"$OUT/$LEG.boot.log" 2>&1
  NODE_PID=$(cat run/node.pid)
  echo "=== leg $LEG: warmup to slot $WARMUP"
  wait_slot "$WARMUP" >/dev/null
  date +%s > "$OUT/$LEG.t0.ts"; slot > "$OUT/$LEG.t0.slot"
  curl -s http://127.0.0.1:8080/metrics > "$OUT/$LEG.t0.metrics"
  : > "$OUT/$LEG.ps.samples"
  ( while kill -0 "$NODE_PID" 2>/dev/null; do
      ps -o rss=,pcpu= -p "$NODE_PID" >> "$OUT/$LEG.ps.samples" 2>/dev/null || break
      sleep 15
    done ) &
  SAMPLER=$!
  TARGET=$(( $(cat "$OUT/$LEG.t0.slot") + MEASURE ))
  echo "=== leg $LEG: measuring to slot $TARGET"
  wait_slot "$TARGET" >/dev/null
  date +%s > "$OUT/$LEG.t1.ts"; slot > "$OUT/$LEG.t1.slot"
  curl -s http://127.0.0.1:8080/metrics > "$OUT/$LEG.t1.metrics"
  curl -s http://127.0.0.1:3500/eth/v1/beacon/states/head/finality_checkpoints > "$OUT/$LEG.finality.json"
  curl -s -X POST -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest", false],"id":1}' \
    http://127.0.0.1:8545 > "$OUT/$LEG.latestblock.json"
  kill "$SAMPLER" 2>/dev/null || true
  grep -c "ERROR" run/logs/node.log > "$OUT/$LEG.errcount" || true
  echo "=== leg $LEG: teardown"
  scripts/devnet-up.sh down >>"$OUT/$LEG.boot.log" 2>&1
done
echo "ALL LEGS DONE"
