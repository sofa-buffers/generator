#!/usr/bin/env python3
"""Build the per-platform sofabgen wheels from the release binaries, into
packaging/pypi/wheels/. Run at publish time (e.g. in the release workflow), then
upload the whole directory with twine.

Usage:
  python3 build-wheels.py --version v1.2.3                # download binaries from the v1.2.3 release
  python3 build-wheels.py --version v1.2.3 --from DIR     # take binaries from a local dir (release asset names)
  python3 build-wheels.py --version v1.2.3 --only win_amd64,win32   # build single targets (testing)

Each wheel ships exactly one file — the Go binary — at

    sofabgen-<version>.data/scripts/sofabgen[.exe]

which pip unpacks straight into the environment's bin/ (Scripts\\ on Windows) and
marks executable. There is no Python code, no import package and no entry point:
`pip install sofabgen` puts the binary on PATH, nothing more. That also means the
wheels are pure data — stdlib zipfile is all it takes to write them, so this
script has no build dependency of its own (the `ziglang` PyPI package ships its
compiler the same way).

Nine binaries become thirteen wheels: a Linux binary is CGO-free and static, so
the same bytes serve glibc and musl, but those are two distinct wheel tags and
therefore two files.

No sdist is built, deliberately: with one present, pip on an unsupported platform
would try to build from source and fail obscurely instead of reporting "no
matching distribution found".
"""

import argparse
import base64
import hashlib
import re
import shutil
import stat
import sys
import urllib.request
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent
OUT = ROOT / "wheels"
REPO = "sofa-buffers/generator"

# A fixed timestamp keeps the wheels byte-reproducible: same binary + same
# version in, same wheel out, whoever builds it.
ZIP_DATE = (2020, 1, 1, 0, 0, 0)


class Target:
    """One release binary and the wheel platform tags it is published under."""

    def __init__(self, goos, goarch, tags, ext=""):
        self.goos = goos
        self.goarch = goarch
        self.tags = tags
        self.ext = ext

    @property
    def asset(self):
        return f"sofabgen-{self.goos}-{self.goarch}{self.ext}"


# Release asset (goos-goarch)  ->  wheel platform tag(s).
#
# manylinux_2_17 / musllinux_1_2 are floors, not requirements: the binaries are
# static and CGO-free, so they need neither glibc nor musl at all. The tags exist
# because a bare `linux_x86_64` wheel is rejected by PyPI, and because pip has to
# be told that a musl host may install this file too.
#
# The armv7l tag likewise only decides *which hosts install the wheel*; the Go
# linux/arm build (ARMv6 baseline) runs fine on an ARMv7 host.
TARGETS = [
    Target("linux", "amd64", ["manylinux_2_17_x86_64", "musllinux_1_2_x86_64"]),
    Target("linux", "386", ["manylinux_2_17_i686", "musllinux_1_2_i686"]),
    Target("linux", "arm64", ["manylinux_2_17_aarch64", "musllinux_1_2_aarch64"]),
    Target("linux", "arm", ["manylinux_2_17_armv7l", "musllinux_1_2_armv7l"]),
    Target("darwin", "amd64", ["macosx_10_9_x86_64"]),
    Target("darwin", "arm64", ["macosx_11_0_arm64"]),
    Target("windows", "amd64", ["win_amd64"], ext=".exe"),
    Target("windows", "386", ["win32"], ext=".exe"),
    Target("windows", "arm64", ["win_arm64"], ext=".exe"),
]

# vMAJOR.MINOR.PATCH with an optional pre-release suffix, in the SemVer spelling
# the release tags use. release.yml's check-version gate accepts a broader set
# than PyPI does — see pep440() for what happens to the rest.
TAG_RE = re.compile(r"^v?(\d+\.\d+\.\d+)(?:-(a|alpha|b|beta|rc)\.?(\d+))?$")
PRERELEASE = {"a": "a", "alpha": "a", "b": "b", "beta": "b", "rc": "rc"}


def pep440(tag):
    """Translate a release tag into the PEP 440 version PyPI will accept.

    The two spellings agree on releases and disagree on pre-releases: npm takes
    `v0.21.0-rc1` literally, PyPI calls the same thing `0.21.0rc1`. Anything that
    has no faithful PEP 440 form is rejected here rather than silently uploaded
    under a version that is not the one the tag names.
    """
    m = TAG_RE.match(tag)
    if not m:
        raise SystemExit(
            f"build-wheels: tag '{tag}' has no PEP 440 equivalent; PyPI accepts "
            f"MAJOR.MINOR.PATCH with an optional a/b/rc pre-release (e.g. "
            f"v1.2.3, v1.2.3-rc1). Refusing to guess a version."
        )
    release, kind, num = m.groups()
    return release if kind is None else f"{release}{PRERELEASE[kind]}{num}"


def fetch(url):
    req = urllib.request.Request(url, headers={"User-Agent": "sofabgen-wheel-builder"})
    with urllib.request.urlopen(req) as res:
        return res.read()


def get_binary(target, version, from_dir):
    """Return the binary bytes for a target, from --from dir or the GitHub release."""
    if from_dir:
        return (Path(from_dir) / target.asset).read_bytes()
    base = f"https://github.com/{REPO}/releases/download/v{version}"
    binary = fetch(f"{base}/{target.asset}")
    want = fetch(f"{base}/{target.asset}.sha256").decode().split()[0]
    got = hashlib.sha256(binary).hexdigest()
    if want and got != want:
        raise SystemExit(
            f"build-wheels: checksum mismatch for {target.asset} (want {want}, got {got})"
        )
    return binary


def metadata(version, description):
    """The PyPI project metadata. The README is the long description — it is what
    renders on the project page, so it is carried in every wheel."""
    return (
        "Metadata-Version: 2.1\n"
        "Name: sofabgen\n"
        f"Version: {version}\n"
        "Summary: SofaBuffers code generator — compile a message definition into "
        "typed encode/decode wrappers.\n"
        "Author: SofaBuffers\n"
        "License: MIT\n"
        f"Project-URL: Homepage, https://github.com/{REPO}\n"
        f"Project-URL: Source, https://github.com/{REPO}\n"
        f"Project-URL: Documentation, https://github.com/{REPO}#readme\n"
        f"Project-URL: Changelog, https://github.com/{REPO}/releases\n"
        "Keywords: sofabuffers,codegen,serialization,cli\n"
        "Classifier: Development Status :: 4 - Beta\n"
        "Classifier: Environment :: Console\n"
        "Classifier: Intended Audience :: Developers\n"
        "Classifier: License :: OSI Approved :: MIT License\n"
        "Classifier: Operating System :: MacOS\n"
        "Classifier: Operating System :: Microsoft :: Windows\n"
        "Classifier: Operating System :: POSIX :: Linux\n"
        "Classifier: Programming Language :: Go\n"
        "Classifier: Topic :: Software Development :: Code Generators\n"
        "Classifier: Topic :: Software Development :: Compilers\n"
        # No Python code runs, so any CPython that pip itself supports will do.
        "Requires-Python: >=3.8\n"
        "Description-Content-Type: text/markdown\n"
        "\n" + description
    )


def wheel_file(plat_tag):
    return (
        "Wheel-Version: 1.0\n"
        "Generator: sofabgen-build-wheels\n"
        # The wheel carries a platform-specific payload, not pure Python.
        "Root-Is-Purelib: false\n"
        f"Tag: py3-none-{plat_tag}\n"
    )


def build_wheel(target, plat_tag, binary, version, description, license_text, outdir):
    """Write one wheel and return its path."""
    dist_info = f"sofabgen-{version}.dist-info"
    scripts = f"sofabgen-{version}.data/scripts"
    path = outdir / f"sofabgen-{version}-py3-none-{plat_tag}.whl"
    records = []

    def add(name, data, mode=0o644):
        info = zipfile.ZipInfo(name, date_time=ZIP_DATE)
        info.create_system = 3  # unix, so the mode below is preserved
        info.external_attr = (stat.S_IFREG | mode) << 16
        info.compress_type = zipfile.ZIP_DEFLATED
        z.writestr(info, data)
        digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=")
        records.append(f"{name},sha256={digest.decode()},{len(data)}")

    with zipfile.ZipFile(path, "w") as z:
        # pip installs *.data/scripts/* into the environment's bin/ and marks it
        # executable; the 0o755 here keeps the bit for anyone unzipping by hand.
        add(f"{scripts}/sofabgen{target.ext}", binary, mode=0o755)
        add(f"{dist_info}/METADATA", metadata(version, description).encode())
        add(f"{dist_info}/WHEEL", wheel_file(plat_tag).encode())
        add(f"{dist_info}/licenses/LICENSE", license_text.encode())
        # RECORD lists every file including itself, with its own hash left empty.
        records.append(f"{dist_info}/RECORD,,")
        add(f"{dist_info}/RECORD", ("\n".join(records) + "\n").encode())

    return path


def main(argv=None):
    ap = argparse.ArgumentParser(
        prog="build-wheels.py", description="Build the per-platform sofabgen wheels."
    )
    # Required, with no in-tree default: the release tag is the single source of
    # truth for the version, exactly as on the npm side.
    ap.add_argument("--version", required=True, metavar="TAG", help="release tag, e.g. v1.2.3")
    ap.add_argument("--from", dest="from_dir", metavar="DIR", help="local dir holding the release binaries")
    ap.add_argument("--only", metavar="TAGS", help="comma-separated wheel platform tags to build")
    ap.add_argument("--out", metavar="DIR", help=f"output directory (default: {OUT.name}/)")
    args = ap.parse_args(argv)

    version = pep440(args.version)
    outdir = Path(args.out) if args.out else OUT
    description = (ROOT / "README.md").read_text(encoding="utf-8")
    license_text = (ROOT.parent.parent / "LICENSE").read_text(encoding="utf-8")

    wanted = None
    if args.only:
        wanted = {t.strip() for t in args.only.split(",") if t.strip()}
        known = {tag for t in TARGETS for tag in t.tags}
        unknown = wanted - known
        if unknown:
            raise SystemExit(
                f"build-wheels: unknown platform tag(s): {', '.join(sorted(unknown))}\n"
                f"known tags: {', '.join(sorted(known))}"
            )

    if outdir.exists():
        shutil.rmtree(outdir)
    outdir.mkdir(parents=True)

    built = []
    for target in TARGETS:
        tags = [t for t in target.tags if wanted is None or t in wanted]
        if not tags:
            continue
        binary = get_binary(target, version, args.from_dir)
        for tag in tags:
            path = build_wheel(target, tag, binary, version, description, license_text, outdir)
            built.append(path)
            print(f"  {path.name}  ({path.stat().st_size / 1e6:.1f} MB)")

    if not built:
        raise SystemExit("build-wheels: nothing to build")

    print(f"\n{len(built)} wheel(s) for sofabgen {version} under {outdir}/")
    print(f"  twine upload {outdir}/*.whl")
    return 0


if __name__ == "__main__":
    sys.exit(main())
