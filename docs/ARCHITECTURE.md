# SofaBuffers Generator — Architecture

> **Status: M0 (Foundations).** This document is the single, up-to-date
> description of how the generator works and is the first thing a maintainer or
> new-language contributor should read. Keeping it current is part of the
> "done when" criterion of every milestone (PLAN §10).
>
> **What exists today (M0):** the language-independent core — CLI, config
> system, parser + hard-gate validation, generic model, `$ref`/shared-type
> resolution, semantic checks, and the frozen IR. **No language backend is
> wired yet**; a run validates a definition, resolves `$ref`, builds the IR,
> and prints a summary.

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
│   └── sbufgen/            # CLI entrypoint (the sbufgen binary, §8.8)
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
│   └── sbufgen-config-schema.json   # config schema (§7.1)
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
> `sbufgen`, so the command package is `cmd/sbufgen` to match.

---

## 5. How to add a new target language

This is the most important long-term workflow. To add `generators/<lang>/`:

1. Create the package with `generator.go` (the `Backend` impl), `visitor.go`
   (the IR Visitor), `builder.go` (source construction), and `templates/`.
2. Implement `generator.Backend`: `Lang() string` and
   `Generate(*ir.Schema, cfg map[string]any) ([]generator.File, error)`.
   Traverse the IR read-only; never mutate it.
3. Call `generator.Register(&backend{})` from the package `init()`, and
   blank-import the package from `cmd/sbufgen` so it self-registers.
4. Add the per-target config keys to `schema/sbufgen-config-schema.json`
   (§7.3) — keep schema and handled keys in lockstep (§7.1).
5. Add a root project / harness template + corpus entries (§9) and wire it into
   the matrix runner.

No edits to the core model, pipeline, IR, or the message schema are required —
adding a language is purely additive.

---

## 6. Config & extension points

- **Config model (§7):** `internal/config` loads YAML/JSON, **validates against
  the embedded config schema as a hard gate** before use, and resolves the
  effective config per target with precedence *default < generic < per-target*.
  Only `--in`/`--out` override the file from the CLI.
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
| M4+ | in progress (conformance matrix + shared vectors, more backends). |
