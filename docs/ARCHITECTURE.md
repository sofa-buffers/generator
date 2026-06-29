# SofaBuffers Generator — Architecture

> **Status: ALL 8 language backends (C, Go, Python, TypeScript, C++, Rust, C#, Java) complete + CI green.** This document is the
> single, up-to-date description of how the generator works and is the first
> thing a maintainer or new-language contributor should read. Keeping it current
> is part of the "done when" criterion of every milestone (PLAN §10), and it is
> updated **before every push to `main`**.
>
> **What exists today:** the language-independent core (CLI, config, parser +
> hard-gate validation, model, `$ref`/shared-type resolution, semantic checks,
> frozen IR) **plus the first language backend — C (`generators/c`)** — which
> generates `object.h`-based code that compiles, round-trips, and is byte-exact
> against the shared wire vectors. GitHub Actions CI (`.github/workflows/ci.yml`)
> runs a hermetic core job and a per-language job (`lang-c`) on every push.
>
> **Milestone model:** each target language is a milestone — a working backend
> with its own CI job and tests, landed on `main` only when green, then on to
> the next language. Order (testable-toolchain first): **C ✓ → Go ✓ → Python ✓
> → TypeScript ✓ → C++ ✓ → Rust ✓ → C# ✓ → Java ✓ — ALL 8 BACKENDS COMPLETE.** C++
> (`generators/cpp`, max-speed `corelib-cpp`) is header-only: each object derives
> `OStreamMessage`+`IStreamMessage` with `serialize`/`deserialize`, nested via
> `os.write(id,child)` / `is.read(child)`; 37 shared vectors byte-exact
> (`tests/cpp/run.sh`, CI job `lang-cpp`). A `corelib: c-cpp` target option emits
> the same code against the **C++ wrapper in `corelib-c-cpp`** (decode pre-sizes
> variable-length fields; Makefile links the C sources); `tests/cpp/run.sh`
> round-trips and checks vectors against **both** C++ corelibs.
> TypeScript (`generators/typescript`) emits classes with `marshal(OStream)` and
> a visitor-based `decode` against `corelib-ts` (64-bit → `bigint`); 27 shared
> vectors byte-exact (`tests/typescript/run.sh`, CI job `lang-typescript`).
> Python (`generators/python`) emits dataclasses + `_marshal`/pull-parser
> `_unmarshal` against `corelib-py`; 37 shared vectors byte-exact
> (`tests/python/run.sh`, CI job `lang-python`).
> Go (`generators/golang`) emits one struct per object with `Marshal`
> (streaming `Encoder`) + a pull-parser `Unmarshal` (`Decoder.Next/Skip`)
> against `corelib-go`, with canonical-JSON struct tags; verified byte-exact on
> 37 shared vectors (`tests/go/run.sh`, CI job `lang-go`).

---

## 1. Overview & responsibilities

The generator is a **definition → typed-wrapper compiler**. It reads a
SofaBuffers message definition (YAML/JSON), validates it, lowers it to a
language-neutral Intermediate Representation (IR), and — once backends are
wired — emits one idiomatic, typed `serialize`/`deserialize` type per object
for a target language.

**Firm boundary:** the **corelib owns the wire format** (varints, byte order,
framing, field-skipping). The generator never touches bytes; it emits typed
calls into each corelib's public encode/decode API (PLAN §4). This is why the
core pipeline is entirely language- and wire-format-independent.

The tool **fails closed**: any validation or analysis error aborts with a
clear, located error, a non-zero exit, and **no output** (PLAN §1).

---

## 2. Architecture & patterns

The design follows four established patterns (PLAN §8):

- **Composite** — the model and IR are trees where every element implements a
  common `Node` interface (`Accept`, `Children`, `NodeName`), so traversal is
  uniform and recursive. See `internal/ir`.
- **Visitor** — generation is a `Visitor` over the IR (`VisitSchema`,
  `VisitMessage`, `VisitNamedType`, `VisitField`). A backend is one visitor
  family; new outputs (a docs visitor, a test-harness visitor) are added
  without touching the model. `ir.Walk` provides the default depth-first walk.
- **Builder** — backends will construct source files through an intent-level
  Builder API (no ad-hoc string concatenation), with formatting separated from
  content. *(Builders arrive with the first backend in M2.)*
- **Strategy** — configurable behaviour (naming, decode model, buffer mode,
  output language) is injected from the validated config, not hard-coded.

Rationale: the patterns keep the core closed for modification but open for
extension — a new language is a new package, never an edit to the core.

---

## 3. Pipeline / flows

```
config (§7) ┐ (resolved: defaults → generic → per-target; --in/--out override paths)
            ▼
YAML / JSON ─▶ [1] Parser     parse + hard-gate validation (resolved doc)
            ─▶ [2] Model      lower validated doc → IR nodes (refs preserved)
            ─▶ [3] Analysis   resolve shared-type graph + semantic checks
            ─▶ [4] IR         frozen, language-neutral Intermediate Representation
            ══ Language Selection Point ══   ← the ONLY place a language is chosen
            ─▶ [5] Backend    Visitor(IR) + Builder → files     (not wired in M0)
            ─▶ [6] Formatter  deterministic formatting           (with the backend)
```

Stage by stage (what it consumes → produces):

| # | Stage | Package | Consumes | Produces |
|---|---|---|---|---|
| 1 | Parser | `internal/parser` | file bytes | unresolved `Document` + **validated** (hard gate) |
| 2 | Model | `internal/model` | validated, unresolved `Document` | `ir.Schema` with unresolved `TypeRef`s + hoisted inline types |
| 3 | Analysis | `internal/analysis` | `ir.Schema` | resolved shared-type graph + semantic checks |
| 4 | IR | `internal/ir` | — | the frozen Composite tree backends consume |
| 5 | Backend | `generators/<lang>` | frozen IR + effective config | `[]generator.File` |
| 6 | Formatter | (in the backend) | builder output | deterministic source |

**The language-independent core ends at stage [4].** A backend is selected only
after the IR is frozen, at the **Language Selection Point** —
`internal/generator.Lookup(lang)`. `internal/pipeline` wires the stages.

### Validation contract (stage [1])

Validation is hand-ported from `schema/README.md` because the schema relies on
Ajv-only features no stock Go validator accepts. The parser reproduces, over
the **`$ref`-resolved** document:

- the **structural** schema (types, per-width default ranges, closed objects,
  required `type`+`id`, name pattern);
- the two **`$data`** cross-field rules (string `default` ≤ `maxlen`; array
  `default` length == `items.count`);
- the six **custom keywords** — `uniqueIds` (every id scope: payload + nested
  struct/union), `uniquePositions` (bitfield), `defaultMatchesEnum`,
  `defaultIdMatchesUnion`, `blobDefaultLength` (base64-decode then compare
  bytes), `int64Range` (exact 64-bit via `math/big`, accepting integer or
  quoted string);
- **dereference-then-validate, generate-from-the-unresolved-document**: the
  validator checks the resolved tree (a dangling `$ref` fails fast), while the
  model lowers the *unresolved* document so a shared `$defs` type becomes one
  shared generated type, never duplicated (PLAN §3.4).
- **cross-file `$ref`** (`internal/parser/external.go`): a `$ref` of the form
  `file.yaml#/$defs/<cat>/<Name>` is inlined at load time — the referenced
  definition (and its same-file dependencies) is merged into the document's own
  `$defs` and the `$ref` rewritten to local, so everything downstream sees a
  self-contained document. Chains flatten transitively. A **recursive `$ref`**
  is rejected (a recursive value member has no finite size).

All problems are reported at once (`allErrors`), sorted by location.

### Two IR states (model vs analysis)

PLAN §8.2 describes a "generic model" and an "IR". In this implementation both
are the same Composite types (`internal/ir`) in two states:

- **post-Model** — `TypeRef.Target == nil` (references by key only);
- **post-Analysis** — every `TypeRef.Target` points at the single shared
  `NamedType`, semantic checks have run, and the tree is **frozen** (backends
  treat it as immutable, PLAN §8.6).

Semantic checks in M0: shared-type resolution (dangling-ref detection), and the
shared `MaxNestingDepth = 256` cap (PLAN §4.2), with recursive-struct back-edges
broken so analysis terminates.

---

## 4. Project structure

```
.
├── cmd/
│   └── sofabgen/            # CLI entrypoint (the sofabgen binary, §8.8)
├── internal/              # GENERIC, language-independent core (imports no backend)
│   ├── pipeline/          #   orchestrates stages [1]–[6]
│   ├── parser/            #   YAML/JSON parse + $ref resolve + hard-gate validation
│   ├── model/             #   lowering: validated doc → IR nodes
│   ├── analysis/          #   shared-type resolution + semantic checks
│   ├── ir/                #   the Composite IR + Visitor (no dependencies)
│   ├── generator/         #   backend CONTRACT only (interface + registry)
│   └── config/            #   config load + config-schema validation (§7)
├── generators/            # LANGUAGE-SPECIFIC backends (none wired in M0)
├── schema/
│   ├── sofabuffers-schema-v1.json   # message-definition schema (authoritative)
│   └── sofabgen-config-schema.json   # config schema (§7.1)
├── schemas.go             # embeds the two schema files into the binary
└── docs/
    ├── PLAN.md            # full design
    └── ARCHITECTURE.md    # this file
```

**Package ↔ stage:** `parser`→[1], `model`→[2], `analysis`→[3], `ir`→[4],
`generators/<lang>`→[5], formatter→[6]; `pipeline` wires them; `config` feeds
all.

**Dependency rule (enforced by package boundaries):** `internal/ir` imports
nothing; the core depends only on the `internal/generator` *interface*, never on
a concrete `generators/*` package. Arrows point inward.

> *Naming note:* PLAN §8.7 sketches `cmd/codegen`; the binary is named
> `sofabgen`, so the command package is `cmd/sofabgen` to match.

---

## 5. How to add a new target language

This is the most important long-term workflow. To add `generators/<lang>/`:

1. Create the package with `generator.go` (the `Backend` impl), `visitor.go`
   (the IR Visitor), `builder.go` (source construction), and `templates/`.
2. Implement `generator.Backend`: `Lang() string` and
   `Generate(*ir.Schema, cfg map[string]any) ([]generator.File, error)`.
   Traverse the IR read-only; never mutate it.
3. Call `generator.Register(&backend{})` from the package `init()`, and
   blank-import the package from `cmd/sofabgen` so it self-registers.
4. Add the per-target config keys to `schema/sofabgen-config-schema.json`
   (§7.3) — keep schema and handled keys in lockstep (§7.1).
5. Add a root project / harness template + corpus entries (§9), a
   `tests/<lang>/run.sh` harness (generate → build → round-trip → conformance
   against that corelib), and a gated Go test mirroring `generators/c`.
6. Add a `lang-<x>` job to `.github/workflows/ci.yml` that runs the harness, so
   the backend is CI-verified on every push.

No edits to the core model, pipeline, IR, or the message schema are required —
adding a language is purely additive. **A language milestone lands on `main`
only when its tests + CI job are green.**

### Reference backend: C (`generators/c`)

The C backend is the worked example to copy. Shape: a `gen` visitor over the IR
+ a `cfile` Builder; per object a struct + static `object.h` descriptor table +
encode/decode/init wrappers; enum→signed / bitfield→unsigned backing;
struct/union/array-of-string → nested sequence object; auto-derived capability
guards + API-version guard + analytic `MAX_SIZE` (`cost.go`). `emit: project`
adds build files + devcontainer wiring + an IR-driven encode/decode JSON harness
(`project.go`). Tests: hermetic structural + determinism, plus
`SOFAB_C_CORELIB`-gated compile/round-trip/vector-conformance (`tests/c/run.sh`).

---

## 6. Config & extension points

- **Config model (§7):** `internal/config` loads YAML/JSON, **validates against
  the embedded config schema as a hard gate** before use, and resolves the
  effective config per target with precedence *default < generic < per-target*.
  Only `--in`/`--out` override the file from the CLI.
- **`omit_defaults` (config):** when true, every backend skips a field equal
  to its effective default (schema `default:`, else the type-zero) and decode
  reconstructs it — protobuf-style sparse encoding, applied to scalar / `fp` /
  `bool` / `enum` / `bitfield` / `string` fields. The 7 dense backends emit a
  conditional write (and Rust gains a schema-default `impl Default` so decode
  reconstructs correctly); C is inherently sparse (the `object.h` encoder
  already omits zero/default fields). Default is off (dense). Tested in
  `tests/matrix/omit_test.go` and round-tripped per language.
- **Capability guards (§5.4), max-size & streaming (§5.5–§5.6):** these are
  backend concerns; the IR already carries the data a backend needs to derive
  required capabilities (the kinds/maxlens/counts per message). They arrive with
  the backends (M2+).
- **Determinism:** model/analysis sort fields by id, named types by key, enum
  consts by value, and flags by pos, so the IR — and future generated output —
  is stable for golden-diff CI (PLAN §8.6).
- **Planned future outputs:** test harnesses, docs, OpenAPI specs, and
  additional languages — all added as new Visitors, no core changes.

---

## 7. Milestone status

| Milestone | State |
|---|---|
| **M0 Foundations** | **done** — core pipeline (CLI, config, parser+validation, model, analysis, IR) implemented and tested; this doc created. Tag `m0`. |
| **M1 Format finalized** | **done** — schema + IR frozen; locked by a deterministic golden IR snapshot (`internal/ir/testdata/example.ir.json`) and the `--dump-ir` flag. Tag `m1`. |
| **M2 First backend (C)** | **done** — `generators/c` emits `object.h`-based struct + descriptor tables + encode/decode/init wrappers + capability guards + API-version guard + MAX_SIZE. `example.yaml` compiles **and round-trips** against the real `corelib-c-cpp` (`tests/c/run.sh`); guards verified to fire. Tag `m2`. |
| **M3 Root-project generator (C)** | **done** — `emit: project` scaffolds a buildable C project (Makefile + CMakeLists + devcontainer wiring + README) with an **IR-driven encode/decode JSON harness** (§9.1). The harness builds against `corelib-c-cpp` and JSON round-trips every field kind. Tag `m3`. |
| **M4 C conformance backbone** | **done** — drives the generated C encoder against the corelib's language-agnostic shared vectors (`assets/test_vectors.json`): **34 vectors byte-exact** (non-zero scalar/string at id 0). Sparse-encoder zero/blob/array cases are covered by the round-trip harness. `tests/c/run.sh` is the one-command backbone. Tag `m4`. |
| **Go backend** | **done** — `generators/golang`: struct + `Marshal`/pull-parser `Unmarshal` against `corelib-go`; `emit: project` Go module + stdlib-json harness; **37 shared vectors byte-exact** (dense encoder also matches zero values). `tests/go/run.sh`. |
| **Python backend** | **done** — `generators/python`: dataclasses + `_marshal`/pull-parser `_unmarshal` against `corelib-py`; stdlib-json harness (blob as `list[int]`, matching C); **37 shared vectors byte-exact**. `tests/python/run.sh`. |
| **TypeScript backend** | **done** — `generators/typescript`: classes + `marshal(OStream)` + visitor-based `decode` against `corelib-ts`; 64-bit → `bigint`; strict-typecheck clean; **27 shared vectors byte-exact**. `tests/typescript/run.sh`. |
| **C++ backend** | **done** — `generators/cpp` (max-speed `corelib-cpp`): header-only structs (OStreamMessage+IStreamMessage), nested via `os.write`/`is.read`, enum-class backing, `OStreamInline<_maxSize>`; **37 shared vectors byte-exact**. `tests/cpp/run.sh`. A `corelib: c-cpp` option targets the **`corelib-c-cpp` C++ wrapper** (same code, decode pre-sizes variable-length fields, Makefile links the C sources); `run.sh` exercises **both** C++ corelibs. |
| **Per-language corpus build** | **done** — every `tests/<lang>/run.sh` now generates and **compiles every corpus definition against the real corelib** (C/C++/Java compile, Go/Rust/C# build, Python imports, TS typechecks) in addition to the example round-trip + vectors. This caught and fixed 3 real backend bugs (Go fp32/fp64 array element type, Go unused import in enum-only files, C# negative-enum cast). |
| **M7 corpus matrix** | **done** — `tests/matrix`: a corner-case corpus (9 positive defs) generated across ALL 8 backends (+ Go-parse check), 11 invalid defs rejected, dangling-ref + nesting-depth-cap enforced. Hermetic (runs in the core CI job). |
| **M8 reproducibility + release** | **done** — golden-output test (regenerate scalars.yaml for every backend, byte-diff vs committed goldens); `.github/workflows/release.yml` builds static `sofabgen` binaries on a `v*` tag — linux (amd64/386/arm64/arm), windows (amd64/386/arm64), macOS (amd64/arm64) — each attached individually (with a `.sha256`) to the release; README quickstart added. |
| **CI** | **done + green** — `.github/workflows/ci.yml`: hermetic core job + `lang-c` + `lang-go` + `lang-python` + `lang-typescript` + `lang-cpp` + `lang-rust` + `lang-csharp` + `lang-java` jobs on every push. |
| **Rust backend** | **done** — `generators/rust` (corelib-rs-no-std): structs + `marshal` + a flat-visitor decode (location-stack state machine), `require!` capability guards + Cargo features, serde-json harness; **37 shared vectors byte-exact**. `tests/rust/run.sh`. |
| **C# backend** | **done** — `generators/csharp` (corelib-cs): classes + `Marshal` + flat-visitor location-stack decode (`IVisitor`); System.Text.Json harness (byte[]<->number[] converter); **37 shared vectors byte-exact**. `tests/csharp/run.sh`. |
| **Java backend** | **done** — `generators/java` (corelib-java, Maven): classes + `marshal` + flat-visitor location-stack decode (nested switch); ints→long (u64 via `toUnsignedString`); Gson harness; **37 shared vectors byte-exact**. `tests/java/run.sh`. |
