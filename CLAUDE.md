# ethereum-node — working notes for agents

One binary running unmodified Geth (execution) + Prysm (beacon) as modules of
one process, joined by the standard Engine API over geth's in-datadir IPC
socket. The README is the source of truth and is kept rigorously accurate —
three external review rounds enforce that every claim is verified.

## Commands

```sh
go build -o bin/ethereum-node ./cmd/ethereum-node   # the binary
go vet ./...
scripts/check-pair.sh   # geth/prysm pair invariants + versions.lock (CI runs it)
scripts/devnet-up.sh [delay-secs]    # full devnet: node+validator+spam+grafana
scripts/devnet-up.sh down            # verified stop of everything
NO_OBSERVABILITY=1 WAIT_FOR_BLOCKS=1 scripts/devnet-up.sh 20   # CI mode
scripts/testnet-up.sh hoodi          # public network follower
```

CI (`.github/workflows/devnet-smoke.yml`) runs the devnet to real blocks and
asserts verified teardown — keep it green.

## Invariants (violating these is always a bug)

- **Never patch the upstream clients.** Both are pinned module versions from
  the proxy. The single exception is `internal/prysmapp/prysmapp.go`, a
  deliberate near-verbatim copy of prysm's `cmd/beacon-chain/main.go`
  (upstream is `package main`) — keep it byte-close and re-diff on every bump.
- **The linked go-ethereum version must be exactly what prysm's go.mod
  requires.** Prysm moves; geth follows.
- **Mirror prysm's go.mod replace directives** in ours (Go ignores replaces
  declared in dependencies). Currently only the json-iterator fork; the
  vendored go-bip39 is functionally identical to upstream and is not mirrored.
- **Never transcribe upstream option lists.** Config delegates to each
  client's own loader (`consensus.settings` → prysm's config-file loader,
  `execution.settings` → geth-native TOML). Typos must fail loudly.
- **Precedence:** CLI flags > config file > defaults; supervisor-owned
  topology (datadirs, engine socket, ports, network posture, genesis) is
  applied after settings decode and wins.
- **Scripts:** long-running processes launch via absolute `$PWD/bin/...`
  paths, are tracked by pid files under `run/`, and are stopped with
  validate → identity-check → TERM → wait → KILL → verify. Never `pkill` a
  pattern that could match another checkout.

## Map

- `cmd/ethereum-node/` — supervisor CLI, config file (`config.go`), wallet
- `internal/gethapp/` — geth embedded as a library (config structs in)
- `internal/prysmapp/` — prysm's CLI made importable (synthesized argv in)
- `configs/` — node config files; `devnet/chain-config.yml` — beacon chain
  params (a different layer than `configs/devnet.yaml`)
- `observability/` — prometheus + grafana compose; dashboards are validated
  against live exporter metric names (they drift across releases)
- `packaging/` — nfpm `.deb` spec + systemd unit; `release.yml` signs and
  publishes the apt repo to `gh-pages` on tag push (setup: packaging/README.md)
- Branch `isolation-ab` — `--isolation=process` + the A/B benchmark harness

## Process notes

- Version bumps: follow the README's "Version bump checklist" exactly.
- macOS: system sleep freezes devnets (slots are wall-clock); unix-socket
  paths cap at ~104 chars, so keep datadirs shallow.
- Devnet genesis funding must happen before the CL state is computed
  (two-pass `--geth-genesis-json-in`); the CL genesis embeds the EL genesis
  hash.
- Commit in small logical units with reasoned messages; README edits
  accompany behavior changes in the same or adjacent commit.
