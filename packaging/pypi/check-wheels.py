#!/usr/bin/env python3
"""Verify the built wheels before they are uploaded.

Usage: python3 check-wheels.py --version v1.2.3 [--dir DIR]

A PyPI version is immutable: once `sofabgen 1.2.3` exists, that file set is what
users get forever, and a wheel that uploads happily can still refuse to install
(a wrong RECORD) or install the wrong thing (a version that is not the tag's).
This is the last gate before the upload, so it re-derives everything from the
archives themselves rather than trusting the builder that just ran:

  - the wheel set is complete — every target tag present, nothing extra;
  - filename version == METADATA version == the tag, in PEP 440 spelling;
  - the WHEEL tag agrees with the filename, and the payload is a script;
  - RECORD covers every member with a matching sha256 and size.
"""

import argparse
import base64
import hashlib
import importlib.util
import sys
import zipfile
from email.parser import Parser
from pathlib import Path

ROOT = Path(__file__).resolve().parent

_spec = importlib.util.spec_from_file_location("build_wheels", ROOT / "build-wheels.py")
bw = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(bw)


def check_wheel(path, version, errors):
    plat = path.name[len(f"sofabgen-{version}-py3-none-") : -len(".whl")]
    dist_info = f"sofabgen-{version}.dist-info"

    def bad(msg):
        errors.append(f"{path.name}: {msg}")

    with zipfile.ZipFile(path) as z:
        names = set(z.namelist())

        scripts = {n for n in names if n.startswith(f"sofabgen-{version}.data/scripts/")}
        if scripts != {f"sofabgen-{version}.data/scripts/sofabgen"} and scripts != {
            f"sofabgen-{version}.data/scripts/sofabgen.exe"
        }:
            bad(f"expected exactly one script payload, found {sorted(scripts)}")

        try:
            meta = Parser().parsestr(z.read(f"{dist_info}/METADATA").decode())
        except KeyError:
            return bad(f"no {dist_info}/METADATA (is the version in the filename right?)")
        if meta["Name"] != "sofabgen":
            bad(f"METADATA Name is {meta['Name']!r}, expected 'sofabgen'")
        if meta["Version"] != version:
            bad(f"METADATA Version is {meta['Version']!r}, expected {version!r}")

        wheel = Parser().parsestr(z.read(f"{dist_info}/WHEEL").decode())
        if wheel["Tag"] != f"py3-none-{plat}":
            bad(f"WHEEL Tag is {wheel['Tag']!r}, but the filename says py3-none-{plat}")

        # RECORD is what pip verifies at install time — a mismatch here is a wheel
        # that uploads fine and then fails on every user's machine.
        recorded = set()
        for line in z.read(f"{dist_info}/RECORD").decode().splitlines():
            name, digest, size = line.rsplit(",", 2)
            recorded.add(name)
            if name == f"{dist_info}/RECORD":
                continue
            if name not in names:
                bad(f"RECORD lists {name}, which is not in the archive")
                continue
            data = z.read(name)
            want = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
            if digest != f"sha256={want}":
                bad(f"RECORD hash mismatch for {name}")
            if int(size) != len(data):
                bad(f"RECORD size mismatch for {name}")
        if recorded != names:
            bad(f"RECORD does not cover {sorted(names - recorded)}")


def main(argv=None):
    ap = argparse.ArgumentParser(prog="check-wheels.py")
    ap.add_argument("--version", required=True, metavar="TAG", help="release tag, e.g. v1.2.3")
    ap.add_argument("--dir", dest="wheel_dir", metavar="DIR", help=f"wheel directory (default: {bw.OUT.name}/)")
    args = ap.parse_args(argv)

    version = bw.pep440(args.version)
    wheel_dir = Path(args.wheel_dir) if args.wheel_dir else bw.OUT

    want = {f"sofabgen-{version}-py3-none-{tag}.whl" for t in bw.TARGETS for tag in t.tags}
    found = {p.name for p in wheel_dir.glob("*.whl")} if wheel_dir.is_dir() else set()

    errors = []
    for missing in sorted(want - found):
        errors.append(f"missing wheel: {missing}")
    for extra in sorted(found - want):
        # An extra wheel is usually a leftover from another version — uploading it
        # would publish a version nobody tagged.
        errors.append(f"unexpected wheel: {extra}")
    for name in sorted(want & found):
        check_wheel(wheel_dir / name, version, errors)

    if errors:
        print(f"check-wheels: {len(errors)} problem(s) with sofabgen {version} in {wheel_dir}/", file=sys.stderr)
        for e in errors:
            print(f"  {e}", file=sys.stderr)
        return 1

    print(f"check-wheels: {len(want)} wheel(s) for sofabgen {version} — all good")
    return 0


if __name__ == "__main__":
    sys.exit(main())
