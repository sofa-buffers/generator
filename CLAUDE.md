# CLAUDE.md — SofaBuffers generator

This repo is the SofaBuffers code generator (`sofabgen`, Go): it compiles
message definitions (YAML/JSON) into typed encode/decode wrappers for one
target language per run. The per-language runtime libraries live in sibling
`corelib-<lang>` repos (in the `sofa-buffers` org); the generator itself never
touches wire bytes.

The main purpose of this file: **how to add a new target/corelib to the
SofaBuffers code generator.**

## Primary design references

When implementing a new target, always use these documents as the primary
design references:

- **`./docs/PLAN.md`** — the original project plan and design goals.
- **`./docs/ARCHITECTURE.md`** — the living architecture document describing
  the current design and implementation. Read §8 (backend contract), §9
  (wire/corelib API contract), §12 (testing & conformance), §13 (repository
  structure & dependency rule), and §14 (step-by-step "How to add a new target
  language") before writing any code.

Key invariant (ARCHITECTURE §1): the **corelib owns the wire format**; the
generator never touches bytes. Generated code makes typed calls into the
per-language corelib. Adding a target is purely additive — a new package under
`generators/<lang>/` that calls `generator.Register(...)`; the core
(parse → validate → IR) is never edited for a new language.

## Use-case profiles

The generator must take the intended use case of the core library into account
during code generation:

- **`maxspeed`** — optimize for maximum runtime throughput, even if this
  increases binary size (full inlining, zero-copy views, minimal
  allocations/boxing).
- **`footprint`** — optimize for minimal resource usage (`.text`, `.rodata`,
  `.data`, `.bss`) for resource-constrained embedded targets (no heap, static
  descriptors, feature-gated wire types).

These correspond to the two optimization axes in ARCHITECTURE §1 ("Design
principles"). Existing placements: footprint = `c`, `cpp` with
`corelib: c-cpp`, `rust` with `corelib: rs-no-std`; maxspeed = everything else.

Both axes are **measured**, not asserted — `tests/bench/results.txt` (ARCHITECTURE
§15) records instructions/op for every row and, for the footprint rows,
`.text`/`.data`/`.bss` cross-compiled to the embedded targets they ship to. If you
change a backend, measure that backend — `tests/bench/run.sh --rows <its rows>` —
and read the diff: that is how a claim on either axis is checked.

## Reference implementations

For additional context and implementation guidance, use an existing generator
for a language with similar requirements as a reference:

- Embedded / `footprint` target → `generators/c/` and the Rust backend's
  `no_std` path (`generators/rust/`).
- Native `maxspeed` target → `generators/cpp/`, `generators/rust/`,
  `generators/zig/`.
- Managed/GC runtime → `generators/java/`, `csharp/`, `golang/`.

Per-target **design** notes live in `./docs/ARCHITECTURE.md`.
`docs/generator/<lang>.md` is **user documentation for that target's config
options** — the options table plus a chapter per non-trivial switch, describing
the current state. Design rationale, issue numbers and change history do not
belong there.

## Definition of done for a new target/corelib

Every implementation of a new target/corelib must include **all** of:

1. **The generator implementation** — a `generators/<lang>/` package,
   registered in `init()` and blank-imported from `cmd/sofabgen`; per-target
   config keys added to `schema/sofabgen-config-schema.json`.
2. **Comprehensive tests** — unit tests for the backend, corpus coverage, and a
   conformance harness `tests/conformance/<lang>/run.sh`
   (generate → build → round-trip → byte-exact shared vectors), plus a
   `lang-<x>` CI job. The corelib repo (`corelib-<lang>`) needs its own full
   test suite against the shared vectors. Also a `tests/bench/` row: the `bench`
   verb in the project harness, an entry in `rows.json`, a `lang/<x>.sh` recipe,
   and a regenerated `results.txt` (ARCHITECTURE §15).
3. **Documentation updates** — a new `docs/generator/<lang>.md`, and updates to
   every existing doc that enumerates targets (README, `generators/README.md`,
   ARCHITECTURE §10 table, config schema docs).

A target is only done when its tests and CI job are green.

## Generated code stays thin — no silent static helpers

Generated code lands in the **user's** source tree, so it must stay clean,
small and readable: made of what their schema actually says, and nothing else.

A **static helper** is code whose shape is the same for every schema — its
schema dependence is only a bound, an element type or a default value, all of
which can be passed as an argument or a type parameter. Array placement, gap
filling, index/length bound checks, payload reassembly and UTF-8 decoding are
the typical cases. Such a helper belongs in the corelib (ARCHITECTURE §8).

When a backend change needs one, **never emit it silently**:

- **Say so.** State explicitly that the helper belongs in the corelib — in the
  PR, the commit message or the report — instead of quietly generating it.
- **Put it in the corelib first**, in its own file/namespace following the
  pattern the other corelibs already use (`collectors.go`, `seq.dart`,
  `decode/seq.ts`, `Seq.java`, `arrays.zig`), with its own tests; then make the
  backend call it.
- **If that corelib change is out of scope** for the task, raise it as an issue
  and name it in the PR as a known gap. An emitted helper is never a temporary
  shortcut: it reaches every user's tree, the corelib's own test suite cannot
  reach it, and fixing it means every user regenerating.

Emit only what differs per schema (field arms, id routing, per-field guards,
declared types) or what names a generated symbol.

The check is mechanical: generate two schemas that differ in bounds, element
types and field count. A block that is identical once literals, field names and
element types are normalised is a static helper. generator#587 is the cleanup
that followed from not doing this — five backends had re-emitted, per field,
logic that belonged in their corelib.

The three §8 overrides still apply — footprint `.text`/`.data`, maxspeed
instructions/op, and output-buffer allocation — but only when **measured**
(`tests/bench/run.sh --rows <rows>`), and the measurement is stated, never
assumed.

## General architectural rule

Whenever an architectural change is made to the generator, or a change affects
existing or future generator targets, the living architecture document
(`./docs/ARCHITECTURE.md`) must be updated to reflect the new design and
rationale. It is kept current before every push to `main`.
