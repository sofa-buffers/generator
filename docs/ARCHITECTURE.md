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
> Status: all 9 language backends (C, C++, Rust, Go, Python, TypeScript, C#, Java, Zig)
> plus the non-code `docs` target (self-contained HTML reference page) are
> implemented and CI-green. Keep this file current — it is updated before
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
| `--lang <target>` | Target backend (`c`, `cpp`, `rust`, `go`, `python`, `java`, `csharp`, `typescript`, `zig`, `docs`). |
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
as immutable. The "freeze" is a **contract, not a mechanism** — nothing makes
the tree immutable at runtime; analysis itself performs exactly reference
resolution plus the nesting-depth check (§5).

---

## 4. Input contract: the definition format

Authoritative spec: **`schema/README.md`** (+ the JSON Schema). Summary:

A document has `version: 1` and at least one of `$defs` / `messages`. A message
has an optional `summary` and a required `payload` (its top-level **id scope**).
Every field requires **`id`** (0 … 2³¹−1) and **`type`**; common optional
metadata is `description` and `deprecated`. **`unit` is allowed only on the ten
numeric types** (`u8…u64`, `i8…i64`, `fp32`, `fp64`) — all other types reject
it; floats additionally allow `decimals` 0–15. All identifiers match
`^[A-Za-z][A-Za-z0-9_]*$`; objects are **closed** (unknown keys are rejected).

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
| Array | `type: array` + `items: {type, count?, ...}` | element `type` ∈ numeric \| `string` \| `blob` \| `boolean` \| `enum` \| `bitfield` \| `struct` \| `union` \| `array` (composite/nested elements carry their own `fields`/`oneof`/`enum`/`bits`/`items`); `count` is **optional** — when present the array is **fixed-length `N`** (exactly `N` logical elements, §11 *fixed-count arrays*), so `default` length ≤ `count` and the unlisted trailing elements are the element default; without it the array is dynamic; `maxlen` only for string/blob elements |
| Struct | `type: struct` + `fields: {...}` or `{$ref}` | nested; **own id scope** |
| Union | `type: union` + `oneof: {...}` or `{$ref}`, optional `default_id` | exactly one option; **own id scope** |

**Bounds and fixed-storage targets.** `maxlen` and array `count` are optional
at the schema level, but the fixed-storage backends (C, the C++ `c-cpp`
profile, `no_std` Rust) require every string/blob/array to be bounded so
storage can be sized at compile time — an unbounded field there is a generation
error (a `checkBounded` pass names the offending field before any code is
emitted). The C++ `c-cpp` and `no_std` Rust profiles let `allow_dynamic` opt a
field into a heap fallback (§9.3); the **C** target has no such escape — the C
object model has no dynamic containers — so for C every string/blob needs a
`maxlen` and every array a `count`, unconditionally. Blob
`default` base64 tolerates embedded whitespace; numeric value-range semantics
beyond the declared width are left to the application.

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
the corelib correctly — see §9): `struct`/`union` and *arrays of composite or
dynamic elements* (`string`/`blob`/`struct`/`union`/nested `array`) become
**sequences** — an array lowers to a **wrapper sequence** whose child ids are the
0-based element index (each opens a fresh id scope); arrays of numeric **and
`enum`/`boolean`/`bitfield`** elements become real **array** wire types
(`enum`→signed, `boolean`/`bitfield`→unsigned — value-converted, no new wire
form); `enum` becomes a **signed (zig-zag) varint** with a backing
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
   `default` length ≤ `maxlen`; array `default` length ≤ `items.count` (a
   shorter `default` leaves the trailing elements at the element default — the
   array is still exactly `count` elements long, §11). All six custom keywords recurse into composite array
   elements (e.g. an array-of-struct element's fields get `uniqueIds`). Array
   `default` elements are additionally validated **per element** (type/range
   check, base64 decode for blob elements, enum membership).
4. **Six custom keywords**:
   - `uniqueIds` — id unique in **every** scope (payload + each struct + each union).
   - `uniquePositions` — bitfield `pos` unique.
   - `defaultMatchesEnum` — enum `default` ∈ declared values (**presence** test, so `default: 0` is checked).
   - `defaultIdMatchesUnion` — union `default_id` matches an option id (presence test).
   - `blobDefaultLength` — base64-decode the blob `default`, compare **byte** length to `maxlen`.
   - `int64Range` — exact 64-bit range for `i64`/`u64` `default`, accepting an integer or a quoted string, checked with a big-integer type.
5. **Enum values are signed 32-bit** (−2³¹ … 2³¹−1), values and `default` alike.
6. **Nesting-depth cap** (`MaxNestingDepth = 256`) and recursive-ref rejection.
   Recursive/dangling refs are rejected fail-fast during `$ref` resolution
   (stage [1]); the depth cap runs in the **analysis** stage ([3]) — both are
   pre-codegen hard gates.
7. **Fail closed** with `allErrors` (report every problem, sorted by location).

---

## 6. The Intermediate Representation (the backend data model)

The IR is the **frozen contract every backend consumes**. It is a Composite tree
traversed by the Visitor pattern — the four node types (`Schema`, `Message`,
`NamedType`, `Field`) implement `Accept`/`Children`/`NodeName`; enum consts,
bitfield flags, `TypeRef`, and `ArrayElem` are plain data, not nodes. A default
depth-first `Walk` helper exists alongside the Visitor. A reimplementation
needs equivalent data structures:

- **`Schema`** — the root: `Version`, an ordered list of `Message`, and the
  **shared named-type graph** `Named` (keyed by canonical name, e.g.
  `struct/Point`) with a deterministic `NamedOrder`.
- **`Message`** — `Name`, `Summary`, ordered `Fields`.
- **`NamedType`** — a shared `struct`/`union`/`enum`/`bitfield`: a `Category`,
  `Name`/`Key`, an optional `Summary`, an `Inline` flag (marks hoisted inline
  definitions; synthetic keys `<parentKey>_<fieldName>` / `<name>_elem`), and
  one of `Fields` (struct/union), `Consts` (enum), `Flags` (bitfield). A
  union's `default_id` is carried on the **referencing field's** `Default` —
  the `NamedType.DefaultID` member exists in the Go structs but is never
  populated; do not rely on it.
- **`Field`** — `Name`, `ID`, `Kind`, metadata (`Description`/`Unit`/
  `Deprecated`), and kind-specific data: `Default` (typed per kind), `Maxlen`,
  `Decimals` (scalars/string/blob); `Elem`/`Count`/`ElemMax` (array) — the
  optional values `Maxlen`/`Count`/`ElemMax` each pair with a presence flag
  (`HasMaxlen`/`HasCount`/`ElemMaxHas`), since 0 is a valid value — plus
  `ElemRef` (composite element → shared `NamedType`) and `ElemItems` (recursive
  `ArrayElem`, array-of-array); `Ref` (composite → shared `NamedType`). A
  composite array element is hoisted to a shared `NamedType` exactly like a
  composite field, so backends resolve both the same way.
- **`Kind`** — the closed leaf/composite enum: `U8 U16 U32 U64 I8 I16 I32 I64
  FP32 FP64 Bool String Blob Array Enum Bitfield Struct Union` (plus a
  zero-value `Invalid` sentinel). Width per kind
  is intrinsic (1/2/4/8 bytes; enum/bitfield width derived from value range / max
  position) — see `internal/ir/layout.go` `AlignRank`.
- **`TypeRef`** — `{Key, Target}`; post-Analysis `Target` is always resolved.

**Determinism (required).** Model/analysis sort messages by name, fields by id
(name as tiebreak), enum consts by value, bitfield flags by pos. `NamedOrder`
is **registration order**, not key-sorted: `$defs` types in fixed category
order (struct → union → enum → bitfield), name-sorted within each category,
then inline-hoisted synthetics appended as encountered during lowering — still
fully deterministic. The IR — and therefore generated
output — is byte-stable, so golden-diff tests are meaningful. The IR is
observable via `--dump-ir` and locked by a golden snapshot.

---

## 7. Configuration model

`internal/config` loads YAML/JSON, **validates it against the embedded config
schema as a hard gate**, then resolves the **effective config per target** with
precedence **built-in default < `generic` < per-target**. Only `--in`/`--out`
override file paths from the CLI.

**The schema lists only honored keys.** Every key the config schema accepts is
consumed by the generator; the schema and the set of consumed keys are kept in
lockstep. (Planning-era reserved keys — `buffer`, `validation`, `naming`,
`file_layout`, `timestamp`, `timestamp_format`, `emit_deprecated`, and a batch
of per-target ones — validated but were never read; they have been pruned.)

**Generic options** (apply to every target; `generic:` block): `emit`
(built-in default `sources`), `namespace`, `input_dir`, `output_dir`,
`tool_banner`, `license`. `namespace` is deliberately *not* a generic default —
it is a per-language concern, so each backend supplies its own idiomatic
default (the unified base name `message`: C++ `message`, C# `Message`, Go/Java
package `message`, C `symbol_prefix` `message_`); set `generic.namespace` to
override.

**Per-target options** (`targets.<lang>:`), documented per language in
`docs/generator/<lang>.md`:

| Option | Targets | Effect |
|---|---|---|
| `corelib` | `cpp` (`cpp`\|`c-cpp`), `rust` (`rs`\|`rs-no-std`) | Selects which corelib the code targets (§9/§10). |
| `namespace` | cpp, csharp (also generic) | Wrapping namespace. |
| `package` | go, java | Package name. |
| `module_path`, `go_version` | go | `go.mod` fields. |
| `symbol_prefix` | c | Prefix on generated C symbols. |
| `allow_dynamic` | cpp (`c-cpp`), rust (`rs-no-std`) | Lets unbounded string/blob/array fields fall back to heap containers instead of failing generation (§9.3). |
| `format` | docs (`html`) | Documentation output format of the non-code `docs` target; `html` is currently the only one. |
| `no_std` | rust | With `corelib: rs-no-std`, emit the `#![no_std]` crate profile (default `true`). |
| `emit` | all | `sources` vs `project`. |
| `license` (generic) | all | SPDX header id; default **none** (§11). |
| `tool_banner` (generic) | all | Tool name stamped in every generated file header (default `sofabgen`). |

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
  Duplicate registration panics at init (surfacing the first time a binary
  wiring both backends runs). The core imports the registry
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
  array `count`) are debug-only assertions, so release builds pay nothing.
  (There is no config knob for this today; a `validation` key existed in the
  schema but was never consumed and has been pruned.)
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
- **Emit pure ASCII *that the generator authors*.** Every byte a backend writes
  on its own — banners, separators, Makefiles, READMEs, scaffolding — must be ASCII
  (`< 0x80`): use ASCII punctuation (`-`, not the em-dash `—`). `TestGeneratedOutputIsASCII`
  sweeps every backend over the corpus + example (whose descriptions are ASCII) in
  sources *and* project mode and fails on any non-ASCII byte. This is about
  generator-authored text — **user-supplied description text passes through
  verbatim**, including UTF-8 (see next).
- **Render all definition metadata as language-idiomatic doc comments.** Every
  metadata field the schema allows becomes a doc comment (or native annotation) in
  each language's documentation-generator format so Doxygen/rustdoc/godoc/Sphinx/
  TSDoc/Javadoc/docfx pick them up. The full set a backend must surface, on the
  matching generated symbol:
  - message `summary` → the generated type doc;
  - field `description` + `unit` → the field/member doc (`unit` appended as
    `(unit: …)`);
  - field `deprecated` → the language's **native deprecation marker** *and* a doc
    note, so both the compiler/IDE and the doc generator see it: `[[deprecated]]`
    (C++), `__attribute__((deprecated))` (C), `@Deprecated` + `@deprecated` (Java),
    `[Obsolete]` + note (C#), `#[deprecated]` (Rust), `@deprecated` TSDoc (TS), the
    godoc `Deprecated:` paragraph (Go), a Sphinx `.. deprecated::` directive
    (Python), a `/// Deprecated.` note (Zig). Because a deprecated field is still
    written/read by the generated encode/decode, the backends whose deprecation
    marker is compiler-enforced (C, C++, C#, Rust) locally suppress the resulting
    self-use warning around the generated internal accesses (`#pragma GCC
    diagnostic`, `#pragma warning disable 618`, `#[allow(deprecated)]`) so
    generated code stays warning-clean;
  - enum constant `description` and bitfield flag `description` (+ a
    `(default: true|false)` note from the flag's `default`) → a doc comment on each
    generated constant. C and Java lower enum/bitfield fields to a raw integer and
    emit no named constants, so there is no symbol to document — they carry only the
    field-level metadata above.

  The doc syntaxes are language-idiomatic: Doxygen `/*! */` + trailing `/**<` (C),
  Doxygen + `///<` (C++), rustdoc `///` (Rust), godoc `//` (Go), class docstring +
  Sphinx `#:` (Python), TSDoc `/** */` (TS), Javadoc `/** */` (Java), XML
  `/// <summary>` (C#). The comment attaches immediately before (or trailing) the
  declaration so it lands inside the right namespace/package/module for the doc
  tool. **Generated comments carry only definition metadata** — never usage/example
  code, changelog history, internal issue/spec references, or other development
  notes. User text is passed through byte-for-byte (UTF-8 included); backends only
  neutralise comment-terminators (`*/` → `* /`) and XML-escape `&<>` (C#), and all
  generator-authored comment text is ASCII. `TestDescriptionsBecomeDocComments`
  (driven by the UTF-8 `testdata/descriptions.yaml`) verifies every backend emits
  the description/summary/unit text on a comment line with UTF-8 preserved and a
  deprecation marker for the deprecated field; each backend's own unit test covers
  its enum-constant, flag, and native-annotation rendering (the `docs` target
  renders the same metadata as HTML-escaped page *content* instead, with `unit` and
  `deprecated` as their own column/badge; there only UTF-8 fidelity is checked).

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

Decoding has **six families**; a backend picks the one its corelib exposes. All
route by `(scope, id)` and are forward-compatible (skip unknown ids).

1. **Flat visitor + location-stack** (Rust, C#, Java, and the C++ `c-cpp`
   wrapper). The corelib drives a `Visitor` with flat callbacks; the generated
   visitor is a `(location, id)` state machine with a stack pushed/popped on
   sequence begin/end. Callbacks: `unsigned(id,v)`, `signed(id,v)`,
   `fp32/fp64(id,v)`, `string(id, total, offset, chunk)` and `blob(...)`
   (delivered in chunks; `total` is the full length), `array_begin(id, kind,
   count)` then element callbacks, `sequence_begin(id)`, `sequence_end()`. This
   is the **reusable template for any new flat-visitor corelib**. String/blob
   callbacks take a **single-shot fast path** — when the whole payload arrives in
   one chunk (`offset == 0 && chunk_len >= total`) they build the value straight
   from the contiguous slice, keeping the byte accumulator only for split
   payloads. Fixed-count native arrays decode into a fixed/primitive member
   (Rust `[T; N]`, Java `long[]/float[]/double[]`, C++ `std::array<T, N>`)
   filled by index, not a grown heap collection; a **count-less** native array
   on a heap target is dynamic instead (C++ `corelib: cpp` gives `std::vector<T>`,
   sized to the wire count on decode — never `std::array<T, 0>`, which would drop
   every element). The C++ `c-cpp` wrapper (the embedded target) goes
   further: it **always** uses fixed-capacity, heap-free containers
   (`docs/generator/cpp.md`) — bounded strings, blobs, and their wrapper-sequence
   arrays (plus struct/union/matrix sequences) decode into schema-sized inline
   storage (`sofab::FixedString<N>` / `sofab::FixedBytes<N>` /
   `sofab::InlineVector<T,N>`) instead of `std::string`/`std::vector`, removing
   message-path heap allocation (pure `corelib: cpp` keeps
   `std::string`/`std::vector`). This is a representation change only — the deferred
   flat-visitor decode model and the wire bytes are unchanged (inline storage is
   address-stable, so it is strictly safer under the deferred decoder). All three
   fixed containers live in the corelib-c-cpp wrapper (`sofab.hpp`) — the generator
   references them rather than emitting them — and are filled via the same
   `read_*` paths as their dynamic counterparts; genuinely
   unbounded fields (no `maxlen`/`count`) are rejected unless `allow_dynamic` opts
   them into a `std::string`/`std::vector` fallback. **Rust `corelib: rs-no-std`
   (`no_std`, on by default) is the direct analog** (`docs/generator/rust.md`):
   bounded strings/blobs/sequence arrays lower to `heapless::String<N>` /
   `heapless::Vec<T,N>` (the `heapless` crate; the corelib stays storage-agnostic),
   `encode` fills a fixed `heapless::Vec<u8, MAX_SIZE>`, the location stack is a
   bounded `heapless::Vec`, `serde` is gated behind a cargo feature, and the crate
   root carries `#![no_std]` — same wire bytes, same `allow_dynamic` rule for
   unbounded fields (an `alloc` fallback). A binary can't be `no_std` on a hosted
   target, so the firmware artifact is the lib (`cargo build --lib
   --no-default-features`); the JSON harness bin is a separate `std` target.
2. **Push child-visitor** (Go). The generated struct implements the corelib's
   `sofab.Visitor`; `Decode<Msg>` runs `sofab.AcceptBytes(buf, m)`, a zero-copy
   cursor over the in-memory buffer that calls a typed method per field
   (`Unsigned/Signed/Float32/Float64/String/Bytes`, `*Array` for native arrays
   delivered widened to 64-bit). Nested scopes descend via `BeginSequence(id)`,
   which returns the child visitor: the nested object itself (`&m.Field`), or a
   small collector for a wrapper-sequence array (string/blob/struct/union/matrix
   elements). A no-op `_visitorBase` embedded in every object supplies defaults
   so each type overrides only the callbacks it uses. This replaced the earlier
   pull-parser to avoid per-byte `bufio.ReadByte` calls; the corelib still
   exposes the pull `Decoder` (family 3) for true streaming callers.
3. **Pull-parser** (Python; Go's corelib still exposes it for streaming). The
   generated `decode` loops `Decoder.Next()` → a field `{id, wire-type}`,
   switches on `id`, reads the typed value, and `Skip()`s unknowns; returns at
   EOF or sequence end.
4. **Child-visitor** (pure C++ `corelib-cpp`). Nested objects decode via
   `is.read(child)` (a child `IStreamMessage`); scalars via `is.read(member)`.
5. **Descriptor-table callback** (C `corelib-c-cpp`). A static descriptor table
   (id → offset → wire type, generated per object) drives
   `sofab_object_encode`/`decode`; a field callback fills members by id. Member
   *layout* is decoupled from wire order (offsets via `offsetof`). A `blob` is a
   **sized blob**: an opaque byte field can be shorter than its `maxlen`, and a
   bare `uint8_t[maxlen]` has no way to recover the used length (it re-encodes
   zero-padded to `maxlen`, and an all-zero short blob collapses to empty —
   silent round-trip data loss, issue #128). So the generator emits a companion
   used-length member immediately before the buffer and the
   `SOFAB_OBJECT_FIELD_BLOB_SIZED` descriptor (the C counterpart of C++
   `sofab::FixedBytes<N>`); `_init` zeroes the struct first because the length
   member is not a descriptor field. Omission is length-driven (empty ⇒ omitted),
   so a non-empty blob `default` materialises on decode but is transmitted rather
   than omitted at its default value — a benign, wire-compatible divergence. A
   blob **array** element is a sized blob too (issue #130): the wrapper-sequence
   holder stores each element as a `{ len; buf[maxlen]; }` slot and emits a
   per-element `SOFAB_OBJECT_FIELD_BLOB_SIZED`, so a sub-`maxlen` element keeps
   its exact length (an empty element is omitted by index, preserving the gap).
6. **Monomorphic pull cursor** (TypeScript). Each type emits a
   `static decodeFrom(c: Cursor)` that loops `c.readHeader()` and runs one
   `switch (c.id)` reading straight into `this.<field>` via typed pull primitives
   (`readUnsigned/readSigned` number-first, `readFp32/64`, `readString`,
   `readBlob` zero-copy view, `readUnsignedArray/readSignedArray/readFp32Array/
   readFp64Array`); a nested message recurses into `Child.decodeFrom(c)` (which
   consumes through its own `SequenceEnd`), a wrapper-sequence array loops
   `readHeader` pushing elements, and `default: c.skip(c.wire)` drops unknown ids.
   Because the only caller of each reader is that one per-type decoder, V8 keeps
   the call sites monomorphic and inlines the loop — replacing the earlier
   push/visitor path, whose shared call sites went **megamorphic** across the
   nested message types' differently-shaped visitor objects. corelib-ts keeps the
   flat `Visitor`/`decode` path too, for streaming callers.

**Decode outcome (MESSAGE_SPEC §7).** Every corelib reports the finish-less
three-valued outcome — COMPLETE / INCOMPLETE / INVALID — and the generated
one-shot decode must not hide it. For corelibs that surface INCOMPLETE as an
error/exception (Go, Rust, C++, C, Python, TS) the fallible decode entry
point (`try_decode`, Go's `(msg, error)`, thrown exceptions) already propagates
all three. The **status-returning** corelibs (C#, Java, Zig) treat INCOMPLETE
as a non-error status (C#/Java: `DecodeStatus` from `Feed`/`status()`; Zig:
`Status` from `feed(chunk)`) and leave the end-of-input verdict to the caller,
so their backends must surface it explicitly:

- C#/Java emit an additional status-surfacing entry point next to the
  back-compat best-effort `Decode`/`decode`: C# `static DecodeStatus
  TryDecode(byte[] data, out T msg)` and Java `static DecodeStatus
  tryDecode(byte[] data, T out) throws SofabException` — the status is
  returned, malformed input still throws (generator#105 / G-0008).
  Project-mode harnesses expose this as a `trydecode` mode (status line, then
  JSON), which the conformance harnesses use to pin "lone `0x80` →
  INCOMPLETE, empty input → COMPLETE".
- Zig has no back-compat surface to preserve, so the single `decode(alloc,
  data)` wrapper itself converts the terminal status: it binds `feed`'s
  `Status` and fails a trailing `.incomplete` with `error.IncompleteMessage`
  from the generated module-level error set `DecodeError = sofab.Error ||
  error{IncompleteMessage}` — a one-shot whole-buffer decode *is* at
  end-of-input, so `.incomplete` means truncation (generator#120; the error
  is deliberately distinct from `error.InvalidMessage` so the §7 outcomes
  never collapse). The Zig conformance harness pins the same two vectors
  through the plain `decode` mode.

#### Decode verdict: over-count scalar arrays are INVALID (all families)

MESSAGE_SPEC §3 makes a scalar-array field's schema `count` its **fixed length
N** (the wire carries `0..N` elements; a short wire count means the rest are the
element default — §11 *fixed-count arrays*), and §7 classifies "a
length or count above its maximum" as **INVALID** — silently accepting it is
non-conformant. Every generated decoder therefore **rejects** a scalar array
whose wire element count exceeds N: the whole decode fails with the backend's
malformed-message error (never clamp, never keep-all). Count-less (dynamic)
arrays have no N and keep every element. Who enforces it differs by family
(generator#100):

- **Corelib-enforced** — C and the C++ `c-cpp` wrapper: the object descriptor /
  `is.read` binds the member's capacity, and the C istream rejects a
  count/capacity mismatch with `SOFAB_RET_E_INVALID_MSG`. No generated guard.
- **Generated guard, corelib error hook** — pure C++ `corelib-cpp`: the
  generated `deserialize` compares the delivered count against N and calls
  `IStreamImpl::invalidate()`, so `feed()`/`try_decode` return
  `Error::InvalidMessage`.
- **Generated guard, sticky flag** — Rust (`inv` on the visitor, surfaced by
  `try_decode` as `Error::InvalidMsg`; distinct from the `err`/`BufferFull`
  capacity-overflow flag) and Zig (`inv` on the decoder; `decode` returns
  `error.InvalidMessage`).
- **Generated guard, error return / throw** — Go (`len(v) > N` in the array
  callback returns `sofab.ErrInvalidMsg` through `AcceptBytes`), Java
  (`arrayBegin` throws `SofabException(INVALID_MSG)` wrapped unchecked), C#
  (`ArrayBegin` throws `SofabException(InvalidMessage)` — the guard also bounds
  the eager `new T[count]` allocation), Python (`raise SofaDecodeError` after
  the whole-array read), TypeScript (`throw SofabError(InvalidMsg)` after the
  whole-array read).

The infallible best-effort entry points kept for back-compat (Rust/C++
`decode`) still discard the verdict; the fallible path is authoritative, and
the conformance harnesses assert the reject through it (§12).

#### Decode verdict: over-index wrapper-array elements are INVALID (heap families)

The **sequence-form analogue** of the over-count scalar rule (generator#142).
A `string`/`blob`/`struct`/`union` element array with a schema `count: N` lowers
to a wrapper sequence whose child ids are the 0-based element index (§4, §9.2). An
element whose wire id is `≥ N` is a schema-bound violation: MESSAGE_SPEC §5.1
recovers a fixed-count wrapper array's length as **`N` for every target** (a
growable-list target default-fills to `N` exactly like a pre-sized one) and §7
makes an element id `≥ N` **`INVALID`**, *never silently truncated to `N`*. The
generated decoder therefore **rejects** an over-index element **before** growing
the container — which also bounds the fill: the id is an unbounded varint, so an
unguarded id-keyed grow materialised `id+1` elements and turned a ~9-byte message
into an arbitrarily large allocation (a heap-amplification DoS). Who enforces it
splits exactly like the scalar case:

- **Heap families reject** — the 9 heap backends (`go`, `rust` std, `cpp`
  `corelib-cpp`, both Python, `java`, `typescript`, `csharp`, `zig`) emit a
  per-element `id >= N` guard using the same INVALID channel as the scalar
  over-count guard (`is.invalidate()` / sticky `inv` / `ErrInvalidMsg` /
  thrown `SofabException`/`SofabError` / `SofaDecodeError`). A dynamic wrapper
  array (no `count`) has no `N` and keeps every delivered index — its length is
  *highest present id + 1* (§5.1).
- **`no_std` Rust also rejects (string/blob)** — the generated `id >= N` guard is
  now emitted on the no_std profile too for `string`/`blob` wrapper elements: it
  fires ahead of the heapless `Vec<_, N>` capacity drop (issue#126) and sets the
  sticky `inv` flag, so the outcome is `INVALID`, converging with the heap
  families (generator#149 / F-0013). This is the index-axis twin of the over-
  `maxlen` no_std reject below — the same "a declared bound binds every target,
  regardless of memory model" rule (§7.1). A `struct`/`union` over-index on no_std
  remains a drop (a separate axis, not part of F-0013).
- **C and C++ `c-cpp` still drop** — bounded by their inline container capacity
  (the issue#126 guard in `_FixedStrSeq`/`_FixedBlobSeq`), an over-index element
  has no slot and is skipped, so decode completes rather than rejecting. This is a
  §7.1 verdict violation the generator **cannot** fix on its own, and — unlike the
  over-`maxlen` case — the C runtime's existing capacity check does **not** reach
  it. The over-`maxlen` reject (corelib-c-cpp#90) works because a `maxlen` maps to
  the read's *buffer capacity*, and the C core already rejects a wire
  `length > target_len` (`istream.c`); the generated code just passes the bound as
  the capacity. An over-index is an element-*count* bound: a fixed-count
  `string`/`blob` array lowers to a **wrapper sequence**, whose elements the core
  delivers one at a time by `id` — it never learns the schema `count`, so no
  capacity check fires. The c-cpp `IStreamImpl` exposes no `invalidate()` hook and
  the C field callback is `void`, so a callback that *does* know the count (`id >=
  N`) has no channel to make `feed()` return `INVALID`. The allocation is bounded
  by construction either way, so the DoS never reached them; only the verdict
  diverges. Tracked as **corelib-c-cpp#92** (add a callback→decoder abort channel;
  a genuinely new affordance, not covered by the #90 capacity-check fix).

#### Decode verdict: over-`maxlen` strings/blobs are INVALID (every target)

The length axis of the same rule (generator "Option B"). MESSAGE_SPEC §7 + **§7.1
("a declared bound binds every target")** make a `maxlen: L` on a `string`/`blob`
a **wire-validity bound**, not a sizing hint: a value whose wire byte length
exceeds `L` is malformed input and **MUST** be reported as `INVALID` on *every*
target, **never silently truncated to `L`** — "two conformant implementations
MUST agree on which messages are valid," regardless of allocation strategy.

- **Heap families reject** — the 9 heap backends now emit a per-field guard at
  the length header (`wire byte length > L → INVALID`) for every bounded
  string/blob, scalar field *and* wrapper-array element, using the same INVALID
  channel as the over-count/over-index guards. It is the **bounded-field twin**
  of the receiver-side `max_dyn_*` limit guards (§9.5): those reject an
  *unbounded* field's length as `LimitExceeded` (policy); this rejects a *bounded*
  field's length as `INVALID` (schema validity). A field is one or the other, so
  they never both fire. Byte length is compared, not character count (a multibyte
  UTF-8 string can exceed `L` bytes while under it in characters).
- **`no_std` Rust also rejects `INVALID`** — its `heapless::String<N>`/`Vec<u8,N>`
  already detected the over-capacity truncation (setting the `BufferFull`/`err`
  flag), but the generated maxlen guard now fires first and sets the `inv` flag,
  so the outcome is `INVALID` (not a capacity error) — converging with the heap
  families. No corelib change was needed.
- **C and C++ `c-cpp` still clamp** — corelib-c-cpp's `FixedString`/`FixedBytes`
  `set_len` truncates to `N` (`len_ = n > N ? N : n`), so an over-`maxlen` value
  is silently accepted, shortened. This is a §7.1 violation the generator cannot
  fix on its own — the c-cpp `IStreamImpl` exposes no `invalidate()` hook (the
  same gap the over-index reject hit) — so it is tracked as **corelib-c-cpp#90**.

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

### 9.5 Decode resource bounds (receiver-side limits)

MESSAGE_SPEC §5.4 bounds the decode *stack* (`MAX_DEPTH`); this is the **heap
analogue** (generator#102). Schema `count`/`maxlen` are optional — a field
without one is dynamic/unbounded, and its decoder would otherwise allocate
whatever the wire claims (heap-exhaustion DoS; count-prefixed arrays are the
sharp *amplification* vector: a ~10-byte message claiming `count = 2^31`).

Three **sofabgen config** keys — `max_dyn_array_count`, `max_dyn_string_len`,
`max_dyn_blob_len` (`generic:`, per-target overridable; **unset = unlimited**,
today's behavior bit-for-bit) — bake receiver-side caps into the generated
code as named constants. The rules, normative for every backend:

- The caps govern **only** fields the schema left unbounded. A schema-bounded
  field is governed by its own bound (#100); a field that legitimately needs
  more than the cap gets an explicit schema bound (the escape hatch).
- Exceeding a cap is a decode **error** in the corelib's `LimitExceeded`
  category — a *policy* rejection, deliberately distinct from `INVALID` (the
  bytes may be perfectly well-formed), and **never a clamp** (the #100 lesson:
  silent clamping is data corruption).
- The check runs at the count/length **header**, before any allocation or
  buffering — a claimed oversize fails fast even if the payload never arrives.
- A corelib never invents its own default cap; absent limits = current
  behavior. A wrapper-sequence array carries no count *header*, but its elements
  are keyed by an unbounded varint **index** and an id-keyed collector grows the
  container to `id+1` — so a single over-index element **is** an amplification
  vector (a ~9-byte message forcing an arbitrarily large allocation), not the
  header-driven kind the config caps guard. Two cases, by whether the field is
  schema-bounded:
  - **Bounded (`count: N`)** — the over-index element is INVALID and rejected
    *before* the grow (generator#142, §9.3 above): this both fixes the verdict
    and bounds the allocation on the heap families. The fixed-storage profiles
    were already capacity-bounded and drop it: a fixed-capacity string/blob-array
    collector that placed an element at its wire index by growing an
    `InlineVector<T,N>` looped forever once full (its `emplace_back()` no-ops at
    `N`), so an untrusted `id >= N` hung the decoder (C++ `corelib: c-cpp`, issue
    #126); the generated per-element loop is now bounded by the container
    capacity and an over-capacity index is dropped (payload skipped, as for a
    native-array over-count, §5.1).
  - **Dynamic (no `count`)** — the array legitimately grows to *highest present
    id + 1* (§5.1), and the config caps' array-count key targets the count
    *header* of a native array, not a wrapper index, so a dynamic wrapper array's
    index growth is **not** currently capped. Its per-element string/blob
    *length* still is (the `total`-header guard below), so total memory tracks
    delivered bytes; an index-only amplifier against a dynamic wrapper array is a
    known residual, tracked separately from #142.

Enforcement by family: **generated visitor guards** (Rust std, Java, C#, Zig,
pure C++ — the corelib callback exposes `count`/`total` pre-allocation; the
corelibs contribute only the error category); **passed into the corelib
decoder** (Go `sofab.WithMax*` options, Python `Decoder(max_*=...)` kwargs,
TypeScript `Cursor(buf, DecodeLimits)` — the corelib allocates, so it
enforces; the generated cap is raised to the largest schema bound of its kind
because these apply globally per decode); pure C++ additionally derives a
streaming reassembly cap (`sofab::Limits{max_buffered_field}` =
max(string/blob caps, largest schema maxlen, largest schema count x 10)) for
its `acc_` buffer. **Statically bounded profiles** (C, C++ `corelib: c-cpp`,
Rust `no_std`) are capacity-bound by construction — the keys are inert.

Independent of the option (bugfix class), no generated decoder may allocate
eagerly from an untrusted wire count: C# and Zig count-less array arms reserve
bounded and grow with delivered elements (the Java #96/#98 pattern).

---

## 10. Per-language backend reference

| Lang | Corelib(s) | Decode model | Notes |
|---|---|---|---|
| **C** | `corelib-c-cpp` | descriptor-table callback | `object.h` struct + static descriptor; `symbol_prefix`; auto capability + API-version guards; analytic `MAX_SIZE`; project mode also emits `Makefile` + `CMakeLists.txt`, `run.sh`, and a devcontainer. |
| **C++** | `corelib-cpp` (default) / `corelib-c-cpp` (`corelib: c-cpp`) | child-visitor / flat-visitor wrapper | header-only `OStreamMessage`+`IStreamMessage`; `c-cpp` decode pre-sizes varlen fields + links the C sources. |
| **Rust** | `corelib-rs` (default) / `corelib-rs-no-std` (`corelib: rs-no-std`) | flat-visitor location-stack | std (throughput, no features) vs no_std (feature-gated, footprint); feature-clean codegen. |
| **Go** | `corelib-go` | push child-visitor | struct implements `sofab.Visitor`; `Decode` via zero-copy `sofab.AcceptBytes`; `BeginSequence` descends into nested objects / array collectors; canonical-JSON tags. |
| **Python** | `corelib-py` | pull-parser | dataclasses + `_marshal`/`_unmarshal`. |
| **TypeScript** | `corelib-ts` | monomorphic pull cursor | classes + `marshal`; per-type `decodeFrom(Cursor)` (monomorphic, inlinable); 64-bit → `bigint` by default, `int64: long`/`number` backs u64/i64 arrays with corelib `Long[]` accessors (and scalars with `number`) for a bigint-free, wire-identical hot path; alloc-free `writeString`. |
| **C#** | `corelib-cs` | flat-visitor location-stack (`IVisitor`) | classes + `Marshal`; `TryDecode(data, out msg)` returns the §7 `DecodeStatus` (#105); System.Text.Json harness. |
| **Java** | `corelib-java` (Maven) | flat-visitor location-stack | classes + `marshal`; ints → `long` (u64 via `toUnsignedString`); `tryDecode(data, out)` returns the §7 `DecodeStatus` (#105); Gson harness. |
| **Zig** | `corelib-zig` | flat-visitor location-stack (comptime duck-typed) | structs with schema defaults in the declaration + `marshal`; zero-copy decode (strings/blobs borrow the input buffer, arrays from a caller allocator); fixed `[N]T` for counted native arrays; hand-rolled JSON harness (exact u64). |
| **docs** | — (non-code) | — | single self-contained HTML reference page (`message.html`): message field tables + cross-linked named types; `format: html` (only format); no conformance harness — nothing executes. |

**Common type mapping:** enum → smallest *signed* backing; bitfield → smallest
*unsigned* backing; fixed numeric array → native fixed array/slice; string/blob
array & struct/union → sequence framing.

**Metadata rendering (see §8 for the contract).** Every backend emits the
definition metadata as doc comments on the generated symbols — message `summary`
on the type; field `description`/`unit` on the member; enum-constant and
bitfield-flag `description` (plus the flag `default` as a `(default: true|false)`
note) on each generated constant. The `deprecated` flag additionally emits the
language's native deprecation marker: `[[deprecated]]` (C++),
`__attribute__((deprecated))` (C), `@Deprecated`+`@deprecated` (Java),
`[Obsolete]` (C#), `#[deprecated]` (Rust), `@deprecated` TSDoc (TS), the godoc
`Deprecated:` paragraph (Go), a Sphinx `.. deprecated::` directive (Python), and a
`/// Deprecated.` note (Zig). Because the generated encode/decode still touches a
deprecated field, C/C++/C#/Rust locally suppress the resulting self-use warning so
generated code stays warning-clean. **C and Java lower enum/bitfield fields to a
raw integer** and emit no named constants, so they carry only the field-level
metadata above. The `docs` target renders the same metadata as HTML page content
(dedicated Unit column, `deprecated` badge). Both corelib variants of C++
(`cpp`/`c-cpp`) and Rust (`rs`/`rs-no-std`) render metadata identically.

---

## 11. Cross-cutting design decisions

- **Fixed-count arrays: the trailing-default-run rule** (MESSAGE_SPEC §3,
  adopted in documentation#18; generator#136 / Crucible F-0010). A field
  declared `count: N` is a **fixed-length** array of exactly `N` logical
  elements — `count` is its *length*, not a capacity the value may fall short
  of. A wire count `M < N` denotes an array whose last `N − M` elements equal
  the **element default**. This binds both directions, and independently of a
  backend's storage model:
  - **Encode (every backend).** The canonical wire carries
    `M' = 1 + index of the last element that differs from the element default`
    (`M' = 0` when all are default); the trailing default run **must not** be
    emitted. So `[7,8,9]` in a `count: 5` u32 field encodes as `23 03 07 08 09`,
    never `23 05 07 08 09 00 00`. This is the array-level analogue of the
    sparse-canonical field rule below. When the trimmed value additionally
    **equals the field's (trimmed) default** — an all-element-default array with
    no non-empty schema `default`, or one matching its `default` — the ordinary
    §2 whole-field ≠-default test drops the field **entirely**; it is **not**
    emitted as an explicit `count: 0` array (generator#139). A growable backend
    must apply that omission test to the *trimmed* value, since the raw slice may
    be empty or shorter than `N` and would never compare equal to the padded
    `N`-element default.
  - **Decode (every backend).** A decoder **must** materialize exactly `N`
    elements — the `M` wire elements at `[0, M)`, element defaults at `[M, N)` —
    so a pre-sized fixed array (`T[N]`, `std::array<T,N>`, `[T; N]`, `[N]T`) and
    a growable list (`[]T`, `list`, `long[]`, `T[]`) recover the same value.
    Fixed-storage backends get this from zero-initialized storage; growable
    backends must pad explicitly.
  - **"Equals the element default" means BIT-PATTERN equality, not `==`.**
    `-0.0 == 0.0` is true in every target language, so a numeric compare would
    trim a trailing `-0.0` and the decoder would rebuild it as `+0.0` — silent
    round-trip data loss. The shared vectors deliberately treat `0.0` and `-0.0`
    as distinct (`array_fp32_specials`), so the compare is on bits
    (`math.Float64bits`, `f64::to_bits`, `Double.doubleToRawLongBits`,
    `Object.is`, `@bitCast`, …). This also keeps NaN (bits ≠ 0) off the trim.
  - **Dynamic (count-less) arrays are exempt** — there is no `N` to refill from,
    so a trailing default element is significant and stays on the wire: `[7,0]`
    encodes as count 2. Nested array-of-array **rows** are wrapper-sequence
    elements, not `count: N` fields, and are likewise not trimmed.
  - **Scope: native (count-prefixed) arrays only** — `u8…u64`, `i8…i64`,
    `fp32`/`fp64`, `boolean`, `enum`, `bitfield`. String/blob/struct/union
    element arrays are wrapper sequences with no wire count; their sparse
    element-id gaps already carry the same meaning (§9.1).
  - **`[M, N)` is the ELEMENT default, not the field's schema default.** A
    schema `default:` describes the whole field when the field is *absent* from
    the wire; once the field is *present* with count `M`, the trailing positions
    are zero. Backends that decode into **pre-initialized fixed storage** must
    therefore reset it: `std::array<T,N> f{1,2,3,0,0}` filled with only `M = 2`
    wire elements would otherwise leak the schema default's `3` into position 2
    (`[1,2,0,0,0]` → `23 02 01 02` → `[1,2,3,0,0]`). `cpp`/`rust`/`zig` emit that
    reset at `array_begin`, **only** when the schema default is non-zero, so
    every other schema's generated code is byte-identical. Growable backends are
    immune — they replace the container wholesale on decode.
  - **`c` satisfies the rule from the corelib, not from generated code**
    (generator#136). It emits no encode *or* decode statements at all — only a
    descriptor table that `corelib-c-cpp` walks — and
    `SOFAB_OBJECT_FIELD_ARRAY` derives the element count structurally as
    `sizeof(member) / sizeof(member[0]) == N`, with no used-length slot, so
    neither half has a seam in generated code. Both therefore live in
    `corelib-c-cpp` (corelib-c-cpp#87): `object.c` trims the trailing zero-element
    run on encode, and `_bind_array_count` clears `[M, N)` on decode. The trim sits
    on the C-only descriptor path deliberately — **not** in the
    `sofab_ostream_write_array_of_*` writers, which the C++ wrapper calls directly
    with dynamic `std::vector`s whose trailing defaults are significant. Generated
    C is only canonical against a corelib carrying that fix; against an older one
    it still interoperates (§3 requires decoders to accept a non-canonical
    encoding). See `docs/generator/c.md`.
  - **Why `cpp`/`rust`/`zig` keep their own `array_begin` reset** even though the
    `c-cpp` profile now also gets the `[M, N)` clear from `corelib-c-cpp`: pure
    `corelib: cpp`, `corelib-rs` and `corelib-zig` are separate libraries without
    it, and the backends emit one code path per profile. Where it is redundant it
    is free.
- **Sparse-canonical encoding** — encoding is **always** sparse (no config
  toggle, MESSAGE_SPEC §2): a field equal to its effective default (schema
  `default:`, else type-zero) is skipped on encode and reconstructed on decode.
  The `!= default` test is applied **per field, except a `sequence`** (a
  `struct`/`union`, and the wrapper form of composite/dynamic-element arrays):
  a sequence is always framed, so an all-default nested object becomes an *empty
  wrapper sequence*, not a dropped field. **Within a wrapper array the same rule
  reaches the elements** (id = index, MESSAGE_SPEC §2): a `string`/`blob`
  **element** is a leaf, so it is **omitted when it equals its element default
  (empty)** — leaving an id gap the decoder fills from the default, so trailing
  default elements collapse (`["a",""]` encodes as `["a"]`, `["",""]` as the
  empty wrapper). A `struct`/`union`/nested-array element is itself a sequence and
  stays framed. (The compact native scalar arrays are exempt — they carry no
  per-element header, so their elements are always serialized in full.) The
  corelibs are dumb codecs, so the
  rule lives in the **generated code**: every imperative backend emits per-field
  guards and, for wrapper-array string/blob elements, a per-element `!= empty`
  guard on encode plus an id-indexed decode collector that gap-fills with the
  element default; a native scalar array materializes its schema default and is
  whole-omitted when equal (else when empty); Rust gains a manual `impl Default`.
  Only the **C** backend defers omission to the `object.h` descriptor (same
  per-field rule; see corelib-c-cpp): when any leaf field has a non-zero
  default it emits a `static const` default image and points the descriptor at
  it via `SOFAB_OBJECT_DESCR_WITH_DEFAULTS` (the corelib seeds `_init` from the
  image and omits fields equal to it); an all-zero-default object keeps the
  plain `SOFAB_OBJECT_DESCR` (compares against zero, zero `.rodata` cost). The
  image is a `.rodata` struct, so the RAM cost is one pointer per descriptor.
  STRING fields are compared by null-terminated content, not raw buffer bytes.
  BLOB fields are **sized blobs**, whose omission is length-driven (omitted iff
  `used_len == 0`) rather than compared against the image — so a non-empty blob
  default is materialised on decode but transmitted, not omitted, at its default
  value (issue #128; `docs/generator/c.md`).
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
   `assets/test_vectors.json` (currently 75 vectors); the generated encoder's
   output must be byte-identical to the subset each language harness's filter
   selects (~37–41 per language). This is what guarantees cross-language
   interop.
2. **Round-trip harness** — `emit: project` builds the generated code against the
   real corelib and round-trips canonical JSON through encode→decode for every
   field kind (`tests/conformance/<lang>/run.sh`). Each harness also feeds one
   **malformed** input — an over-count scalar array (5 elements against
   `someuintarray`'s `count: 4`) — and asserts the decode **fails** (INVALID,
   §9.3), while the `count == N` control still decodes (generator#100). The
   harness `decode` therefore uses the fallible entry point everywhere (Rust
   `try_decode`, C++ `try_decode`, …).
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
6. **CI** — a hermetic core job + one `lang-<x>` job per target, on every
   push to `main`, every pull request, and manual dispatch. Each `lang-<x>` job
   additionally uploads the generated sources (example + realworld + corpus,
   built by `tests/gen-artifacts.sh`, including the non-default corelib
   variants for C++/Rust) as a downloadable artifact. `lang-docs` is
   artifact-only (the rendered HTML reference pages) — nothing executes, so it
   has no conformance step.
7. **Hermetic unit layer** — Go unit tests beside the code:
   `internal/{parser,analysis,config,pipeline,ir}` and per-backend
   `generators/*/backend_test.go` (plus gated corelib round-trip tests), and
   dedicated matrix suites for sparse omission (`omit_test.go`), shared refs
   (`refs_test.go`), the multi-file real-world example (`realworld_test.go`),
   ASCII output, and doc comments (§8).

---

## 13. Repository structure & dependency rule

```
cmd/sofabgen/            CLI entrypoint (the sofabgen binary)
internal/                GENERIC, language-independent core (imports no backend)
  pipeline/              orchestrates stages [1]–[5] (stage [6] formatting lives inside each backend)
  parser/                YAML/JSON parse + $ref resolve + hard-gate validation
  model/                 lowering: validated doc → IR nodes
  analysis/              ref resolution + nesting-depth check (freeze-by-contract)
  ir/                    the Composite IR + Visitor + layout helper (no deps)
  generator/             backend CONTRACT only (interface + registry + license helper)
  config/                config load + config-schema validation
generators/<lang>/       LANGUAGE-SPECIFIC backends (self-register; Go's dir is
                         golang/, its --lang key "go")
schema/                  message-definition schema + config schema (+ README spec)
schemas.go               embeds the schema files into the binary
docs/                    ARCHITECTURE.md (this — living source of truth), generator/ (per-lang config),
                         PLAN.md (HISTORICAL original plan; rationale lifted into this file),
                         plans/ (feature design docs), perf-patches/ (generated-code performance
                         fixes: rationale + reference diffs, now folded into the backends)
examples/                example config + message definitions (incl. the multi-file realworld/ set)
assets/                  project logo/icon (README images)
tests/                   conformance/<lang>/run.sh harnesses + matrix/ hermetic Go tests (+ README);
                         gen-artifacts.sh builds the per-language CI artifact bundle
.github/workflows/       ci.yml (hermetic + lang-<x> jobs), release.yml
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
(which is vector-verified). Further known drift: `NamedType.DefaultID` is
declared but never populated (§6). (The planning-era `cpp-embedded` target was
removed from the config schema — embedded C++ shipped as the `cpp` target's
`corelib: c-cpp` profile instead.)

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
