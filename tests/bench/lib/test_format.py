#!/usr/bin/env python3
"""Unit tests for format.py — run with
`python3 -m unittest discover -s tests/bench/lib`.

Stdlib only, like the harness itself, and hermetic: no corelib checkout, no
compiler, no Callgrind. `git_sha`, `tool_version` and `python_engine` are the only
things format.py asks the outside world for, and all three are stubbed, so what is
under test is the rendering — which is where the bug was.

What these guard is the results.txt HEADER. The numbers are only committable
because the header says what produced them, and a partial `run.sh --rows <id>` run
resolves a corelib SHA only for the row it measured: it used to write a header
naming that one corelib and drop the other eleven, stripping provenance off numbers
that were still in the file and making the output uncommittable (#487).

The merge that replaces it has three hard edges, one on each side:

  * it must not invent an entry;
  * it must not run at all on a full run, whose header is a statement about one run
    and has to stay that;
  * and it must not STAMP either. `--rows` selects per row while a corelib entry
    covers every row that corelib built, so a run that re-measures some of them and
    changes the entry would attribute this run's provenance to cells it carried. No
    header is true then, and the run is refused rather than written.
"""

import importlib.util
import io
import json
import re
import sys
import tempfile
import unittest
from contextlib import redirect_stdout, redirect_stderr
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent


def _load(name, filename):
    spec = importlib.util.spec_from_file_location(name, HERE / filename)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


fmt = _load("bench_format", "format.py")

# A stand-in rows.json: three corelibs, one footprint row on its own schema, and the
# two python rows (the only ones that carry a `sofab-engine` line). corelib-py backs
# two rows and corelib-c-cpp/corelib-go one each, which is the split the real file
# has — eight of twelve corelibs back more than one row. Synthetic rather than the
# real rows.json so that adding a bench row cannot turn these into failures.
SPEC = {
    "schema": "s.yaml",
    "rows": [
        {"id": "alpha", "lang": "c", "corelib": "corelib-c-cpp", "schema": "s2.yaml",
         "profile": "footprint", "method": "toggle", "archs": ["ARMv6-m"]},
        {"id": "beta", "lang": "go", "corelib": "corelib-go",
         "profile": "maxspeed", "method": "toggle", "archs": []},
        {"id": "python", "lang": "python", "corelib": "corelib-py",
         "profile": "maxspeed", "method": "subtract", "archs": [], "engine": "pure"},
        {"id": "python-native", "lang": "python", "corelib": "corelib-py",
         "profile": "maxspeed", "method": "subtract", "archs": [], "engine": "native"},
    ],
}

ALL_CORELIBS = ["corelib-c-cpp", "corelib-go", "corelib-py"]

# What the first (full) run resolved.
SHAS = {"corelib-c-cpp": "aaaaaaa", "corelib-go": "bbbbbbb", "corelib-py": "ccccccc"}

ALL_IRS = [("alpha", "100", "200"), ("beta", "300", "400"),
           ("python", "500", "600"), ("python-native", "700", "800")]
ALL_SIZES = [("alpha", "ARMv6-m", "1000", "0", "8")]

# Both python rows, at their committed values: the smallest selection that re-measures
# the whole of corelib-py, which is what a partial run has to do before it may move
# that repo's entry.
PY_IRS = [("python", "500", "600"), ("python-native", "700", "800")]


def corelib_line(text):
    return next(l for l in text.splitlines() if l.startswith("# corelib:"))


def corelib_shas(text):
    """The `# corelib:` line as {short repo name: sha}."""
    return {f[0]: f[1] for f in
            (p.split() for p in corelib_line(text).split(":", 1)[1].split("|"))}


def engine_lines(text):
    return [l.split() for l in text.splitlines() if l.startswith("sofab-engine")]


def toolchain_lines(text):
    """The `## toolchain` table as [(tool, version, rows)].

    Splits the padded columns rather than the whitespace, and only inside that
    section — a version can be `(not found)`, which is two whitespace fields, and
    that is precisely the line this reader must not lose (#502).
    """
    out, inside = [], False
    for line in text.splitlines():
        if line.startswith("## "):
            inside = line[3:].strip() == "toolchain"
        elif inside and line.strip() and not line.startswith("tool"):
            f = re.split(r"\s{2,}", line.strip())
            if len(f) == 3:
                out.append(tuple(f))
    return out


def toolchain(text):
    """The `## toolchain` table as {tool: version}, minus the sofab-engine lines."""
    return {t: v for t, v, _ in toolchain_lines(text) if t != "sofab-engine"}


def schema_hashes(text):
    """{schema path: sha256} off the `# schema:` lines."""
    return {f[2]: f[4] for f in
            (l.split() for l in text.splitlines() if l.startswith("# schema:"))}


def ir_rows(text):
    """{row: (encode, decode)} out of the instruction-cost table."""
    return {f[0]: (f[3], f[4]) for f in
            (l.split() for l in text.splitlines())
            if len(f) == 5 and f[2] in ("toggle", "subtract")}


class FormatHeaderTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        (self.root / "s.yaml").write_text("# stand-in schema\n")
        (self.root / "s2.yaml").write_text("# stand-in schema, alpha's own\n")
        (self.root / "rows.json").write_text(json.dumps(SPEC))

        # The three things format.py asks the environment for. A checkout dir is
        # spelled /checkouts/<repo> and never touched — git_sha reads the name.
        # `tools` is keyed by the probed binary, so a test can move one compiler.
        self.shas = dict(SHAS)
        self.tools = {}
        self.engine = lambda d, pin_pure: "pure" if pin_pure else "native"
        for target, stub in (
            ("git_sha", lambda d: self.shas.get(Path(d).name)),
            ("tool_version", lambda argv: self.tools.get(argv[0], "1.0")),
            ("python_engine", lambda d, pin_pure: self.engine(d, pin_pure)),
        ):
            p = mock.patch.object(fmt, target, stub)
            p.start()
            self.addCleanup(p.stop)

    def render(self, *, irs=(), sizes=(), corelibs=(), previous=None, partial=False,
               raw=False, previous_raw=None):
        w = self.root
        (w / "irs.tsv").write_text("".join("\t".join(r) + "\n" for r in irs))
        (w / "sizes.tsv").write_text("".join("\t".join(r) + "\n" for r in sizes))
        (w / "corelibs.tsv").write_text(
            "".join(f"{repo}\t/checkouts/{repo}\n" for repo in corelibs))
        argv = ["format.py",
                "--rows", str(w / "rows.json"),
                "--sizes", str(w / "sizes.tsv"),
                "--irs", str(w / "irs.tsv"),
                "--root", str(w),
                "--corelibs", str(w / "corelibs.tsv")]
        if previous is not None:
            (w / "previous.txt").write_text(previous)
            argv += ["--previous", str(w / "previous.txt")]
        # The raw sidecar is written to a file rather than stdout, so a test that
        # wants it asks for it and reads self.raw afterwards.
        self.raw = None
        if raw or previous_raw is not None:
            argv += ["--raw-out", str(w / "raw.txt")]
            if (w / "raw.txt").exists():
                (w / "raw.txt").unlink()
        if previous_raw is not None:
            (w / "previous-raw.txt").write_text(previous_raw)
            argv += ["--previous-raw", str(w / "previous-raw.txt")]
        if partial:
            argv.append("--partial")
        buf, err = io.StringIO(), io.StringIO()
        try:
            with mock.patch.object(sys, "argv", argv), \
                 redirect_stdout(buf), redirect_stderr(err):
                fmt.main()
        finally:
            self.stderr = err.getvalue()
            if (w / "raw.txt").exists():
                self.raw = (w / "raw.txt").read_text()
        return buf.getvalue()

    def refusal(self, **kw):
        """Render expecting a refusal; returns the message."""
        with self.assertRaises(SystemExit) as cm:
            self.render(**kw)
        return str(cm.exception)

    def committed(self):
        """A results.txt as a full run writes it: every row measured, every SHA."""
        return self.render(irs=ALL_IRS, sizes=ALL_SIZES, corelibs=ALL_CORELIBS)

    # -- the bug -------------------------------------------------------------

    def test_partial_run_updates_the_measured_repo_and_keeps_the_others(self):
        """The defect in #487, stated as the rule that replaces it."""
        prev = self.committed()
        self.assertEqual(corelib_shas(prev), {"c-cpp": "aaaaaaa", "go": "bbbbbbb",
                                              "py": "ccccccc"})

        self.shas["corelib-go"] = "9999999"          # the go corelib moved
        out = self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                          previous=prev, partial=True)

        self.assertEqual(corelib_shas(out),
                         {"c-cpp": "aaaaaaa",   # not re-measured, keeps its SHA
                          "go": "9999999",      # measured this run, and beta is the
                          "py": "ccccccc"})     #   only row corelib-go builds
        # The cells the carried SHAs describe are still there — that is what makes
        # carrying them the true statement rather than a convenient one.
        self.assertEqual(ir_rows(out)["alpha"], ("100", "200"))
        self.assertEqual(ir_rows(out)["beta"], ("333", "444"))

    def test_the_sofab_engine_lines_survive_a_partial_run(self):
        prev = self.committed()
        self.assertEqual(engine_lines(prev),
                         [["sofab-engine", "pure", "python"],
                          ["sofab-engine", "native", "python-native"]])

        out = self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                          previous=prev, partial=True)
        self.assertEqual(engine_lines(out), engine_lines(prev))

    def test_a_measured_python_row_wins_over_the_carried_engine(self):
        """Carrying must never shadow a measurement: python-native re-measured on a
        checkout whose extension has gone missing has to report `pure`."""
        prev = self.committed()
        self.engine = lambda d, pin_pure: "pure"
        out = self.render(irs=PY_IRS, corelibs=["corelib-py"],
                          previous=prev, partial=True)
        self.assertEqual(engine_lines(out),
                         [["sofab-engine", "pure", "python"],
                          ["sofab-engine", "pure", "python-native"]])

    def test_the_toolchain_versions_survive_a_partial_run(self):
        """The same defect one line down. `tool_version()` probes the host for every
        tool in rows.json whatever was measured, so a refresh on a box where the go
        toolchain has moved (or gone) rewrote the version attributing beta's carried
        number."""
        prev = self.committed()
        self.assertEqual(toolchain(prev)["go"], "1.0")

        self.tools["go"] = None            # -> "(not found)"
        out = self.render(sizes=ALL_SIZES, corelibs=["corelib-c-cpp"],
                          previous=prev, partial=True)
        self.assertEqual(toolchain(out)["go"], "1.0")
        self.assertEqual(ir_rows(out)["beta"], ("300", "400"))
        # ... and a tool whose rows this run DID measure reports what it read.
        self.tools["arm-none-eabi-gcc"] = "2.0"
        out = self.render(sizes=ALL_SIZES, corelibs=["corelib-c-cpp"],
                          previous=prev, partial=True)
        self.assertEqual(toolchain(out)["arm-none-eabi-gcc"], "2.0")

    def test_the_schema_hashes_survive_a_partial_run(self):
        """And one line up. The hash is taken from the WORKING TREE, so an edited
        schema re-attributed every carried row measured on the old one."""
        prev = self.committed()
        before = schema_hashes(prev)

        (self.root / "s2.yaml").write_text("# edited after alpha was measured\n")
        out = self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                          previous=prev, partial=True)
        self.assertEqual(schema_hashes(out), before)

    # -- the stamping edge ---------------------------------------------------

    def test_re_measuring_part_of_a_corelibs_rows_is_refused_not_stamped(self):
        """`--rows` selects per row; a corelib entry covers every row that corelib
        built. Moving the entry after re-measuring only some of them would say the
        carried cells were built with a SHA they never saw — and, worse, read as
        "the corelib bump cost those rows nothing"."""
        prev = self.committed()
        self.shas["corelib-py"] = "ddddddd"

        msg = self.refusal(irs=[("python", "555", "666")], corelibs=["corelib-py"],
                           previous=prev, partial=True)
        self.assertIn("corelib-py moved ccccccc -> ddddddd", msg)
        self.assertIn("python-native", msg)
        self.assertIn("--rows python,python-native", msg)

        # Measuring the whole of what the entry covers is the fix, and is silent.
        out = self.render(irs=[("python", "555", "666"), ("python-native", "777", "888")],
                          corelibs=["corelib-py"], previous=prev, partial=True)
        self.assertEqual(corelib_shas(out)["py"], "ddddddd")

    def test_a_partial_run_on_an_unmoved_corelib_is_never_refused(self):
        """The refusal is about a CHANGED entry, not about a narrow selection: the
        one-row refresh this whole feature exists for stays silent."""
        prev = self.committed()
        out = self.render(irs=[("python", "555", "666")], corelibs=["corelib-py"],
                          previous=prev, partial=True)
        self.assertEqual(corelib_shas(out)["py"], "ccccccc")
        self.assertEqual(ir_rows(out)["python"], ("555", "666"))

    def test_a_toolchain_that_moved_under_a_carried_row_is_refused(self):
        """Same rule, applied to a tool that spans the measured and the carried half
        of the file. valgrind measures every Ir row, so no partial run can restate
        its version truthfully — and the message says so."""
        prev = self.committed()
        self.tools["valgrind"] = "3.99"

        msg = self.refusal(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                           previous=prev, partial=True)
        self.assertIn("valgrind moved 1.0 -> 3.99", msg)
        self.assertIn("without --rows (a full run)", msg)

    def test_an_edited_schema_under_a_carried_row_is_refused(self):
        prev = self.committed()
        (self.root / "s.yaml").write_text("# edited\n")

        msg = self.refusal(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                           previous=prev, partial=True)
        self.assertIn("schema s.yaml moved", msg)
        self.assertIn("python,python-native", msg)

    def test_a_probe_that_describes_no_cell_in_this_file_is_not_a_conflict(self):
        """A `--rows` run on a partly-provisioned box must still work. The go
        toolchain moving while no go row is measured is a reading about nothing, so
        the committed value stands and the run says so rather than refusing."""
        prev = self.committed()
        self.tools["go"] = "9.99.9"
        out = self.render(irs=PY_IRS, corelibs=["corelib-py"],
                          previous=prev, partial=True)
        self.assertEqual(toolchain(out)["go"], "1.0")
        self.assertIn("no row it describes was measured", self.stderr)

    # -- the other edges -----------------------------------------------------

    def test_a_full_run_writes_a_header_entirely_its_own(self):
        """#464's "one provenance" property. A full run passes no --partial, so the
        previous header is read and then not used: every SHA in the file is this
        run's, exactly as every cell is. It is also never refused — the refusal is a
        property of merging, and a full run does not merge."""
        prev = self.committed()
        self.shas["corelib-go"] = "9999999"
        self.tools["valgrind"] = "3.99"
        out = self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                          previous=prev)
        self.assertEqual(corelib_shas(out), {"go": "9999999"})
        self.assertEqual(engine_lines(out), [])
        self.assertEqual(toolchain(out)["valgrind"], "3.99")

    def test_a_repo_in_neither_the_previous_header_nor_this_run_never_appears(self):
        """The merged line is the union of the two sources and nothing else — not
        rows.json's corelib column, which would name a repo no run ever resolved."""
        prev = self.committed()
        out = self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                          previous=prev, partial=True)
        self.assertEqual(set(corelib_shas(out)), {"c-cpp", "go", "py"})

    def test_a_carried_sha_whose_rows_left_the_file_is_dropped(self):
        """Never invent: the header is a claim about cells, so a SHA for a row that
        has no cell here — deleted from rows.json, or never committed — is a guess.

        Both shapes at once: `zig` is in the previous header with no row in the spec
        at all, and `alpha` (corelib-c-cpp) has a row but no committed cell.
        """
        prev = self.committed()
        prev = prev.replace("| py ccccccc", "| py ccccccc | zig zzzzzzz")
        prev = "\n".join(l for l in prev.splitlines()
                         if not l.startswith("alpha")) + "\n"

        out = self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                          previous=prev, partial=True)
        self.assertEqual(corelib_shas(out), {"go": "bbbbbbb", "py": "ccccccc"})
        self.assertNotIn("alpha", ir_rows(out))

    def test_a_carried_engine_whose_row_left_the_file_is_dropped(self):
        """The `sofab-engine` mirror of the test above — the same "never invent"
        edge, on the other merged family."""
        prev = self.committed()
        prev = "\n".join(l for l in prev.splitlines()
                         if not l.startswith("python-native")) + "\n"

        out = self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                          previous=prev, partial=True)
        self.assertEqual(engine_lines(out), [["sofab-engine", "pure", "python"]])

    def test_an_unresolved_sha_is_neither_carried_nor_laundered(self):
        """`(unknown)` is a recorded failure, not a SHA. A repo measured this run
        with an unresolvable checkout must not silently inherit the old SHA, and the
        next partial run must not carry `(unknown)` forward as if it were one."""
        prev = self.committed()
        self.shas["corelib-go"] = None
        out = self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                          previous=prev, partial=True)
        self.assertEqual(corelib_shas(out)["go"], "(unknown)")

        self.shas["corelib-py"] = "ddddddd"
        again = self.render(irs=PY_IRS, corelibs=["corelib-py"],
                            previous=out, partial=True)
        self.assertEqual(corelib_shas(again), {"c-cpp": "aaaaaaa", "py": "ddddddd"})

    def test_a_measured_rows_failed_engine_probe_is_not_laundered(self):
        """The engine mirror of `(unknown)`. Both python rows were re-measured, so
        the previous engines describe neither of them any more; a probe that failed
        must record that rather than inherit a value this run never established."""
        prev = self.committed()
        self.engine = lambda d, pin_pure: None
        out = self.render(irs=PY_IRS, corelibs=["corelib-py"],
                          previous=prev, partial=True)
        self.assertEqual(engine_lines(out),
                         [["sofab-engine", "(unknown)", "python"],
                          ["sofab-engine", "(unknown)", "python-native"]])

    def test_an_unreadable_previous_corelib_entry_is_reported(self):
        """A hand-edited, reflowed or annotated header parses as fewer entries, which
        would silently re-gut the file into exactly the #487 output. Cheap to say."""
        prev = self.committed()
        prev = prev.replace("| py ccccccc", "| py ccccccc extra | zig (unknown)")

        out = self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                          previous=prev, partial=True)
        self.assertIn("could not read 1 entry", self.stderr)
        self.assertIn("py ccccccc extra", self.stderr)
        self.assertNotIn("zig", self.stderr)     # "(unknown)" is dropped on purpose
        self.assertNotIn("py", corelib_shas(out))

    # -- the previous reader (#502) ------------------------------------------
    #
    # parse_previous() is the whole carry-forward, so what it cannot read is what a
    # partial run silently rewrites. Two shapes it could not read: a version holding
    # a space, and a label with more than one line.

    def test_a_tool_recorded_as_not_found_survives_a_partial_run(self):
        """`(not found)` is a recorded fact about the box the cells were measured
        on — a compiler that vanished is exactly the kind of thing that moves a
        number — and the docstring says it is carried. It was not: the value holds a
        space, so the line split into four whitespace fields, matched neither branch
        and was dropped, and the next partial run re-probed the tool and wrote
        whatever the box answered over a row it had only carried. That launders away
        the one record `(not found)` exists to keep.

        Fails before the fix: `go` reads `1.0` here, the version this run probed for
        a row it never measured.
        """
        self.tools["go"] = None                   # the box that measured beta had no go
        prev = self.committed()
        self.assertIn(("go", "(not found)", "beta"), toolchain_lines(prev))

        self.tools.pop("go")                      # ...and this box has one again
        out = self.render(sizes=ALL_SIZES, corelibs=["corelib-c-cpp"],
                          previous=prev, partial=True)
        self.assertEqual(toolchain(out)["go"], "(not found)")
        self.assertEqual(ir_rows(out)["beta"], ("300", "400"))   # still carried
        self.assertIn("no row it describes was measured", self.stderr)

    def test_two_lines_under_one_label_do_not_collapse(self):
        """The table is not a label->version map. `sofab-engine` writes one line per
        python row, deliberately, because the two rows run different corelib-py
        engines — so a reader keyed by label alone keeps only the last line. That is
        #492's defect on the reading side, and the reason `tools` is keyed by the
        pair the line actually states.

        Fails before the fix: `tools` holds one `sofab-engine` entry, `native`.
        """
        prev = self.root / "prev.txt"
        prev.write_text(self.committed())
        tools = fmt.parse_previous(prev).tools
        self.assertEqual(tools[(fmt.ENGINE_LABEL, "python")], "pure")
        self.assertEqual(tools[(fmt.ENGINE_LABEL, "python-native")], "native")
        # The ordinary lines are keyed the same way, by the rows column they name.
        self.assertEqual(tools[("go", "beta")], "1.0")
        self.assertEqual(tools[("valgrind", "all")], "1.0")
        # And `engines` is now a view of that table rather than a second parse, so
        # the two readers cannot disagree about what the file says.
        self.assertEqual(fmt.parse_previous(prev).engines,
                         {"python": "pure", "python-native": "native"})

    def test_a_label_with_disagreeing_lines_answers_for_no_other_row(self):
        """How the writer looks a line up when rows.json has moved under the file.
        An exact (label, rows) hit answers; otherwise the label answers only when
        every line it has agrees, because carrying one of two disagreeing lines onto
        a line naming neither would state a version the file never recorded."""
        tools = {("sofab-engine", "python"): "pure",
                 ("sofab-engine", "python-native"): "native",
                 ("gcc", "alpha"): "15.2.0"}
        self.assertEqual(fmt.previous_tool(tools, "sofab-engine", "python"), "pure")
        self.assertIsNone(fmt.previous_tool(tools, "sofab-engine", "python,gamma"))
        # One agreeing line answers for a rows column it does not name: that is the
        # row added to rows.json since the file was written, on an unmoved compiler.
        self.assertEqual(fmt.previous_tool(tools, "gcc", "alpha,gamma"), "15.2.0")
        self.assertIsNone(fmt.previous_tool(tools, "zig", "zig"))

    def test_a_row_added_to_a_tools_line_keeps_the_committed_version(self):
        """The same fallback through the writer, on the value that has to survive
        it: gcc was `(not found)` when alpha was measured, and rows.json has since
        gained a second row for gcc, so no line in the previous file names this
        run's rows column. The absence is still what the file records for those
        cells, and a re-probe of this box is not.

        Fails before the fix: the `(not found)` line was never read back, so this
        writes `9.9.9` — a version alpha's carried number was never measured with.
        """
        self.tools["gcc"] = None
        prev = self.committed()
        self.assertIn(("gcc", "(not found)", "alpha"), toolchain_lines(prev))

        spec = json.loads(json.dumps(SPEC))
        spec["rows"].append({"id": "gamma", "lang": "c", "corelib": "corelib-c-cpp",
                             "profile": "footprint", "method": "toggle", "archs": []})
        (self.root / "rows.json").write_text(json.dumps(spec))
        self.tools["gcc"] = "9.9.9"
        out = self.render(irs=PY_IRS, corelibs=["corelib-py"],
                          previous=prev, partial=True)
        self.assertIn(("gcc", "(not found)", "alpha,gamma"), toolchain_lines(out))

    # -- the round trip ------------------------------------------------------

    def test_two_partial_runs_in_a_row_do_not_degrade_the_header(self):
        """The property that makes a partial run committable at all: its output is a
        valid --previous for the next one, so measuring go and then py leaves the
        c-cpp SHA that neither run touched intact rather than eroding it one run at
        a time."""
        prev = self.committed()

        self.shas["corelib-go"] = "9999999"
        first = self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                            previous=prev, partial=True)

        self.shas["corelib-py"] = "ddddddd"
        second = self.render(irs=PY_IRS, corelibs=["corelib-py"],
                             previous=first, partial=True)

        self.assertEqual(corelib_shas(second), {"c-cpp": "aaaaaaa",
                                                "go": "9999999",
                                                "py": "ddddddd"})
        self.assertEqual(engine_lines(second), engine_lines(prev))
        # And the cells behind them: every row is still in the file, and the two
        # python cells were re-measured at their committed values, so nothing moved.
        self.assertEqual(ir_rows(second),
                         {"alpha": ("100", "200"), "beta": ("333", "444"),
                          "python": ("500", "600"), "python-native": ("700", "800")})

    def test_a_partial_run_that_changes_nothing_reproduces_the_file(self):
        """Byte-identical, not merely equivalent: results.txt is committed, so a
        re-measurement that reads the same numbers must produce no diff at all."""
        prev = self.committed()
        out = self.render(irs=[("beta", "300", "400")], corelibs=["corelib-go"],
                          previous=prev, partial=True)
        self.assertEqual(out, prev)

    # -- the raw sidecar (#489) ----------------------------------------------
    #
    # results.txt holds a cell still while a reading is within NOISE_BAND of it, and
    # THROWS THE READING AWAY. That is why #488 could not be dated from the repository
    # at all, and why re-measuring every row found seven more cells held off a number
    # that reproduces to the instruction in two independent measurement contexts (and
    # five further cells that read a third value, which is a different finding — see
    # tests/bench/README.md). results-raw.txt is the record that closes it, so what
    # these pin is the one property that makes it a record: it states what was
    # measured, never what is committed.

    def test_the_sidecar_holds_the_reading_where_results_holds_the_baseline(self):
        prev = self.committed()
        self.assertEqual(ir_rows(prev)["beta"], ("300", "400"))
        # A partial run needs a sidecar to carry, so give it one from a full run.
        self.render(irs=ALL_IRS, sizes=ALL_SIZES, corelibs=ALL_CORELIBS, raw=True)
        carried = self.raw

        # +0.2% on encode: inside the 0.3% band, so results.txt must not move...
        out = self.render(irs=[("beta", "300.6", "400")], corelibs=["corelib-go"],
                          previous=prev, partial=True, previous_raw=carried)
        self.assertEqual(ir_rows(out)["beta"], ("300", "400"))
        # ...and the sidecar must say what was actually read, or the step is lost.
        self.assertEqual(ir_rows(self.raw)["beta"], ("300.6", "400"))

    def test_a_partial_run_with_no_sidecar_to_carry_is_refused(self):
        """results.txt survives a partial run because --previous is committed and
        always there. The sidecar's carry-forward reads --previous-raw, which can be
        absent (deleted, or a checkout predating it) — and then a `--rows` run would
        quietly replace the committed record with just the rows it measured. That is a
        truncation nothing else would catch, so it is refused instead."""
        prev = self.render(irs=ALL_IRS, sizes=ALL_SIZES, corelibs=ALL_CORELIBS,
                           raw=True)
        with self.assertRaises(SystemExit):
            self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                        previous=prev, partial=True, raw=True)
        self.assertIsNone(self.raw)
        self.assertIn("refusing", self.stderr)

    def test_a_row_the_sidecar_did_not_measure_keeps_its_previous_reading(self):
        """Same carry-forward rule as results.txt: a partial run must not blank the
        rows it did not touch, or the record would only ever hold one row."""
        first = self.render(irs=ALL_IRS, sizes=ALL_SIZES, corelibs=ALL_CORELIBS,
                            raw=True)
        self.assertEqual(ir_rows(first)["alpha"], ("100", "200"))

        self.render(irs=[("beta", "333", "444")], corelibs=["corelib-go"],
                    previous=first, partial=True, previous_raw=self.raw)
        self.assertEqual(ir_rows(self.raw),
                         {"alpha": ("100", "200"), "beta": ("333", "444"),
                          "python": ("500", "600"), "python-native": ("700", "800")})

    def test_a_run_that_asks_for_no_sidecar_writes_none(self):
        """An --out preview and bench.yml's per-row artifacts are a second measuring
        device; their readings are not this repository's record."""
        self.render(irs=ALL_IRS, sizes=ALL_SIZES, corelibs=ALL_CORELIBS)
        self.assertIsNone(self.raw)

    def test_a_refused_run_writes_no_sidecar_either(self):
        """The pair has to come from one run. A header this run cannot make true
        leaves results.txt alone, so it must leave the record alone too."""
        prev = self.render(irs=ALL_IRS, sizes=ALL_SIZES, corelibs=ALL_CORELIBS,
                           raw=True)
        before = self.raw
        self.shas["corelib-py"] = "9999999"
        with self.assertRaises(SystemExit):
            self.render(irs=[("python", "555", "666")], corelibs=["corelib-py"],
                        previous=prev, partial=True, previous_raw=before)
        self.assertIsNone(self.raw)


if __name__ == "__main__":
    unittest.main()
