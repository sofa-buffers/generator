---
name: release
description: Cut a SofaBuffers generator release — pick or validate the version, sync every "pin to the latest release" doc example through a PR, then tag main and push to trigger the automated build+publish pipeline (GitHub release, npm, PyPI). Takes an optional version override (e.g. "/release 0.24.0"); without one, asks for a bump. Use when asked to release, cut/ship a release, tag a version, or publish a new sofabgen.
user-invocable: true
---

# /release — cut a generator release

## How releases work here (read this before changing the process)

**The git tag `vMAJOR.MINOR.PATCH` is the single source of truth for the
version.** Nothing in this repo is bumped in-tree for an ordinary release —
`packaging/npm/PUBLISHING.md` and `packaging/pypi/PUBLISHING.md` both say it
plainly: "releasing is just pushing a tag."

Pushing a `v*` tag runs `.github/workflows/release.yml`:

1. **`check-version`** — the tag must match
   `^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)*$` (semver, optional
   pre-release/build suffix) or the run fails before touching anything else.
2. **`build`** — ten static `sofabgen` cross-builds
   (linux/darwin/windows × amd64/386/arm64/arm as available), version injected
   via `-ldflags -X main.version=<tag>`. The committed placeholder is
   `cmd/sofabgen/main.go`'s `var version = "0.0.0-dev"` — it is never hand-edited.
3. **`release`** — asserts the linux-amd64 binary's `--version` actually
   reports the tag, then publishes the GitHub release
   (`generate_release_notes: true`; **the title is the tag name only** — see
   the "release title" note in Boundaries).
4. **`npm-publish`** — builds and publishes ten npm packages via OIDC trusted
   publishing. The committed `packaging/npm/package.json` version is a
   `0.0.0-dev` placeholder; the job injects the tag at build time and then
   guards that every package's version equals the tag before publishing. This
   is already self-checking — do not "fix" the placeholder.
5. **`pypi-publish`** — builds and publishes thirteen wheels the same way
   (`packaging/pypi/` has no committed version at all;
   `build-wheels.py --version` is required).
6. **`verify-published`** — installs from both registries on all three OSes
   and checks every platform artifact actually landed.

**What that pipeline does *not* cover:** plain documentation that shows a
reader "pin `setup-sofabgen` to the latest release." Those are just strings —
release.yml never touches them, so they drift silently. That already
happened: `README.md` pinned `v0.22.0` while
`.github/actions/setup-sofabgen/action.yml`'s own usage example pinned
`v0.19.4`, three releases apart, until `.github/workflows/version-consistency.yml`
was added to catch it (modelled on the corelib repos' workflow of the same
name). That gate runs on `push: tags: ['v*']` and fails the tag's Actions run
if any pinned example doesn't equal the tag. **This skill exists to keep that
gate green**: it updates every pinned example to the new version *before* the
tag is pushed, since the check only runs after — a red version-consistency run
does not block the release pipeline (it's a separate workflow), so skipping
this step ships a release with silently stale docs, not a failed build.

The two anchors both the gate and this skill key off (grep for both, don't
hardcode file:line — a new pin anywhere is picked up automatically):

1. `setup-sofabgen@vX.Y.Z` — a versioned `uses:` pin.
2. `version: vX.Y.Z   # ... defaults to the latest release` — the paired
   composite-action `with:` example. The trailing comment is what tells this
   apart from `.github/workflows/action.yml`'s **deliberate** pin to an old
   version (`version: v0.19.3`, testing that pinning itself works) — that one
   must never be "fixed" to the latest tag.

If a future doc adds another "pin to the latest release" example, tell the
user to mark it the same way so both the gate and this skill find it.

## What it does, in order

### 1. Preflight

Run the essentials of `/cleanup`: fetch `--prune`, confirm the tree is clean,
on `main`, and `main` is not behind `origin/main`. Stop and report if any of
that fails — a release must be cut from a real, current `main`, and a dirty
tree is never touched.

### 2. Resolve the target version

- **Explicit arg** (`/release 0.24.0` or `/release v0.24.0`): normalize to
  bare `MAJOR.MINOR.PATCH[-prerelease]`, then validate it against the same
  regex `check-version` uses. Reject and stop on a bad format — don't try to
  "fix" it.
- **No arg**: read the latest tag (`git describe --tags --abbrev=0` /
  `gh release list --limit 1`), compute the patch/minor/major bump candidates,
  and ask the user to pick (`AskUserQuestion`) — don't guess feat-vs-fix from
  commit messages, this repo's bump size isn't mechanically derivable from
  history.

Either way, confirm the resulting tag doesn't already exist
(`git tag -l vX.Y.Z`, `gh release view vX.Y.Z`) — stop if it does, a published
version is immutable and a re-tag is not a fix.

### 3. Sync the doc-pinned versions

Grep the tracked tree for the two anchors above. For every match whose
captured version differs from the target, edit it to the target version —
this includes the `uses: …@vX.Y.Z` line, the `version: vX.Y.Z` input example,
and the README comparison-table row (all found by the same greps, so nothing
needs to be enumerated by hand here).

If nothing needed changing (already in sync — e.g. re-running this after the
sync PR merged), skip straight to step 5.

Otherwise commit on a new branch (`release/vX.Y.Z-docs` or similar), push, and
open a PR with `gh pr create`. **Stop here and hand it to the user** — per
this repo's standing rule, auto mode does not merge its own PRs without an
explicit grant, and this one gates whether the tag will come out clean.
Tell the user plainly: merge this PR, then re-invoke `/release vX.Y.Z` (same
version) to continue to tagging.

### 4. Re-verify after merge

On a re-invocation with the docs now in sync on `main` (step 1's preflight
already re-fetches), confirm the greps from step 3 now find no mismatches
before moving on — this is the same check `version-consistency.yml` will run
against the tag, done locally first so a mistake surfaces before it's public.

### 5. Confirm before the irreversible step

Pushing a tag is not like pushing a branch: it immediately fires
`release.yml`, which publishes to the GitHub releases page, npmjs.com, and
PyPI — **both registries are immutable per version**, there is no un-publish.
Always confirm explicitly before this step regardless of auto-mode, stating:
the exact tag, that it targets `origin/main`'s current tip, and what it
triggers (10 cross-builds, a GitHub release, 10 npm packages, 13 PyPI wheels,
then the `verify-published` install smoke tests).

### 6. Tag and push

```sh
git tag -a vX.Y.Z -m vX.Y.Z
git push origin vX.Y.Z
```

An annotated tag, message is the tag name only — matches this repo's "a
release title is just the tag" convention (the GitHub release title is set
from it automatically; don't hand-write a summary into either).

### 7. Report, don't block

Give the user the two Actions runs to watch (`release.yml` and
`version-consistency.yml`, e.g. `gh run list --workflow=release.yml -L 1`).
`verify-published` alone polls registries in a retry loop and the whole chain
commonly runs well past ten minutes — do not sit in a synchronous wait for
it. Point at watching the specific run rather than the whole Actions list.

## Boundaries

- **Never** hand-edit a committed version placeholder
  (`cmd/sofabgen/main.go`'s `0.0.0-dev`, `packaging/npm/package.json`'s
  `0.0.0-dev`) to a real version — both are injected and guarded by
  `release.yml` itself; editing them fixes nothing and contradicts
  "nothing is bumped in-tree."
- **Never** touch `.github/workflows/action.yml`'s `version: v0.19.3` pinned
  smoke-test step — it deliberately tests pinning to an old version, it is
  not a "latest release" example.
- **Never** rename or move `.github/workflows/release.yml` — its filename is
  part of the npm/PyPI OIDC trusted-publisher identity (see the banner at the
  top of that file). The same applies to this skill's changes: don't touch
  that file at all.
- **Never** force-push, delete, or re-create a tag that was already pushed.
  If a publish job fails after the tag went out, the fix is re-running that
  *job* for the existing tag (`gh run rerun <id> --failed`), per
  `packaging/pypi/PUBLISHING.md`'s "re-running a failed publish" — never a
  new tag for the same version.
- **Never** merge the doc-sync PR from step 3 without the user's explicit
  go-ahead.
- **Never** write a summary into a release title or an annotated tag message
  — the tag name is the whole title; detail belongs in the auto-generated
  release notes body, which `release.yml` already produces.
