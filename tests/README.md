# Tests

Two tiers, by what they need to run:

| Tier | Where | Needs | Run |
|------|-------|-------|-----|
| **Hermetic matrix tests** | [`matrix/`](matrix/) | just Go | `go test ./tests/matrix/` |
| **Per-language conformance harnesses** | [`conformance/<lang>/`](conformance/) | that language's toolchain + its corelib | `./tests/conformance/<lang>/run.sh` |

```
tests/
├── matrix/                 # Tier 1 — hermetic Go tests (run in the CI "hermetic" job)
│   ├── matrix_test.go      #   generate every corpus def across ALL backends; reject invalid defs
│   ├── golden_test.go      #   regenerate scalars.yaml per backend, byte-diff vs committed goldens
│   ├── refs_test.go        #   $ref / shared-type graph resolution
│   ├── omit_test.go        #   sparse-canonical marshal
│   ├── realworld_test.go   #   the multi-file vehicle_telemetry schema
│   ├── corpus/             #   definition corpus — see corpus/README.md
│   │   ├── defs/           #     15 positive corner-case definitions
│   │   ├── invalid/        #     22 definitions that MUST be rejected
│   │   └── shared/         #     $defs reused across defs
│   └── testdata/golden/    #   committed golden output, one dir per backend
│
├── conformance/            # Tier 2 — per-language integration harnesses (one CI job each: lang-<x>)
│   ├── c/        { run.sh, example_roundtrip.c }
│   ├── cpp/      { run.sh, check_vectors.py }
│   ├── go/       { run.sh }
│   ├── python/   { run.sh }
│   ├── java/     { run.sh, check_vectors.py }
│   ├── csharp/   { run.sh, check_vectors.py }
│   ├── rust/     { run.sh, check_vectors.py }
│   └── typescript/ { run.sh, check_vectors.py }
│
└── gen-artifacts.sh        # shared: generate example sources per language (CI artifacts)
```

## Tier 1 — `matrix/` (hermetic)

Pure Go tests, no language toolchain or corelib required, so they run in the
hermetic CI core job on every push. They exercise the generator itself: every
corpus definition generates across all registered backends (8 languages plus
the `docs` target), every invalid definition is
rejected, the IR/`$ref` graph resolves, and regenerated output is byte-identical
to the committed goldens (the reproducibility gate). The corpus is documented in
[`matrix/corpus/README.md`](matrix/corpus/README.md).

```sh
go test ./tests/matrix/
```

## Tier 2 — `conformance/<lang>/run.sh`

Each harness is the real end-to-end check for one language:

**generate → build the generated code against the real corelib → JSON
encode/decode round-trip → byte-exact shared-vector conformance → compile every
corpus definition.**

They need that language's toolchain installed. By default each `run.sh` **clones
the corelib** into a temp dir; to test against a local checkout, pass its path as
`$1` or set the env var:

| Lang | Corelib(s) | Path arg / env var | Extra files |
|------|-----------|--------------------|-------------|
| `c` | corelib-c-cpp | `$1` / `SOFAB_C_CORELIB` | `example_roundtrip.c` |
| `cpp` | corelib-cpp **and** corelib-c-cpp | `$1` `$2` / `SOFAB_CPP_DIR` `SOFAB_C_DIR` | `check_vectors.py` |
| `rust` | corelib-rs-no-std **and** corelib-rs | `$1` `$2` / `SOFAB_RS_CORELIB` `SOFAB_RS_STD_CORELIB` | `check_vectors.py` |
| `go` | corelib-go | `$1` / `SOFAB_GO_CORELIB` | |
| `python` | corelib-py | `$1` / `SOFAB_PY_CORELIB` | |
| `java` | corelib-java | `$1` / `SOFAB_JAVA_CORELIB` | `check_vectors.py` |
| `csharp` | corelib-cs | `$1` / `SOFAB_CS_CORELIB` | `check_vectors.py` |
| `typescript` | corelib-ts | `$1` / `SOFAB_TS_CORELIB` | `check_vectors.py` |

`cpp` and `rust` each exercise **both** of their corelibs (the `corelib` config
option). `check_vectors.py` drives the generated harness against the corelib's
shared `assets/test_vectors.json` and asserts byte-exact output.

```sh
# clone the corelib(s) automatically:
./tests/conformance/cpp/run.sh
# or point at local checkouts:
./tests/conformance/cpp/run.sh /path/to/corelib-cpp /path/to/corelib-c-cpp
```

Each harness maps to a CI job named `lang-<x>` in `.github/workflows/ci.yml`.

## `gen-artifacts.sh`

Generates the example/corpus sources for one language into a directory, which CI
uploads as the `generated-<lang>` artifact. For `cpp` and `rust` it emits **both**
corelib variants (default + the alternate, under `<name>-<corelib>/`).

```sh
./tests/gen-artifacts.sh <lang> <out-dir>
```
