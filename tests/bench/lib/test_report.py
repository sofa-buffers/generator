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


def results(ir=None, tools=None):
    """A results.txt-shaped file. `ir` and `tools` default to the fixture above; pass
    a subset to build the artifact a `run.sh --rows <id> --out` run writes."""
    ir = IR if ir is None else ir
    tools = TOOLS if tools is None else tools
    out = ["# sofabgen bench results — regenerate with tests/bench/run.sh",
           "#",
           "# corelib:   c-cpp aaaaaaa | go bbbbbbb | py ccccccc",
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


def drift_table(text):
    """The toolchain-drift section as [(tool, committed, this run, rows)].

    Scoped to that section: the outlier and moved tables have the same pipe shape,
    and a report can carry all three at once.
    """
    rows, inside = [], False
    for line in text.splitlines():
        if line.startswith("### "):
            inside = line.startswith("### Toolchain differs")
        elif inside and line.startswith("| ") and "---" not in line \
                and not line.startswith("| tool |"):
            rows.append(tuple(c.strip().strip("`")
                              for c in line.strip("|").split("|")))
    return rows


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
        _, _, tools = rep.parse(self.one(results()))
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
