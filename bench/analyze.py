#!/usr/bin/env python3
"""Diff prometheus histogram snapshots per A/B leg and print the comparison.

Usage: analyze.py <outdir> [legs...]
"""
import sys, re, json

HISTS = [
    ("get_payload_v1_latency_milliseconds", "engine_getPayload"),
    ("new_payload_v1_latency_milliseconds", "engine_newPayload"),
    ("forkchoice_updated_v1_latency_milliseconds", "engine_forkchoiceUpdated"),
    ("commitment_count_max_21", "blobs per block"),
]

def parse(path):
    out = {}
    for line in open(path):
        if line.startswith("#"):
            continue
        m = re.match(r'^(\w+)(?:\{([^}]*)\})? ([0-9eE+.na-]+)', line)
        if not m:
            continue
        name, labels, val = m.group(1), m.group(2) or "", m.group(3)
        try:
            out[(name, labels)] = float(val)
        except ValueError:
            pass
    return out

def hist_delta(t0, t1, base):
    s = t1.get((base + "_sum", ""), 0) - t0.get((base + "_sum", ""), 0)
    c = t1.get((base + "_count", ""), 0) - t0.get((base + "_count", ""), 0)
    buckets = []
    for (name, labels), v1 in t1.items():
        if name != base + "_bucket":
            continue
        le = re.search(r'le="([^"]+)"', labels).group(1)
        d = v1 - t0.get((name, labels), 0)
        buckets.append((float("inf") if le == "+Inf" else float(le), d))
    buckets.sort()
    return s, c, buckets

def quantile(buckets, count, q):
    if not count:
        return float("nan")
    target = q * count
    prev_le, prev_cum = 0.0, 0.0
    for le, cum in buckets:
        if cum >= target:
            if le == float("inf"):
                return prev_le
            if cum == prev_cum:
                return le
            return prev_le + (le - prev_le) * (target - prev_cum) / (cum - prev_cum)
        prev_le, prev_cum = le, cum
    return buckets[-1][0] if buckets else float("nan")

def main():
    outdir = sys.argv[1]
    legs = sys.argv[2:] or ["ipc", "inproc", "typed"]
    rows = {}
    for leg in legs:
        t0 = parse(f"{outdir}/{leg}.t0.metrics")
        t1 = parse(f"{outdir}/{leg}.t1.metrics")
        slots = int(open(f"{outdir}/{leg}.t1.slot").read()) - int(open(f"{outdir}/{leg}.t0.slot").read())
        rows[leg] = {"slots": slots}
        for base, label in HISTS:
            s, c, b = hist_delta(t0, t1, base)
            rows[leg][label] = {
                "count": c,
                "mean": s / c if c else float("nan"),
                "p50": quantile(b, c, 0.50),
                "p90": quantile(b, c, 0.90),
                "p99": quantile(b, c, 0.99),
            }
        rss = [float(l.split()[0]) for l in open(f"{outdir}/{leg}.ps.samples") if l.strip()]
        cpu = [float(l.split()[1]) for l in open(f"{outdir}/{leg}.ps.samples") if l.strip()]
        rss.sort(); cpu.sort()
        rows[leg]["rss_mib_median"] = rss[len(rss)//2] / 1024 if rss else float("nan")
        rows[leg]["cpu_pct_median"] = cpu[len(cpu)//2] if cpu else float("nan")
        fin = json.load(open(f"{outdir}/{leg}.finality.json"))["data"]
        rows[leg]["finalized_epoch"] = fin["finalized"]["epoch"]
        blk = json.load(open(f"{outdir}/{leg}.latestblock.json"))["result"]
        rows[leg]["last_blobgas"] = int(blk.get("blobGasUsed", "0x0"), 16)

    for leg in legs:
        r = rows[leg]
        print(f"\n== {leg}  (slots={r['slots']}, finalized_epoch={r['finalized_epoch']}, "
              f"rss_med={r['rss_mib_median']:.0f}MiB, cpu_med={r['cpu_pct_median']:.0f}%, "
              f"last_blobGasUsed={r['last_blobgas']})")
        for _, label in HISTS:
            m = r[label]
            unit = "" if "blobs" in label else " ms"
            print(f"  {label:28s} n={m['count']:5.0f}  mean={m['mean']:8.3f}{unit}  "
                  f"p50={m['p50']:8.3f}  p90={m['p90']:8.3f}  p99={m['p99']:8.3f}")
    print()
    json.dump(rows, open(f"{outdir}/summary.json", "w"), indent=1)
    print(f"summary written to {outdir}/summary.json")

if __name__ == "__main__":
    main()
