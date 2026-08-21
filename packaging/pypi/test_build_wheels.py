#!/usr/bin/env python3
"""Unit tests for build-wheels.py and check-wheels.py — run with
`python3 -m unittest discover -s packaging/pypi`.

Stdlib only, like the builder itself. What they are really guarding is the part
PyPI cannot warn us about in advance: a wheel is accepted on upload but only
*installs* if its RECORD hashes, its tags and its script path are right, and by
then the version is immutable. So the tests build real wheels from a stand-in
binary and read them back the way pip would.
"""

import base64
import contextlib
import hashlib
import io
import importlib.util
import tempfile
import unittest
import zipfile
from email.parser import Parser
from pathlib import Path

ROOT = Path(__file__).resolve().parent

def _load(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / filename)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


bw = _load("build_wheels", "build-wheels.py")
cw = _load("check_wheels", "check-wheels.py")

FAKE_BINARY = b"\x7fELF not really, but bytes are bytes\n"


def fake_dist(tmp):
    """A directory of stand-in release assets, named exactly like the real ones."""
    d = Path(tmp) / "dist"
    d.mkdir()
    for t in bw.TARGETS:
        (d / t.asset).write_bytes(FAKE_BINARY + t.asset.encode())
    return d


class TestPep440(unittest.TestCase):
    def test_translates_the_tag_spellings_we_release(self):
        for tag, want in [
            ("v1.2.3", "1.2.3"),
            ("1.2.3", "1.2.3"),
            ("v0.22.0", "0.22.0"),
            ("v1.2.3-rc1", "1.2.3rc1"),
            ("v1.2.3-rc.1", "1.2.3rc1"),
            ("v1.2.3-beta2", "1.2.3b2"),
            ("v1.2.3-b2", "1.2.3b2"),
            ("v1.2.3-alpha1", "1.2.3a1"),
            ("v1.2.3-a1", "1.2.3a1"),
        ]:
            with self.subTest(tag=tag):
                self.assertEqual(bw.pep440(tag), want)

    def test_print_version_hands_the_rule_to_other_callers(self):
        # The post-publish verification needs the published version of a tag; it
        # asks for it here instead of reimplementing the translation in YAML.
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            bw.main(["--version", "v1.2.3-rc1", "--print-version"])
        self.assertEqual(out.getvalue().strip(), "1.2.3rc1")

    def test_refuses_what_it_cannot_translate_faithfully(self):
        # release.yml's check-version gate is broader than PEP 440; anything it
        # lets through that PyPI would rename must stop here, not be guessed at.
        for tag in ["v1.2.3-nightly", "v1.2.3+build7", "v1.2", "v1.2.3-rc", "banana"]:
            with self.subTest(tag=tag):
                with self.assertRaises(SystemExit):
                    bw.pep440(tag)


class TestBuild(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dist = fake_dist(self.tmp.name)
        self.out = Path(self.tmp.name) / "wheels"

    def build(self, *extra):
        bw.main(["--version", "v1.2.3", "--from", str(self.dist), "--out", str(self.out), *extra])
        return sorted(self.out.glob("*.whl"))

    def test_every_target_tag_becomes_exactly_one_wheel(self):
        wheels = self.build()
        want = {f"sofabgen-1.2.3-py3-none-{tag}.whl" for t in bw.TARGETS for tag in t.tags}
        self.assertEqual({w.name for w in wheels}, want)
        # Nine binaries, thirteen wheels: the Linux ones are published twice.
        self.assertEqual(len(wheels), 13)

    def test_wheel_holds_the_binary_as_a_script_and_nothing_else(self):
        self.build("--only", "manylinux_2_17_x86_64")
        wheel = self.out / "sofabgen-1.2.3-py3-none-manylinux_2_17_x86_64.whl"
        with zipfile.ZipFile(wheel) as z:
            names = z.namelist()
            self.assertEqual(
                sorted(names),
                sorted(
                    [
                        "sofabgen-1.2.3.data/scripts/sofabgen",
                        "sofabgen-1.2.3.dist-info/METADATA",
                        "sofabgen-1.2.3.dist-info/WHEEL",
                        "sofabgen-1.2.3.dist-info/licenses/LICENSE",
                        "sofabgen-1.2.3.dist-info/RECORD",
                    ]
                ),
            )
            script = z.getinfo("sofabgen-1.2.3.data/scripts/sofabgen")
            self.assertEqual(z.read(script.filename), FAKE_BINARY + b"sofabgen-linux-amd64")
            # 0o755 survives an unzip by hand (pip sets the bit itself).
            self.assertEqual((script.external_attr >> 16) & 0o777, 0o755)

    def test_windows_wheels_carry_the_exe(self):
        self.build("--only", "win_amd64")
        with zipfile.ZipFile(self.out / "sofabgen-1.2.3-py3-none-win_amd64.whl") as z:
            self.assertIn("sofabgen-1.2.3.data/scripts/sofabgen.exe", z.namelist())

    def test_record_hashes_and_sizes_match_the_files(self):
        # A wrong RECORD is the classic way a wheel uploads fine and then refuses
        # to install, so check every line against the archive it describes.
        self.build("--only", "macosx_11_0_arm64")
        with zipfile.ZipFile(self.out / "sofabgen-1.2.3-py3-none-macosx_11_0_arm64.whl") as z:
            record = z.read("sofabgen-1.2.3.dist-info/RECORD").decode().splitlines()
            seen = set()
            for line in record:
                name, digest, size = line.rsplit(",", 2)
                seen.add(name)
                if name.endswith("dist-info/RECORD"):
                    self.assertEqual((digest, size), ("", ""))  # RECORD never hashes itself
                    continue
                data = z.read(name)
                want = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=")
                self.assertEqual(digest, f"sha256={want.decode()}", name)
                self.assertEqual(int(size), len(data), name)
            self.assertEqual(seen, set(z.namelist()))  # RECORD covers the whole archive

    def test_wheel_tag_agrees_with_the_filename(self):
        wheels = self.build()
        for wheel in wheels:
            plat = wheel.name[len("sofabgen-1.2.3-py3-none-") : -len(".whl")]
            with zipfile.ZipFile(wheel) as z:
                meta = Parser().parsestr(z.read("sofabgen-1.2.3.dist-info/WHEEL").decode())
            self.assertEqual(meta["Tag"], f"py3-none-{plat}", wheel.name)
            self.assertEqual(meta["Root-Is-Purelib"], "false", wheel.name)

    def test_metadata_names_the_project_and_carries_the_readme(self):
        self.build("--only", "win32")
        with zipfile.ZipFile(self.out / "sofabgen-1.2.3-py3-none-win32.whl") as z:
            meta = Parser().parsestr(z.read("sofabgen-1.2.3.dist-info/METADATA").decode())
        self.assertEqual(meta["Name"], "sofabgen")
        self.assertEqual(meta["Version"], "1.2.3")
        self.assertEqual(meta["Description-Content-Type"], "text/markdown")
        self.assertEqual(meta.get_payload(), (ROOT / "README.md").read_text(encoding="utf-8"))

    def test_prerelease_tag_reaches_the_filename_in_pep440_form(self):
        bw.main(["--version", "v1.2.3-rc1", "--from", str(self.dist), "--out", str(self.out),
                 "--only", "win_amd64"])
        self.assertTrue((self.out / "sofabgen-1.2.3rc1-py3-none-win_amd64.whl").exists())

    def test_wheels_are_byte_reproducible(self):
        first = (self.build("--only", "win_amd64")[0]).read_bytes()
        second = (self.build("--only", "win_amd64")[0]).read_bytes()
        self.assertEqual(first, second)

    def test_unknown_platform_tag_is_refused(self):
        with self.assertRaises(SystemExit):
            self.build("--only", "manylinux_2_17_s390x")


class TestCheck(unittest.TestCase):
    """The pre-upload guard. It exists because a PyPI version is immutable, so it
    has to catch what the builder got wrong before the upload, not after."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dist = fake_dist(self.tmp.name)
        self.out = Path(self.tmp.name) / "wheels"
        bw.main(["--version", "v1.2.3", "--from", str(self.dist), "--out", str(self.out)])

    def check(self, version="v1.2.3"):
        return cw.main(["--version", version, "--dir", str(self.out)])

    def test_passes_on_a_complete_build(self):
        self.assertEqual(self.check(), 0)

    def test_catches_a_missing_wheel(self):
        (self.out / "sofabgen-1.2.3-py3-none-win_arm64.whl").unlink()
        self.assertEqual(self.check(), 1)

    def test_catches_a_stray_wheel_from_another_version(self):
        # Uploading this would publish a version nobody tagged.
        stray = self.out / "sofabgen-1.2.3-py3-none-manylinux_2_17_s390x.whl"
        stray.write_bytes((self.out / "sofabgen-1.2.3-py3-none-win32.whl").read_bytes())
        self.assertEqual(self.check(), 1)

    def test_catches_wheels_built_for_a_different_tag(self):
        self.assertEqual(self.check("v1.2.4"), 1)

    def test_catches_a_corrupted_member(self):
        # Rewrite one member so its RECORD hash stops matching — the failure mode
        # that uploads fine and then refuses to install on every machine.
        wheel = self.out / "sofabgen-1.2.3-py3-none-win32.whl"
        with zipfile.ZipFile(wheel) as z:
            items = [(i, z.read(i.filename)) for i in z.infolist()]
        with zipfile.ZipFile(wheel, "w") as z:
            for info, data in items:
                z.writestr(info, data + b"\n" if info.filename.endswith("WHEEL") else data)
        self.assertEqual(self.check(), 1)

    def test_refuses_a_tag_with_no_pep440_form(self):
        with self.assertRaises(SystemExit):
            self.check("v1.2.3-nightly")


if __name__ == "__main__":
    unittest.main()
