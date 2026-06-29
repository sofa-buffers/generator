# SofaBuffers Generator — Architecture & Requirements

> **Purpose of this document.** A complete, language-agnostic description of
> *what* the SofaBuffers code generator is and *how* it is structured —
> sufficient to **reimplement it from scratch in another language**. It specifies
> the contracts (input format, validation, IR, wire/corelib API, output) and the
> design decisions, not Go specifics. Where a contract is normatively defined
> elsewhere, this document points to it:
>
> - **Input definition format + validation rules** — `schema/README.md` and
>   `schema/sofabuffers-schema-v1.json` (authoritative; §4–§5 here summarise).
> - **Wire format** — the [SofaBuffers wire-format docs](https://github.com/sofa-buffers/documentation)
>   and any `corelib-*` repository (§9 here summarises the contract the generated
>   code targets).
> - **Config format** — `schema/sofabgen-config-schema.json` and `docs/generator/`.
>
> Status: all 8 language backends (C, C++, Rust, Go, Python, TypeScript, C#, Java)
> are implemented and CI-green. Keep this file current — it is updated before
> every push to `main`.

---

## 1. Purpose, scope, and the firm boundary

The generator is a **definition → typed-wrapper compiler**. It reads a
SofaBuffers *message definition* (YAML/JSON), validates it, lowers it to a
language-neutral **Intermediate Representation (IR)**, and emits one idiomatic,
typed `encode`/`decode` type per object for a chosen target language.

**Firm boundary — the corelib owns the wire format.** The generator never
touches bytes: no varint encoding, byte order, framing, or field-skipping lives
in it. Generated code makes *typed calls* into a per-language runtime library
(the **corelib**) that implements the wire format (§9). Consequences:

- The entire core pipeline (parse → validate → IR) is wire-format- and
  language-independent.
- Cross-language interop is guaranteed by every corelib implementing the *same*
  wire format, verified by shared byte-exact vectors (§12) — not by the
  generator.
- A reimplementation must reproduce the **definition format**, the
  **validation**, the **IR semantics**, and the **typed calls each corelib
  expects** — but never a byte encoder.

**Fail closed.** Any parse, validation, or analysis error aborts with a clear,
located message, a non-zero exit, and **no output**. Invalid definitions are
never code-generated. All problems are reported at once.

### Design principles (the "why")

- **Per-target optimization mandate.** The generated wrapper must add *zero
  overhead* and steer each backend onto its corelib's fast/small path. There are
  really **two optimization axes**, and every backend sits on one: **minimal
  footprint** (the embedded targets — C, the C++ `c-cpp` wrapper, `no_std` Rust:
  optimize for code/RAM size, no heap) and **maximum speed / throughput**
  (everything else). "Max speed" (C++) and "high throughput" (Go/Python/Java/C#/
  TS) are the *same goal* at different ceilings — header-only C++20 can reach the
  metal (full inlining, zero-copy views, stack buffers), managed runtimes go as
  fast as their runtime allows (minimize allocations/boxing). This single mandate
  is *why* there are corelib options, multiple decode models, capability gating,
  and width-minimizing layout/writes.
- **The generator is a normal hosted program; only the *emitted* code carries
  target constraints.** The generator itself need not be `no_std`/embedded — it
  ships as a single, minimal-dependency, statically-linked, cross-compiled
  executable (Windows/Linux/macOS × x86/x86-64/ARM/ARM64). Only the Rust/C it
  *emits* is `no_std`/heap-free.
- **Hardest constraints first.** The IR and emitter were proven against the worst
  case (no-heap, no_std, static descriptors) before the throughput backends, so
  the IR carries everything the strictest target needs; the throughput languages
  then share an almost-identical `OStream`/`IStream`+Visitor shape and reuse it.
- **Closed for modification, open for extension.** The four patterns (§8) keep
  the core fixed while a new language is a new package — never a core edit.

---

## 2. System context

```
        definition file(s)  ─┐                         ┌─▶  generated source files
        (.yaml / .json)      │                         │    (one typed type per object;
                             ▼                         │     "sources" or full "project")
   config file ──▶ [ sofabgen generator ] ────────────┘
   (.yaml / .json)              │
                                └── targets one language per run (--lang)

   generated code ──calls──▶ corelib-<lang>  (owns the wire format; not produced here)
```

- **Inputs:** one definition file or a folder of them; a config file selecting
  the target and options; CLI flags (`--lang`, `--in`, `--out`, …).
- **Output:** for the selected language, either bare **sources** (the message
  types) or a buildable **project** (sources + build files + a JSON
  encode/decode harness).
- **External dependency at *runtime of the generated code*:** the corelib for
  that language. The generator itself has no runtime dependency on it.

### CLI surface (`cmd/sofabgen`)

| Flag | Meaning |
|---|---|
| `--config <file>` | Config file (carries all options; §7). |
| `--lang <target>` | Target backend (`c`, `cpp`, `rust`, `go`, `python`, `java`, `csharp`, `typescript`). |
| `--in <file\|dir>` | Definition input (overrides `generic.input_dir`). |
| `--out <dir>` | Output folder (overrides `generic.output_dir`). |
| `--print-defaults` | Print the effective resolved config for `--lang` and exit. |
| `--dump-ir` | Print the built IR as JSON and exit (no codegen) — the IR contract is observable/golden-tested. |
| `--version` | Print version and exit. |

---

## 3. The compilation pipeline

```
config (resolved: defaults → generic → per-target; --in/--out override paths)
   │
   ▼
[1] Parser     parse YAML/JSON, resolve $ref, HARD-GATE validate  → unresolved Document
[2] Model      lower validated doc → IR nodes, hoist inline types → ir.Schema (refs by key)
[3] Analysis   resolve shared-type graph + semantic checks, freeze → ir.Schema (refs resolved)
[4] IR         frozen, language-neutral Composite tree
══ Language Selection Point ══   ← the ONLY place a language is chosen
[5] Backend    Visitor(IR) + Builder                              → []File
[6] Formatter  deterministic formatting (inside the backend)      → source bytes
```

| # | Stage | Consumes | Produces |
|---|---|---|---|
| 1 | **Parser** | file bytes | `$ref`-resolved + **validated** unresolved `Document` |
| 2 | **Model** | validated `Document` | `ir.Schema` with unresolved `TypeRef`s + hoisted inline types |
| 3 | **Analysis** | `ir.Schema` | resolved shared-type graph + semantic checks; tree frozen |
| 4 | **IR** | — | the frozen Composite tree backends consume |
| 5 | **Backend** | frozen IR + effective config | `[]File` (path + bytes) |
| 6 | **Formatter** | builder output | deterministic source |

**The language-independent core ends at stage [4].** A backend is selected only
after the IR is frozen, at the **Language Selection Point** — a registry lookup
by language key. Stages [1]–[4] know nothing about any target language.

**Two IR states.** The same Composite types carry two states: *post-Model*
(`TypeRef.Target == nil`, references by key only) and *post-Analysis* (every
`TypeRef.Target` points at the single shared `NamedType`, checks have run, tree
frozen). Backends only ever see the frozen post-Analysis state and must treat it
as immutable.

---

## 4. Input contract: the definition format

Authoritative spec: **`schema/README.md`** (+ the JSON Schema). Summary:

A document has `version: 1` and at least one of `$defs` / `messages`. A message
has an optional `summary` and a required `payload` (its top-level **id scope**).
Every field requires **`id`** (0 … 2³¹−1) and **`type`**; common optional
metadata is `description`, `unit`, `deprecated` (floats also allow `decimals`
0–15). All identifiers match `^[A-Za-z][A-Za-z0-9_]*$`; objects are **closed**
(unknown keys are rejected).

**Field types and their declaration keys:**

| Category | Types / form | Key constraints |
|---|---|---|
| Unsigned int | `u8 u16 u32 u64` | optional `default` (range-checked; `u64` > 2⁵³ must be a quoted string) |
| Signed int | `i8 i16 i32 i64` | optional `default` (zig-zag on wire; `i64` past ±2⁵³ must be a quoted string) |
| Float | `fp32 fp64` | optional `default` (number), `decimals` 0–15 |
| Bool | `boolean` | optional `default` |
| String | `string` | optional `maxlen`, `default`; UTF-8 |
| Blob | `blob` | optional `maxlen` (caps **decoded** bytes), `default` is base64 |
| Enum | `type: enum` + `enum: {NAME: int \| {value,description}}` or `{$ref}` | values **signed 32-bit**, may be negative; `default` must be a declared value |
| Bitfield | `type: bitfield` + `bits: {FLAG: {pos 0–63, default?}}` or `{$ref}` | each `pos` unique |
| Array | `type: array` + `items: {type, count, maxlen?}` | fixed `count`; element `type` ∈ numeric \| `string` \| `blob`; `maxlen` only for string/blob elements |
| Struct | `type: struct` + `fields: {...}` or `{$ref}` | nested; **own id scope** |
| Union | `type: union` + `oneof: {...}` or `{$ref}`, optional `default_id` | exactly one option; **own id scope** |

**Id scopes.** `id` is the wire key a decoder uses to route/skip fields. Ids must
be unique **within each scope**, and **each struct/union opens a fresh scope**
(so nested ids never collide with the parent). The three scope kinds: a
message `payload`, a `struct`'s `fields`, a `union`'s `oneof`.

**Shared types (`$ref`).** A `{$ref: "#/$defs/<category>/<Name>"}` reuses a
definition from `$defs` so it becomes **one shared generated type** (inline
definitions duplicate). Cross-file refs `file.yaml#/$defs/...` are inlined at
load time and flattened transitively; **recursive refs are rejected** (a
recursive value member has no finite size).

**How definition types lower onto the wire** (the generator must route these to
the corelib correctly — see §9): `struct`/`union` and *arrays of string/blob*
become **sequences** (open a fresh id scope); arrays of numerics become real
**array** wire types; `enum` becomes a **signed (zig-zag) varint** with a backing
width = smallest signed int covering its value range; `bitfield` becomes an
**unsigned varint** with a backing width = smallest unsigned int covering its
highest `pos`. `sequence` is a wire type only — there is no `sequence` keyword in
the definition language.

---

## 5. Validation contract (the hard gate)

Plain JSON-Schema (draft-07) validation is **not sufficient**; a conforming
validator must reproduce all of `schema/README.md` §Validation. Checklist:

1. **Structural** schema: types, per-width default ranges, closed objects,
   required `type`+`id`, identifier pattern.
2. **Dereference-then-validate, generate-from-unresolved**: resolve every `$ref`
   and validate the *resolved* tree (a dangling ref fails fast), but lower the
   *unresolved* document so a shared `$defs` type stays a single generated type.
3. **`$data` cross-field rules** (no stock validator runs these): string
   `default` length ≤ `maxlen`; array `default` length == `items.count`.
4. **Six custom keywords**:
   - `uniqueIds` — id unique in **every** scope (payload + each struct + each union).
   - `uniquePositions` — bitfield `pos` unique.
   - `defaultMatchesEnum` — enum `default` ∈ declared values (**presence** test, so `default: 0` is checked).
   - `defaultIdMatchesUnion` — union `default_id` matches an option id (presence test).
   - `blobDefaultLength` — base64-decode the blob `default`, compare **byte** length to `maxlen`.
   - `int64Range` — exact 64-bit range for `i64`/`u64` `default`, accepting an integer or a quoted string, checked with a big-integer type.
5. **Enum values are signed 32-bit** (−2³¹ … 2³¹−1), values and `default` alike.
6. **Nesting-depth cap** (`MaxNestingDepth = 256`) and recursive-ref rejection.
7. **Fail closed** with `allErrors` (report every problem, sorted by location).

---

## 6. The Intermediate Representation (the backend data model)

The IR is the **frozen contract every backend consumes**. It is a Composite tree
(every node implements `Accept`/`Children`/`NodeName`) traversed by the Visitor
pattern. A reimplementation needs equivalent data structures:

- **`Schema`** — the root: `Version`, an ordered list of `Message`, and the
  **shared named-type graph** `Named` (keyed by canonical name, e.g.
  `struct/Point`) with a deterministic `NamedOrder`.
- **`Message`** — `Name`, `Summary`, ordered `Fields`.
- **`NamedType`** — a shared `struct`/`union`/`enum`/`bitfield`: a `Category`,
  `Name`/`Key`, and one of `Fields` (struct/union), `Consts` (enum), `Flags`
  (bitfield); unions also carry an optional `DefaultID`.
- **`Field`** — `Name`, `ID`, `Kind`, metadata (`Description`/`Unit`/
  `Deprecated`), and kind-specific data: `Default` (typed per kind), `Maxlen`,
  `Decimals` (scalars/string/blob); `Elem`/`Count`/`ElemMax` (array); `Ref`
  (composite → shared `NamedType`).
- **`Kind`** — the closed leaf/composite enum: `U8 U16 U32 U64 I8 I16 I32 I64
  FP32 FP64 Bool String Blob Array Enum Bitfield Struct Union`. Width per kind
  is intrinsic (1/2/4/8 bytes; enum/bitfield width derived from value range / max
  position) — see `internal/ir/layout.go` `AlignRank`.
- **`TypeRef`** — `{Key, Target}`; post-Analysis `Target` is always resolved.

**Determinism (required).** Model/analysis sort fields by id, named types by key,
enum consts by value, bitfield flags by pos. The IR — and therefore generated
output — is byte-stable, so golden-diff tests are meaningful. The IR is
observable via `--dump-ir` and locked by a golden snapshot.

---

## 7. Configuration model

`internal/config` loads YAML/JSON, **validates it against the embedded config
schema as a hard gate**, then resolves the **effective config per target** with
precedence **built-in default < `generic` < per-target**. Only `--in`/`--out`
override file paths from the CLI.

**Generic options** (apply to every target; `generic:` block). Built-in
defaults: `namespace=sofabuffers`, `emit=sources`, `timestamp=true`,
`timestamp_format=iso8601`, `emit_deprecated=true`, `validation=debug`,
`file_layout=file_per_message`. Others: `input_dir`, `output_dir`,
`tool_banner`, `license`, `omit_defaults`, `naming`.

**Per-target options** (`targets.<lang>:`). The config schema is the full
*intended* surface; backends today consume only a subset (the rest validate but
are reserved — documented per language in `docs/generator/<lang>.md`). The
**honored, behaviour-changing** options:

| Option | Targets | Effect |
|---|---|---|
| `corelib` | `cpp` (`cpp`\|`c-cpp`), `rust` (`rs`\|`rs-no-std`) | Selects which corelib the code targets (§9/§10). |
| `namespace` | cpp, csharp (also generic) | Wrapping namespace. |
| `package` | go, java | Package name. |
| `module_path`, `go_version` | go | `go.mod` fields. |
| `symbol_prefix` | c | Prefix on generated C symbols. |
| `emit` | all | `sources` vs `project`. |
| `omit_defaults` | all | Sparse encoding (§11). |
| `license` (generic) | all | SPDX header id; default **none** (§11). |

A reimplementation should keep the config schema and the set of honored keys in
lockstep, and resolve with the same precedence.

---

## 8. Backend contract & code-generation model

A backend is a self-contained, additive plugin. The contract:

- **Interface**: `Lang() string` (the `--lang` key) and
  `Generate(schema, cfg) ([]File, error)` where `File = {Path, Content}`. The
  backend traverses the **read-only** frozen IR and returns files; it must never
  mutate the IR.
- **Registry / self-registration**: each backend registers itself by language
  key into a central registry at init; the CLI selects via `Lookup(lang)`.
  Duplicate registration is a build-time error. The core imports the registry
  *interface* only, never a concrete backend — dependencies point inward.
- **Patterns**: **Visitor** over the IR for traversal; **Builder** for source
  construction (intent-level line/file builders, formatting separated from
  content — no ad-hoc string concatenation); **Strategy** for config-injected
  behaviour (corelib, namespace, omit, layout).
- **Emit modes** (`emit`): `sources` = just the message types; `project` = a
  buildable project (build files + an encode/decode **canonical-JSON harness**
  that the conformance tests drive).
- **Determinism**: identical (definition, config) → byte-identical output.

### Generated-code principles (every backend follows these)

These shared rules keep the wrapper zero-overhead and the output interoperable —
a reimplementation should emit code that honors all of them:

- **Stay on the corelib's typed fast path.** Always call the dedicated typed
  writers/readers (`write_unsigned`, `write_fp32`, `write_array_*`, …); never
  touch the wire format from generated code (§1 firm boundary).
- **Emit fields in ascending id order** — deterministic output, and lets the
  decoder (and where applicable the encoder) dispatch optimally.
- **Decode by `switch` on field id**, not an if-chain — compilers build a jump
  table; unknown ids fall through to the corelib's skip path, giving
  forward/backward compatibility for free.
- **Resolve everything at generation time.** Field ids, type mappings, enum
  backing widths, array element kinds/counts, `maxlen` — all known statically, so
  bake them in as constants/literals; nothing is computed at runtime.
- **No reflection / no runtime schema** — all dispatch is concrete generated
  code. (The sole exception is C, which *deliberately* uses a static descriptor
  table for footprint.)
- **Pick the narrowest correct type** — map each integer to its exact width;
  enum → smallest *signed* backing, bitfield → smallest *unsigned* backing; avoid
  widening on the hot path (§11 natural-width writes).
- **Validate cheaply or not at all on the hot path** — bounds checks (`maxlen`,
  array `count`) are debug-only assertions (or an opt-in validate mode), so
  release builds pay nothing.
- **Escape reserved-word field names.** A schema field name may collide with a
  target-language keyword (`where`, `class`, `int`, …); the backend must make it a
  valid identifier — *escape* where the language allows (Rust `r#name`, C#
  `@name`), *mangle* otherwise (C/C++/Java/Python trailing `_`), or be keyword-safe
  by construction (Go exports/capitalises; TS allows keyword member names). A few
  words can't be escaped at all (Rust `self`/`Self`/`crate`/`super`) and must be
  mangled. The **wire is unaffected** (keyed by id) and the **JSON name stays the
  original** — keep the raw name for JSON keys, and add a rename when the
  identifier was mangled (escapes like `r#`/`@` are serializer-transparent). The
  `keywords.yaml` corpus compiles a keyword-heavy schema in every backend to guard
  this (and any new backend). Per-backend helpers: `cIdent`/`cppIdent`/`csIdent`/
  `javaIdent`/`pyIdent`/`rustIdent`.

**Adding a language is purely additive** — a new `generators/<lang>/` package + a
blank import + per-target schema keys + a `tests/conformance/<lang>/run.sh` + a CI job. No
edits to the core, IR, or message schema. See §14.

---

## 9. Wire-format & corelib API contract

This is the contract the generator targets: which **typed calls** it emits and
which **decode model** the generated code uses. The generator never encodes
bytes. The **byte-level wire format** (varint/zig-zag encoding, little-endian
order, FIXLEN length-subtype framing, the field header layout) is **not repeated
here** — it is normatively specified in the
[SofaBuffers wire-format documentation](https://github.com/sofa-buffers/documentation),
and each `corelib-*` README documents its own API surface. A generator
reimplementation needs §9.1–§9.4; a corelib port needs the wire-format docs.

### 9.1 Wire-type taxonomy (for routing)

The generator only needs the *mapping* from authoring types to the eight wire
types, to route each field to the right corelib call. Each field's header
carries the field `id` and a 3-bit wire type:

| Tag | Wire type | Authoring types routed to it |
|---|---|---|
| `0b000` | varint unsigned | `u8…u64`, `boolean`, `bitfield` |
| `0b001` | varint signed (zig-zag) | `i8…i64`, `enum` |
| `0b010` | fixed-length value | `fp32`, `fp64`, `string`, `blob` |
| `0b011` / `0b100` | array of unsigned / signed | numeric arrays |
| `0b101` | array of fixed-length | `fp32`/`fp64` arrays |
| `0b110` / `0b111` | sequence start / end | `struct`, `union`, arrays of string/blob |

`struct`/`union` and arrays of string/blob are routed through `sequence_begin …
sequence_end` (each opens a fresh id scope). Decoders route by id within the
current scope and **skip unknown fields** by wire type (forward/backward
compatibility). Full framing details: the wire-format docs above.

### 9.2 Encode API (OStream)

Encoding is **streaming**: an `OStream` writes into a caller buffer and invokes a
flush sink when full (so a message can exceed RAM). The generated `encode`
serialises each field in schema/id order via these operations (names per
corelib; canonical set):

`write_unsigned(id, v)` · `write_signed(id, v)` · `write_boolean(id, v)` ·
`write_fp32(id, v)` · `write_fp64(id, v)` · `write_string(id, s)` ·
`write_blob(id, ptr, len)` · `write_array_unsigned/signed(id, elems)` ·
`write_array_fp32/fp64(id, elems)` · `write_sequence_begin(id)` ·
`write_sequence_end()`.

Integers are written at their **natural width** (the varint output is
value-based, so the bytes are identical to a widened write; this lets
width-reduced corelib builds compile — §11).

### 9.3 Decode models

Decoding has **four families**; a backend picks the one its corelib exposes. All
route by `(scope, id)` and are forward-compatible (skip unknown ids).

1. **Flat visitor + location-stack** (Rust, C#, Java, and the C++ `c-cpp`
   wrapper). The corelib drives a `Visitor` with flat callbacks; the generated
   visitor is a `(location, id)` state machine with a stack pushed/popped on
   sequence begin/end. Callbacks: `unsigned(id,v)`, `signed(id,v)`,
   `fp32/fp64(id,v)`, `string(id, total, offset, chunk)` and `blob(...)`
   (delivered in chunks; `total` is the full length), `array_begin(id, kind,
   count)` then element callbacks, `sequence_begin(id)`, `sequence_end()`. This
   is the **reusable template for any new flat-visitor corelib**.
2. **Pull-parser** (Go, Python). The generated `decode` loops `Decoder.Next()`
   → a field `{id, wire-type}`, switches on `id`, reads the typed value, and
   `Skip()`s unknowns; returns at EOF or sequence end.
3. **Child-visitor** (pure C++ `corelib-cpp`). Nested objects decode via
   `is.read(child)` (a child `IStreamMessage`); scalars via `is.read(member)`.
4. **Descriptor-table callback** (C `corelib-c-cpp`). A static descriptor table
   (id → offset → wire type, generated per object) drives
   `sofab_object_encode`/`decode`; a field callback fills members by id. Member
   *layout* is decoupled from wire order (offsets via `offsetof`).

### 9.4 Capability / value-width model

Footprint-tunable corelibs gate wire types behind build switches; the generator
must (a) only emit calls for the wire types a message uses, and (b) surface a
guard so a stripped corelib + a message needing a missing feature fails loudly.
The authoritative switch lists live in each corelib's README — the generator
only needs to mirror their *names* and gate on the schema's used features:

- **corelib-rs-no-std** — Cargo features (`fixlen`, `array`, `sequence`, `fp64`,
  `value64`); see its [README](https://github.com/sofa-buffers/corelib-rs-no-std).
  The generated crate sets `default-features = false` + exactly the features the
  schema uses, emits only the `Visitor` callbacks those types need, and a
  `require!` guard asserts the set.
- **corelib-c-cpp** — `SOFAB_DISABLE_*` macros (`FIXLEN`, `ARRAY`, `SEQUENCE`,
  `FP64`, `INT64`); see its [README](https://github.com/sofa-buffers/corelib-c-cpp).
  Generated C emits per-feature `#error` guards (only for features it uses); the
  C++ wrapper hard-requires FIXLEN+SEQUENCE and gates ARRAY/FP64/INT64.
- **Value width** — disabling 64-bit integers narrows the value type to 32-bit;
  a schema with no `u64`/`i64` field then builds against the smaller corelib.

---

## 10. Per-language backend reference

| Lang | Corelib(s) | Decode model | Notes |
|---|---|---|---|
| **C** | `corelib-c-cpp` | descriptor-table callback | `object.h` struct + static descriptor; `symbol_prefix`; auto capability + API-version guards; analytic `MAX_SIZE`. |
| **C++** | `corelib-cpp` (default) / `corelib-c-cpp` (`corelib: c-cpp`) | child-visitor / flat-visitor wrapper | header-only `OStreamMessage`+`IStreamMessage`; `c-cpp` decode pre-sizes varlen fields + links the C sources. |
| **Rust** | `corelib-rs` (default) / `corelib-rs-no-std` (`corelib: rs-no-std`) | flat-visitor location-stack | std (throughput, no features) vs no_std (feature-gated, footprint); feature-clean codegen. |
| **Go** | `corelib-go` | pull-parser | struct + `Marshal`/`Unmarshal` (`Decoder.Next/Skip`); canonical-JSON tags. |
| **Python** | `corelib-py` | pull-parser | dataclasses + `_marshal`/`_unmarshal`. |
| **TypeScript** | `corelib-ts` | flat-visitor | classes + `marshal`; 64-bit → `bigint`. |
| **C#** | `corelib-cs` | flat-visitor location-stack (`IVisitor`) | classes + `Marshal`; System.Text.Json harness. |
| **Java** | `corelib-java` (Maven) | flat-visitor location-stack | classes + `marshal`; ints → `long` (u64 via `toUnsignedString`); Gson harness. |

**Common type mapping:** enum → smallest *signed* backing; bitfield → smallest
*unsigned* backing; fixed numeric array → native fixed array/slice; string/blob
array & struct/union → sequence framing.

---

## 11. Cross-cutting design decisions

- **`omit_defaults`** — when on, a field equal to its effective default (schema
  `default:`, else type-zero) is skipped on encode and reconstructed on decode
  (protobuf-style sparse). Applies to scalar/fp/bool/enum/bitfield/string. C is
  inherently sparse; the others emit a conditional write (Rust also gains a
  schema-default `impl Default`). Default off.
- **Widest-first member layout** — value-type backends declare struct members by
  alignment widest-first (8→4→2→1, stable within a width; composite/heap = 8) to
  cut native padding, via the shared `AlignRank`/`SortedForLayout`. Applied to C,
  C++, Go (where declaration order drives layout); skipped for Rust (compiler
  reorders) and managed C#/Java. **Declaration-only** — encode/descriptor stay in
  schema/id order, so the wire bytes are byte-identical.
- **Configurable SPDX license** — a single generic `license` option sets the
  `SPDX-License-Identifier` in every generated file's header, for all targets.
  Default is **no license** (no SPDX line); `MIT`/`Apache-2.0`/… emit one;
  `none` is the explicit omit.
- **Natural-width integer writes** — encode writes each integer at its natural
  width (not a forced 64-bit cast); byte-identical varint output, and lets a
  width-reduced corelib build compile.
- **Capability guards & analytic max-size** — backends derive required corelib
  capabilities and (for fixed-storage targets) a compile-time upper-bound buffer
  size from the IR.
- **Canonical-JSON harness** — `emit: project` includes a JSON encode/decode CLI
  used by the conformance tests; field-type ↔ JSON conventions are fixed per
  backend (a few known cross-language JSON discrepancies remain — see §13/open
  items).

---

## 12. Testing & conformance strategy

A reimplementation is **conformant** when it reproduces these gates:

1. **Byte-exact shared vectors** — each corelib ships
   `assets/test_vectors.json`; the generated encoder's output must be
   byte-identical to the vectors (per language: 27–37 vectors). This is what
   guarantees cross-language interop.
2. **Round-trip harness** — `emit: project` builds the generated code against the
   real corelib and round-trips canonical JSON through encode→decode for every
   field kind (`tests/conformance/<lang>/run.sh`).
3. **Corpus** (`tests/matrix`) — a corner-case corpus generated across **all**
   backends; invalid defs are rejected; dangling-ref + depth-cap enforced.
   Per-language `run.sh` additionally **compiles/builds every corpus def** against
   the corelib.
4. **Corelib feature-subset matrix** — C (and the gated C++ wrapper) build
   generated code against each `SOFAB_DISABLE_*` config paired with a matching
   def, plus negative guard checks; Rust's no-std corpus spans the feature
   subsets.
5. **Golden reproducibility** — regenerate a fixed def for every backend and
   byte-diff against committed goldens (`tests/matrix/testdata/golden/`); plus a
   frozen IR golden.
6. **CI** — a hermetic core job + one `lang-<x>` job per language, on every push.

---

## 13. Repository structure & dependency rule

```
cmd/sofabgen/            CLI entrypoint (the sofabgen binary)
internal/                GENERIC, language-independent core (imports no backend)
  pipeline/              orchestrates stages [1]–[6]
  parser/                YAML/JSON parse + $ref resolve + hard-gate validation
  model/                 lowering: validated doc → IR nodes
  analysis/              shared-type resolution + semantic checks (freeze)
  ir/                    the Composite IR + Visitor + layout helper (no deps)
  generator/             backend CONTRACT only (interface + registry + license helper)
  config/                config load + config-schema validation
generators/<lang>/       LANGUAGE-SPECIFIC backends (self-register)
schema/                  message-definition schema + config schema (+ README spec)
schemas.go               embeds the schema files into the binary
docs/                    ARCHITECTURE.md (this — living source of truth), generator/ (per-lang config),
                         PLAN.md (HISTORICAL original plan; rationale lifted into this file)
tests/                   conformance/<lang>/run.sh harnesses + matrix/ hermetic Go tests (+ README)
```

**Dependency rule (enforced by package boundaries):** `internal/ir` imports
nothing; the core depends only on the `generator` *interface*, never on a
concrete `generators/*`. Arrows point inward — adding a language never edits the
core.

**Known open items (for interop hardening):** the canonical-JSON harness has a
few cross-language inconsistencies to reconcile for *true* JSON interop (blob is
`number[]` in C/Python/C++/Rust/C#/Java but base64 in Go; `u64` is a JSON number
everywhere except a string in TS); schema defaults are applied per-backend except
Rust (derive `Default` = zeros). These do not affect the **binary** wire interop
(which is vector-verified).

---

## 14. How to add a new target language

1. Create `generators/<lang>/` implementing the backend interface (`Lang`,
   `Generate`); traverse the IR read-only via the Visitor; build source with a
   Builder.
2. Register the backend at `init()` and blank-import it from `cmd/sofabgen`.
3. Add the per-target config keys to `schema/sofabgen-config-schema.json` and a
   `docs/generator/<lang>.md`.
4. Add a project/harness template, corpus coverage, and a `tests/conformance/<lang>/run.sh`
   (generate → build → round-trip → byte-exact vectors) plus a gated unit test.
5. Add a `lang-<x>` CI job running the harness.

A language milestone lands on `main` only when its tests + CI job are green, and
this document is updated to match.
