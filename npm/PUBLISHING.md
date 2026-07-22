# Publishing the npm distribution (maintainers)

Maintainer runbook for the `@sofa-buffers/generator` npm packages. This is **not**
shipped to npm — the published package's `files` is only `bin/sofabgen.js` +
`README.md`, so this file stays in the repo. End-user docs live in
[`README.md`](./README.md); the design rationale is in
[`../docs/ARCHITECTURE.md`](../docs/ARCHITECTURE.md) (§Distribution).

## Packages

Ten packages are published together:

- **`@sofa-buffers/generator`** — the launcher (this directory). Tiny: a
  `bin/sofabgen.js` shim and an `optionalDependencies` entry per platform. No
  binary, no install script.
- **`@sofa-buffers/generator-<platform>-<arch>`** — nine per-platform packages
  (`linux-x64`, `linux-ia32`, `linux-arm64`, `linux-arm`, `darwin-x64`,
  `darwin-arm64`, `win32-x64`, `win32-ia32`, `win32-arm64`). Each declares
  matching `os`/`cpu`, ships one binary at `bin/sofabgen[.exe]`, and carries a
  short README pointing at the main package. Built from the release binaries —
  **never committed** (`npm/packages/` is git-ignored).

## Automated publish (the normal path)

Publishing is automated by the `npm-publish` job in
[`../.github/workflows/release.yml`](../.github/workflows/release.yml). On a `v*`
tag it builds the platform packages from the just-released binaries
(`build-platform-packages.js --from ../dist --version <tag>`) and publishes them
via **npm trusted publishing (OIDC, no token)** with automatic
[provenance](https://docs.npmjs.com/generating-provenance-statements) — platform
packages first so the main package's optional deps resolve on install.

**Releasing is just pushing a tag** — there is nothing to bump in-tree:

```sh
git tag v<version>
git push origin v<version>
```

## Version ↔ release coupling

**The release tag `v<version>` is the single source of truth for the version.**
The committed `version` in `package.json` is a placeholder (`0.0.0-dev`) and is
never published as-is: the `npm-publish` job injects the tag at publish time
(`build-platform-packages.js --version <tag>`), rewriting the version and every
`optionalDependencies` pin in lockstep, so the published version always equals
the tag the binaries came from. A guard in the workflow asserts this equality
across the main package and all platform packages before publishing.

The `npm` smoke workflow (`../.github/workflows/npm.yml`) doesn't read the
placeholder either — it resolves the latest published release and exercises the
production path against that.

## Trusted publishing prerequisites (npmjs.com)

Auth is OIDC — **no `NPM_TOKEN`**. npm exchanges the GitHub Actions id-token for a
short-lived publish credential. For this to work, **every one of the ten
packages** must have a **Trusted Publisher** configured on npmjs.com:

| Field | Value |
|---|---|
| Provider | GitHub Actions |
| Organization or user | `sofa-buffers` |
| Repository | `generator` |
| Workflow filename | `release.yml` |
| Environment | *(leave blank — the job defines none)* |

Notes / failure modes:

- The **workflow filename is part of the OIDC identity.** Renaming
  `release.yml` breaks publishing (see the banner at the top of that file).
- If a package's Trusted Publisher is missing/mismatched, `npm publish` fails with
  **`E404`** ("Not found") on that package — npm returns 404 rather than 403 to
  avoid leaking existence. The GitHub release still succeeds, so the break is
  silent to the binary / `go install` path; only the npm distribution stalls.
- OIDC **cannot create** a package. Each package's first-ever version must be
  bootstrapped once by hand (see the manual path below); the workflow publishes
  every version after that.

## Manual publish (bootstrap or backfill)

```sh
cd npm
# Download each binary from the v<version> release, verify its sha256, and write
# the 9 platform packages into npm/packages/  (or: --from <dir> to copy locally).
# --version is REQUIRED: the committed version is a placeholder.
node scripts/build-platform-packages.js --version v<version>
# Publish platform packages FIRST, then the main package (so its optional deps resolve).
for d in packages/*/; do npm publish "$d" --access public; done
npm publish . --access public
```

`--version <x>` supplies the real version and rewrites the root `package.json`
(version + every `optionalDependencies` pin) in lockstep; without it the script
refuses to run.

## Re-running a failed publish

If only `npm-publish` failed (e.g. a Trusted Publisher was missing), fix the
config and re-run just that job for the existing tag — no re-tag needed:

```sh
gh run rerun <run-id> --repo sofa-buffers/generator --failed
```
