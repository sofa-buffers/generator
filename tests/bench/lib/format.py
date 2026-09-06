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

Rows measured this run come from --irs/--sizes; any row not measured (a partial
`run.sh --rows c` run) keeps its previously committed values, parsed back out of
--previous. Under --partial the same applies to every header line that attributes
those values -- see parse_previous() and choose(). A partial run that cannot state a
true header refuses to write one at all, rather than stamping this run's provenance
onto cells it never measured.
"""

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
from collections import namedtuple
from pathlib import Path

# (label, argv) — label is what lands in the header.
# What compiled each row. Ir/op is the instruction count of a particular binary, so
# every one of these moves numbers on unchanged generator and unchanged corelib —
# recording only the host gcc and rustc (as this did) left five languages able to
# shift a row with nothing in the file to show for it.
LANG_TOOLCHAINS = {
    "c":          [("gcc", ["gcc", "-dumpfullversion"])],
    "cpp":        [("g++", ["g++", "-dumpfullversion"])],
    "rust":       [("rustc", ["rustc", "--version"])],
    "go":         [("go", ["go", "version"])],
    "zig":        [("zig", ["zig", "version"])],
    "typescript": [("node", ["node", "--version"])],
    "python":     [("python3", ["python3", "--version"])],
    "java":       [("javac", ["javac", "-version"])],
    # The Kotlin plugin refuses to run on a JDK newer than it knows, so this row
    # may be measured on a DIFFERENT JVM than the java row -- see
    # tests/bench/lang/kotlin.sh. Resolving the same knob the recipe does is what
    # keeps the record true: a row measured on another runtime with the table
    # showing the box default would be a second measuring device with nothing in
    # the file to show for it.
    "kotlin":     [("kotlin-jdk", ["sh", "-c",
                    'J="${SOFAB_KOTLIN_JDK:-${JAVA_HOME:-}}"; '
                    'if [ -x "$J/bin/java" ]; then exec "$J/bin/java" -version 2>&1; '
                    'else exec java -version 2>&1; fi'])],
    "csharp":     [("dotnet", ["dotnet", "--version"])],
}

# Footprint rows cross-compile, so the arch decides the compiler. Matched on the
# arch name as it appears in rows.json.
ARCH_TOOLCHAINS = [
    ("ARM",   ("arm-none-eabi-gcc", ["arm-none-eabi-gcc", "-dumpfullversion"])),
    ("RV",    ("riscv64-unknown-elf-gcc", ["riscv64-unknown-elf-gcc", "-dumpfullversion"])),
    ("thumb", ("rustc", ["rustc", "--version"])),
]

# Measures every Ir row, so its own version belongs in the record too.
HOST_TOOLCHAINS = [("valgrind", ["valgrind", "--version"])]

# Two tables: Ir is one number per row (measured on the host), footprint is one per
# (row, arch). Keeping them separate beats a wide table full of "-".
IR_COLS = ["row", "profile", "method", "encode_ir/op", "decode_ir/op"]
IR_WIDTHS = [19, 11, 10, 14, 13]

SZ_COLS = ["row", "profile", "arch", "text", "data", "bss"]
SZ_WIDTHS = [19, 11, 16, 8, 7, 6]

# Three columns on purpose: parse_previous() and report.py both read this table by
# splitting the padded columns and gating on the count, so the rows list is
# comma-joined WITHOUT spaces to keep every line at exactly three columns.
TC_COLS = ["tool", "version", "rows"]
TC_WIDTHS = [26, 14, 0]


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


def python_engine(corelib_dir, pin_pure):
    """Which corelib-py engine a row actually got — the thing that decides its number.

    corelib-py picks at import time: the compiled Cython accelerator
    (``sofab._speedups``) when it is present, else the pure-Python classes, and the
    choice is an ImportError swallowed inside ``sofab/__init__.py``. Nothing in the
    process says which one ran, and the gap between them is not subtle — the corelib
    landed "restore the accelerator's hot paths (encode 3.0x, decode 1.5x)" without
    the row moving one instruction, because the bench had never built the extension.
    A row whose engine is invisible cannot be diffed, so record it like a compiler
    version: ask the same interpreter, under the same environment the recipe used.

    ``pin_pure`` mirrors the recipe's SOFAB_PUREPYTHON pin. It is not cosmetic: both
    python rows share one corelib checkout, so once python-native has built the
    extension in it, an unpinned probe would report "native" for the pure row too and
    the table would contradict the numbers.
    """
    env = {**os.environ, "PYTHONPATH": str(Path(corelib_dir) / "src")}
    if pin_pure:
        env["SOFAB_PUREPYTHON"] = "1"
    else:
        env.pop("SOFAB_PUREPYTHON", None)
    try:
        out = subprocess.run(
            ["python3", "-c", "import sofab; print(sofab.IMPL)"],
            capture_output=True, text=True, timeout=30, env=env,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if out.returncode != 0:
        return None
    impl = out.stdout.strip()
    # "python" is the fallback engine, not a version of anything — name it so a
    # reader does not take it for the interpreter line directly above.
    return {"python": "pure", "native": "native"}.get(impl, impl or None)


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
# worth seeing. The perf changes this tool exists to catch are 1%+ (see
# docs/perf-patches/, where the wins are tens of percent), and every row's jitter has
# now been measured: three or more back-to-back `run.sh --rows <id>` on an unchanged
# tree, raw readings in tests/bench/README.md, "Measured jitter, per row".
#
# What that table says about this number:
#
#   * sixteen of the twenty-four rows reproduce EXACTLY, both halves. For those the
#     band buys nothing and hides up to 0.3%, and cells in the committed file are in
#     fact being held off a reading that repeats to the instruction. How many, at any
#     moment, is a property of the two committed files and not of this source: read
#     it off them (diff results.txt against results-raw.txt), or see the counted
#     table in tests/bench/README.md, "What the band is hiding right now".
#   * the widest deviation a reading showed from a committed value on a well-behaved
#     row is kotlin encode at 0.122% (17221 against the 17200 its readings cluster
#     on) -- one excursion in eleven readings at that row's committed reps, the rest
#     inside 0.023%. That is the quantity to compare this constant against: the band
#     is a one-sided deviation from the committed value, NOT a max-min spread. So the
#     band is ~2.5x the widest measured deviation, NOT the ~10x this comment used to
#     claim, nor the ~50x that kotlin's first three runs suggested (#473). There is
#     no headroom to narrow: 0.001 would sit BELOW that excursion and flip the cell
#     on roughly one reading in eleven, and 0.002 clears it by only 1.6x.
#   * and one row is beyond any band's reach: `go` encode is bimodal, 18625 or 20468
#     over 53 runs (9.1%, 30x this band), so that cell moves at random whatever this
#     is set to. Sizing the band cannot fix it; that is issue #494, not this one.
#
# So 0.003 stands, sized from the data rather than from a figure documented in
# corelib-java. The lever that acts on MASKING is not this constant but the raw
# sidecar next to it (results-raw.txt), which records what was actually read.
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

    "Only CPython and the toggle rows are bit-reproducible" was the reason given for
    holding at all, and it is not the split the measurements found (#489). What
    reproduces exactly is a property of the RUNTIME, not of the method: the two python
    rows, all three ts rows to within 1-5 Ir, and fourteen of the fifteen toggle rows
    repeat to the instruction, while `go` — a toggle row — is the noisiest row in the
    file by two orders of magnitude (#494). Per-row readings: tests/bench/README.md.

    Hysteresis is honest about the rows that do move. Two properties worth knowing —

    * it is order-dependent (the committed value is the reference), which is the
      point: `results.txt` is a baseline, not a fresh reading each time;
    * a change smaller than the band is held back, and stays invisible until it (or
      the sum of it and later ones) crosses. This is not hypothetical and not rare:
      kotlin decode's cell held at 32655 across four corelib-kotlin-mp bumps, a JDK
      change and the whole receiver-cap series, then moved +0.3001% in one run,
      surfacing a step that had landed weeks earlier (#488). Re-measuring every row
      found more of the same: cells held off a value that reproduces to the
      instruction in two independent measurement contexts — rust-rs decode by
      0.159%, cpp-c-cpp-dyn decode by 0.143%, zig encode by 0.121% — with no
      jitter to absorb them and nothing that owns them. The current count and the
      per-cell table live in tests/bench/README.md; they are a property of the
      committed files and move on every run, so they are not restated here.

      Two contexts is the load-bearing part. A cell that ONE re-measurement puts off
      the committed value may be a masked step, or may be the build context moving:
      the same campaign read five further cells at a THIRD value, at an identical
      corelib SHA, and those are unattributed rather than masked.

    Raising a row's reps is the answer to JITTER, not to that. It shrinks the raw
    spread; the band is a fixed 0.3% RELATIVE and does not move with it, so more reps
    cannot surface a masked step -- measured on kotlin, doubling the delta left the
    spread at the 1-2 Ir resolution floor for +30% wall clock.

    Nor can NOISE_BAND be narrowed to shrink the masking: at 0.003 it is already only
    ~2.5x the widest measured deviation from a committed value (see the constant
    above). What DOES surface a masked step is recording the reading this run
    actually took, which is what the raw sidecar written next to results.txt is for —
    a held cell is then a diff in results-raw.txt and `git log` dates it, instead of
    leaving no trace at all.
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


# The header's own lines, in the form results.txt writes them. parse_previous()
# reads them back, so the literals live in one place.
CORELIB_PREFIX = "# corelib:   "
SCHEMA_PREFIX = "# schema:  "
ENGINE_LABEL = "sofab-engine"

# What a previously committed results.txt gives back: the cells, and every header
# line that is a claim ABOUT those cells. `notes` carries whatever could not be read
# back, so a header about to be rewritten cannot degrade in silence.
Previous = namedtuple("Previous", "sizes irs corelibs engines tools schemas notes")


def parse_previous(path):
    """Recover previously committed state, so a partial `run.sh --rows c` keeps
    the rest of the file intact instead of blanking it.

    Cells: `sizes` {(row,arch): (text,data,bss)}, `irs` {row: (enc,dec)}.

    Header: `corelibs` {short repo name: sha} off the `# corelib:` line, `tools`
    {(label, rows column): version} and `engines` {row: engine} off the
    `## toolchain` table, and `schemas` {path: sha256} off the `# schema:` lines.
    Those are recovered for exactly the same reason the cells are. A partial run
    resolves a SHA only for the corelibs it checked out, probes an engine only for a
    python row it measured, and probes the host for tools and schema files that may
    have nothing to do with the row it ran — so building the header from this run
    alone dropped eleven of twelve SHAs and both engine lines, and rewrote the
    toolchain and schema lines for twenty-three rows it never touched (#487).

    "(unknown)" is not carried, for a SHA or for an engine: it is a recorded failure
    to resolve one, and re-printing it would launder it into the next run's header as
    if it were a value. A tool's "(not found)" IS carried — that one is a recorded
    fact about the environment the cells were measured in, not a failure to look.

    `tools` is keyed by the PAIR and not by the label, and the toolchain table is
    read inside its own section rather than by field count, for the two reasons
    report.py's parse() is (#492, #502). The table is not a label->version map:
    `sofab-engine` gets one line per python row, deliberately, so a label keyed
    alone collapses to the last line — and a version containing a space
    ("(not found)", the one drift the writer goes out of its way to record) yields
    four whitespace fields, matched no branch, and was dropped entirely. `engines`
    is a view of that same table rather than a second parse, so the reader has one
    rule where it used to have a rule plus an exception.
    """
    sizes, irs, corelibs, engines, tools, schemas, notes = {}, {}, {}, {}, {}, {}, []
    if not path or not Path(path).exists():
        return Previous(sizes, irs, corelibs, engines, tools, schemas, notes)
    saw_corelib = False
    section = None
    for line in Path(path).read_text().splitlines():
        if line.startswith("## "):
            section = line[3:].strip()
            continue
        if line.startswith("# corelib:"):
            saw_corelib = True
            dropped = []
            for part in line.split(":", 1)[1].split("|"):
                f = part.split()
                if len(f) == 2 and f[1] != "(unknown)":
                    corelibs[f[0]] = f[1]
                elif len(f) == 2 or not part.strip():
                    pass                      # "<repo> (unknown)", or padding
                else:
                    # A hand-edited, reflowed or annotated entry. Dropping it
                    # silently re-guts the header into exactly the #487 output, so
                    # say so rather than let the next commit carry the loss.
                    dropped.append(part.strip())
            if dropped:
                notes.append("could not read %d entr%s from the previous "
                             "'# corelib:' line (%s); their provenance is dropped"
                             % (len(dropped), "y" if len(dropped) == 1 else "ies",
                                ", ".join(repr(d) for d in dropped)))
            continue
        if line.startswith("# schema:"):
            f = line.split()
            if len(f) >= 5 and f[3] == "sha256":
                schemas[f[2]] = f[4]
            continue
        if line.startswith("#") or not line.strip() or line.startswith("row"):
            continue
        if section == "toolchain":
            # Split the padded columns, not the whitespace: a version can contain a
            # space ("(not found)"), and splitting on whitespace made that line four
            # fields, which matched neither branch and lost the record. Columns are
            # ljust()-padded, so two spaces always separate them -- but only here:
            # the Ir and footprint tables hold values that FILL their column
            # ("rust-rs-no-std-dyn" is 18 of 19), leaving one space, so the same
            # split would merge two of their fields. Hence the section, exactly as
            # report.py does it.
            f = re.split(r"\s{2,}", line.strip())
            if len(f) == len(TC_COLS) and f[0] != TC_COLS[0]:
                tools[(f[0], f[2])] = f[1]
            elif f and f[0] != TC_COLS[0]:
                # A toolchain line whose columns ran together (or that was edited by
                # hand). The writer pads so this cannot happen -- see fmt_tc() -- and
                # if it does anyway, say so: dropped in silence, the next partial run
                # re-probes that tool and writes this host's answer over rows it only
                # carried, which is exactly the #487 loss.
                notes.append("could not read the toolchain line %r; that tool is "
                             "re-probed rather than carried" % line.strip())
            continue
        f = line.split()
        if len(f) == len(SZ_COLS) and f[2] not in ("toggle", "subtract"):
            sizes[(f[0], f[2])] = (f[3], f[4], f[5])
        elif len(f) == len(IR_COLS) and f[2] in ("toggle", "subtract"):
            irs[f[0]] = (f[3], f[4])
    # The engine lines are toolchain lines like any other, so they are parsed with
    # the rest and narrowed here. "(unknown)" is dropped for the reason above: it is
    # a failed probe, and the merge must not carry it as if it were a reading.
    for (label, where), version in tools.items():
        if label == ENGINE_LABEL and version != "(unknown)":
            for rid in where.split(","):
                engines[rid] = version
    if not saw_corelib and (corelibs or irs or sizes):
        notes.append("the previous file has no '# corelib:' line; this run can only "
                     "attribute the rows it measured")
    return Previous(sizes, irs, corelibs, engines, tools, schemas, notes)


def previous_tool(tools, label, where):
    """What a parsed `## toolchain` table states for ONE line, or None.

    The line is (label, rows column), because that is what the table records: a
    label alone is not a key (`sofab-engine` has one line per python row). An exact
    hit answers; otherwise -- rows.json gained or lost a row since the previous file
    was written, so this run's rows column names a set that file never wrote -- fall
    back to the version it records for the label elsewhere, but only when every line
    it carries for that label agrees. A label whose version is known to vary per row
    then answers for nothing, which is the point: carrying one of two disagreeing
    lines onto a line that names neither would state a version the previous file
    never recorded for these rows. Same rule as report.py's tool_diff().
    """
    if (label, where) in tools:
        return tools[(label, where)]
    elsewhere = {v for (lbl, _), v in tools.items() if lbl == label}
    return elsewhere.pop() if len(elsewhere) == 1 else None


def parse_previous_raw(path):
    """The raw sidecar's cells, {row: (encode, decode)} — so a partial run carries
    the rows it did not measure instead of dropping them, exactly as results.txt does.

    Deliberately not parse_previous(): the sidecar has no header to merge (see
    render_raw), so reusing that parser would only produce a "no '# corelib:' line"
    note about a file that is not supposed to have one.
    """
    out = {}
    if not path or not Path(path).exists():
        return out
    for line in Path(path).read_text().splitlines():
        if line.startswith("#") or not line.strip():
            continue
        f = line.split()
        if len(f) == len(IR_COLS) and f[2] in ("toggle", "subtract"):
            out[f[0]] = (f[3], f[4])
    return out


def render_raw(lines):
    """The Ir readings this run actually took, with no hysteresis applied.

    results.txt holds a cell still while a reading is within NOISE_BAND of it, which
    is what keeps the committed file from wobbling — and it also means the reading is
    DISCARDED. That is not a small loss. When kotlin decode finally crossed the band
    it surfaced a step that had landed weeks and six regenerations earlier, and
    because every intermediate reading had been thrown away, the repository could not
    say which of four corelib bumps, a JDK change and 820 lines of codegen owned it
    (#488). Re-measuring every row later found more cells in the same state, on rows
    that reproduce to the instruction (#489); the count and the table are in
    tests/bench/README.md, because they are a property of the committed files.

    This file is the fix, and it is deliberately the cheapest one: the same table,
    same order, same widths, holding what was measured rather than what is committed.
    A held cell is then a diff HERE while results.txt stays still, and `git log -p`
    dates it to a run. It is a record, not a gate — nothing reads it back but the next
    run's carry-forward, and unlike results.txt it is EXPECTED to move on rows with
    real jitter, so read it for what a run saw and results.txt for the verdict.

    No header: every claim about what produced these numbers — corelib SHAs,
    toolchain versions, schema hashes — is in results.txt beside it, written by the
    same run from the same probes, and duplicating the merge machinery here would
    give two headers that could disagree.
    """
    return [
        "# RAW Ir readings — regenerate with tests/bench/run.sh. See lib/format.py.",
        "#",
        "# What tests/bench/results.txt would hold if it did not hold cells still: the",
        "# reading each row's last measurement actually produced. A row this run did not",
        "# measure keeps its previous reading, as in results.txt.",
        "#",
        "# Read results.txt for the verdict and its header for provenance. This file is",
        "# the record: a cell held back by the noise band shows up here and nowhere else,",
        "# and jittery rows (go encode above all — #494) move here without meaning",
        "# anything. See tests/bench/README.md, \"Measured jitter, per row\".",
        "#",
        "".join(str(v).ljust(w) for v, w in zip(IR_COLS, IR_WIDTHS)).rstrip(),
    ] + lines


def choose(new, old, describes, measured_ids, report):
    """Which value one header entry should state, on a partial run.

    Every entry -- a corelib SHA, a toolchain version, a schema hash, a python row's
    engine -- makes one claim: *this is what the cells below were measured against*.
    `describes` is the set of rows in this file that the entry covers, and
    `measured_ids` the rows this run actually re-measured. Four cases, and only the
    last is a problem:

    * this run resolved nothing for the entry (`new is None`) -> carry the committed
      value; the cells it describes were not re-measured either, so it is still true;
    * nothing committed to contradict, or the two agree -> this run's value;
    * this run re-measured EVERY row the entry covers -> this run's value;
    * this run re-measured some of them and the value moved -> the entry would be
      true of the cells it just measured and false of the ones it carried, and the
      line has no room to say both. `report("refuse", ...)`; main() then refuses the
      whole run rather than stamping this run's value onto cells that were never
      built with it (#487). The unmeasured half is the dangerous one: the header
      moved and those numbers did not, which reads as "the corelib bump cost them
      nothing".

    A probe that describes NO cell in this file (a `--rows kotlin` run on a box with
    no zig, whose zig rows are all carried) is not a conflict: it is a reading about
    nothing, so the committed value stands. That case is reported as "carry" so the
    run can mention it, not refuse it.
    """
    if new is None:
        return old
    if old is None or old == new:
        return new
    stale = describes - measured_ids
    if not stale:
        return new
    if describes & measured_ids:
        report("refuse", old, new, stale)
        return new
    report("carry", old, new, stale)
    return old


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rows", required=True)
    ap.add_argument("--sizes", required=True)
    ap.add_argument("--irs", required=True)
    ap.add_argument("--previous")
    # The raw sidecar (see render_raw). Both optional: a run that is not updating the
    # committed pair — an --out preview, bench.yml's per-row artifacts — writes no
    # record, because a second measuring device's readings are not this file's history.
    ap.add_argument("--raw-out", help="write the un-stabilized Ir table here")
    ap.add_argument("--previous-raw", help="the committed raw sidecar, for carry-forward")
    ap.add_argument("--root", required=True)
    ap.add_argument("--corelibs", help="TSV: <repo>\\t<checkout dir>")
    # Passed by run.sh only when it was given --rows. Deciding it here instead
    # (measured rows vs rows.json) would be wrong in the dangerous direction: a FULL
    # run legitimately measures fewer rows than rows.json lists — a backend with no
    # bench verb yet, a missing lang recipe, an `"ir": false` row — so it would look
    # partial and start carrying header lines forward. As a flag, "a full run's
    # header is entirely its own" is a property of the call rather than a guess.
    ap.add_argument("--partial", action="store_true",
                    help="only some rows were measured (run.sh --rows): merge the "
                         "header with --previous entry by entry, instead of writing "
                         "it from this run alone; refuse if no merged header would "
                         "be true")
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

    prev = parse_previous(args.previous)
    prev_raw = parse_previous_raw(args.previous_raw)

    # A partial run CARRIES the rows it did not measure, in both files. results.txt
    # gets that from --previous, which is committed and always there; the sidecar gets
    # it from --previous-raw, which may not be (deleted, or a checkout predating it).
    # With nothing to carry, `raw = measured or prev_raw.get(id)` below keeps only the
    # rows this run touched, and run.sh then copies that one-row file over the
    # committed sidecar — a permanent truncation, silent, and with no analogue of the
    # header refusal that protects results.txt. So refuse, on the same principle: a
    # partial run may not write a record it cannot make complete.
    if args.partial and args.raw_out and not prev_raw:
        print("refusing: --partial with no readings to carry in "
              f"{args.previous_raw or '(no --previous-raw)'} — a partial run would "
              "write a sidecar holding only the rows it measured. Regenerate both "
              "files with a full tests/bench/run.sh.", file=sys.stderr)
        sys.exit(2)

    def fmt(vals, widths):
        return "".join(str(v).ljust(w) for v, w in zip(vals, widths)).rstrip()

    def fmt_tc(vals):
        """The `## toolchain` row, padded so its columns can never run together.

        Both readers of that table -- parse_previous() above and report.py's
        parse() -- split it on two-or-more spaces, because a version can contain one
        ("(not found)"). A value that FILLS or overflows its column leaves no
        separator, the split merges two fields, and the line is dropped: silently, so
        the next partial run re-probes that tool and stamps this host's answer onto
        rows it only carried. tool_version() can produce such a value -- when its
        version regex misses, it returns the first line of the tool's output, of any
        length ("1.98.0-nightly+x" already overflows, and "Apple clang 17" holds a
        space as well).

        Scoped to this table on purpose: the same pad on IR_WIDTHS would move
        `rust-rs-no-std-dyn` (18 chars in a 19-wide column) and rewrite results.txt.
        Every toolchain line the committed file holds is already under its width, so
        this changes no byte of it either.
        """
        return "".join(str(v).ljust(max(w, len(str(v)) + 2))
                       for v, w in zip(vals, TC_WIDTHS)).rstrip()

    rows = sorted(spec["rows"], key=lambda r: r["id"])

    # Both tables are rendered BEFORE the header, because the header is a claim about
    # the cells this file ends up carrying: a corelib SHA carried forward for a row
    # that has no cell here would be provenance about nothing. `in_file` is that set.
    ir_lines, sz_lines, in_file = [], [], set()
    raw_lines = []

    for row in rows:
        # `"ir": false` rows are known-unmeasurable with a reason recorded in
        # rows.json. Omit them entirely rather than carrying a stale value (or a
        # "!") forward from whatever the file happened to hold before.
        if not row.get("ir", True):
            continue
        measured = irs.get(row["id"])
        prev_ir = prev.irs.get(row["id"])
        if measured:
            # Hold the committed number still through noise; move it on signal.
            vals = tuple(stabilize(m, p)
                         for m, p in zip(measured, prev_ir or (None, None)))
        else:
            vals = prev_ir
        if not vals:
            continue
        in_file.add(row["id"])
        ir_lines.append(fmt([row["id"], row["profile"], row["method"], *vals], IR_WIDTHS))
        # The same cell WITHOUT the hysteresis — see render_raw().
        raw = measured or prev_raw.get(row["id"])
        if raw:
            raw_lines.append(fmt([row["id"], row["profile"], row["method"], *raw],
                                 IR_WIDTHS))

    for row in rows:
        for arch in row["archs"]:
            key = (row["id"], arch)
            vals = sizes.get(key) or prev.sizes.get(key)
            if not vals:
                continue
            in_file.add(row["id"])
            sz_lines.append(fmt([row["id"], row["profile"], arch, *vals], SZ_WIDTHS))

    # The rows this run actually re-measured — the ones whose cells above are this
    # run's. Every header entry is a claim about a set of cells, so this is what
    # decides whether this run may state it. A `!` cell counts as measured: the run
    # did build that row against this run's corelib and toolchain.
    measured_ids = set(irs) | {row for row, _ in sizes}

    # Entries this run may not state, collected as they are found and acted on once,
    # below, so a refusal names every conflicting line rather than the first.
    refusals, carried_notes = [], []

    def report(what):
        def record(kind, old, new, stale):
            (refusals if kind == "refuse" else carried_notes).append(
                (what, old, new, sorted(stale)))
        return record

    for note in prev.notes if args.partial else []:
        print(f"warning: {note}", file=sys.stderr)

    out = []
    out.append("# sofabgen bench results — regenerate with tests/bench/run.sh")
    out.append("#")
    out.append("# Cost of the GENERATED code plus the corelib it calls. Lower is better. This is a")
    out.append("# DIFF tool: change the generator, re-run, read `git diff`. See tests/bench/README.md.")
    out.append("#")
    out.append("#   Ir/op     instructions retired for ONE op (Callgrind). Independent of CPU clock")
    out.append("#             and OS scheduling, so it compares across machines. Host x86-64, -O3.")
    out.append("#             A cell holds its value until a reading moves >0.3%, so a row that")
    out.append("#             is not bit-reproducible cannot dirty this file. What each run READ,")
    out.append("#             held cells included, is in results-raw.txt beside this. See README.")
    out.append("#   footprint .text/.data/.bss in bytes, -Os, cross-compiled to the targets the")
    out.append("#             footprint profiles actually ship to.")
    out.append("#")

    # One line per distinct schema, naming its rows once there is more than one. The
    # hash is here for the reason it always was — edit a schema and every number
    # measured on it legitimately moves, and the header says so — and it is per
    # schema for the same reason: a row measures the top-level `schema` unless it
    # names its own, so a single hash would be a true statement about some rows and
    # a false one about the rest. The default line names no rows ("all the others"),
    # which keeps it byte-stable as rows are added to it.
    by_schema = {}
    for row in spec["rows"]:
        by_schema.setdefault(row.get("schema", spec["schema"]), []).append(row["id"])
    default = spec["schema"]
    for schema in [default] + sorted(s for s in by_schema if s != default):
        digest = sha256(root / schema)
        if args.partial:
            # Hashed from the WORKING TREE, so an edited schema would otherwise
            # re-attribute every carried row measured on the old one.
            digest = choose(digest, prev.schemas.get(schema),
                            set(by_schema[schema]) & in_file, measured_ids,
                            report(f"schema {schema}"))
        line = f"{SCHEMA_PREFIX}{schema}  sha256 {digest}"
        if schema != default:
            line += "  rows: " + ",".join(sorted(by_schema[schema]))
        out.append(line)
    out.append("#")
    out.append("# Numbers shift when anything below shifts. Check here FIRST: if the header is")
    out.append("# unchanged and a number moved, the generator caused it.")

    # tool -> (argv, rows it built). Built from rows.json so the mapping cannot
    # drift from the rows actually in the file.
    used = {}
    for row in spec["rows"]:
        for lbl, argv in LANG_TOOLCHAINS.get(row["lang"], []):
            used.setdefault(lbl, (argv, set()))[1].add(row["id"])
        for arch in row["archs"]:
            for prefix, (lbl, argv) in ARCH_TOOLCHAINS:
                if arch.startswith(prefix):
                    used.setdefault(lbl, (argv, set()))[1].add(row["id"])
    for lbl, argv in HOST_TOOLCHAINS:
        used.setdefault(lbl, (argv, {r["id"] for r in spec["rows"]}))

    all_ids = {r["id"] for r in spec["rows"]}
    toolchain_rows = []
    for lbl in sorted(used):
        argv, rows_ = used[lbl]
        # "all" keeps the line readable and stops it churning every time a row is
        # added; readers of this column must treat it as matching any row.
        where = "all" if rows_ == all_ids else ",".join(sorted(rows_))
        # "(not found)" is deliberate: a tool that vanished from the environment is
        # exactly the kind of thing that silently changes a number, so record its
        # absence rather than dropping the line.
        version = tool_version(argv) or "(not found)"
        if args.partial:
            # Probed from the HOST, for every tool in rows.json, whatever this run
            # measured. Without this a one-row refresh on a partly-provisioned box
            # rewrote (or "(not found)"-ed) the version of tools whose rows it only
            # carried -- erasing a recorded version rather than recording an absence.
            version = choose(version, previous_tool(prev.tools, lbl, where),
                             rows_ & in_file, measured_ids, report(lbl))
        toolchain_rows.append((lbl, version, where))

    corelib_dirs = {}
    if args.corelibs and Path(args.corelibs).exists():
        for line in Path(args.corelibs).read_text().splitlines():
            if line.strip():
                repo, d = line.split("\t")
                corelib_dirs[repo] = d

    # A python row's engine belongs in this table for the same reason the compilers
    # do: it decides the number, and it can change without the generator or the
    # corelib SHA changing. One line PER ROW, because the two python rows deliberately
    # run different engines — a single line could only be wrong for one of them. Only
    # for rows actually measured: the label must not appear for a --rows run that
    # never touched python.
    engines = {}
    if "corelib-py" in corelib_dirs:
        for row in rows:
            if row["lang"] != "python" or row["id"] not in irs:
                continue
            # "(unknown)" rather than no entry when the probe fails, mirroring an
            # unresolvable SHA: this row WAS re-measured, so a carried engine would
            # attribute a new number to an engine this run never established.
            engines[row["id"]] = python_engine(
                corelib_dirs["corelib-py"],
                pin_pure=row.get("engine") != "native") or "(unknown)"
    if args.partial:
        # A python row this run did not measure keeps the engine its committed number
        # was measured with — the same statement the carried number itself makes.
        python_ids = {r["id"] for r in spec["rows"] if r["lang"] == "python"}
        merged = {}
        for rid in set(engines) | set(prev.engines):
            if rid not in python_ids or rid not in in_file:
                continue      # provenance about a row with no cell here is a guess
            value = choose(engines.get(rid), prev.engines.get(rid), {rid},
                           measured_ids, report(f"{ENGINE_LABEL} {rid}"))
            if value is not None:
                merged[rid] = value
        engines = merged
    if engines:
        for rid in sorted(engines):
            toolchain_rows.append((ENGINE_LABEL, engines[rid], rid))
        toolchain_rows.sort(key=lambda t: (t[0], t[2]))

    # Per repo, not per file: a corelib checked out this run contributes the SHA this
    # run resolved, one that was not keeps the SHA its committed cells were measured
    # against. Merging on a FULL run is not merely unnecessary, it would be wrong —
    # every cell there is this run's, so every SHA must be too (#464).
    shas = {repo.replace("corelib-", ""): git_sha(d) or "(unknown)"
            for repo, d in corelib_dirs.items()}
    if args.partial:
        # Per REPO, but `--rows` selects per ROW and eight of twelve corelibs back
        # more than one: re-measuring a strict subset of a repo's rows may not stamp
        # this run's SHA onto the siblings it carried, which is what choose() refuses.
        rows_of_corelib = {}
        for row in spec["rows"]:
            rows_of_corelib.setdefault(row["corelib"].replace("corelib-", ""),
                                       set()).add(row["id"])
        merged = {}
        for repo in set(shas) | set(prev.corelibs):
            # A carried SHA is dropped when none of the rows it built is in this file
            # any more: provenance about nothing is a guess, not a record.
            describes = rows_of_corelib.get(repo, set()) & in_file
            if not describes:
                continue
            value = choose(shas.get(repo), prev.corelibs.get(repo), describes,
                           measured_ids, report(f"corelib-{repo}"))
            if value is not None:
                merged[repo] = value
        shas = merged
    if shas:
        out.append(CORELIB_PREFIX + " | ".join(f"{r} {shas[r]}" for r in sorted(shas)))
    out.append("#")

    out.append("## instruction cost")
    out.append(fmt(IR_COLS, IR_WIDTHS))
    out += ir_lines

    out.append("")
    out.append("## toolchain")
    out.append(fmt_tc(TC_COLS))
    for vals in toolchain_rows:
        out.append(fmt_tc(vals))

    out.append("")
    out.append("## footprint")
    out.append(fmt(SZ_COLS, SZ_WIDTHS))
    out += sz_lines

    for what, old, new, stale in carried_notes:
        print(f"note: {what} reads {new} here but no row it describes was measured; "
              f"keeping the committed {old}", file=sys.stderr)

    # Nothing has been written yet, so refusing here leaves results.txt exactly as it
    # was -- the honest outcome, because no header this format can write is true of
    # both halves of the file. The fix is to widen the run, not to weaken the claim.
    if refusals:
        msg = ["refusing to write a header this run cannot make true:"]
        widen = set(measured_ids)
        for what, old, new, stale in refusals:
            msg.append(f"  {what} moved {old} -> {new}, but "
                       f"{','.join(stale)} {'was' if len(stale) == 1 else 'were'} "
                       f"not re-measured against it")
            widen |= set(stale)
        msg.append("results.txt is unchanged. Re-run " +
                   ("without --rows (a full run)" if widen >= all_ids
                    else "with --rows " + ",".join(sorted(widen))))
        sys.exit("\n".join(msg))

    # After the refusal, never before: a run whose header cannot be written leaves the
    # pair exactly as it was, so the record and the table stay from the same run.
    if args.raw_out:
        Path(args.raw_out).write_text("\n".join(render_raw(raw_lines)) + "\n")

    sys.stdout.write("\n".join(out) + "\n")


if __name__ == "__main__":
    main()
