#!/usr/bin/env bash
# Boot a fresh single-node devnet with the combined ethereum-node binary plus
# a separate Prysm validator client (64 interop validators).
#
# Usage: scripts/devnet-up.sh [genesis-delay-seconds]
#
# macOS note: system sleep freezes the devnet (slots get skipped, finality
# stalls until ~2 full epochs after wake). Keep the machine awake while the
# devnet runs, e.g. `caffeinate -is` in another terminal.
set -euo pipefail
cd "$(dirname "$0")/.."

DELAY="${1:-45}"
FEE_RECIPIENT=0x878705ba3f8bc32fcf7f4caa1a35e72af65cf766

echo ">> cleaning previous devnet state"
rm -rf run/data run/genesis.json run/genesis.ssz
mkdir -p run/logs

if [ ! -x bin/ethereum-node ]; then
    echo ">> building ethereum-node"
    go build -o bin/ethereum-node ./cmd/ethereum-node
fi
if [ ! -x bin/validator ]; then
    echo ">> building prysm validator"
    if [ -d ../prysm ]; then
        (cd ../prysm && go build -o "$PWD/../ethereum-node/bin/validator" ./cmd/validator)
    else
        # no local clone: install the exact version go.mod pins
        PRYSM_VERSION=$(go list -m -f '{{.Version}}' github.com/OffchainLabs/prysm/v7)
        GOBIN="$PWD/bin" go install "github.com/OffchainLabs/prysm/v7/cmd/validator@${PRYSM_VERSION}"
    fi
fi
if [ ! -d run/wallet ]; then
    echo ">> creating devnet validator wallet"
    ./bin/ethereum-node devnet-wallet --wallet-dir=run/wallet --num-validators=64
fi

echo ">> generating genesis (delay ${DELAY}s)"
./bin/ethereum-node testnet generate-genesis \
    --fork=fulu \
    --num-validators=64 \
    --genesis-time-delay="${DELAY}" \
    --chain-config-file=devnet/config.yml \
    --geth-genesis-json-out=run/genesis.json \
    --output-ssz=run/genesis.ssz

echo ">> starting ethereum-node (geth + prysm beacon, one process)"
./bin/ethereum-node run \
    --datadir=run/data \
    --el-genesis=run/genesis.json \
    --cl-genesis-state=run/genesis.ssz \
    --cl-chain-config=devnet/config.yml \
    --beacon-flag no-discovery \
    --beacon-flag supernode \
    --beacon-flag min-sync-peers=0 \
    --beacon-flag minimum-peers-per-subnet=0 \
    --beacon-flag contract-deployment-block=0 \
    > run/logs/node.log 2>&1 &
echo "   pid $! (logs: run/logs/node.log)"

echo ">> starting prysm validator (separate process)"
./bin/validator \
    --accept-terms-of-use \
    --datadir=run/data/validator \
    --beacon-rpc-provider=127.0.0.1:4000 \
    --chain-config-file=devnet/config.yml \
    --wallet-dir=run/wallet \
    --wallet-password-file=run/wallet/password.txt \
    --suggested-fee-recipient="${FEE_RECIPIENT}" \
    --monitoring-host=127.0.0.1 \
    --monitoring-port=8081 \
    > run/logs/validator.log 2>&1 &
echo "   pid $! (logs: run/logs/validator.log)"

echo ">> devnet coming up; genesis in ${DELAY}s. Watch it with:"
echo "   tail -f run/logs/node.log"
echo "   curl -s -X POST -H 'Content-Type: application/json' \\"
echo "     -d '{\"jsonrpc\":\"2.0\",\"method\":\"eth_blockNumber\",\"params\":[],\"id\":1}' http://127.0.0.1:8545"
echo "   curl -s http://127.0.0.1:3500/eth/v1/beacon/headers/head"
