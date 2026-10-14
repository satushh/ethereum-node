# Engine-transport A/B harness (branch engine-typed-ab)

Measures the engine calls per transport leg on the local devnet, following
the README's benchmark discipline: same binary, one flag flipped, warmup
discarded, guardrails checked.

```sh
bench/run-legs.sh /tmp/ab-out            # ipc, inproc, typed; 90 slots each
bench/run-legs.sh /tmp/ab-out typed      # one leg
python3 bench/analyze.py /tmp/ab-out ipc inproc typed
```

Per leg it boots a fresh devnet (`ENGINE_TRANSPORT=<leg>`), waits 10 warmup
slots, snapshots the beacon exporter, measures 90 slots while sampling the
node's RSS/CPU every 15s, snapshots again, and tears down verifiedly. The
analyzer diffs the histogram snapshots (prysm's own engine latency
histograms; sub-millisecond resolution needs the float-observation fix from
the sibling prysm branch) and prints mean/p50/p90/p99 per call plus
blobs/block, proposal counts, and finality.

Validity checks before believing a run: every leg should show n=90
getPayload calls (90/90 proposals), ~4.0 blobs/block under default spam,
and `<leg>.ps.samples` line count ≈ window/15 (fewer means the machine
slept mid-leg; slots are wall-clock, so a slept leg is fiction; rerun it).

`summary.json` holds the run behind the numbers in the README's Prysm PR 5
section (2026-10-14, M-series laptop).
