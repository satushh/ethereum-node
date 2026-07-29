# ethereum-node — Geth + Prysm in one binary, one process

A composition experiment: link the unmodified sources of
[go-ethereum](https://github.com/ethereum/go-ethereum) (execution layer) and
[Prysm](https://github.com/OffchainLabs/prysm) (consensus layer) into a single
native executable that runs both as modules of one process. Prysm's Bazel
removal made this possible: both clients now build with the ordinary Go
toolchain, so a small integration module can compose them the way any Go
program composes libraries.

## Quick start

Needs: git, Go (the right toolchain auto-downloads), and optionally Docker
for the Grafana dashboard. First build downloads both clients' module graphs.

```sh
git clone https://github.com/satushh/ethereum-node.git
cd ethereum-node
go build -o bin/ethereum-node ./cmd/ethereum-node

scripts/devnet-up.sh               # devnet: node + validator + grafana, one command
                                   # (node config: configs/devnet.yaml)
tail -f run/logs/node.log          # geth + prysm logs, one process, one stream
open http://127.0.0.1:3001         # dashboards (needs docker; skipped if absent)

# watch it produce and finalize blocks
curl -s localhost:8545 -X POST -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'
curl -s localhost:3500/eth/v1/beacon/states/head/finality_checkpoints
```

Or join a public testnet instead of the devnet (also brings up the
dashboard; checkpoint sync puts the beacon at head in minutes, geth's snap
sync then needs hours and tens of GB):

```sh
scripts/testnet-up.sh hoodi        # equivalently:
./bin/ethereum-node run --config configs/hoodi.yaml
```

Blocks appear within ~1 minute; finalization after ~13 minutes (2 epochs).
Stop: Ctrl-C (or kill) stops both halves cleanly;
`scripts/observability-up.sh down` for the dashboard. Laptop note: system
sleep freezes the devnet — keep the machine awake (`caffeinate -is`).
Details in [Build](#build), [Run a local devnet](#run-a-local-devnet) and
[Observability](#observability).

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

Design points (see the repo-level discussion that motivated this):

- **Geth and Prysm stay independent projects.** This module contains no copied
  client code paths beyond a verbatim, importable adaptation of Prysm's
  `cmd/beacon-chain/main.go` (which is `package main` upstream and therefore
  cannot be linked). Both clients are ordinary pinned Go module versions
  fetched from the module proxy; local clones are only an optional `go.work`
  overlay for hacking on either client.
- **Version alignment.** Prysm's `go.mod` requires `go-ethereum v1.17.4`, so
  the go-ethereum clone is checked out at exactly `v1.17.4`. One Go binary can
  contain only one go-ethereum, and it should be the one Prysm's imports were
  compiled against.
- **The Engine API boundary is preserved.** The consensus module talks to the
  execution module over geth's IPC socket inside the datadir — the same
  serialized Engine API used between separate processes, just with zero
  operator configuration (no JWT, no localhost port). An authenticated HTTP
  engine endpoint stays available on 127.0.0.1:8551 as an escape hatch for
  pointing an external CL at the embedded EL. An in-process transport
  (`rpc.DialInProc`, already used by geth's blsync mode) is the obvious next
  optimization, kept out of v0 on purpose.
- **Databases and P2P stacks remain separate** (execution/ and beacon/ under
  one datadir root; devp2p and libp2p side by side).

## Why one binary?

Both teams keep developing and releasing independently; what merges is the
artifact. That alone buys a lot:

**Node-operator UX.** One file to download, verify, upgrade and run: one
command, one datadir, one config, one service unit, one log stream. The whole
Engine-API plumbing class of setup errors disappears — no JWT secret to
generate and share, no authrpc port to expose, no endpoint URL to mistype;
the two halves find each other on a private IPC socket inside the datadir.
And an incompatible EL/CL version pair becomes impossible to run: the bundle
*is* a tested pair (`ethereum-node version` reports the exact upstream
versions inside).

**Operations.** One lifecycle owner: startup ordering (EL first, CL dials a
socket that already exists) and graceful shutdown (one SIGTERM drains the
beacon services, then persists EL state) are coordinated in-process instead
of by systemd unit ordering and luck. One place for status/health, one
telemetry destination, one resource budget — instead of two daemons with two
monitoring stacks and two half-overlapping failure modes. When something
stalls, the interleaved EL+CL log is already correlated.

**Performance headroom** (deliberately not claimed yet — needs benchmarks).
Today the Engine API still crosses a local socket as JSON even though both
ends share an address space. The staged wins: drop the socket and auth for
geth's in-process RPC pipe (`rpc.DialInProc`), then typed in-memory calls to
stop serializing multi-megabyte payloads entirely; share scheduling and
backpressure between EL and CL instead of two processes contending blindly
for the same cores and disk; collapse duplicated runtime overhead (two
metrics/pprof/telemetry stacks, two fd budgets, two GC tunings) into one.

**Ecosystem.** A single artifact is what makes `apt install ethereum-node`
realistic (Prysm's Bazel-removal plan already targets reproducible builds and
.deb packaging), which lowers the bar for home stakers — a decentralization
win, not just convenience. A unified node is also the natural adoption target
for coordinated EL+CL networking work (shared QUIC listeners, joint bandwidth
scheduling, erasure-coded payload propagation à la ethp2p).

**What it deliberately does not change.** Execution and consensus databases,
P2P stacks and protocol responsibilities stay separate — that duplication is
protocol design, not waste. The honest cost of one process is crash
isolation: a panic in either module now takes down both. A production version
would keep a `--isolation=process` escape hatch (same binary, supervised
child processes) for operators who want the old failure domain back.

## How v0 was built, from first principles

Strip away the CLIs and each client is a Go constructor with a start method.
The whole build reduces to four observations:

**What geth needs to start.** `cmd/geth` is a thin flag-parser around public
packages: fill a `node.Config` (datadir, IPC, HTTP, p2p) and an
`ethconfig.Config` (genesis, sync mode), then call
`node.New` → `utils.RegisterEthService` → `catalyst.Register` →
`stack.Start()`. Every one of those is importable, so the execution module
(`internal/gethapp/`, 137 lines) just makes the same calls with config
structs instead of flags. Nothing else is required.

**What prysm needs to start.** The beacon node is the opposite shape: its
constructor `node.New(cliCtx, ...)` reads its many options through a
`cli.Context`, so it cannot be wired from plain structs without rewriting
upstream. The honest alternative is to keep the upstream CLI app but make it
importable: `internal/prysmapp/` is `cmd/beacon-chain/main.go` copied
verbatim into a package with a `Run(ctx, argv)` entrypoint, and the
supervisor synthesizes exactly the argv it would have received on a command
line.

**What connects them.** Nothing new. Geth serves the engine API on its IPC
socket (no JWT on IPC — the reason Prysm's docs already recommend it), and
Prysm accepts a socket path as `--execution-endpoint`. "Combining" is
therefore just call order:

```
1. start geth            → it creates <datadir>/execution/geth.ipc
2. run prysm in-process  → --execution-endpoint=<that path>
3. prysm blocks until SIGINT/SIGTERM (its own handler — the shutdown driver)
4. when it returns, close the geth stack
```

**What must agree.** Genesis and identity: the EL genesis hash is embedded in
the CL genesis state, and the chain ID must match `DEPOSIT_CHAIN_ID` (Prysm
verifies via `eth_chainId` on connect). `prysmctl` already emits both files
from one command, so it is mounted as `ethereum-node testnet
generate-genesis` and the invariant holds by construction.

That is the entire trick: one process, two constructors, and a socket path
passed by variable instead of configured by an operator.

## How a slot flows through the process

Everything below happens inside the one `ethereum-node` process; the only
things crossing a process boundary are the validator's gRPC calls and libp2p
gossip. Each numbered step names the log line it produces, so you can watch
this exact loop in `run/logs/node.log`.

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

The IPC arrows are the standard Engine API — same semantics, same JSON — on a
socket private to the datadir. Nothing about the protocol was changed, which
is exactly why the unmodified upstream code runs.

## Is this just the two clients glued together?

Today: functionally yes, deliberately. The engine path still serializes the
same JSON over a socket, so behaviour, test coverage and cross-client
compatibility carry over untouched — that is the right property for a v0.
What changes immediately is everything around the clients (setup, lifecycle,
one artifact, correlated logs). The interesting part is what the shared
address space makes possible next. The transport ladder:

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
         (go-ethereum/eth/catalyst/api.go:768)
```

Redundant work between the two halves, and what in-process composition can do
about it:

| Redundancy | Two processes | This binary today | Possible in one process |
|---|---|---|---|
| Engine transport | JSON + socket + JWT per call | JSON + socket (no JWT) | typed calls, zero serialization |
| Blob delivery (`engine_getBlobsV2/V3`) | multi-MB hex-JSON round trips | same | blob refs handed over in memory |
| EL liveness + eth1 data | polling `eth_chainId`/headers over RPC | same | subscribe to geth's chain event feed directly |
| Payload storage | every execution payload stored twice: in geth's chain DB *and* inside beacon blocks in Prysm's DB | same | store bodies once, CL references EL storage |
| Observability | two metrics endpoints, two pprof servers, two trace configs to wire to one dashboard | one process, but still two stacks | one registry/provider, EL spans nested inside CL block-import traces |
| Sync coordination | "is the EL synced yet" heuristics over RPC timeouts | same | direct state queries, shared backpressure |
| Lifecycle | systemd unit ordering + health-check scripts | one supervisor, ordered start/stop | — (done) |
| Setup | two configs, JWT generation, endpoint URLs | one command | — (done) |

None of the protocol-level duplication goes away, on purpose: execution state
vs beacon state, txpool vs attestation/operation pools, devp2p vs libp2p are
different protocol responsibilities, not waste. Sharing the *networking
substrate* (QUIC listeners, bandwidth scheduling, discovery) is plausible
later — that is the ethp2p direction — but is explicitly out of scope here.

A caution against over-promising: Engine API serialization is small next to
EVM execution, state I/O and BLS verification, so the typed transport is a
proposal-latency and allocation win, not a throughput revolution. The two
candidates with genuinely large wins are the blob path (biggest bytes moved
per slot on a mainnet-like load) and payload storage dedup (beacon DBs are
dominated by embedded execution payloads). Measure before claiming either.

## Upstream changes required: none

Neither clone is modified — `git status` in both is clean; all integration
lives in this module (~1,200 lines total, mostly boilerplate):

| Piece | Lines | Nature |
|---|---|---|
| `internal/prysmapp/prysmapp.go` | 398 | near-verbatim copy of `prysm/cmd/beacon-chain/main.go`, needed only because upstream is `package main` |
| `internal/gethapp/gethapp.go` | 137 | new, thin wrapper over geth's public embedding API (`node`, `cmd/utils`, `eth/catalyst`) |
| `cmd/ethereum-node/` (main.go + wallet.go) | 280 | new: supervisor CLI, arg plumbing, devnet wallet helper |
| `devnet/config.yml` + `scripts/devnet-up.sh` | 120 | devnet fixtures |
| `go.mod` | ~270 | pinned module versions + 1 replace mirrored from prysm's go.mod; the rest generated by `go mod tidy` |

So the real hand-written integration is ~420 lines. The one upstream change
that *would* help (per the design discussion): Prysm exporting its beacon app
wiring as an importable package (e.g. `runtime/beaconapp`) with a thin
`cmd/beacon-chain` shim — that would delete the 398-line copy here and turn
this module into pure composition. Geth already needs nothing: its embedding
surface is public.

## Layout

```
ethereum-node/
  cmd/ethereum-node/    the combined binary (run | beacon | devnet-wallet | testnet | version)
  internal/gethapp/     programmatic geth embed (~ stripped cmd/geth makeFullNode)
  internal/prysmapp/    importable adaptation of prysm cmd/beacon-chain/main.go
  devnet/config.yml     single-node devnet chain config (mainnet preset, all forks at genesis)
  scripts/devnet-up.sh  boot a local devnet end to end
  ../go-ethereum        optional clone for local dev (go.work overlay)
  ../prysm              optional clone for local dev (go.work overlay)
```

## Build

Both clients resolve from the Go module proxy — a single clone builds:

```sh
git clone https://github.com/satushh/ethereum-node.git ethereum-node
cd ethereum-node
go build -o bin/ethereum-node ./cmd/ethereum-node   # Go >= 1.26.5 (auto-downloaded)
```

go.mod pins prysm (currently a develop pseudo-version) and go-ethereum at
exactly the version that prysm commit requires. To hack on either client
locally, don't touch go.mod — drop clones next door and add a gitignored
workspace overlay:

```sh
git clone https://github.com/OffchainLabs/prysm.git ../prysm
git clone --branch v1.17.4 https://github.com/ethereum/go-ethereum.git ../go-ethereum
go work init . && go work use ../prysm ../go-ethereum
```

Bumping versions later = update the prysm require, re-align the go-ethereum
require to the new prysm go.mod, re-diff `internal/prysmapp/` against
upstream's `cmd/beacon-chain/main.go`, then `go mod tidy && go build`.

## Run a local devnet

```sh
scripts/devnet-up.sh          # generates genesis, starts node + validator
tail -f run/logs/node.log     # geth-format and prysm-format lines interleaved
```

Verify it is making progress:

```sh
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' http://127.0.0.1:8545
curl -s http://127.0.0.1:3500/eth/v1/beacon/headers/head
```

Stop with a single Ctrl-C / SIGTERM: Prysm's signal handler shuts the beacon
module down, after which the supervisor closes the geth stack.

### The same devnet, flag by flag

`scripts/devnet-up.sh` hides the moving parts; here they are spelled out.
Copy-paste from the repo root (needs nothing but Go):

```sh
go build -o bin/ethereum-node ./cmd/ethereum-node
GOBIN=$PWD/bin go install github.com/OffchainLabs/prysm/v7/cmd/validator@$(go list -m -f '{{.Version}}' github.com/OffchainLabs/prysm/v7)

./bin/ethereum-node devnet-wallet --wallet-dir=run/wallet --num-validators=64
./bin/ethereum-node testnet generate-genesis --fork=fulu --num-validators=64 \
    --genesis-time-delay=45 --chain-config-file=devnet/config.yml \
    --geth-genesis-json-out=run/genesis.json --output-ssz=run/genesis.ssz

./bin/ethereum-node run \
    --datadir=run/data \
    --el-genesis=run/genesis.json \
    --cl-genesis-state=run/genesis.ssz \
    --cl-chain-config=devnet/config.yml \
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

and in a second terminal, the validator client (signs with the 64 keys the
wallet step created):

```sh
./bin/validator --accept-terms-of-use --datadir=run/data/validator \
    --beacon-rpc-provider=127.0.0.1:4000 --chain-config-file=devnet/config.yml \
    --wallet-dir=run/wallet --wallet-password-file=run/wallet/password.txt
```

Each flag above represents one kind of knob:

- `--datadir` — supervisor topology: one root, `execution/` + `beacon/` inside
- `--el-genesis` / `--cl-genesis-state` / `--cl-chain-config` — the devnet
  genesis fixtures the two halves must agree on
- `--http.port` — a curated geth option (the supervisor-owned shortlist)
- `--verbosity`, `--metrics`, `--fee-recipient` — shared knobs the supervisor
  fans out to both modules
- `--beacon-flag <anything>` — inline passthrough to the full upstream Prysm
  flag surface; the file-based equivalent is `consensus.settings`, and the
  geth equivalent is `execution.settings` (see the config-file section)

## Run on a public testnet (Hoodi)

```sh
./bin/ethereum-node run --network=hoodi --datadir=./hoodi-data
```

`--network` (hoodi|sepolia|mainnet) switches both halves to upstream presets:
geth gets its built-in genesis, bootnodes, discovery and snap sync; the
beacon gets Prysm's network flag plus checkpoint sync
(`--checkpoint-sync-url`, default `https://checkpoint-sync.<network>.ethpandaops.io`).
What to expect on Hoodi:

- Beacon at the network head within minutes (checkpoint sync, ~13 peers on
  first try); blocks import optimistically while the EL catches up.
- Geth snap sync is the long pole: hours, and the bulk of the disk. Budget
  ~150 GB for comfort; expect meaningfully less used in practice.
- Keep disk small: the defaults already avoid the expensive choices — no
  `--supernode` (custodies only a fraction of PeerDAS columns), no backfill,
  blob/column data self-prunes after ~18 days. Add
  `--beacon-flag beacon-db-pruning` to cap the beacon DB too.
- No validator keys are involved; this is a following node. The validator
  client remains separate and optional.

## Single config file

Everything `run` accepts can live in one YAML (`--config`), with explicit CLI
flags overriding file values and unset keys keeping each client's stock
defaults. Examples in `configs/`:

```yaml
node:                          # supervisor-level topology: datadir, network,
  datadir: ./run/hoodi-data    # ports, verbosity, metrics — the keys the
  network: hoodi               # supervisor itself needs to wire the two halves

execution:
  settings: |                  # THE FULL GETH OPTION SURFACE: geth-native
    [Eth]                      # TOML, identical to `geth dumpconfig` output,
    DatabaseCache = 4096       # decoded with geth's own semantics
    [Node.P2P]
    MaxPeers = 100

consensus:
  settings:                    # THE FULL BEACON OPTION SURFACE: upstream
    beacon-db-pruning: true    # prysm flag names, fed verbatim to prysm's
    min-sync-peers: 0          # own --config-file loader
```

One rule decides precedence: **supervisor-owned topology always wins** —
datadir, the engine socket, ports, network posture and genesis come from the
`node`/`execution` top-level keys (or CLI flags, which override file values);
everything else is `settings`, delegated to each client's own loader. Typos
fail loudly on both sides: unknown consensus keys are checked against the
embedded beacon node's flag set, unknown execution fields get geth's own
"field not defined in ethconfig.Config" error with a godoc link.

Everything in the file has an inline equivalent and vice versa: the
supervisor keys are the CLI flags, `consensus.settings` ↔
`--beacon-flag key=value`, and `execution.settings` ↔ repeated
`--el-setting` TOML lines (inline overrides file everywhere).

```sh
ethereum-node run --config configs/hoodi.yaml
```

## Observability

Metrics are on by default (`--metrics`, on 127.0.0.1 only): geth's exporter
at `:6060/debug/metrics/prometheus`, the beacon node at `:8080/metrics`, and
the devnet validator at `:8081/metrics`. A ready-made Prometheus + Grafana
stack (the node runs natively; only these two run in Docker) lives in
`observability/` — a trimmed-down cousin of
[nalepae/infra](https://github.com/nalepae/infra):

```sh
scripts/observability-up.sh        # grafana: http://127.0.0.1:3001 (no login)
scripts/observability-up.sh down
```

Three dashboards are auto-provisioned:

- **ethereum-node** — the combined view: both halves of the process on one
  screen, plus a geth-details row (head/safe/finalized pointers, txpool, DB
  size, devp2p bandwidth, RPC rate — which on this node is literally the
  engine API traffic over IPC).
- **Beacon node (detailed)** — adapted from
  [nalepae/infra](https://github.com/nalepae/infra); 79 panels, minus the few
  needing log/trace datasources this stack doesn't run.
- **Geth node (detailed)** — adapted from
  [Grafana dashboard 14053](https://grafana.com/grafana/dashboards/14053-geth-overview/),
  with every query validated against the live v1.17.4 exporter and dead
  targets dropped (several per-phase timing metrics no longer exist).

The compact combined dashboard shows: CL head slot / justified / finalized epochs next to
the EL head block, CL and EL peer counts, state-transition timing, memory of
all three processes, and per-module `up` status. One quirk found while
building it: geth v1.17.4 declares `chain/inserts` but never updates it
(dead metric upstream), so block-processing timing comes from the Prysm side
(`state_transition_processing_milliseconds`).

This is still two metrics stacks scraped separately — collapsing them into
one root-owned registry is future-work #2.

## Subcommands

- `run` — the combined node. `--datadir` gets `execution/` and `beacon/`
  subdirectories. `--beacon-flag <flag>` passes extra flags through to the
  embedded beacon node.
- `beacon [args...]` — the full upstream Prysm beacon-chain CLI (flags,
  `db`/`jwt` subcommands) running embedded; useful for debugging.
- `testnet generate-genesis` — Prysm's genesis generator, mounted unchanged
  from `prysmctl` so the devnet needs no second tool. ("testnet" here means
  *creating a private network's genesis*, not joining Sepolia/Hoodi — that is
  future-work #1.)
- `devnet-wallet` — builds a validator wallet from the deterministic interop
  keys matching `--num-validators` premined at genesis.
- `version` — reports the versions of both bundled components.

## Future work — and what each item buys

**Status:** item 1 is complete — network presets
(`--network=hoodi|sepolia|mainnet`, verified live on Hoodi) and the single
config file (`--config`, see above) have both shipped, along with devnet
observability. The immediate next milestone is root-owned process globals
(item 2).

Roughly in implementation order:

1. **Public network presets** (`--network=sepolia|hoodi|mainnet`): geth's
   built-in genesis + bootnodes, Prysm's network flag + checkpoint-sync URL,
   devnet-only flags dropped. Both clients already ship all of this upstream
   (geth: `params/bootnodes.go` + embedded genesis; prysm: `--hoodi` etc. +
   `--checkpoint-sync-url`, reachable today via `--beacon-flag` passthrough) —
   the entire change is ~30 lines in `internal/gethapp/`, which currently
   hardcodes the devnet posture (`NoDiscovery`, `NoDial`, loopback listen,
   `FullSync`, explicit genesis file). Same item, same reason: a single
   config file (`ethereum-node.yaml` with `node` / `execution` / `consensus`
   sections) — prysm already loads YAML config files in its Before hook, so
   the supervisor splits one file and feeds the consensus section to prysm's
   existing loader while execution keys map onto geth's config structs.
   Everything not set keeps each client's stock defaults, exactly as flags do
   today. *Buys:* the one-command, one-config node for real networks — the
   actual operator-facing product — and the first realistic workload for
   every measurement below.
2. **Root-owned process globals**: one logging setup (shared writer,
   per-module prefixes instead of two interleaved formats), one OpenTelemetry
   provider, one Prometheus registry and metrics endpoint, one pprof server.
   Today the modules can fight over globals — Prysm's trace-verbosity path
   (`internal/prysmapp/prysmapp.go`, `startNode`) overwrites the geth log
   handler that `internal/gethapp/gethapp.go` installed. *Buys:* coherent
   observability (EL `newPayload` spans nested inside CL block-import traces
   with no traceparent-over-HTTP plumbing) and no last-writer-wins config
   bugs — the prerequisite for everything being measurable in one place.
3. **Engine transport ladder** (see diagram above): first
   `rpc.DialInProc` (`go-ethereum/rpc/inproc.go:25`) — no socket, no
   syscalls, same JSON; then a typed `EngineCaller`
   (`prysm/beacon-chain/execution/engine_client.go:53`) implemented directly
   on `catalyst.ConsensusAPI` (`go-ethereum/eth/catalyst/api.go`). *Buys:*
   proposal-path latency and allocation churn; the JSON path stays available
   both as the compatibility boundary and as the differential-testing oracle
   (run both transports on the same inputs, assert identical results).
4. **In-memory blob delivery**: `engine_getBlobsV2/V3` responses handed over
   as references instead of hex-JSON. *Buys:* the largest per-slot byte
   movement on a mainnet-like load disappears; faster data-column
   reconstruction under PeerDAS.
5. **Payload storage dedup**: stop storing execution payloads twice (geth
   chain DB + embedded in beacon blocks in Prysm's DB); CL keeps headers and
   references EL bodies. *Buys:* the single biggest disk win — beacon DBs are
   dominated by embedded payloads — at the cost of careful pruning
   coordination, which is why it comes after the interfaces above exist.
6. **Shared scheduling/backpressure**: CL exposes slot deadlines to the EL,
   EL exposes sync/compaction pressure to the CL, instead of both inferring
   via timeouts. *Buys:* fewer missed proposals and attestation deadline
   breaches on constrained hardware, where the two halves currently contend
   blindly.
7. **`--isolation=process` escape hatch**: same binary re-executes its two
   modules as supervised children over the same IPC path. *Buys:* restores
   crash isolation for operators who want it — and doubles as the cleanest
   A/B harness for measuring what single-process actually saves (identical
   code, only the process boundary changes).
8. **Release pairing + packaging**: a `versions.lock` of tested
   (geth, prysm) tag pairs, compatibility tests in CI, reproducible builds,
   `.deb` + systemd unit, an aggregate `status`/`doctor` endpoint. Aligns
   with Prysm's Bazel-removal plan (which already targets reproducible
   releases and Debian packaging). *Buys:* `apt install ethereum-node`, and
   security releases that rebuild against the last known-good counterpart
   without synchronizing upstream release trains.
9. **Upstream `beaconapp` export**: Prysm exposing its app wiring as an
   importable package. *Buys:* deletes the 398-line copy in
   `internal/prysmapp/`, making this repo pure composition (see "Upstream
   changes"). Geth needs nothing.
10. **Shared networking substrate** (ethp2p direction): joint QUIC listeners,
    bandwidth scheduling, discovery. Explicitly out of scope until the
    protocol work matures; listed for completeness.

Housekeeping when bumping prysm: re-align the go-ethereum require to the new
prysm go.mod, re-check prysm's replace directives (only the json-iterator
fork currently needs mirroring — its vendored go-bip39 is functionally
identical to upstream v1.1.0, so upstream is used directly), and re-diff
`internal/prysmapp/` against upstream's main.go.

## Version bump checklist

What to check when a new prysm or geth release lands. (This list is what the
future release-pairing CI, future-work #8, automates.)

**Bumping prysm** (`go.mod` require → new tag):

1. Read the new prysm go.mod and re-align three things: our go-ethereum
   require to **exactly** the version it requires (the one-go-ethereum
   invariant), our mirrored replace directives (new/changed ones must be
   restated — Go ignores replaces in dependencies; re-diff the vendored
   go-bip39 if it changed), and the `go` directive (toolchain
   auto-downloads).
2. Re-diff `internal/prysmapp/prysmapp.go` against upstream
   `cmd/beacon-chain/main.go` — the one deliberate copy. Flag list
   additions, `before`-hook changes, and the `node.New` signature are the
   usual drift points. Disappears if upstream ever exports a beaconapp.
3. `go mod tidy && go build` — compile errors here are the cheap alarm.

**Bumping geth**: never independently — only to the version the new prysm
requires. `internal/gethapp/` is compile-checked against geth's embedding
API (config struct fields, `RegisterEthService`/`catalyst.Register`/
`SetupMetrics` signatures), and the network presets track geth's `params`
(networks get added and retired upstream).

**Both, after any bump:**

4. Devnet smoke: `scripts/devnet-up.sh` → block per slot, finality at ~13
   min, clean SIGTERM. New forks need new epoch/version keys in
   `devnet/config.yml` (fork versions must not collide with mainnet's
   registry) and possibly a new `--fork` name for generate-genesis.
5. Public-network smoke: `scripts/testnet-up.sh hoodi` → checkpoint sync,
   peers, optimistic import.
6. Dashboard queries: metric names drift (geth v1.17.4 already carries a
   declared-but-dead `chain/inserts`); re-validate panel exprs against the
   live exporters — the adaptation script's check (extract metric names from
   every expr, verify against `/debug/metrics/prometheus` and `/metrics`)
   is the model.
7. Version strings: the README's "currently v1.17.4" mentions.

What deliberately needs **no** attention: consensus flags in the config file
(`consensus.settings` delegates to prysm's own loader), the engine API
version (both ends are upstream code, so new `engine_*Vx` methods arrive in
lockstep), and the devnet validator build (the scripts install the exact
version go.mod pins).

## Measuring against v0

v0 is the baseline every step above must beat on like-for-like runs. The
experimental design that keeps comparisons honest: **same clones, same
binary where possible, one variable at a time.**

Configurations to compare:

```
 A  two processes, engine over IPC          standalone geth + `ethereum-node
    (upstream status quo)                   beacon` in a second process — the
                                            identical code split in two, so A/B
                                            isolates the process boundary itself
 B  this v0: one process, IPC JSON          scripts/devnet-up.sh
 C  one process, rpc.DialInProc             future work #3, first rung
 D  one process, typed EngineCaller         future work #3, second rung
```

What to measure, and where it already exists today:

- **Proposal path latency**: Prysm logs `sinceSlotStartTime` on
  `Building block` / `Finished building block` / `Synced new block`; geth
  logs `elapsed=` on `Imported new potential chain segment`. Already
  parseable from `run/logs/node.log` with grep — no instrumentation needed
  for B vs C vs D deltas.
- **Engine call latency/counts**: re-enable metrics for benchmark runs (drop
  `--disable-monitoring`, geth side `--metrics` equivalent in
  `internal/gethapp/gethapp.go`) and scrape per-method Engine API timers from
  both sides; after future-work #2 they land in one registry.
- **Bytes over the engine boundary**: point `--execution-endpoint` through a
  tiny counting proxy in modes A–C; mode D's number is zero by construction.
- **Resource**: RSS/CPU via `ps -o rss=,pcpu= -p $(pgrep -f "ethereum-node run")`
  sampled per epoch; GC pauses and allocation profiles via pprof
  (`--beacon-flag pprof` today; one pprof endpoint after #2).
- **Disk** (for dedup, #5): `du -sh run/data/execution run/data/beacon` after
  a fixed number of epochs under identical tx + blob load.
- **Chain health under load**: missed-proposal count (produced blocks vs
  elapsed slots), attestation inclusion distance, reorg count — the
  regression guardrails, not the wins.

Methodology, learned the hard way on this very machine:

- Keep the box awake (`caffeinate -is`), on mains power, and idle otherwise;
  a macOS sleep froze this devnet mid-run and stalled finality for an hour.
- Drive load or the numbers are fiction: an empty-block devnet exercises
  neither the EVM nor the blob path. Point a tx spammer (and later a blob-tx
  spammer) at `127.0.0.1:8545`.
- Discard the first epoch (warmup, follow-distance startup noise), run ≥ 200
  slots per configuration, repeat 3×, and compare medians and p95/p99 —
  distributions, not means; GC makes means lie.
- Change one rung of the ladder at a time, from identical genesis files.
- For real-network numbers, run paired nodes on the same testnet
  (e.g. Hoodi) on matched hardware — peers are uncontrollable, so compare
  long-run distributions, never short windows.

## FAQ for the cautious client dev

**Did you fork or patch either client?**
No. Both clients are unmodified pinned module versions fetched from the Go
proxy (checksums verified via the Go checksum database). The only
upstream-derived code is the `internal/prysmapp/` copy, kept byte-close to
upstream precisely so `diff` against `cmd/beacon-chain/main.go` stays trivial
on every version bump.

**Is any consensus or validation logic touched?**
No. The Engine API remains the boundary — same JSON, same semantics — served
by unmodified geth catalyst code and consumed by unmodified Prysm
execution-client code. The devnet ran with `execution_optimistic: false`
throughout: every payload fully executed and verified by the embedded EL.

**One go-ethereum is linked into the binary — which one?**
Exactly the version Prysm's go.mod requires (v1.17.4 today). That is the
bundle's core invariant: Prysm's own imports and the running EL are the same
code. Bumping either side means re-aligning the pair and re-running the
compatibility tests — which is what a versions.lock CI encodes later.

**What about process-global state colliding?**
The known list: logging (two formatters share stderr, and Prysm's
trace-verbosity path overwrites the geth log handler), metrics registries,
pprof, GOMAXPROCS (Prysm's automaxprocs side-effect import), and signal
handlers — Prysm's is deliberately kept as the shutdown driver. The devnet
defaults sidestep the collisions (beacon monitoring off); root-owned globals
is future-work #2 and a hard requirement before anything production-shaped.

**A panic in one module kills both, right?**
Yes — the honest cost of one process. Both clients already tolerate unclean
death (journal/replay on restart), and `--isolation=process` (future-work #7)
restores separate failure domains from the same binary for operators who
want them.

**Does this couple the two teams' releases?**
No. Both develop and release independently; a bundle is a *tested pair* of
existing releases. A security release on either side rebuilds against the
last known-good counterpart instead of waiting for the other team.

**Can the embedded halves still talk to external counterparts?**
Yes, deliberately. `ethereum-node beacon --execution-endpoint=<any EL>` is
the stock Prysm CLI running embedded, and the embedded geth keeps authrpc on
127.0.0.1:8551 so an external CL can attach. Cross-client compatibility is
why the serialized Engine API path never goes away.

**Why is the validator not in-process too?**
Keys want their own security and failure domain, and slashing risk makes that
non-negotiable for anything beyond toys. A `--with-validator` devnet
convenience could exist later; the default split stays.

**What is the licensing situation?**
`internal/prysmapp/` is a copy of GPL-3.0 Prysm code and the binary links
geth (LGPL-3.0/GPL-3.0), so this repo must carry a GPL-3.0 LICENSE before
any distribution of source or binaries.

## Upstream observations

Small gaps and paper cuts noticed in the clients while composing them — none
serious, all candidates for upstream issues/PRs. Kept here so they don't get
lost.

**go-ethereum (v1.17.4):**

- `chain/inserts` (`core/blockchain.go:102`) is declared but never updated —
  a dead metric. There is consequently no exported block-import timing
  summary; dashboards must take timing from the CL side.
- `cmd/utils.RegisterEthService` and friends call `Fatalf` (process exit)
  instead of returning errors — hostile to embedding, which the rest of the
  geth API surface is otherwise very good at.
- An IPC path longer than the OS socket limit (~104 chars on macOS) warns but
  then fails with a cryptic `bind: invalid argument`; failing fast with the
  warning's text would save operators a confused minute.

**Prysm (develop @ ce28535):**

- `genesis/initialize.go:41` logs `genesis provider failed` *without the
  underlying error* (`err` is dropped). A misconfigured checkpoint URL is
  invisible: the run dies later with a generic "genesis state has not been
  initialized". One `WithError(err)` fixes it.
- No latency histograms for `engine_newPayload` / `engine_forkchoiceUpdated`
  (only the getBlobs family has duration metrics) — the two hottest engine
  calls are unobservable without log parsing.
- `cmd/beacon-chain` is `package main` with configuration read through the
  `cli.Context` across packages, so embedding requires copying `main.go`. An
  importable app package (e.g. `runtime/beaconapp`) would make the beacon
  node composable; this repo is the concrete consumer.
- The replace directives in go.mod burden every downstream consumer (Go
  ignores replaces in dependencies): the vendored `third_party/go-bip39` is
  functionally identical to upstream `tyler-smith/go-bip39@v1.1.0` and looks
  Bazel-era; the json-iterator fork pin may also be worth revisiting.
- With `--interop-num-validators` deprecated, spinning up devnet validators
  requires a full wallet ceremony around the deterministic interop keys
  (this repo grew a `devnet-wallet` command for it); a supported lightweight
  devnet path would be friendlier.
- At startup on a fresh chain, eth1 follow-distance checks log at ERROR level
  ("Beacon node is not respecting the follow distance") for a benign,
  self-resolving condition — WARN would match its severity.
