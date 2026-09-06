#!/usr/bin/env python3
"""Compare re-measured bench rows against the committed results.txt.

Reads the committed file plus one measured file per row (each the full results.txt
shape, but with only its own row freshly measured) and writes a markdown report.

The report is triage, not a dump. A raw diff treats a 0.4% wobble and a doubled row
the same, and says nothing at all about a measurement that failed — so this splits
what it finds into: failures, outliers, ordinary movement, and provenance drift.

Provenance is checked FIRST and on purpose. A row's Ir/op is the instruction count
of a particular binary: generated code, built by a toolchain, against a corelib
checkout, from a schema. The header records all three, and a row can move a long way
because one of them moved — the measuring runner and the devcontainer that produced
results.txt pin different compilers, and run.sh clones the corelibs unpinned — so the
report names them before anyone reads a number as a regression.

Exit status is 1 only when a measurement FAILED, never for drift of any kind: a row
that moved is information, a row that could not be measured is a broken run. That
matters most for the corelib SHAs, which differ on most runs by design.
"""

import argparse
import json
import re
import sys
from collections import namedtuple
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

# The cells, plus every header line that is a claim about what produced them. All
# three claims are compared: a row's number is the instruction count of generated
# code, built by a toolchain, against a corelib checkout, from a schema — and any of
# the three can move a row with the generator unchanged (#501).
Parsed = namedtuple("Parsed", "ir sizes tools corelibs schemas notes")


def parse(path):
    """-> Parsed(ir, sizes, tools, corelibs, schemas, notes).

    ir: {row: (enc, dec)}; sizes: {(row, arch): (text, data, bss)};
    tools: {(tool, row): version} from the `## toolchain` table -- ONE entry per
    row the line names, and row None for a line whose rows column is `all`.

    corelibs: {short repo name: sha} off the `# corelib:` line, which has no rows
    column: it is one statement about the whole file. `notes` holds the entries of
    that line that could not be read at all.

    schemas: {(path, row): sha256} off the `# schema:` lines -- the same shape as
    `tools`, compared by schema_diff(). A `# schema:` line with no rows column is
    format.py's DEFAULT schema, the one every row uses unless it names its own, and
    is stored under row None; see schemas_for() for why that resolves differently
    from the toolchain's `all`, and schema_diff() for why the comparison differs too.

    Keyed by the pair and not by the tool because the table is not a tool->version
    map: `sofab-engine` gets one line PER python row, deliberately, since the two
    rows run different corelib-py engines and a single line could only be right for
    one of them. Keyed by name alone the second line overwrote the first, so a
    healthy `python` artifact read as engine drift and a `python` row that really had
    flipped engines read as no drift at all (#492).
    """
    ir, sizes, tools, corelibs, schemas, notes = {}, {}, {}, {}, {}, []
    section = None
    for line in Path(path).read_text().splitlines():
        if line.startswith("## "):
            section = line[3:].strip()
            continue
        if line.startswith("# corelib:"):
            # "<repo> <sha> | <repo> <sha> | ...". "(unknown)" is kept rather than
            # skipped: format.py writes it when a checkout could not be resolved, and
            # a run that could not say what it built against, where the committed
            # file could, is a difference worth naming.
            for part in line.split(":", 1)[1].split("|"):
                f = part.split()
                if len(f) == 2:
                    corelibs[f[0]] = f[1]
                elif part.strip():
                    # Reflowed, annotated or hand-edited: not two fields, so not a
                    # SHA this reader can compare. Dropped silently that corelib
                    # simply left the comparison and the report read as agreement --
                    # the same silent loss format.py's parse_previous() refuses on
                    # this very line. Recorded here and printed by main().
                    notes.append(part.strip())
            continue
        if line.startswith("# schema:"):
            # "# schema:  <path>  sha256 <digest>[  rows: a,b,c]".
            f = line.split()
            if len(f) >= 5 and f[3] == "sha256":
                # A line that NAMES a rows column describes exactly the rows it
                # lists -- including none at all, if it lists none. Only a line with
                # no rows column is format.py's default, covering every row that
                # names no schema of its own, so the two must not be conflated: a
                # trailing bare `rows:` read as the default would attribute one
                # row's schema to every row in the file.
                if len(f) > 5 and f[5] == "rows:":
                    where = f[6].split(",") if len(f) > 6 else []
                else:
                    where = [None]
                for r in where:
                    schemas[(f[2], r)] = f[4]
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
    return Parsed(ir, sizes, tools, corelibs, schemas, notes)


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
    """{tool: version} as the parsed `## toolchain` table states it for ONE row.

    That is also the filter: every measured file carries the full table, so without
    it a go artifact would report the C++ or Zig compiler as drifted — true, and
    irrelevant to the number being judged. A line naming the row wins over the `all`
    wildcard, which is what a per-row `sofab-engine` line is for.
    """
    out = {name: v for (name, r), v in tools.items() if r is None}
    out.update({name: v for (name, r), v in tools.items() if r == row})
    return out


def schemas_for(schemas, row):
    """{path: digest} for the schema ONE row was measured from.

    The `# schema:` table resolves differently from the toolchain one, and reusing
    tools_for() here would over-attribute. format.py writes the default line with no
    rows column at all, meaning "every row that does not name its own" — not `all`.
    So a line naming this row REPLACES the default rather than joining it, and an
    edit to the top-level schema says nothing about a row measured on another one.
    """
    own = {path: d for (path, r), d in schemas.items() if r == row}
    return own or {path: d for (path, r), d in schemas.items() if r is None}


def tool_diff(old, new, row):
    """Differing toolchain entries as (tool, committed, measured), for THIS row.

    Only the toolchain: the `# schema:` digests have the same table shape but not the
    same fallback, and go through schema_diff().

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


def schema_diff(old, new, row):
    """Differing `# schema:` entries as (path, committed, measured), for THIS row.

    Deliberately not tool_diff(). Its fallback -- an entry this run names that the
    committed table has no line for borrows the version recorded for that name
    elsewhere -- is right for the toolchain, where a name present on one side only
    means a tool added since results.txt was written. For a schema it is inert, and
    it hides the largest schema change there is: a row measured on a DIFFERENT file.
    schemas_for() yields exactly one entry per row (format.py writes one line per
    distinct file, and a row is either named by one of them or covered by the
    default), so the borrow finds only the measured file's own digest and the
    `committed != v` guard can never fire. Measured on the committed results.txt with
    `cpp-cpp-unbounded` moved onto a new `wide_ingest.yaml` line in every artifact,
    the whole report was `Every measured row matches the committed file within 0.3%.`

    So here a path the committed file does not attribute to this row is compared
    against what it DOES attribute to it, path and digest both -- the path is the
    change. That covers the mirror case too: a row whose own line was deleted, which
    falls back onto the default schema.
    """
    was, now = schemas_for(old, row), schemas_for(new, row)
    out = []
    for path, digest in sorted(now.items()):
        if path in was:
            committed = was[path]
        elif was:
            committed = ", ".join(f"{p} {d}" for p, d in sorted(was.items()))
        else:
            committed = "(not recorded)"
        if committed != digest:
            out.append((path, committed, digest))
    return out


def rows_col(rows_, seen):
    """The `rows` cell of a drift table: `all` when every artifact that stated this
    kind of provenance shows the difference, `all but <ids>` when it is shorter to
    name the exceptions, else the rows that do.

    One runner measures every row, so the same compiler difference is repeated in
    twenty-four artifacts and reads as one line. `seen` is the artifacts that made
    the claim at all, which is not the same set for each table -- bench.yml's
    per-row artifacts each name only the corelib they cloned.

    The `all but` form is not cosmetic. Editing the DEFAULT schema is the headline
    case of the schema comparison, and it covers every row except the four measured
    on another file: enumerated, that is one markdown cell holding twenty back-ticked
    ids, which scrolls sideways in a step summary and buries the interesting half --
    the rows the edit could NOT have moved. Whichever side is shorter is printed.
    """
    missing = [r for r in sorted(seen) if r not in rows_]
    if not missing:
        return "all"
    if len(missing) < len(rows_):
        return "all but " + ", ".join(f"`{r}`" for r in missing)
    return ", ".join(f"`{r}`" for r in rows_)


def corelib_of_row():
    """{row id: short corelib name} out of rows.json, or {} if it cannot be read.

    The `# corelib:` line has no rows column, so without this the corelib table's
    `rows` cell would mean "the artifacts that stated this SHA" while the identically
    headed toolchain column means "the rows this difference could have moved". In
    bench.yml's shape the two coincide, because each per-row artifact names only the
    corelib that row cloned; they diverge the moment an artifact carries the whole
    line (a local full run, or any --partial merge), and then a moved corelib-zig is
    listed against a java row that never links it -- a blanket excuse handed to the
    reader for the one number the report should be pointing at.

    rows.json is a cross-check, not an input: unreadable, it falls back to today's
    behaviour rather than failing a run over a file the report does not need.
    """
    try:
        spec = json.loads((Path(__file__).resolve().parent.parent / "rows.json")
                          .read_text())
        return {r["id"]: r["corelib"].replace("corelib-", "")
                for r in spec["rows"] if "id" in r and "corelib" in r}
    except (OSError, ValueError, TypeError, KeyError):
        return {}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--committed", required=True)
    ap.add_argument("--measured", nargs="+", required=True,
                    help="measured result files; the row is taken from the filename")
    args = ap.parse_args()

    base = parse(args.committed)
    base_ir, base_sz, base_tool = base.ir, base.sizes, base.tools

    failures, outliers, moved = [], [], []
    tools_seen, cores_seen, schemas_seen = {}, {}, {}
    # Entries of a `# corelib:` line that could not be read at all, per file. A
    # corelib that silently leaves the comparison reads as agreement.
    unreadable = [("the committed file", base.notes)] if base.notes else []

    for path in args.measured:
        m = re.search(r"bench-(.+)\.txt$", Path(path).name)
        if not m:
            continue
        row = m.group(1)
        try:
            got = parse(path)
        except OSError as e:
            failures.append((row, f"unreadable measured file: {e}"))
            continue
        ir, sz = got.ir, got.sizes
        if got.tools:
            # The whole table; tool_diff() narrows it to the tools that built THIS
            # row, per the measured file's own rows column.
            tools_seen[row] = got.tools
        # Recorded only when the artifact states them at all: an absent header line
        # is a file that makes no claim, not a claim that the provenance is empty.
        if got.corelibs:
            cores_seen[row] = got.corelibs
        if got.schemas:
            schemas_seen[row] = got.schemas
        if got.notes:
            unreadable.append((f"`{row}`", got.notes))

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
            out.append(f"| {name} | {old} | {new} | {rows_col(rows_, tools_seen)} |")
        out.append("")

    # The corelib the row was built against, and the schema it was built from. Both
    # decide a number exactly as the compiler does, both were recorded in the header
    # and neither was ever compared (#501).
    core_drift = {}
    builds = corelib_of_row()
    for row, cores in sorted(cores_seen.items()):
        for repo, sha in sorted(cores.items()):
            # The rows column has to mean the same thing as the toolchain table's, so
            # a SHA is attributed only to a row that repo actually builds. A row
            # rows.json does not know (renamed, or a synthetic file) keeps the old
            # behaviour: the artifacts that stated the SHA.
            if builds.get(row, repo) != repo:
                continue
            # Only what the artifact itself states: a repo it does not name is a
            # corelib this run had no opinion about, not one that vanished.
            was = base.corelibs.get(repo, "(not recorded)")
            if was != sha:
                core_drift.setdefault((repo, was, sha), []).append(row)
    if core_drift:
        out += [
            "### Corelib checkouts differ from the committed file",
            "",
            "Context, not a finding: run.sh clones the corelibs unpinned, so a moved",
            "SHA is the ordinary case. It never affects the exit status; why it is",
            "reported at all is in tests/bench/README.md.",
            "",
            "| corelib | committed | this run | rows |",
            "|---|---|---|---|",
        ]
        for (repo, old, new), rows_ in sorted(core_drift.items()):
            out.append(f"| {repo} | {old} | {new} | {rows_col(rows_, cores_seen)} |")
        out.append("")
    for where, parts in unreadable:
        out += [f"Could not read {len(parts)} "
                f"entr{'y' if len(parts) == 1 else 'ies'} from the `# corelib:` line "
                f"in {where} ({', '.join('`' + x + '`' for x in parts)}); those "
                "checkouts are not compared.", ""]

    schema_drift = {}
    for row, sch in sorted(schemas_seen.items()):
        for name, old, new in schema_diff(base.schemas, sch, row):
            schema_drift.setdefault((name, old, new), []).append(row)
    if schema_drift:
        out += [
            "### Schema digests differ from the committed file",
            "",
            "The message definition is an input to the generated code, so an edited",
            "schema legitimately moves every number measured on it — and only those.",
            "Rows are compared against the digest their own schema line names.",
            "",
            "| schema | committed | this run | rows |",
            "|---|---|---|---|",
        ]
        for (name, old, new), rows_ in sorted(schema_drift.items()):
            out.append(f"| {name} | {old} | {new} | {rows_col(rows_, schemas_seen)} |")
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

    differs = [what for what, d in (("toolchains", drift),
                                    ("corelib checkouts", core_drift),
                                    ("schemas", schema_drift)) if d]
    if differs and (outliers or moved):
        out += [f"> Provenance differs ({', '.join(differs)}; see above), so movement",
                "> here is not by itself evidence that the generated code changed.", ""]

    sys.stdout.write("\n".join(out) + "\n")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
