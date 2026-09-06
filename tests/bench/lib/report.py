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

sys.path.insert(0, str(Path(__file__).resolve().parent))
from format import NOISE_BAND  # noqa: E402  (same directory; its argv work is in main())

# Below this a reading is noise: results.txt itself holds a cell until it moves this
# far, so anything under it would not have changed the file. Taken FROM format.py
# rather than copied: a report that bucketed on a different threshold than the file it
# compares against would call a move "moved" that results.txt had held.
#
# "Noise" is the rule, not a guarantee. Per-row jitter is measured in
# tests/bench/README.md: sixteen rows repeat exactly and would show nothing under this
# threshold, while `go` encode swings 9.1% on an unchanged tree (#494) and therefore
# lands in the outlier bucket for no reason a commit caused.
HOLD_PCT = NOISE_BAND * 100

# Above this a row is called out separately. Not a statistical bound — a threshold
# low enough to catch a real codegen regression and high enough that JIT rows do not
# trip it every run.
OUTLIER_PCT = 5.0

IR_METHODS = ("toggle", "subtract")


# A `## toolchain` line with no rows column at all can only be hand-written --
# format.py always writes three fields. It is recorded under this sentinel so the
# committed side can still answer for that tool, and it matches no row, which is what
# the old set()-valued entry did. A row id is never empty, so it cannot collide.
NO_ROW = ""


def parse(path):
    """-> (ir, sizes, toolchain).

    ir: {row: (enc, dec)}; sizes: {(row, arch): (text, data, bss)};
    toolchain: {(tool, row): version} from the `## toolchain` table -- ONE entry per
    row the line names, and row None for a line whose rows column is `all`.

    Keyed by the pair and not by the tool because the table is not a tool->version
    map: `sofab-engine` gets one line PER python row, deliberately, since the two
    rows run different corelib-py engines and a single line could only be right for
    one of them. Keyed by name alone the second line overwrote the first, so a
    healthy `python` artifact read as engine drift and a `python` row that really had
    flipped engines read as no drift at all (#492).

    The header's other provenance -- the `# corelib:` SHAs and the `# schema:`
    digests, both of which move numbers exactly as a compiler version does -- is NOT
    read here and therefore not compared; #501 tracks that. It used to be parsed and
    then dropped unread, which implied a check that was never made.
    """
    ir, sizes, tools = {}, {}, {}
    section = None
    for line in Path(path).read_text().splitlines():
        if line.startswith("## "):
            section = line[3:].strip()
            continue
        if line.startswith("#") or not line.strip() or line.startswith("row"):
            continue
        if section == "toolchain":
            # Split the padded columns, not the whitespace: format.py writes
            # "(not found)" as a version when a tool has vanished from the
            # environment, and that value contains a space. Split on whitespace it
            # parsed as version "(not" with rows "found)", which names no real row --
            # so the one drift format.py goes out of its way to record was the one
            # the report could not read. Columns are ljust()-padded, so two spaces
            # always separate them. format.py's own reader has the mirror of this
            # defect and drops such a line entirely (#502).
            f = re.split(r"\s{2,}", line.strip())
            if len(f) >= 2 and f[0] != "tool":
                # "all" is format.py's shorthand for "built every row"; None here
                # means universal, so the per-row view below always picks it up.
                where = ([None] if len(f) > 2 and f[2] == "all"
                         else f[2].split(",") if len(f) > 2 else [NO_ROW])
                for r in where:
                    tools[(f[0], r)] = f[1]
            continue
        f = line.split()
        if len(f) == 5 and f[2] in IR_METHODS:
            ir[f[0]] = (f[3], f[4])
        elif len(f) == 6 and f[2] not in IR_METHODS:
            sizes[(f[0], f[2])] = (f[3], f[4], f[5])
    return ir, sizes, tools


def pct(old, new):
    """Percent change, or None when either side is not a number (e.g. a '!' cell)."""
    try:
        o, n = float(old), float(new)
    except (TypeError, ValueError):
        return None
    if o == 0:
        return None
    return (n - o) / o * 100.0


def tools_for(tools, row):
    """{tool: version} as a parsed `## toolchain` table states it for ONE row.

    That is also the filter: every measured file carries the full table, so without
    it a go artifact would report the C++ or Zig compiler as drifted — true, and
    irrelevant to the number being judged. A line naming the row wins over the `all`
    wildcard, which is what a per-row `sofab-engine` line is for.
    """
    out = {name: v for (name, r), v in tools.items() if r is None}
    out.update({name: v for (name, r), v in tools.items() if r == row})
    return out


def tool_diff(old, new, row):
    """Differing toolchain entries as (name, committed, measured), for THIS row.

    When the committed table has no line naming this row for a tool, fall back to the
    version it records for that tool elsewhere — but only when every line EITHER file
    carries for that tool agrees. That keeps a row added since results.txt was
    written from reporting every compiler as "(not recorded)" on an unchanged
    toolchain, while a tool whose version is known to vary per row never borrows a
    line that does not name this row.

    Both tables and not just the committed one, because the committed file can hold a
    single `sofab-engine` line — the state right after a second python row is added
    to rows.json and before results.txt is regenerated. One line agrees with itself
    trivially, so keyed on the committed side alone it would answer for the row it
    does not name, which is the collapse this whole change is about.

    Disagreement is the only signal available here, so the limit is exact: when every
    line either file carries for a per-row tool happens to record the SAME version,
    that tool is indistinguishable from a global one and the fallback still answers.
    It then answers with the version the measured file itself reports for the tool
    elsewhere, so the comparison says what it would have said anyway.
    """
    was, now = tools_for(old, row), tools_for(new, row)
    out = []
    for name, v in sorted(now.items()):
        if name in was:
            committed = was[name]
        else:
            elsewhere = {ver for (t, _), ver in list(old.items()) + list(new.items())
                         if t == name}
            committed = (elsewhere.pop() if len(elsewhere) == 1
                         else "(not recorded)")
        if committed != v:
            out.append((name, committed, v))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--committed", required=True)
    ap.add_argument("--measured", nargs="+", required=True,
                    help="measured result files; the row is taken from the filename")
    args = ap.parse_args()

    base_ir, base_sz, base_tool = parse(args.committed)

    failures, outliers, moved, tools_seen = [], [], [], {}

    for path in args.measured:
        m = re.search(r"bench-(.+)\.txt$", Path(path).name)
        if not m:
            continue
        row = m.group(1)
        try:
            ir, sz, tool = parse(path)
        except OSError as e:
            failures.append((row, f"unreadable measured file: {e}"))
            continue
        if tool:
            # The whole table; tool_diff() narrows it to the tools that built THIS
            # row, per the measured file's own rows column.
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
        for name, old, new in tool_diff(base_tool, t, row):
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
