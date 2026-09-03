# CI

> How the workflows are laid out, and how to test a generator change that needs
> an unreleased corelib. Repository structure is ARCHITECTURE §13; the
> conformance contract is §12.

## The jobs

`.github/workflows/ci.yml` runs on every push and pull request:

| job | what it proves |
|---|---|
| `hermetic` | the generator builds and its unit + matrix tests pass, with no network |
| `lang-<x>` (10) | generated code for `<x>` compiles against the real corelib, round-trips JSON, encodes the shared vectors byte for byte, and decodes all 131 of them into a message declaring only their anchors — so every other field must be skipped — and, where its containers grow, replays the `sequence_growth` block (ARCHITECTURE §12 item 1) |
| `lang-docs` | the `docs` target renders |
| `build-binaries` | release artifacts (main only) |

Each `lang-<x>` job is a thin wrapper around `tests/conformance/<x>/run.sh`. The
script is the source of truth and runs the same way locally — that is deliberate:
a red CI job must be reproducible with one command.

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

Every runner prints what it used, so a log never leaves you guessing:

```
==> corelib-cpp @ feat/type-reconciliation-seam
==> corelib-c-cpp @ main
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
