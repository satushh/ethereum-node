# ethereum-node — Geth + Prysm in one binary, one process

A composition experiment: link the unmodified sources of
[go-ethereum](https://github.com/ethereum/go-ethereum) (execution) and
[Prysm](https://github.com/OffchainLabs/prysm) (consensus) into one native
executable running both as modules of one process. Prysm's Bazel removal made
this possible: both clients now build with the ordinary Go toolchain, so a
small integration module composes them like any two Go libraries.

## Quick start

Needs git, Go (the right toolchain auto-downloads), python3 (devnet genesis
step), and optionally Docker for Grafana. First build downloads both clients'
module graphs.

```sh
git clone https://github.com/satushh/ethereum-node.git
cd ethereum-node
go build -o bin/ethereum-node ./cmd/ethereum-node

scripts/devnet-up.sh               # devnet: node + validator + tx spam + grafana
tail -f run/logs/node.log          # geth + prysm logs, one process, one stream
open http://127.0.0.1:3001         # dashboards (needs docker; skipped if absent)

# watch it produce and finalize blocks
curl -s localhost:8545 -X POST -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'
curl -s localhost:3500/eth/v1/beacon/states/head/finality_checkpoints
```

First blocks in ~1 minute, carrying real transactions (a small spammer runs
from genesis-funded dev accounts; `NO_SPAM=1` disables); finalization at ~13
minutes. Stop everything with `scripts/devnet-up.sh down`. Laptop note:
system sleep freezes the devnet — keep the machine awake (`caffeinate -is`).

Or join a public testnet (checkpoint sync puts the beacon at head in minutes;
geth's snap sync then needs hours and ~100 GB):

```sh
scripts/testnet-up.sh hoodi        # equivalently:
./bin/ethereum-node run --config configs/hoodi.yaml
```

## Architecture

```
+--------------------------------------------------------------+
|            ethereum-node (one process, one binary)           |
|                                                              |
|  cmd/ethereum-node/main.go — config, lifecycle, shutdown     |
|                                                              |
|  +--------------------+     Engine API      +--------------+ |
|  | internal/gethapp   |<------------------->| internal/    | |
|  | geth node stack    |  private IPC socket | prysmapp     | |
|  |                    |  in the datadir     | beacon node  | |
|  | execution/ datadir |  (no JWT, no ports) | beacon/      | |
|  | devp2p             |                     | libp2p/QUIC  | |
|  +--------------------+                     +--------------+ |
+--------------------------------------------------------------+

        prysm validator client remains a separate process
```

- **Geth and Prysm stay independent projects.** Both are ordinary pinned Go
  module versions from the module proxy; local clones are only an optional
  gitignored `go.work` overlay for hacking on either client. Nothing upstream
  is modified.
- **Version alignment.** One Go binary links exactly one go-ethereum, so it
  must be the version Prysm's go.mod requires (v1.17.4 today). Prysm can be
  any version; geth follows it.
- **The Engine API boundary is preserved** — same serialized JSON, on geth's
  IPC socket inside the datadir (no JWT on IPC), zero operator wiring. The
  authenticated HTTP engine endpoint stays on 127.0.0.1:8551 as an escape
  hatch for attaching an external CL.
- **Databases and P2P stacks remain separate** (`execution/` and `beacon/`
  under one datadir; devp2p and libp2p side by side) — that duplication is
  protocol design, not waste. The honest cost of one process is crash
  isolation: a panic in either module takes down both (`--isolation=process`,
  implemented on the `isolation-ab` branch, restores separate failure domains
  from the same binary).

**How it was built.** Geth's CLI is a thin flag-parser over public
constructors, so `internal/gethapp/` fills `node.Config`/`ethconfig.Config`
structs and calls `node.New` → `utils.RegisterEthService` →
`catalyst.Register` → `stack.Start()` — geth as a library. Prysm's
constructor reads options through a `cli.Context` across many packages, so it
can't be struct-wired: `internal/prysmapp/` is upstream's
`cmd/beacon-chain/main.go` copied near-verbatim into an importable package
(deviations: `Run(ctx, argv)` entrypoint, errors returned instead of
`log.Fatal`, a `KnownFlags` helper for config validation, log prefix, and
upstream's help template + panic re-log wrapper omitted), and the supervisor
synthesizes its argv — prysm as its own CLI, in-process. Combining is then just call order:

```
1. start geth            → it creates <datadir>/execution/geth.ipc
2. run prysm in-process  → --execution-endpoint=<that path>
3. prysm blocks until SIGINT/SIGTERM (its own handler — the shutdown driver)
4. when it returns, close the geth stack
```

The two halves must agree on genesis (the EL genesis hash is embedded in the
CL genesis state; chain ID checked via `eth_chainId` on connect) —
`ethereum-node testnet generate-genesis` (prysmctl's generator, mounted
unchanged) emits a matching pair by construction.

The entire integration (~1,850 lines; ~1,150 hand-written once the copied
file and the mostly-generated go.mod are excluded):

| Piece | Lines | Nature |
|---|---|---|
| `internal/prysmapp/prysmapp.go` | ~410 | near-verbatim copy of prysm's `main.go` (upstream is `package main`) |
| `internal/gethapp/gethapp.go` | ~290 | thin wrapper over geth's public embedding API |
| `cmd/ethereum-node/` | ~540 | supervisor CLI, config file, devnet wallet |
| `cmd/devnet-spam/` + `devnet/` + `scripts/` | ~340 | devnet fixtures and tooling |
| `go.mod` | ~270 | pinned versions + 1 replace mirrored from prysm's go.mod |

The upstream changes that would help most are small and listed in
[Upstream PRs to propose](#upstream-prs-to-propose) — chief among them Prysm
exporting its app wiring as an importable package, which deletes the copied
file and makes this pure composition.

## How a slot flows through the process

Only the validator's gRPC and libp2p gossip cross a process boundary. Each
step names the log line it produces — watch the loop in `run/logs/node.log`.

```
 prysm validator ......... separate process, gRPC to 127.0.0.1:4000
      │
      ▼
┌── prysmapp module (consensus) ─────┐        ┌── gethapp module (execution) ──────┐
│                                    │        │                                    │
│ 1 proposer duty at slot start      │ engine │                                    │
│   "Building block"                 │  API   │                                    │
│   fetch payload ───────────────────┼─(IPC)──┼─▶ 2 engine_getPayloadV5            │
│                                    │        │     hand over payload built since  │
│ 3 wrap payload in beacon block,    │        │     the previous slot              │
│   sign, gossip via libp2p          │        │     "Stopping work on payload"     │
│                                    │        │                                    │
│ 4 import the block:                │        │                                    │
│   state transition + DA checks     │        │                                    │
│   "Synced new block"               │        │                                    │
│   verify payload ──────────────────┼─(IPC)──┼─▶ 5 engine_newPayloadV4            │
│                                    │        │     execute txs, verify state root │
│                                    │        │     "Imported new potential        │
│                                    │        │      chain segment"                │
│ 6 fork choice picks new head       │        │                                    │
│   "Forkchoice updated with         │        │                                    │
│    payload attributes"─────────────┼─(IPC)──┼─▶ 7 engine_forkchoiceUpdatedV3     │
│                                    │        │     set canonical head; start      │
│                                    │        │     building next slot's payload   │
│ 8 attestations arrive (gRPC +      │        │     "Chain head was updated"       │
│   gossip), pooled for next block   │        │     "Starting work on payload"     │
│                                    │        │                                    │
│ beacon DB (bolt)   libp2p/QUIC     │        │ chain DB (pebble)   devp2p         │
└────────────────────────────────────┘        └────────────────────────────────────┘
         datadir/beacon/                                datadir/execution/
                     └────────── one datadir, one process ──────────┘
```

## What one process unlocks

Today this is functionally the two clients glued together — same JSON engine
bytes, deliberately, so upstream behavior and test coverage carry over. What
changes immediately is everything around them: one artifact, one command, no
JWT/endpoint setup, impossible version mismatches, coordinated
startup/shutdown, one correlated log stream. The shared address space is what
matters next:

```
 today   Prysm ──JSON──▶ unix socket ──▶ Geth      same bytes as two processes,
         engine API over in-datadir IPC            zero operator config, no JWT

 next    Prysm ──JSON──▶ rpc.DialInProc ──▶ Geth   no socket, no syscalls, no
         (go-ethereum/rpc/inproc.go:25, already    connection management; still
         used by geth's blsync mode)               JSON in memory

 later   Prysm ──typed Go calls──▶ Geth            no serialization at all:
         implement EngineCaller                    ExecutableData/blobs passed
         (prysm/beacon-chain/execution/            by reference, validated
         engine_client.go:53) directly on          against the JSON path by
         catalyst.ConsensusAPI                     running both in tests
         (go-ethereum/eth/catalyst/api.go:90)
```

| Redundancy | Two processes | This binary today | Possible in one process |
|---|---|---|---|
| Engine transport | JSON + socket + JWT per call | JSON + socket (no JWT) | typed calls, zero serialization |
| Blob delivery (`engine_getBlobsV2/V3`) | multi-MB hex-JSON round trips | same | blob refs handed over in memory |
| EL liveness + eth1 data | polling over RPC | same | subscribe to geth's chain event feed |
| Payload storage | every payload stored twice (geth chain DB + inside beacon blocks in prysm's DB) | same | store bodies once, CL references EL |
| Observability | two metrics/pprof/trace stacks | one process, still two stacks | one registry; EL spans inside CL traces |
| Sync coordination | "is the EL synced" timeout heuristics | same | direct state queries, shared backpressure |
| Lifecycle / setup | systemd ordering, JWT, endpoint URLs | one command, ordered start/stop | — (done) |

Caution against over-promising: engine serialization is small next to EVM
execution, state I/O and BLS, so the typed transport is a latency/allocation
win, not a throughput revolution. The genuinely large candidates are the blob
path and payload-storage dedup — measure before claiming either. Sharing the
networking substrate (QUIC, bandwidth, discovery — the ethp2p direction) is
explicitly out of scope here.

## Build and hack

Both clients resolve from the Go module proxy — a single clone builds
(Go ≥ 1.26.5, auto-downloaded). To hack on either client, don't touch
go.mod — add a gitignored workspace overlay:

```sh
git clone https://github.com/OffchainLabs/prysm.git ../prysm
git clone --branch v1.17.4 https://github.com/ethereum/go-ethereum.git ../go-ethereum
go work init . && go work use ../prysm ../go-ethereum
```

```
ethereum-node/
  cmd/ethereum-node/       the combined binary (run | beacon | devnet-wallet | testnet | version)
  internal/gethapp/        geth embedded as a library
  internal/prysmapp/       prysm's CLI made importable
  configs/                 node config files (devnet.yaml, hoodi.yaml)
  devnet/chain-config.yml  devnet beacon chain parameters (forks, slot time)
  observability/           prometheus + grafana compose stack
  scripts/                 devnet-up.sh, testnet-up.sh, observability-up.sh
```

Subcommands: `run` (the node), `beacon` (full upstream Prysm CLI, embedded —
debugging escape hatch), `testnet generate-genesis` (prysmctl's generator for
*private-network* genesis), `devnet-wallet` (wallet from the deterministic
interop keys), `version` (reports both bundled versions).

## The devnet, flag by flag

`scripts/devnet-up.sh` does all of this (plus tx spam and dashboards); spelled
out, with each flag representing one kind of knob:

```sh
go build -o bin/ethereum-node ./cmd/ethereum-node
go build -o bin/validator github.com/OffchainLabs/prysm/v7/cmd/validator

./bin/ethereum-node devnet-wallet --wallet-dir=run/wallet --num-validators=64
./bin/ethereum-node testnet generate-genesis --fork=fulu --num-validators=64 \
    --genesis-time-delay=45 --chain-config-file=devnet/chain-config.yml \
    --geth-genesis-json-out=run/genesis.json --output-ssz=run/genesis.ssz

./bin/ethereum-node run \
    --datadir=run/data \
    --el-genesis=run/genesis.json \
    --cl-genesis-state=run/genesis.ssz \
    --cl-chain-config=devnet/chain-config.yml \
    --http.port=8545 \
    --verbosity=info \
    --metrics \
    --fee-recipient=0x878705ba3f8bc32fcf7f4caa1a35e72af65cf766 \
    --beacon-flag no-discovery \
    --beacon-flag supernode \
    --beacon-flag min-sync-peers=0 \
    --beacon-flag minimum-peers-per-subnet=0 \
    --beacon-flag contract-deployment-block=0
```

and in a second terminal, the validator client:

```sh
./bin/validator --accept-terms-of-use --datadir=run/data/validator \
    --beacon-rpc-provider=127.0.0.1:4000 --chain-config-file=devnet/chain-config.yml \
    --wallet-dir=run/wallet --wallet-password-file=run/wallet/password.txt
```

- `--datadir` — supervisor topology: one root, `execution/` + `beacon/` inside
- `--el-genesis` / `--cl-genesis-state` / `--cl-chain-config` — genesis
  fixtures the two halves must agree on
- `--http.port` — a curated geth option; `--verbosity`/`--metrics`/
  `--fee-recipient` — shared knobs fanned out to both modules
- `--beacon-flag <anything>` — inline passthrough to the full Prysm flag
  surface (file equivalent: `consensus.settings`); geth's equivalent is
  `--el-setting` / `execution.settings`

The script funds 3 spam accounts in genesis via **two-pass generation**
(pass 1 → template, inject allocs, pass 2 with `--geth-genesis-json-in`) —
funding must happen *before* the CL state is computed because that state
embeds the EL genesis hash; editing genesis.json afterwards desyncs the
halves and no block can build.

## Run on a public testnet (Hoodi)

```sh
./bin/ethereum-node run --network=hoodi --datadir=./hoodi-data
```

`--network` (hoodi|sepolia|mainnet; only Hoodi verified so far) switches both
halves to upstream presets: geth's built-in genesis, bootnodes, discovery and
snap sync; Prysm's network flag plus checkpoint sync (`--checkpoint-sync-url`,
default `https://checkpoint-sync.<network>.ethpandaops.io`).

- Beacon at network head within minutes (checkpoint sync); blocks import
  optimistically while the EL catches up.
- Measured 2026-07-29 (M-series laptop, home broadband): **1h20m to fully
  validating, 105 GB total** — 104 GB execution (state alone 73 GiB: 45M
  accounts, 279M storage slots), 0.7 GB beacon with pruning. Budget ~150 GB.
- Defaults stay cheap: no `--supernode`, no backfill, blob/column data
  self-prunes after ~18 days; add `beacon-db-pruning: true` to cap the
  beacon DB.
- A following node — no validator keys involved.
- **Adopting existing datadirs:** on-disk layouts are identical to the
  standalone clients', so `execution.datadir` / `consensus.datadir` (or
  `--el-datadir` / `--cl-datadir`) can point straight at an existing geth /
  prysm datadir and resume it in place — the clients' own genesis checks
  reject a wrong network. Data must come from client versions no newer than
  the pinned pair (DB schemas migrate forward, not back).

## Single config file

Everything `run` accepts fits in one YAML (`--config`); explicit CLI flags
override file values, unset keys keep each client's stock defaults. Examples
in `configs/`.

```yaml
node:                          # supervisor-level topology: datadir, network,
  datadir: ./run/hoodi-data    # ports, verbosity, metrics
  network: hoodi

execution:
  settings: |                  # THE FULL GETH OPTION SURFACE: geth-native
    [Eth]                      # TOML (`geth dumpconfig` format), decoded
    DatabaseCache = 4096       # with geth's own semantics
    [Node.P2P]
    MaxPeers = 100

consensus:
  settings:                    # THE FULL BEACON OPTION SURFACE: upstream
    beacon-db-pruning: true    # prysm flag names, fed verbatim to prysm's
    min-sync-peers: 0          # own --config-file loader
```

Precedence rule: **supervisor-owned topology wins** — datadir, the engine
socket, HTTP/authrpc host+port, p2p listen and network posture, genesis and
network id are applied *after* the settings decode; everything else is
`settings`, delegated to each client's own loader — no option list is
transcribed by hand, so coverage can't rot across releases. Two honest
limits: `execution.settings` covers the `[Eth]`/`[Node]`/`[Metrics]`
sections of `geth dumpconfig` (no Ethstats; a few fields only matter with
services this build doesn't register), and settings *can* still tune knobs
adjacent to topology (e.g. discovery internals, metrics details) when you
ask them to. Typos fail loudly on both sides (consensus keys checked against
the embedded flag set; execution fields get geth's own "field not defined"
error; unknown top-level YAML keys in the file are rejected too). One edge:
a zero/empty value in the file cannot override a non-zero default — use the
CLI flag for that. Full file/inline parity: supervisor keys ↔ CLI flags,
`consensus.settings` ↔ `--beacon-flag`, `execution.settings` ↔
`--el-setting` (inline overrides file; `--beacon-flag` args land last on the
synthesized argv, so they can even override supervisor-set beacon args — an
escape hatch, mind your feet).

## Observability

Metrics are on by default (`--metrics`, 127.0.0.1 only): geth at
`:6060/debug/metrics/prometheus`, beacon at `:8080/metrics`, devnet validator
at `:8081/metrics`. The compose stack in `observability/` (node runs
natively; only Prometheus + Grafana in Docker — a trimmed-down cousin of
[nalepae/infra](https://github.com/nalepae/infra)):

```sh
scripts/observability-up.sh        # grafana: http://127.0.0.1:3001 (no login)
scripts/observability-up.sh down
```

(Container-to-host scraping is verified on macOS/Docker Desktop; on native
Linux, `host.docker.internal` is not loopback, so either run the compose
stack with host networking or bind the exporters beyond 127.0.0.1.)

Three auto-provisioned dashboards, every panel title prefixed `EL:`/`CL:`:

- **ethereum-node** — both halves on one screen: CL slot/justified/finalized
  vs EL head block, peers, state-transition timing, memory, txpool, DB size,
  devp2p bandwidth, RPC rate (dominated by the engine-over-IPC calls on an
  otherwise idle node; the counter covers all RPC transports).
- **Beacon node (detailed)** — adapted from nalepae/infra (79 panels, minus
  those needing log/trace datasources this stack doesn't run).
- **Geth node (detailed)** — adapted from
  [Grafana dashboard 14053](https://grafana.com/grafana/dashboards/14053-geth-overview/),
  every query validated against the live v1.17.4 exporter, dead targets
  dropped.

Quirk worth knowing: geth v1.17.4 declares `chain/inserts` but never updates
it (dead metric), so block-processing timing comes from Prysm's
`state_transition_processing_milliseconds`. Still two metrics stacks scraped
separately — one root-owned registry is roadmap item 4.

## Roadmap

**Status:** item 1 shipped (network presets verified on Hoodi + the single
config file), devnet observability shipped, item 8 (`--isolation=process`)
implemented with first A/B numbers on the `isolation-ab` branch, and item 3's
first rung implemented and measured on the `engine-inproc-ab` branch against
the now-open injection-seam PR (numbers under Upstream PRs). Next: item 2
(release pipeline).

1. **Network presets + one config file** — done (see above).
2. **Release pairing + packaging** — versions.lock of tested (geth, prysm)
   pairs, CI, reproducible builds (riding Prysm's Docker-based release
   work), published binaries + images with a signing story for the combined
   artifact, `.deb` + systemd, a `doctor` endpoint. Buys
   `apt install ethereum-node` — and it is the prerequisite for announcing
   anything wider.
3. **Engine transport ladder** — `rpc.DialInProc` (no socket, same JSON),
   then a typed `EngineCaller` on `catalyst.ConsensusAPI` (no JSON). The
   serialized path stays as compatibility boundary and differential-test
   oracle. Buys proposal-path latency and allocation churn. First rung
   running and measured on the `engine-inproc-ab` branch via the injection
   seam PR (see Upstream PRs); the measured residual — JSON codec + rpc
   dispatch, 0.5–1.7 ms per call — is what the typed rung removes.
4. **Root-owned process globals** — one logging setup, one OTel provider, one
   Prometheus registry/endpoint, one pprof. Today the modules can fight over
   globals (Prysm's trace-verbosity path overwrites the geth log handler).
   Buys coherent observability and no last-writer-wins config bugs.
5. **In-memory blob delivery** — `getBlobsV2/V3` by reference instead of
   hex-JSON; the largest per-slot byte movement disappears.
6. **Payload storage dedup** — stop storing every payload twice; dedicated
   DBs stay, the EL exposes a read API instead (schema unification would
   couple the teams' migrations — the coupling this project exists to
   avoid). The biggest disk win; needs careful pruning coordination.
7. **Shared scheduling/backpressure and sync coordination** — CL slot
   deadlines ↔ EL sync/compaction pressure instead of timeout guessing; the
   longer-term inversion (CL-driven EL sync: forward-feed and backfill
   instead of FCU-triggered independent download) is a substantial upstream
   sync rework, so the first step is measuring the redundant download/storage
   during a fresh public-network sync from this node's single log stream.
8. **`--isolation=process`** — same binary, supervised child processes:
   crash isolation back, and the honest A/B harness (only the process
   boundary flips). Implemented on the `isolation-ab` branch.
9. **Shared networking substrate** (ethp2p) — out of scope until the
   protocol work matures.

## Upstream PRs to propose

Small, self-contained changes to the upstream projects that this repo is the
concrete consumer for (details and file references in Upstream
observations):

**Prysm:**

1. **Execution-client injection seam** — an option on the execution service
   to supply a ready RPC client (or an `EngineCaller` implementation)
   instead of only an endpoint string. Unblocks the in-process and typed
   engine transports (roadmap 3) without patches. **Opened:
   [prysm#17334](https://github.com/OffchainLabs/prysm/pull/17334)**
   (`execution.WithRPCClientDialer`) — and validated end-to-end here first:
   the supervisor hands Prysm a dialer returning geth's `stack.Attach()`
   in-process client (`--engine-transport=inproc`, `engine-inproc-ab`
   branch), and a devnet A/B — same binary, same 30 tx/s spam, 90 spam-filled
   slots per leg, only the transport flag flipped, socketlessness verified at
   the fd level — measured these client-observed means:

   | engine call | ipc socket | in-proc | delta |
   |---|---|---|---|
   | `engine_getPayload` | 3.03 ms | 2.28 ms | −25% |
   | `engine_forkchoiceUpdated` | 1.32 ms | 1.15 ms | −13% |
   | `engine_newPayload` | 4.58 ms | 4.51 ms | −1.5% |

   p50/p90/p99 moved the same direction on every method, and geth's
   server-side medians were identical across the legs (±0.03 ms), so the
   deltas are transport, not chain noise; whole-process CPU over the window
   ran ~7% lower in-proc (one run — indicative, not proven). The residual
   client−server gap (~0.5 ms forkchoiceUpdated, ~1.1 ms newPayload,
   ~1.7 ms getPayload) is JSON codec + dispatch: unreachable by transport
   swaps, 2–5× what the socket cost, and exactly the typed-`EngineCaller`
   rung's target. Devnet payloads are small (~360 transfers per block);
   both the saving and the residual grow with payload size on public
   networks.
2. **Importable app wiring** (`beaconapp`-style package with a thin
   `cmd/beacon-chain` shim) — deletes this repo's one copied file and makes
   the composition pure.
3. **Attach the error to the genesis-provider failure log**
   (`genesis/initialize.go`) — a one-liner that turns an invisible
   misconfiguration into a diagnosable one.
4. **Sub-millisecond engine latency observations** — the engine-call
   histograms exist after all (see the corrected observation below), but
   they observe integer-truncated `.Milliseconds()` into buckets flooring
   at 25 ms: sub-millisecond calls record as zero and everything under
   25 ms lands in one bucket. Float observations + finer buckets is a
   two-line change, proven during the #17334 A/B (the fix ships in the
   `pr-17334-ab` measurement branch), where truncation would have erased
   the entire measured effect.

**Geth:**

1. **Error-returning variants of the `cmd/utils` registration helpers**
   (`RegisterEthService` and friends call `Fatalf` today) — makes the
   otherwise excellent embedding surface safe for hosts that need cleanup.
2. **Fix or remove the dead `chain/inserts` metric** and consider restoring
   a block-import timing summary.
3. **Fail fast on over-long IPC paths** with the limit in the error text,
   instead of a later cryptic `bind: invalid argument`.

## Version bump checklist

What to check when a new prysm or geth release lands (this is what the
release-pairing CI of item 2 automates):

**Bumping prysm** (`go.mod` require → new tag):

1. Read the new prysm go.mod and re-align: our go-ethereum require to
   **exactly** what it requires; mirrored replace directives (Go ignores
   replaces in dependencies — currently only the json-iterator fork; prysm's
   vendored go-bip39 is functionally identical to upstream v1.1.0 so it isn't
   mirrored); the `go` toolchain directive.
2. Re-diff `internal/prysmapp/prysmapp.go` against upstream
   `cmd/beacon-chain/main.go` — the one deliberate copy; this also picks up
   new flags. Usual drift: the flag list, the Before hook, `node.New`'s
   signature.
3. `go mod tidy && go build` — the compiler is the cheap alarm.

**Bumping geth:** never independently — only to what the new prysm requires.
`gethapp` is compile-checked against geth's embedding API; preset *contents*
(genesis, bootnodes) come straight from geth's `params`, though the supported
network names are a three-case switch in `networkPreset` that needs a line
when upstream adds or retires one.

**After any bump:** devnet smoke (block/slot, finality ~13 min, clean
shutdown; new forks need new keys in `devnet/chain-config.yml` — fork
versions must not collide with mainnet's registry — and possibly a new
`--fork` name), Hoodi smoke (checkpoint sync, peers, optimistic import),
dashboard query re-validation against the live exporters (metric names
drift), and the README's version mentions.

Needs **no** attention: consensus/execution settings in the config file
(delegated to each client's own loader), engine API versions (both ends are
upstream code, so new `engine_*Vx` methods arrive in lockstep), the devnet
validator build (scripts build the exact pinned version — unless a `go.work`
overlay is active, in which case local clones win, by design).

## Measuring against v0

First results exist on the `isolation-ab` branch (`bench/RESULTS.md`):
identical latency medians between one process and supervised child processes
(both on IPC-JSON — the boundary, not the transport, was measured) and a
~16% RSS saving vs the realistic two-process baseline. Methodology for every
future rung:

```
 A  two processes, engine over IPC     the realistic baseline / --isolation=process
 B  this v0: one process, IPC JSON     scripts/devnet-up.sh
 C  one process, rpc.DialInProc        roadmap 3, first rung
 D  one process, typed EngineCaller    roadmap 3, second rung
```

- **Proposal/import latency** from the node's own logs (`sinceSlotStartTime`,
  `elapsed=`) — no instrumentation needed.
- **Engine bytes**: counting proxy on the endpoint for A–C; D is zero by
  construction. **Resource**: RSS/CPU sampling + pprof. **Disk** (for roadmap 6):
  `du` per module after fixed epochs under identical load.
- **Guardrails**: missed proposals, attestation inclusion distance, reorgs.
- Discipline, learned on this machine: keep the box awake (`caffeinate`),
  drive tx/blob load or the numbers are fiction, discard warmup, ≥200 slots
  ×3 repeats, compare medians/p99 not means, change one variable at a time;
  on real networks compare long-run distributions on matched hardware.

## FAQ for the cautious client dev

**Did you fork or patch either client?**
No. Both are unmodified pinned module versions from the Go proxy (sumdb
verified). The only upstream-derived code is the `internal/prysmapp/` copy,
kept byte-close so `diff` against upstream stays trivial on every bump.

**Is any consensus or validation logic touched?**
No. The Engine API remains the boundary — same JSON, same semantics. The
devnet runs with `execution_optimistic: false` throughout: every payload
fully executed and verified by the embedded EL.

**Which go-ethereum is linked into the binary?**
Exactly the version Prysm's go.mod requires (v1.17.4 today) — the bundle's
core invariant: Prysm's imports and the running EL are the same code.

**What about process-global state colliding?**
The known list: logging (two formatters share stderr; Prysm's trace-verbosity
path overwrites the geth log handler), metrics registries, pprof, GOMAXPROCS
(Prysm's automaxprocs import), signal handlers (Prysm's is deliberately the
shutdown driver). Root-owned globals is roadmap item 4 and a hard requirement
before anything production-shaped.

**A panic in one module kills both, right?**
Yes — the honest cost of one process. Both clients tolerate unclean death
(journal/replay on restart), and `--isolation=process` restores separate
failure domains from the same binary.

**Does this couple the two teams' releases?**
No. Both develop and release independently; a bundle pins a pair of existing
releases (`ethereum-node version` reports exactly which), with pair *testing*
being the devnet/Hoodi smokes today and the roadmap's CI later. A security
release on either side rebuilds against the last known-good counterpart.

**Can the embedded halves still talk to external counterparts?**
Yes, deliberately: `ethereum-node beacon --execution-endpoint=<any EL>` is
stock Prysm, and the embedded geth keeps authrpc on 127.0.0.1:8551 for an
external CL. The serialized Engine API path never goes away.

**Why is the validator not in-process too?**
Keys want their own security and failure domain; slashing risk makes that
non-negotiable beyond toys.

**Licensing?**
`internal/prysmapp/` is copied GPL-3.0 Prysm code and the binary links geth
(LGPL/GPL), so the repo carries GPL-3.0.

## Upstream observations

Paper cuts found while composing the clients — none serious, all candidates
for upstream issues/PRs.

**go-ethereum (v1.17.4):**

- `chain/inserts` (`core/blockchain.go:102`) is declared but never updated —
  a dead metric; there is no exported block-import timing summary.
- `cmd/utils.RegisterEthService` and friends call `Fatalf` (process exit)
  instead of returning errors — hostile to embedding.
- An over-long IPC path (~104-char OS limit) warns, then fails with a cryptic
  `bind: invalid argument`; failing fast with the warning's text would help.

**Prysm (develop @ ce28535):**

- `genesis/initialize.go:41` logs `genesis provider failed` *without the
  error* — a misconfigured checkpoint URL becomes an invisible failure. One
  `WithError(err)` fixes it.
- Engine-call latency histograms exist (`new_payload_v1_latency_milliseconds`
  and friends — easy to miss: unlike the `beacon_engine_getBlobs*` family
  their names don't contain "engine"; an earlier revision of this list
  wrongly claimed they were absent) but observe integer-truncated
  `.Milliseconds()` into buckets flooring at 25 ms — sub-millisecond calls
  record as zero, and every call under 25 ms is indistinguishable.
- `cmd/beacon-chain` is `package main` with config read through `cli.Context`
  across packages — embedding requires copying `main.go`; an importable app
  package (`runtime/beaconapp`) would fix it. This repo is the concrete
  consumer.
- go.mod replace directives burden every consumer (Go ignores replaces in
  dependencies); the vendored `third_party/go-bip39` is functionally
  identical to upstream v1.1.0 and looks Bazel-era.
- With `--interop-num-validators` deprecated, devnet validators need a full
  wallet ceremony around the interop keys (hence this repo's `devnet-wallet`).
- Fresh-chain eth1 follow-distance checks log a benign, self-resolving
  condition at ERROR severity.
