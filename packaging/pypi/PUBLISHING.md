# Publishing the PyPI distribution (maintainers)

Maintainer runbook for the `sofabgen` PyPI project. End-user docs live in
[`README.md`](./README.md) — that file is embedded in every wheel as the long
description, so it *is* what renders on the project page. The design rationale is
in [`../../docs/ARCHITECTURE.md`](../../docs/ARCHITECTURE.md) (§Distribution).

## What is published

**One project, `sofabgen`, and thirteen wheels per version.** PyPI resolves the
host natively through wheel tags, so there is no launcher package and no
per-platform sub-project — the npm channel needs both, this one does not.

Nine release binaries become thirteen wheels because the four Linux binaries are
published twice: the same static, CGO-free binary serves glibc and musl, but
`manylinux_2_17_*` and `musllinux_1_2_*` are distinct tags (and a bare
`linux_x86_64` wheel is rejected by PyPI outright).

**No sdist**, deliberately. With one present, pip on an unsupported platform would
try to *build* from source and fail obscurely instead of reporting that no
matching distribution exists.

The wheels are built from the release binaries at publish time and **never
committed** (`wheels/` is git-ignored).

## Automated publish (the normal path)

Publishing is automated by the `pypi-publish` job in
[`../../.github/workflows/release.yml`](../../.github/workflows/release.yml). On a
`v*` tag it builds the wheels from the just-released binaries
(`build-wheels.py --from dist --version <tag>`), runs `check-wheels.py` over them,
and uploads via **PyPI trusted publishing (OIDC, no token)** with automatic
[PEP 740 attestations](https://docs.pypi.org/attestations/).

**Releasing is just pushing a tag** — there is nothing to bump in-tree:

```sh
git tag v<version>
git push origin v<version>
```

## Version ↔ release coupling

**The release tag `v<version>` is the single source of truth for the version.**
There is no committed version anywhere in `packaging/pypi/` — `build-wheels.py`
requires `--version <tag>` and refuses to run without it.

One wrinkle the npm side does not have: **PEP 440 is not SemVer.** npm takes
`v0.21.0-rc1` literally; PyPI calls the same thing `0.21.0rc1`. `pep440()` in
`build-wheels.py` translates the `a`/`b`/`rc` spellings (`-rc1`, `-rc.1`, `-beta2`,
`-alpha1`, …) and **refuses** anything else — `v1.2.3-nightly` or `v1.2.3+build7`
would fail the job rather than upload under a version that is not the one the tag
names. `release.yml`'s `check-version` gate is deliberately broader than this, so
a tag can pass it and still stop here.

`check-wheels.py` then asserts, before anything leaves the runner, that the wheel
set is complete, that filename version == METADATA version == the tag, that each
`WHEEL` tag agrees with its filename, and that every RECORD covers its archive
with matching hashes. A PyPI version is immutable once uploaded; that is the last
gate.

## Trusted publishing prerequisites (pypi.org)

Auth is OIDC — **no API token**. PyPI exchanges the GitHub Actions id-token for a
short-lived upload credential. Configure a **Trusted Publisher** on the project:

| Field | Value |
|---|---|
| Provider | GitHub Actions |
| Owner | `sofa-buffers` |
| Repository | `generator` |
| Workflow filename | `release.yml` |
| Environment | `pypi` |

Notes / failure modes:

- The **workflow filename is part of the OIDC identity.** Renaming `release.yml`
  breaks publishing (see the banner at the top of that file).
- **The environment name is matched too**, and this is where the two registries
  differ: PyPI's publisher names `pypi` and the job declares
  `environment: pypi`; npm's publisher leaves the environment **blank** and its
  job declares none. Cross the two and the publish fails.
- A mismatch surfaces as an **`invalid-publisher`** error from PyPI. The GitHub
  release and the npm publish still succeed, so the break is silent to every
  other install path — only PyPI stalls.
- The repo needs a GitHub environment named `pypi` (Settings → Environments). It
  can be left unprotected, or given a required reviewer if you want a human gate
  in front of every upload.

## Bootstrap: the first-ever version

Unlike npm, **PyPI OIDC can create the project** — there is no manual first
publish. Before the first tag, file a **pending publisher**, with the same field
values as the table above plus the project name `sofabgen`.

**File it at the organization level**, not under a personal account:
[`pypi.org/organizations/sofa-buffers/`](https://pypi.org/organizations/sofa-buffers/)
→ **Publishing**. A pending publisher filed from a personal account makes *that
account* the owner of the project on first upload; filed from the organization, the
**organization owns it** regardless of who filled in the form. On the first
successful upload it converts into a normal publisher by itself.

A pending publisher does **not** reserve the name — if someone else registers
`sofabgen` first, it silently becomes invalid.

## Optional: a dry run against TestPyPI

To rehearse the whole path before it is real, file a second pending publisher on
[test.pypi.org](https://test.pypi.org) and point the job at it for one tag:

```yaml
      - uses: pypa/gh-action-pypi-publish@release/v1
        with:
          packages-dir: packaging/pypi/wheels
          repository-url: https://test.pypi.org/legacy/
```

Costs one throwaway tag and proves everything except the production publisher.

## Manual publish (fallback)

Only needed if OIDC is unavailable. Requires a PyPI API token:

```sh
cd packaging/pypi
python3 build-wheels.py --version v<version>   # downloads + verifies the release binaries
python3 check-wheels.py --version v<version>
twine upload wheels/*.whl
```

`--version <x>` is required; `--from <dir>` takes the binaries from a local
directory instead of downloading them. Note that a manual upload carries **no
attestations** — those only come from trusted publishing.

## Re-running a failed publish

If only `pypi-publish` failed (e.g. the publisher was misconfigured), fix it and
re-run just that job for the existing tag — no re-tag needed:

```sh
gh run rerun <run-id> --repo sofa-buffers/generator --failed
```

## Testing

- `packaging` job (`ci.yml`) — unit tests for both scripts on every PR: RECORD
  hashes and coverage, tag agreement, script layout, METADATA, the PEP 440 table,
  reproducibility, and each guard failure the checker is supposed to catch.
- `pypi` workflow (`pypi.yml`) — on every change under `packaging/pypi/`, builds
  the host wheel from the **latest published release** on all three runner OSes,
  `pip install`s it into a clean venv, runs the CLI it puts on `PATH`, generates
  real code, and uninstalls it again. That is the only place the install semantics
  themselves (executable bit, `bin/` vs `Scripts\`) are actually exercised.
