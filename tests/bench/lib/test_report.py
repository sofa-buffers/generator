#!/usr/bin/env python3
"""Unit tests for report.py — run with
`python3 -m unittest discover -s tests/bench/lib`.

Stdlib only and hermetic, like test_format.py beside it: report.py reads two files
and writes markdown, so nothing here needs a corelib, a compiler or Callgrind.

What these guard is the `## toolchain` comparison, which the report prints FIRST and
on purpose — Ir/op is the instruction count of a particular binary, so a reader has
to be told the compiler moved before reading a row as a regression.

The table is not a tool->version map. `sofab-engine` gets one line PER python row,
deliberately: the two rows run different corelib-py engines, and a single line could
only be right for one of them. Keying the parsed table by tool name alone collapsed
the two, and the collapse was wrong in both directions at once (#492):

  * the committed file's second line won, so a `python` artifact that ran exactly the
    engine the file records was reported as engine drift on every run;
  * and a `python` row that had really flipped engines — the one thing the per-row
    line was added to make visible — compared native against native and reported
    nothing.

Both sides of the comparison collapsed the same way, so the committed file agreed
with itself and the defect was invisible in the ordinary case. The fixtures below are
therefore ASYMMETRIC on purpose.

The other two header entries are guarded the same way (#501). A number is the cost of
generated code, built by a toolchain, against a CORELIB CHECKOUT, from a SCHEMA: the
header records all three and the report used to compare one, so a corelib bump or an
edited schema under a moved row looked exactly like a generator regression. Corelib
differences are context and never touch the exit status — run.sh clones the corelibs
unpinned, so they differ on most runs by design.
"""

import importlib.util
import io
import sys
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent


def _load(name, filename):
    spec = importlib.util.spec_from_file_location(name, HERE / filename)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


rep = _load("bench_report", "report.py")
fmt = _load("bench_format", "format.py")

# A stand-in results.txt: one C row, one go row, and the two python rows — the only
# ones that carry a `sofab-engine` line. Synthetic rather than the real results.txt so
# that adding a bench row or re-measuring one cannot turn these into failures.
IR = {"alpha": ("100", "200"), "beta": ("300", "400"),
      "python": ("500", "600"), "python-native": ("700", "800")}

# (tool, version, rows column). `all` is format.py's shorthand for "built every row";
# every other line names the rows it built, and the report filters on it.
TOOLS = [("gcc", "15.2.0", "alpha"),
         ("go", "1.24.4", "beta"),
         ("python3", "3.14.4", "python,python-native"),
         ("valgrind", "3.26.0", "all"),
         ("sofab-engine", "pure", "python"),
         ("sofab-engine", "native", "python-native")]


# The other two header entries the report reads (#501). The corelib line has no rows
# column — it is one statement about the whole file — while `# schema:` lines have the
# same per-row shape as the toolchain table: a default line covering every row that
# does not name its own, and one line per schema that does. `alpha` is the footprint
# row measured on its own schema, as in the real rows.json.
CORELIBS = {"c-cpp": "aaaaaaa", "go": "bbbbbbb", "py": "ccccccc"}
SCHEMAS = [("s.yaml", "111111111111", None),
           ("s2.yaml", "222222222222", "alpha")]


def results(ir=None, tools=None, corelibs=None, schemas=None):
    """A results.txt-shaped file. Every part defaults to the fixture above; pass a
    subset to build the artifact a `run.sh --rows <id> --out` run writes."""
    ir = IR if ir is None else ir
    tools = TOOLS if tools is None else tools
    corelibs = CORELIBS if corelibs is None else corelibs
    schemas = SCHEMAS if schemas is None else schemas
    out = ["# sofabgen bench results — regenerate with tests/bench/run.sh",
           "#"]
    for path, digest, rows_ in schemas:
        out.append(f"# schema:  {path}  sha256 {digest}"
                   + (f"  rows: {rows_}" if rows_ else ""))
    out += ["#",
            "# corelib:   " + " | ".join(f"{r} {corelibs[r]}" for r in sorted(corelibs)),
            "",
            "## instruction cost",
            "row       profile    method    encode_ir/op  decode_ir/op"]
    for row in sorted(ir):
        profile = "footprint" if row == "alpha" else "maxspeed"
        method = "subtract" if row.startswith("python") else "toggle"
        out.append(f"{row}  {profile}  {method}  {ir[row][0]}  {ir[row][1]}")
    out += ["", "## toolchain", "tool          version   rows"]
    out += [f"{t}  {v}  {r}" for t, v, r in sorted(tools, key=lambda x: (x[0], x[2]))]
    out += ["", "## footprint",
            "row       profile    arch      text  data  bss",
            "alpha     footprint  ARMv6-m   1000  0     8", ""]
    return "\n".join(out) + "\n"


def engine(tools, row, version):
    """`tools` with the sofab-engine line of one row set to `version`."""
    return [(t, version if (t, r) == ("sofab-engine", row) else v, r)
            for t, v, r in tools]


def table(text, title):
    """One `### `-headed table as [(entry, committed, this run, rows)].

    Scoped to its own section: every provenance table has the same pipe shape, as do
    the outlier and moved tables, and one report can carry all of them at once.
    """
    rows, inside = [], False
    for line in text.splitlines():
        if line.startswith("### "):
            inside = line.startswith(title)
        elif inside and line.startswith("| ") and "---" not in line \
                and line.split("|")[1].strip() not in ("tool", "corelib", "schema"):
            # Row ids are code-quoted, one by one in a list of several.
            rows.append(tuple(c.strip().replace("`", "")
                              for c in line.strip("|").split("|")))
    return rows


def drift_table(text):
    return table(text, "### Toolchain differs")


def corelib_table(text):
    return table(text, "### Corelib checkouts differ")


def schema_table(text):
    return table(text, "### Schema digests differ")


class ToolchainDriftTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)

    def report(self, committed, measured):
        """Run report.py the way bench.yml does. `measured` is {row: file text}; the
        row is taken from the filename, so it is spelled bench-<row>.txt."""
        (self.root / "committed.txt").write_text(committed)
        argv = ["report.py", "--committed", str(self.root / "committed.txt"),
                "--measured"]
        for row, text in sorted(measured.items()):
            (self.root / f"bench-{row}.txt").write_text(text)
            argv.append(str(self.root / f"bench-{row}.txt"))
        buf = io.StringIO()
        with mock.patch.object(sys, "argv", argv), redirect_stdout(buf):
            self.status = rep.main()
        return buf.getvalue()

    def one(self, text):
        """A single file on disk, for the tests that call parse() directly."""
        p = self.root / "one.txt"
        p.write_text(text)
        return p

    # -- the bug -------------------------------------------------------------

    def test_a_python_row_that_flipped_engines_is_reported(self):
        """The defect, stated as the rule that replaces it. corelib-py picks its
        engine at import time and swallows the ImportError, so a `python` row whose
        SOFAB_PUREPYTHON pin did not take runs the accelerator instead — 3x on
        encode, with nothing else in the file to show for it. That is the one thing
        the per-row engine line exists to make visible.

        Fails before the fix: both files collapse to the LAST engine line (`native`),
        so the two sides agree and the report is silent.
        """
        measured = results(tools=engine(TOOLS, "python", "native"))
        out = self.report(results(), {"python": measured})
        self.assertEqual(drift_table(out), [("sofab-engine", "pure", "native", "all")])

    def test_a_python_row_on_the_engine_the_file_records_is_not_reported(self):
        """The same collapse, the other way round, and the case that actually ran on
        every workflow_dispatch: `run.sh --rows python --out` writes ONE engine line,
        for the row it probed. Compared against a committed file collapsed to
        `python-native`'s line, a perfectly healthy row read as `native -> pure`.

        Fails before the fix, which reports drift here.
        """
        artifact = results(ir={"python": IR["python"]},
                           tools=[t for t in TOOLS if t[2] != "python-native"])
        out = self.report(results(), {"python": artifact})
        self.assertEqual(drift_table(out), [])
        self.assertEqual(self.status, 0)

    def test_the_per_row_filter_sees_both_engine_lines(self):
        """The filter that keeps a go artifact from reporting the Zig compiler reads
        the same collapsed map, so it saw only the last line's row set. Stated
        directly on tools_for(), since that is where both halves come from.

        Fails before the fix: `sofab-engine` resolves to `native` for both rows.
        """
        tools = rep.parse(self.one(results())).tools
        self.assertEqual(rep.tools_for(tools, "python")["sofab-engine"], "pure")
        self.assertEqual(rep.tools_for(tools, "python-native")["sofab-engine"],
                         "native")
        # And the rest of the table is unchanged by the re-keying: a row still sees
        # the tools that built it, plus the `all` wildcard, and nothing else.
        self.assertEqual(rep.tools_for(tools, "beta"),
                         {"go": "1.24.4", "valgrind": "3.26.0"})

    def test_both_python_rows_are_judged_against_their_own_line(self):
        """The two rows measured in one run, one of them degraded. The report has to
        name that row and only that row — a drift line covering `all` would send a
        reader looking for a regression in the row that is fine.

        Fails before the fix, which reports nothing for either.
        """
        flipped = results(tools=engine(TOOLS, "python", "native"))
        out = self.report(results(), {"python": flipped, "python-native": flipped})
        self.assertEqual(drift_table(out),
                         [("sofab-engine", "pure", "native", "python")])

    # -- the provenance that was recorded and never compared (#501) ----------
    #
    # A row's Ir/op is the cost of generated code, built by a toolchain, against a
    # corelib checkout, from a schema. The header records all four; the report
    # compared the toolchain and nothing else, so a corelib bump or an edited schema
    # under a moved row looked exactly like a generator regression.

    def test_a_corelib_that_moved_is_reported_for_the_rows_that_saw_it(self):
        """The measurement in #501: `py 792f583` rewritten to `py deadbee` in one
        artifact changed no character of the report and left rc=0. The SHA was parsed
        and dropped unread.

        Fails before the fix, which prints no corelib section at all.
        """
        out = self.report(results(), {"python": results(corelibs=dict(CORELIBS,
                                                                     py="ddddddd")),
                                      "beta": results()})
        self.assertEqual(corelib_table(out),
                         [("py", "ccccccc", "ddddddd", "python")])

    def test_a_moved_corelib_is_context_and_never_changes_the_exit_status(self):
        """The judgement this section turns on. run.sh clones every corelib from its
        default branch and never pins it, so a SHA differing from the committed file
        is the NORMAL case — most runs will print this table. Exit 1 here would fail
        the ordinary run and the tool would be switched off within a week, so it
        follows the rule report.py already states: 1 only for a measurement that
        FAILED, never for drift. The numbers section is unaffected.

        Fails before the fix, which prints nothing for a corelib that moved.
        """
        moved = {r: "9999999" for r in CORELIBS}
        out = self.report(results(), {row: results(corelibs=moved) for row in IR})
        self.assertEqual(len(corelib_table(out)), len(CORELIBS))
        self.assertEqual(self.status, 0)
        self.assertIn("Every measured row matches the committed file", out)
        # And the `rows` column means what the toolchain table's does: the rows this
        # corelib builds, not the artifacts that stated the SHA. `# corelib:` has no
        # rows column, and here every artifact carries every SHA (a local full run,
        # or any --partial merge), so without rows.json a moved corelib-py would be
        # listed against the C row that never links it. `python`/`python-native` are
        # real ids and rows.json says corelib-py builds exactly them, so neither is
        # attributed to c-cpp or go; `alpha` and `beta` are not in rows.json at all,
        # so they keep the old behaviour and every repo still answers for them.
        self.assertEqual({t[0]: t[3] for t in corelib_table(out)},
                         {"c-cpp": "alpha, beta", "go": "alpha, beta", "py": "all"})

    def test_an_artifact_reports_only_the_corelibs_it_names(self):
        """bench.yml's per-row artifact is written WITHOUT --partial, so its header
        names only the corelib that row cloned — the other eleven are absent, not
        empty. A file that makes no claim must produce no difference, or every row
        of a real CI run would report eleven phantom corelibs.

        Fails before the fix on the line it does name.
        """
        out = self.report(results(), {"beta": results(corelibs={"go": "9999999"})})
        self.assertEqual(corelib_table(out), [("go", "bbbbbbb", "9999999", "all")])

    def test_an_edited_schema_is_reported_for_the_rows_measured_on_it(self):
        """The second measurement in #501: `sha256 6e6451af0e06` -> `000000000000`
        in an artifact changed no character of the report. `s2.yaml` is alpha's own
        schema, and both artifacts carry the whole table, so the per-row filter is
        what keeps beta out of it — beta was measured on the default schema and an
        edit to alpha's says nothing about it.

        Fails before the fix, which reads no `# schema:` line at all.
        """
        edited = [(p, "999999999999" if p == "s2.yaml" else d, r)
                  for p, d, r in SCHEMAS]
        out = self.report(results(), {"alpha": results(schemas=edited),
                                      "beta": results(schemas=edited)})
        self.assertEqual(schema_table(out),
                         [("s2.yaml", "222222222222", "999999999999", "alpha")])
        self.assertEqual(self.status, 0)

    def test_the_default_schema_line_covers_the_rows_that_name_no_other(self):
        """The default line has no rows column, which is format.py's "every row that
        does not name its own" — not `all`. So an edit to it reaches beta and the
        python rows and stops at alpha, which was measured on s2.yaml and cannot have
        moved because of it.

        Fails before the fix. It would also fail if the default line were treated as
        a wildcard the way the toolchain table's `all` is: alpha would be named too,
        sending a reader to look for a regression in a row the edit could not touch.
        """
        edited = [(p, "999999999999" if p == "s.yaml" else d, r) for p, d, r in SCHEMAS]
        out = self.report(results(), {row: results(schemas=edited) for row in IR})
        # Named by exception, because that is the shorter half and the interesting
        # one: on the real rows.json this same edit covers twenty of twenty-four
        # rows, and enumerated it is one table cell that scrolls sideways in a step
        # summary while burying the four rows the edit could not have moved.
        self.assertEqual(schema_table(out),
                         [("s.yaml", "111111111111", "999999999999",
                           "all but alpha")])

    def test_the_three_kinds_of_provenance_are_reported_together(self):
        """One artifact, all three moved. They are separate sections, in the order a
        reader needs them, and all three land above the numbers."""
        out = self.report(results(), {"beta": results(
            tools=[(t, "9.9.9" if t == "go" else v, r) for t, v, r in TOOLS],
            corelibs=dict(CORELIBS, go="9999999"),
            schemas=[(p, "999999999999" if p == "s.yaml" else d, r)
                     for p, d, r in SCHEMAS])})
        self.assertEqual(drift_table(out), [("go", "1.24.4", "9.9.9", "all")])
        self.assertEqual(corelib_table(out), [("go", "bbbbbbb", "9999999", "all")])
        self.assertEqual(schema_table(out),
                         [("s.yaml", "111111111111", "999999999999", "all")])
        heads = [l for l in out.splitlines() if l.startswith("### ")]
        self.assertEqual(heads, ["### Toolchain differs from the committed file",
                                 "### Corelib checkouts differ from the committed file",
                                 "### Schema digests differ from the committed file"])
        self.assertEqual(self.status, 0)

    def test_a_row_measured_on_a_different_schema_is_reported(self):
        """The schema change that moves a number hardest: the row is measured on
        another FILE, not an edit of the same one. It is also the one the borrow
        fallback swallowed. schemas_for() yields exactly one entry per row, so when
        the artifact names a path the committed file does not attribute to this row,
        tool_diff() fell back to "the version recorded for that name elsewhere" --
        and elsewhere is the measured file's own digest, so `committed != v` could
        never fire. Measured on the committed results.txt with `cpp-cpp-unbounded`
        moved onto a new `wide_ingest.yaml` line, the entire report was
        `Every measured row matches the committed file within 0.3%.`

        Fails before schema_diff(): the table is empty.
        """
        moved = [("s3.yaml", "333333333333", "alpha")] + [
            (p, d, r) for p, d, r in SCHEMAS if r != "alpha"]
        out = self.report(results(), {"alpha": results(schemas=moved)})
        self.assertEqual(schema_table(out),
                         [("s3.yaml", "s2.yaml 222222222222", "333333333333",
                           "all")])
        self.assertEqual(self.status, 0)

    def test_a_row_that_fell_back_to_the_default_schema_is_reported(self):
        """The mirror: alpha's own `# schema:` line is gone, so it is now measured on
        the default schema every other row uses. Same class of change, same silence
        before the fix -- and the committed side is named in full, because what moved
        is the path.

        Fails before schema_diff(): the table is empty.
        """
        dropped = [(p, d, r) for p, d, r in SCHEMAS if r != "alpha"]
        out = self.report(results(), {"alpha": results(schemas=dropped)})
        self.assertEqual(schema_table(out),
                         [("s.yaml", "s2.yaml 222222222222", "111111111111",
                           "all")])

    def test_a_schema_line_that_lists_no_rows_describes_no_row(self):
        """A `# schema:` line ending in a bare `rows:` names a rows column and lists
        nothing, so it describes no row. Read as the DEFAULT line -- which is what a
        `len(f) > 6` guard alone does -- one row's schema would be attributed to
        every row that names none of its own, the exact over-attribution
        schemas_for() exists to prevent. format.py never writes that shape, so this
        guards a latent hole rather than a live one."""
        text = results() + "# schema:  s4.yaml  sha256 444444444444  rows:\n"
        schemas = rep.parse(self.one(text)).schemas
        self.assertNotIn(("s4.yaml", None), schemas)
        self.assertEqual(rep.schemas_for(schemas, "beta"), {"s.yaml": "111111111111"})

    def test_an_unreadable_corelib_entry_is_named_not_dropped(self):
        """An entry that does not split into exactly two fields -- reflowed,
        annotated or hand-edited -- cannot be compared. Dropped in silence that
        corelib simply leaves the comparison and the report reads as agreement, which
        is the failure mode this whole change is about; format.py's reader collects
        the same loss into a note on the same line, for the same reason.

        Fails before the fix, which prints nothing and compares no corelib at all.
        """
        broken = results().replace("| go bbbbbbb", "| go bbbbbbb ccc")
        out = self.report(results(), {"beta": broken})
        self.assertIn("Could not read 1 entry from the `# corelib:` line in `beta` "
                      "(`go bbbbbbb ccc`); those checkouts are not compared.", out)
        self.assertEqual(corelib_table(out), [])
        self.assertEqual(self.status, 0)

    # -- what must not change ------------------------------------------------

    def test_a_report_over_two_agreeing_files_is_unchanged(self):
        """The report is read by humans off a step summary, so its text is part of
        the contract. Two files that agree must produce exactly this, character for
        character. Passes on both sides of the fix, deliberately — it is the
        stability guard, not a demonstration of the bug."""
        both = results()
        out = self.report(both, {row: both for row in IR})
        self.assertEqual(out, "## Bench\n\n"
                              "Every measured row matches the committed file "
                              "within 0.3%.\n\n")
        self.assertEqual(self.status, 0)

    def test_a_compiler_that_moved_is_reported_for_its_own_rows_only(self):
        """The ordinary case the re-keying must not disturb: one runner with another
        gcc and valgrind, and artifacts that each carry the whole table. Also the
        guard on the `all` wildcard, which has no row of its own to be keyed by."""
        moved = [(t, "9.9.9" if t in ("gcc", "valgrind") else v, r)
                 for t, v, r in TOOLS]
        out = self.report(results(), {"alpha": results(tools=moved),
                                      "beta": results(tools=moved)})
        self.assertEqual(drift_table(out),
                         [("gcc", "15.2.0", "9.9.9", "alpha"),
                          ("valgrind", "3.26.0", "9.9.9", "all")])

    def test_a_row_added_since_the_committed_file_reports_no_phantom_drift(self):
        """A row in rows.json but not yet in results.txt: the committed table names
        no rows for it, so a strict per-row lookup would report every tool that built
        it as `(not recorded)` on an unchanged toolchain. A tool whose committed lines
        all agree answers for the new row too; the two `sofab-engine` lines do not
        agree, so they never answer for each other."""
        new = dict(IR, gamma=("900", "1000"))
        tools = TOOLS + [("gcc", "15.2.0", "gamma")]
        out = self.report(results(), {"gamma": results(ir=new, tools=tools)})
        self.assertEqual(drift_table(out), [])
        # The row itself is still reported as new, which is the true statement.
        self.assertIn("| `gamma` | encode | - | 900 |", out)

    def test_a_single_committed_engine_line_does_not_answer_for_the_other_row(self):
        """The fallback above, at the one state where it is dangerous: `results.txt`
        holding exactly ONE `sofab-engine` line — right after a second python row is
        added to rows.json and before the file is regenerated. Keyed on the committed
        side alone, "every line agrees" is satisfied trivially by the one line and it
        answers for a row it does not name, which is the collapse this change is
        about; the version reported would be a statement the committed file never
        makes. Computing the set over BOTH tables sees the disagreement.

        Fails before this guard: the report reads `| sofab-engine | native | pure |`.
        """
        committed = results(tools=[t for t in TOOLS if t[2] != "python"])
        out = self.report(committed, {"python": results()})
        self.assertEqual(drift_table(out),
                         [("sofab-engine", "(not recorded)", "pure", "all")])

    def test_a_tool_that_vanished_from_the_environment_is_reported(self):
        """format.py writes `(not found)` as a version when a probe finds no tool,
        deliberately — a compiler that disappeared is exactly the kind of thing that
        silently moves a number. That value contains a space, and the table was split
        on whitespace, so the line parsed as version `(not` with rows `found)`: it
        named no real row, the filter dropped it, and the one drift the writer goes
        out of its way to record was the one the reader could not see.

        Fails before the fix, which prints nothing at all for this file.
        """
        gone = [(t, "(not found)" if t == "go" else v, r) for t, v, r in TOOLS]
        out = self.report(results(), {"beta": results(tools=gone)})
        self.assertEqual(drift_table(out), [("go", "1.24.4", "(not found)", "all")])

    def test_sofab_engine_is_the_only_label_that_can_appear_twice(self):
        """Why the fix is written against the (tool, row) pair rather than
        special-cased on `sofab-engine`. format.py builds every other line through
        `used`, a dict keyed by label, so a tool named by two sources — `rustc`, from
        the rust rows and from the thumbv6m cross-target — is merged into ONE line
        whose rows column is the union. Only the engine loop appends a second line
        under a label it has already written. If that ever stops being true this test
        fails and the report keeps working, because it keys by row either way."""
        labels = ([lbl for probes in fmt.LANG_TOOLCHAINS.values() for lbl, _ in probes]
                  + [lbl for _, (lbl, _) in fmt.ARCH_TOOLCHAINS]
                  + [lbl for lbl, _ in fmt.HOST_TOOLCHAINS])
        self.assertIn("rustc", labels)                  # declared twice, one line...
        self.assertNotIn(fmt.ENGINE_LABEL, labels)      # ...and never this label


if __name__ == "__main__":
    unittest.main()
