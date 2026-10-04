# CI

> How the workflows are laid out, and how to test a generator change that needs
> an unreleased corelib. Repository structure is ARCHITECTURE §13; the
> conformance contract is §12.

## The jobs

`.github/workflows/ci.yml` runs on every push and pull request, and nightly
(03:17 UTC, default branch) so a corelib change that breaks generated code is
seen within a day without waiting for a generator change:

| job | what it proves |
|---|---|
| `hermetic` | the generator builds and its unit + matrix tests pass, with no network |
| `vector-copies` | every corelib's `assets/test_vectors.json` on `main` is byte-identical to `corelib-c-cpp`'s (see below) |
| `lang-<x>` (10) | generated code for `<x>` compiles against the real corelib, round-trips JSON, encodes the shared vectors byte for byte, and decodes all 131 of them into a message declaring only their anchors — so every other field must be skipped — and, where its containers grow, replays the `sequence_growth` block (ARCHITECTURE §12 item 1) |
| `lang-docs` | the `docs` target renders |
| `build-binaries` | release artifacts (main only) |

Each `lang-<x>` job is a thin wrapper around `tests/conformance/<x>/run.sh`. The
script is the source of truth and runs the same way locally — that is deliberate:
a red CI job must be reproducible with one command.

### Backend tests that need a corelib

Some Go tests beside a backend (`generators/{golang,python,c,dart}/`) build and
run generated code against a real corelib. They are gated on `SOFAB_<X>_CORELIB`
(and the target toolchain) and **skip** without it, so `hermetic` stays free of
corelibs and toolchains. `generators/{typescript,rust}/` join the same runner
without needing a corelib: their gated tests want a real `prettier` or
`rustfmt`, which the hermetic job does not have either, and
`generators/zig/` is gated on the `zig` binary (its layout test holds the
backend's line breaks to `zig fmt`). Every suite calls `run_backend_tests`,
including those whose package has no gated test yet (`csharp`, `java`,
`kotlin`), so a gated test written later is picked up on day one. The
`lang-<x>` job is where they run: its `run.sh` calls `run_backend_tests`
(`tests/conformance/lib/backend_tests.sh`), which runs the backend's **whole**
test package with the corelib variable set, no `-run` filter, and fails on any
`--- SKIP` — in a lang job nothing has a reason to skip. A new gated test is
therefore covered the moment it is written; there is
no allowlist to forget to extend.

The one exception is a test gated on a **canonical formatter**, which is
optional by design (ARCHITECTURE §12 gate 10): `run.sh` may name a formatter it
has established this run cannot use — absent, or installed at a version the
suite does not pin — and a skip whose own reason names that tool is then
reported with a `!!!!` banner instead of failing. Every `lang-*` job that
holds generated code to a formatter sets `SOFAB_FORMAT_STRICT=1`, which turns
that banner — and the suite's own skipped formatter gates — back into failures,
so in CI nothing is skipped either way.

`lang-cpp` also needs the ARM newlib toolchain (`arm-none-eabi-g++`), for the leg
that re-measures the header-macro escape list on newlib and syntax-checks
`reserved.yaml` for the embedded profiles. Without it the leg prints a `SKIP` and
`run.sh` still exits 0 -- right on a laptop, wrong in CI -- so the job sets
`SOFAB_CROSS_STRICT=1`, which turns the skip into a failure.

`lang-c` and `lang-cpp` additionally need the **ASan runtime** (`libasan`) on the
image: their decode-ownership check is built with `-fsanitize=address`, because a
freed buffer usually still reads back the bytes that were in it and a plain value
comparison would print a pass over a dangling pointer. Both scripts preflight it
and say so by name rather than failing at link time.

## Where the corelibs come from

A conformance runner needs a corelib checkout. It takes one of:

1. a **path** given as an argument or through the target's `SOFAB_*_DIR` /
   `SOFAB_*_CORELIB` variable — how you test against a local working tree;
2. otherwise a **clone**, at the ref described below.

```sh
# against local checkouts
tests/conformance/cpp/run.sh ~/corelibs/corelib-cpp ~/corelibs/corelib-c-cpp

# against clones (what CI does)
tests/conformance/cpp/run.sh
```

## Pinning a corelib branch

A generator change and the corelib change it needs land in **two repositories**,
and the corelib usually has to merge first. Until it does, `lang-<x>` builds
today's generated code against yesterday's corelib and fails — which says nothing
about the change.

To test the pair, pin the corelib branch in **`.github/corelib-refs`**:

```
# Corelib branches this branch is developed against (docs/CI.md).
# Delete before merging: every entry pins a corelib to unmerged code.
SOFAB_CORELIB_CPP_REF=feat/type-reconciliation-seam
```

One `KEY=branch` per line. The variable name is derived from the repository name
— uppercased, dashes to underscores, `_REF` appended:

| repository | variable |
|---|---|
| `corelib-cpp` | `SOFAB_CORELIB_CPP_REF` |
| `corelib-c-cpp` | `SOFAB_CORELIB_C_CPP_REF` |
| `corelib-rs-no-std` | `SOFAB_CORELIB_RS_NO_STD_REF` |
| `corelib-go` | `SOFAB_CORELIB_GO_REF` |
| `corelib-kotlin-mp` | `SOFAB_CORELIB_KOTLIN_MP_REF` |

The same variables work locally:

```sh
SOFAB_CORELIB_CPP_REF=feat/type-reconciliation-seam tests/conformance/cpp/run.sh
```

Every runner prints what it used, so a log never leaves you guessing. A clone
logs its ref and the commit that ref resolved to; a path logs the checkout's
commit, or says it is not a git checkout. Each runner then prints the blob SHA of
the `assets/test_vectors.json` it reads:

```
==> corelib-cpp @ feat/type-reconciliation-seam (<40-hex commit>)
==> corelib-c-cpp @ main (<40-hex commit>)
==> corelib-cpp: /path/to/corelib-cpp @ <40-hex commit>
==> corelib-cpp: /path/to/non-git-dir (not a git checkout)
==> corelib-c-cpp: assets/test_vectors.json blob <40-hex blob SHA>
```

### Two rules that keep this honest

**A pin is ignored on `main`.** The composite action
(`.github/actions/corelib-refs`) exports the file only for branch builds. A pin
someone forgets to delete is then a leftover line, not a `main` that is green
against code nobody merged.

**A ref that does not exist fails the run.** `clone_corelib`
(`tests/conformance/lib/corelib.sh`) never falls back to `main`:

```
FAIL: cannot clone corelib-cpp at ref 'tippfehler' ($SOFAB_CORELIB_CPP_REF)
```

A typo that silently tested the wrong library would be worse than the red job the
pin exists to fix.

### Merge order

1. Merge the corelib PR.
2. Delete `.github/corelib-refs` (or just the entry) in the generator PR.
3. Re-run `lang-<x>`; it now builds against `main` on both sides.

Step 2 is what makes the pin safe to introduce: the file's presence in the diff
is the reminder, and the reviewer sees the dependency without being told.

## The vector copies

The shared vectors live in `assets/test_vectors.json` of **every** corelib, with
`corelib-c-cpp`'s copy canonical, and each `lang-<x>` suite reads its own
corelib's copy. `tests/conformance/lib/check_vector_copies.sh` compares the git
blob SHA of every corelib's copy on `main` (GitHub contents API) with the
canonical one and exits non-zero, naming each corelib that differs. It runs in
the `vector-copies` job, so the nightly run covers it too. An unreachable API is
a failure, never a skip. To check a local checkout or a fork, name it:

```sh
tests/conformance/lib/check_vector_copies.sh corelib-go=/path/to/corelib-go
```

## The bench workflow stays manual

`.github/workflows/bench.yml` (the footprint `.text`/`.data`/`.bss` and the
instructions-per-op rows, ARCHITECTURE §15) runs on `workflow_dispatch` only, and
that is deliberate rather than an omission. It compares against
`tests/bench/results.txt`, which is regenerated by hand in the devcontainer, and
instructions per op belong to a particular binary: the CI runner pins its own
compiler, so it is a second measuring device with its own scale (the same commit
reads 56,237 Ir/op there and 24,698 in the devcontainer). A schedule would
produce a red or a drifting report every week for reasons no commit caused, and
nothing would act on it. What guards the footprint profiles on every push is the
conformance suites: the stripped `SOFAB_DISABLE_*` builds run, and the embedded
cross-compile leg is strict. A footprint claim is checked by running
`tests/bench/run.sh --rows <rows>` for the backend that changed and reading the
diff.
