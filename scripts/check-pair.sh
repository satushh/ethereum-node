#!/usr/bin/env bash
# Mechanically enforce the (geth, prysm) pair invariants that the version
# bump checklist (README) states in prose:
#
#   1. go.mod's go-ethereum require is EXACTLY what the pinned prysm's
#      go.mod requires (one binary links one go-ethereum, and it must be
#      the one prysm's imports were compiled against).
#   2. Every replace directive in prysm's go.mod is mirrored in ours
#      (Go ignores replaces declared in dependencies), except replaces
#      pointing into prysm's own tree (./third_party/...), which cannot
#      and must not be mirrored.
#   3. The go directive matches prysm's.
#   4. versions.lock's top entry records the pair go.mod actually pins.
#
# CI runs this on every push/PR; run it manually after any bump. Exits
# non-zero with a FAIL line per violation.
set -euo pipefail
cd "$(dirname "$0")/.."

# Check the pinned modules, not a local go.work overlay of client clones.
export GOWORK=off

prysm=$(go list -m -f '{{.Version}}' github.com/OffchainLabs/prysm/v7)
geth=$(go list -m -f '{{.Version}}' github.com/ethereum/go-ethereum)
echo ">> pinned pair: prysm ${prysm}, geth ${geth}"

# The pinned prysm's own go.mod, fetched from the module proxy.
pmod=$(go mod download -json "github.com/OffchainLabs/prysm/v7@${prysm}" \
  | sed -n 's/.*"GoMod": *"\(.*\)".*/\1/p')
[ -n "$pmod" ] && [ -f "$pmod" ] || { echo "FAIL: could not fetch prysm's go.mod"; exit 1; }

lock_prysm=$(sed -n 's/^- prysm: *\([^ ]*\).*/\1/p' versions.lock | head -1)
lock_geth=$(awk '$1=="geth:"{print $2; exit}' versions.lock)

OURS_JSON="$(go mod edit -json)" \
THEIRS_JSON="$(go mod edit -json "$pmod")" \
PRYSM="$prysm" GETH="$geth" LOCK_PRYSM="$lock_prysm" LOCK_GETH="$lock_geth" \
python3 - <<'PY'
import json, os, sys

ours = json.loads(os.environ["OURS_JSON"])
theirs = json.loads(os.environ["THEIRS_JSON"])
fails = []

def require(mod, path):
    for r in mod.get("Require") or []:
        if r["Path"] == path:
            return r["Version"]
    return None

# 1. geth version = prysm's requirement
want_geth = require(theirs, "github.com/ethereum/go-ethereum")
if os.environ["GETH"] != want_geth:
    fails.append(f"go-ethereum {os.environ['GETH']} but prysm {os.environ['PRYSM']} requires {want_geth}")

# 2. prysm's replaces mirrored (except prysm-internal ./ paths)
def key(r): return (r["Old"]["Path"], r["Old"].get("Version", ""))
def val(r): return (r["New"]["Path"], r["New"].get("Version", ""))
mirrored = {key(r): val(r) for r in ours.get("Replace") or []}
for r in theirs.get("Replace") or []:
    if r["New"]["Path"].startswith("./") or r["New"]["Path"].startswith("../"):
        continue  # vendored inside prysm's tree; deliberately not mirrored
    if mirrored.get(key(r)) != val(r):
        fails.append(f"replace not mirrored: {r['Old']['Path']} => {r['New']['Path']} {r['New'].get('Version','')}")

# 3. go directive aligned
if ours.get("Go") != theirs.get("Go"):
    fails.append(f"go directive {ours.get('Go')} but prysm's is {theirs.get('Go')}")

# 4. versions.lock top entry = go.mod pins
if os.environ["LOCK_PRYSM"] != os.environ["PRYSM"]:
    fails.append(f"versions.lock top prysm {os.environ['LOCK_PRYSM']} != go.mod {os.environ['PRYSM']}")
if os.environ["LOCK_GETH"] != os.environ["GETH"]:
    fails.append(f"versions.lock top geth {os.environ['LOCK_GETH']} != go.mod {os.environ['GETH']}")

if fails:
    for f in fails:
        print(f"FAIL: {f}")
    sys.exit(1)
print(">> pair check OK: geth matches prysm's requirement, replaces mirrored, go directive aligned, versions.lock current")
PY
