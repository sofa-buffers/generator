#!/usr/bin/env python3
"""Render tests/bench/results.txt.

The file is committed, so its whole value rests on being byte-identical when
nothing changed. Two rules follow from that and are load-bearing:

  * everything is sorted and column-aligned to fixed widths, so a real change
    touches one line rather than reflowing the table;
  * the header records the corelib SHAs and toolchain versions the numbers came
    from. If the numbers move and the header didn't, the generator did it. That
    provenance is what lets the corelibs stay unpinned (they must match the
    generated code built against them).

Rows measured this run come from --measured; any row not measured (a partial
`run.sh --rows c` run) keeps its previously committed values, parsed back out of
--previous.
"""

import argparse
import hashlib
import json
import re
import subprocess
import sys
from pathlib import Path

# (label, argv) — label is what lands in the header.
TOOLCHAINS = [
    ("gcc", ["gcc", "-dumpfullversion"]),
    ("arm-none-eabi-gcc", ["arm-none-eabi-gcc", "-dumpfullversion"]),
    ("riscv64-unknown-elf-gcc", ["riscv64-unknown-elf-gcc", "-dumpfullversion"]),
    ("rustc", ["rustc", "--version"]),
]

# Two tables: Ir is one number per row (measured on the host), footprint is one per
# (row, arch). Keeping them separate beats a wide table full of "-".
IR_COLS = ["row", "profile", "method", "encode_ir/op", "decode_ir/op"]
IR_WIDTHS = [17, 11, 10, 14, 13]

SZ_COLS = ["row", "profile", "arch", "text", "data", "bss"]
SZ_WIDTHS = [17, 11, 16, 8, 7, 6]


def tool_version(argv):
    try:
        out = subprocess.run(argv, capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.SubprocessError):
        return None
    if out.returncode != 0:
        return None
    text = out.stdout.strip() or out.stderr.strip()
    # `rustc --version` -> "rustc 1.97.1 (8bab26f4f 2026-07-14)"; keep the number.
    m = re.search(r"\d+\.\d+(\.\d+)?", text)
    return m.group(0) if m else text.splitlines()[0]


def git_sha(path):
    try:
        out = subprocess.run(
            ["git", "-C", str(path), "rev-parse", "--short", "HEAD"],
            capture_output=True, text=True, timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    return out.stdout.strip() if out.returncode == 0 else None


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()[:12]


# Ir counts as unchanged while it stays inside this band of the committed value.
#
# It has to sit above the measurement's own noise and below the smallest regression
# worth seeing. The subtract rows' documented jitter is ~0.03%
# (corelib-java/bench/run_callgrind.sh); the perf changes this tool exists to catch
# are 1%+ (see docs/perf-patches/, where the wins are tens of percent). 0.3% is an
# order of magnitude clear of both edges.
NOISE_BAND = 0.003


def stabilize(new, prev):
    """Return the Ir to commit: the previous value while `new` is within the noise
    band of it, else `new`.

    results.txt is committed, so a cell that flips run-to-run with no code change
    makes the file dirty for no reason — and a file that is always dirty is one
    nobody regenerates or trusts.

    Rounding was tried first and does not work: every deterministic rounding has
    bucket edges, and a raw value sitting on one flips regardless. The idempotence
    check caught exactly that on two of three subtract rows (csharp decode
    71100<->71200, java encode 16500<->16600 — both raws sat on a 3-s.f. edge).
    The real cause is that a JIT's instruction count is not bit-reproducible; only
    CPython and the toggle rows are.

    Hysteresis is honest about that: it holds the number still through noise and
    moves it on signal. Two properties worth knowing —

    * it is order-dependent (the committed value is the reference), which is the
      point: `results.txt` is a baseline, not a fresh reading each time;
    * a drift smaller than the band per run could accumulate unseen. Acceptable
      here because the band is ~10x the measured jitter and generator changes move
      these numbers in steps, not creeps. If that ever stops being true, tighten the
      band by raising that row's reps (which shrinks its raw jitter), rather than
      widening it.
    """
    if prev in (None, "!") or new == "!":
        return new
    try:
        n, p = float(new), float(prev)
    except ValueError:
        return new
    if p > 0 and abs(n - p) / p < NOISE_BAND:
        return prev
    return new


def parse_previous(path):
    """Recover previously committed values, so a partial `run.sh --rows c` keeps
    the rest of the file intact instead of blanking it.

    Returns (sizes, irs): {(row,arch): (text,data,bss)} and {row: (enc,dec)}.
    """
    sizes, irs = {}, {}
    if not path or not Path(path).exists():
        return sizes, irs
    for line in Path(path).read_text().splitlines():
        if line.startswith("#") or not line.strip() or line.startswith("row"):
            continue
        f = line.split()
        if len(f) == len(SZ_COLS) and f[2] not in ("toggle", "subtract"):
            sizes[(f[0], f[2])] = (f[3], f[4], f[5])
        elif len(f) == len(IR_COLS) and f[2] in ("toggle", "subtract"):
            irs[f[0]] = (f[3], f[4])
    return sizes, irs


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rows", required=True)
    ap.add_argument("--sizes", required=True)
    ap.add_argument("--irs", required=True)
    ap.add_argument("--previous")
    ap.add_argument("--root", required=True)
    ap.add_argument("--corelibs", help="TSV: <repo>\\t<checkout dir>")
    args = ap.parse_args()

    spec = json.loads(Path(args.rows).read_text())
    root = Path(args.root)

    sizes = {}
    for line in Path(args.sizes).read_text().splitlines():
        if line.strip():
            row, arch, text, data, bss = line.split("\t")
            sizes[(row, arch)] = (text, data, bss)

    irs = {}
    for line in Path(args.irs).read_text().splitlines():
        if line.strip():
            row, enc, dec = line.split("\t")
            irs[row] = (enc, dec)

    prev_sizes, prev_irs = parse_previous(args.previous)

    out = []
    out.append("# sofabgen bench results — regenerate with tests/bench/run.sh")
    out.append("#")
    out.append("# Cost of the GENERATED code plus the corelib it calls. Lower is better. This is a")
    out.append("# DIFF tool: change the generator, re-run, read `git diff`. See tests/bench/README.md.")
    out.append("#")
    out.append("#   Ir/op     instructions retired for ONE op (Callgrind). Independent of CPU clock")
    out.append("#             and OS scheduling, so it compares across machines. Host x86-64, -O3.")
    out.append("#             A cell holds its value until a reading moves >0.3% (the JIT rows are")
    out.append("#             not bit-reproducible), so noise cannot dirty this file. See README.")
    out.append("#   footprint .text/.data/.bss in bytes, -Os, cross-compiled to the targets the")
    out.append("#             footprint profiles actually ship to.")
    out.append("#")

    schema = spec["schema"]
    out.append(f"# schema:  {schema}  sha256 {sha256(root / schema)}")
    out.append("#")
    out.append("# Numbers shift when anything below shifts. Check here FIRST: if the header is")
    out.append("# unchanged and a number moved, the generator caused it.")

    tools = [(lbl, tool_version(a)) for lbl, a in TOOLCHAINS]
    tools = [(lbl, v) for lbl, v in tools if v]
    out.append("# toolchain: " + " | ".join(f"{lbl} {v}" for lbl, v in tools))

    if args.corelibs and Path(args.corelibs).exists():
        shas = []
        for line in sorted(set(Path(args.corelibs).read_text().splitlines())):
            if not line.strip():
                continue
            repo, d = line.split("\t")
            sha = git_sha(d)
            shas.append(f"{repo.replace('corelib-', '')} {sha or '(unknown)'}")
        if shas:
            out.append("# corelib:   " + " | ".join(shas))
    out.append("#")

    def fmt(vals, widths):
        return "".join(str(v).ljust(w) for v, w in zip(vals, widths)).rstrip()

    rows = sorted(spec["rows"], key=lambda r: r["id"])

    out.append("## instruction cost")
    out.append(fmt(IR_COLS, IR_WIDTHS))
    for row in rows:
        # `"ir": false` rows are known-unmeasurable with a reason recorded in
        # rows.json. Omit them entirely rather than carrying a stale value (or a
        # "!") forward from whatever the file happened to hold before.
        if not row.get("ir", True):
            continue
        measured = irs.get(row["id"])
        prev = prev_irs.get(row["id"])
        if measured:
            # Hold the committed number still through noise; move it on signal.
            vals = tuple(stabilize(m, p) for m, p in zip(measured, prev or (None, None)))
        else:
            vals = prev
        if not vals:
            continue
        out.append(fmt([row["id"], row["profile"], row["method"], *vals], IR_WIDTHS))

    out.append("")
    out.append("## footprint")
    out.append(fmt(SZ_COLS, SZ_WIDTHS))
    for row in rows:
        for arch in row["archs"]:
            key = (row["id"], arch)
            vals = sizes.get(key) or prev_sizes.get(key)
            if not vals:
                continue
            out.append(fmt([row["id"], row["profile"], arch, *vals], SZ_WIDTHS))

    sys.stdout.write("\n".join(out) + "\n")


if __name__ == "__main__":
    main()
