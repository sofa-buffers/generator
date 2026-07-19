#!/usr/bin/env python3
"""Compare re-measured bench rows against the committed results.txt.

Reads the committed file plus one measured file per row (each the full results.txt
shape, but with only its own row freshly measured) and writes a markdown report.

The report is triage, not a dump. A raw diff treats a 0.4% wobble and a doubled row
the same, and says nothing at all about a measurement that failed — so this splits
what it finds into: failures, outliers, ordinary movement, and toolchain drift.

Toolchain drift is checked FIRST and on purpose. The measuring runner and the
devcontainer that produced results.txt pin different compiler versions, and Ir/op is
the instruction count of a particular binary. A row can move a long way for that
reason alone, so the report has to name it before anyone reads a number as a
regression.

Exit status is 1 only when a measurement FAILED, never for drift: a row that moved is
information, a row that could not be measured is a broken run.
"""

import argparse
import re
import sys
from pathlib import Path

# Below this a reading is noise: results.txt itself holds a cell until it moves this
# far (lib/format.py), so anything under it would not have changed the file.
HOLD_PCT = 0.3

# Above this a row is called out separately. Not a statistical bound — a threshold
# low enough to catch a real codegen regression and high enough that JIT rows do not
# trip it every run.
OUTLIER_PCT = 5.0

IR_METHODS = ("toggle", "subtract")


def parse(path):
    """-> (ir, sizes, toolchain, corelib). ir: {row: (enc, dec)} as strings."""
    ir, sizes, tool, core = {}, {}, None, None
    for line in Path(path).read_text().splitlines():
        if line.startswith("# toolchain:"):
            tool = line.split(":", 1)[1].strip()
            continue
        if line.startswith("# corelib:"):
            core = line.split(":", 1)[1].strip()
            continue
        if line.startswith("#") or not line.strip() or line.startswith("row"):
            continue
        f = line.split()
        if len(f) == 5 and f[2] in IR_METHODS:
            ir[f[0]] = (f[3], f[4])
        elif len(f) == 6 and f[2] not in IR_METHODS:
            sizes[(f[0], f[2])] = (f[3], f[4], f[5])
    return ir, sizes, tool, core


def pct(old, new):
    """Percent change, or None when either side is not a number (e.g. a '!' cell)."""
    try:
        o, n = float(old), float(new)
    except (TypeError, ValueError):
        return None
    if o == 0:
        return None
    return (n - o) / o * 100.0


def tool_diff(a, b):
    """Entries that differ between two '# toolchain:' lines, as (name, old, new)."""
    def split(s):
        out = {}
        for part in (s or "").split("|"):
            part = part.strip()
            if not part:
                continue
            name, _, ver = part.rpartition(" ")
            out[name.strip() or part] = ver.strip()
        return out

    old, new = split(a), split(b)
    # Only tools the measured run actually used. A row's job installs what that row
    # needs, so the cross compilers are absent from (say) the go job — reporting
    # that as a difference would bury the real ones.
    return [(k, old.get(k, "(not recorded)"), new[k])
            for k in sorted(new) if old.get(k) != new[k]]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--committed", required=True)
    ap.add_argument("--measured", nargs="+", required=True,
                    help="measured result files; the row is taken from the filename")
    args = ap.parse_args()

    base_ir, base_sz, base_tool, base_core = parse(args.committed)

    failures, outliers, moved, tools_seen = [], [], [], {}

    for path in args.measured:
        m = re.search(r"bench-(.+)\.txt$", Path(path).name)
        if not m:
            continue
        row = m.group(1)
        try:
            ir, sz, tool, _ = parse(path)
        except OSError as e:
            failures.append((row, f"unreadable measured file: {e}"))
            continue
        if tool:
            tools_seen[row] = tool

        vals = ir.get(row)
        if not vals:
            # No row at all: the matrix job died before writing, or the row name
            # changed. Either way this is a broken run, not a quiet no-op.
            failures.append((row, "no measured value in the artifact"))
            continue

        for label, new, old in (("encode", vals[0], (base_ir.get(row) or ("", ""))[0]),
                                ("decode", vals[1], (base_ir.get(row) or ("", ""))[1])):
            if new == "!":
                failures.append((row, f"{label}: measurement failed (`!`)"))
                continue
            if not old:
                moved.append((row, label, "-", new, None))
                continue
            d = pct(old, new)
            if d is None:
                failures.append((row, f"{label}: non-numeric cell ({old!r} -> {new!r})"))
            elif abs(d) >= OUTLIER_PCT:
                outliers.append((row, label, old, new, d))
            elif abs(d) > HOLD_PCT:
                moved.append((row, label, old, new, d))

        for (r, arch), s in sz.items():
            if r != row:
                continue
            if "!" in s:
                failures.append((row, f"footprint {arch}: measurement failed (`!`)"))
                continue
            b = base_sz.get((r, arch))
            if b and b != s:
                d = pct(b[0], s[0])
                bucket = outliers if d is not None and abs(d) >= OUTLIER_PCT else moved
                bucket.append((row, f"footprint {arch} .text", b[0], s[0], d))

    out = ["## Bench", ""]

    # One runner measured every row, so the same difference would otherwise be
    # repeated per row. Collapse to unique (tool, committed, this run) and name the
    # rows only when a difference does not apply to all of them.
    drift = {}
    for row, t in sorted(tools_seen.items()):
        for name, old, new in tool_diff(base_tool, t):
            drift.setdefault((name, old, new), []).append(row)
    if drift:
        out += [
            "### Toolchain differs from the committed file",
            "",
            "Read this before any number below. Ir/op is the instruction count of a",
            "particular binary, so a different compiler moves rows on identical code.",
            "",
            "| tool | committed | this run | rows |",
            "|---|---|---|---|",
        ]
        for (name, old, new), rows_ in sorted(drift.items()):
            where = "all" if len(rows_) == len(tools_seen) else ", ".join(f"`{r}`" for r in rows_)
            out.append(f"| {name} | {old} | {new} | {where} |")
        out.append("")

    if failures:
        out += ["### ❌ Failed measurements", ""]
        out += [f"- `{r}` — {why}" for r, why in failures]
        out += ["", "A failed cell writes `!` and would overwrite a committed value.", ""]

    if outliers:
        out += [f"### ⚠️ Outliers (>= {OUTLIER_PCT}%)", "",
                "| row | metric | committed | this run | change |", "|---|---|---|---|---|"]
        out += [f"| `{r}` | {m} | {o} | {n} | {d:+.1f}% |"
                for r, m, o, n, d in sorted(outliers, key=lambda x: -abs(x[4] or 0))]
        out.append("")

    if moved:
        out += [f"### Moved (> {HOLD_PCT}%, under the outlier threshold)", "",
                "| row | metric | committed | this run | change |", "|---|---|---|---|---|"]
        out += [f"| `{r}` | {m} | {o} | {n} | " +
                (f"{d:+.1f}% |" if d is not None else "- |")
                for r, m, o, n, d in moved]
        out.append("")

    if not (failures or outliers or moved):
        out += ["Every measured row matches the committed file within "
                f"{HOLD_PCT}%.", ""]

    if drift and (outliers or moved):
        out += ["> Toolchains differ (see above), so movement here is not by itself",
                "> evidence of a generator or corelib regression.", ""]

    sys.stdout.write("\n".join(out) + "\n")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
