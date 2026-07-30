#!/usr/bin/env python3
"""Extract benchmark distributions from an ethereum-node devnet log.

Reads the combined log, strips ANSI, and reports for slots >= warmup:
  build   - "Finished building block"  sinceSlotStartTime (proposal path)
  import  - "Synced new block"         sinceSlotStartTime (gossip+import path)
  sttrans - "Finished applying state transition" per-block txCount
Usage: collect.py <node.log> [warmup_slot=32]
"""
import json
import re
import statistics
import sys

ANSI = re.compile(r"\x1b\[[0-9;]*m")
DUR = re.compile(r"(?:(\d+)m)?(?:([\d.]+)s)?(?:([\d.]+)ms)?(?:([\d.]+)µs)?$")


def parse_dur_ms(s):
    m = DUR.match(s)
    if not m or not any(m.groups()):
        return None
    mins, secs, ms, us = m.groups()
    total = 0.0
    if mins:
        total += float(mins) * 60_000
    if secs:
        total += float(secs) * 1_000
    if ms:
        total += float(ms)
    if us:
        total += float(us) / 1_000
    return total


def pct(vals, p):
    vals = sorted(vals)
    return vals[min(len(vals) - 1, int(len(vals) * p))]


def main(path, warmup=32):
    build, imp, txs = [], [], []
    for raw in open(path, errors="ignore"):
        line = ANSI.sub("", raw)
        slot = re.search(r"slot=(\d+)", line)
        if slot and int(slot.group(1)) < warmup:
            continue
        if "Finished building block" in line or "Synced new block" in line:
            m = re.search(r"sinceSlotStartTime=([\dm.µs]+)", line)
            d = parse_dur_ms(m.group(1)) if m else None
            if d is None:
                continue
            (build if "building" in line else imp).append(d)
        elif "Finished applying state transition" in line:
            m = re.search(r"txCount=(\d+)", line)
            if m:
                txs.append(int(m.group(1)))

    def dist(vals):
        if not vals:
            return None
        return {
            "n": len(vals),
            "median_ms": round(statistics.median(vals), 2),
            "p90_ms": round(pct(vals, 0.90), 2),
            "p99_ms": round(pct(vals, 0.99), 2),
        }

    print(json.dumps({
        "build": dist(build),
        "import": dist(imp),
        "blocks": len(txs),
        "txs_total": sum(txs),
        "txs_per_block_median": statistics.median(txs) if txs else 0,
    }, indent=1))


if __name__ == "__main__":
    main(sys.argv[1], int(sys.argv[2]) if len(sys.argv) > 2 else 32)
