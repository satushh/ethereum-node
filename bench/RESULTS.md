# A/B: one process vs supervised child processes

First measured comparison of the two isolation modes — **same binary, same
resolved config, same IPC-JSON engine transport; only the process boundary
differs.** Run 2026-07-30 on an Apple-silicon laptop (14 cores), macOS.

Setup per leg: fresh 6s-slot devnet (mainnet preset otherwise), 64
validators, 50 tx/s spam (fully saturated: 300 tx in every block), 200
slots, first 32 slots discarded as warmup. Collected from the node's own
logs (`sinceSlotStartTime`) and 5s RSS sampling summed across the node's
processes (validator and spammer excluded).

| Metric | `--isolation=single` | `--isolation=process` | delta |
|---|---|---|---|
| Blocks / txs (identical work) | 176 / 52,800 | 176 / 52,800 | — |
| Proposal path, median | 154.3 ms | 154.4 ms | ~0 |
| Proposal path, p90 | 167.4 ms | 168.7 ms | ~0 |
| Proposal path, p99 | 178.9 ms | 200.3 ms | −21 ms (−11%) |
| Import path, median | 192.6 ms | 193.2 ms | ~0 |
| Import path, p90 | 210.2 ms | 212.2 ms | ~0 |
| Import path, p99 | 245.4 ms | 254.9 ms | −10 ms (−4%) |
| RSS, mean | **283 MiB** | **378 MiB** | **−95 MiB (−25%)** |
| RSS, max | 354 MiB | 439 MiB | −85 MiB |

## Reading

- **Latency medians are indistinguishable.** Exactly what the README
  predicted: at devnet scale, engine-API transport cost is noise next to
  execution and BLS work, and both modes use the same IPC-JSON transport
  anyway. This measurement isolates the *process boundary*, not the
  transport — the transport ladder (in-proc, typed calls) is future work
  and would need this harness re-run per rung.
- **The memory win is real and structural: ~95 MiB (~25%) lower RSS** for
  one process instead of three. That is one Go runtime (heap, GC metadata,
  scheduler, code pages) instead of three — the "collapsed duplicated
  runtime overhead" claim, now with a number. Expect the absolute gap to
  matter more on small machines (Raspberry-class stakers) than on servers.
- **Tail latencies (p99) lean single-process** — plausible (fewer
  cross-process wakeups on the payload handoff path) but n≈2 samples at
  p99 per leg; treat as suggestive until repeated.

## Caveats

Single run per mode (methodology calls for 3×); devnet scale (no real
network, gossip, or state size); 6s slots; both legs share the machine
with the OS. Reproduce with:

```sh
bench/run-one.sh single 200 50 && bench/run-one.sh process 200 50
```
