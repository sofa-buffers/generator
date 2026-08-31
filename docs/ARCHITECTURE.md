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
> Status: all 11 language backends (C, C++, Rust, Go, Python, TypeScript, C#, Java,
> Kotlin, Zig, Dart)
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
| `--lang <target>` | Target backend (`c`, `cpp`, `rust`, `go`, `python`, `java`, `kotlin`, `csharp`, `typescript`, `zig`, `dart`, `docs`). |
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
| Array | `type: array` + `items: {type, count?, ...}` | element `type` ∈ numeric \| `string` \| `blob` \| `boolean` \| `enum` \| `bitfield` \| `struct` \| `union` \| `array` (composite/nested elements carry their own `fields`/`oneof`/`enum`/`bits`/`items`); `count` is **optional** — it is a **capacity**: the array may carry `0 .. count` elements and `count` never reaches the wire (§11 *`count` is a capacity*), so a `default` shorter than `count` stays that length and is **not** padded; without it the array is unbounded; `maxlen` only for string/blob elements |
| Struct | `type: struct` + `fields: {...}` or `{$ref}` | nested; **own id scope** |
| Union | `type: union` + `oneof: {...}` or `{$ref}`, optional `default_id` | exactly one option; **own id scope** |

**Bounds and fixed-storage targets.** `maxlen` and array `count` are optional
at the schema level, but the fixed-storage backends (C, the C++ `c-cpp`
profile, `no_std` Rust) require every string/blob/array to be bounded so
storage can be sized at compile time — an unbounded field there is a generation
error (a `checkBounded` pass names the offending field before any code is
emitted). That holds in both C++ `c-cpp` storage modes: `allow_dynamic` chooses
the container a *bounded* field lives in, never whether a bound is required
(§9.3), and the `no_std` Rust profile follows the same rule. The pure `cpp`
profile also offers `allow_dynamic: false`, but there it is an optimisation
rather than a requirement: it applies per field wherever a bound exists and
leaves the rest dynamic, so it never turns an unbounded field into an error; the **C** target has
no such escape — the C
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
| `package` | go, java, kotlin | Package name. |
| `module_path`, `go_version` | go | `go.mod` fields. |
| `symbol_prefix` | c | Prefix on generated C symbols. |
| `allow_dynamic` | cpp (both corelibs) | Storage for **bounded** fields: `true` = `std::string`/`std::vector`, `false` = inline `FixedString`/`FixedBytes`/`InlineVector`. Defaults `true` on `corelib: cpp`, `false` on `c-cpp`. On `c-cpp` bounds stay mandatory in both modes; on `cpp` an unbounded field simply keeps its dynamic container, so the switch applies per field and never fails a build (§9.3). |
| `allow_dynamic` | rust (both corelibs) | Storage for **bounded** fields: `true` = `String`/`Vec`, `false` = fixed-capacity `heapless::String<N>`/`heapless::Vec<T, N>`. Defaults `true` on `corelib: rs`, `false` on `rs-no-std` — the same corelib-keyed default the C++ switch carries. On `rs-no-std` bounds stay mandatory in both modes and become decode-path checks; on `rs` an unbounded field simply keeps its dynamic container, so the switch applies per field and never fails a build (§9.3). Selecting it on `rs` adds a `heapless` dependency to the generated crate. |
| `format` | docs (`html`) | Documentation output format of the non-code `docs` target; `html` is currently the only one. |
| `no_std` | rust | With `corelib: rs-no-std`, emit the `#![no_std]` crate profile (default `true`). |
| `max_message_size` | c, cpp, rust, go, java, kotlin, csharp, zig, dart, python, typescript | Ceiling on a message's encoded size (default 4096). Fills in for a message the schema cannot bound (emitted as `MAX_SIZE_LIMIT`); when set explicitly it is also a budget a computed worst case may not exceed (§9.6). |
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
- **Use the closed public name set** (CORELIB_PLAN §6.1.1) — a generated type lands
  in the *user's* namespace, so the entry points are fixed and only the casing
  adapts: `encode()` / `decode(bytes)` / `try_decode(bytes)` for the one-shot
  convenience, `serialize(ostream)` / `deserialize(istream, …)` for the streaming
  pair the corelib drives, `encode_to(ostream)` for the streaming encode a caller
  drives, and `decoder()` → `feed(chunk)` / `finish()` for the streaming reader.
  No second spelling for either — no `serialize_to`, `to_bytes`, `from_bytes`,
  `decode_from`, `decode_into`, `marshal`/`unmarshal`. The `marshal`/`unmarshal`
  spelling is **gone family-wide** (generator#239 API pass), and so is
  TypeScript's `decodeFrom`/`decodeInto` pair (generator#384; with the cursor path
  itself withdrawn there is no intermediate step left to name — `decode(bytes)` runs
  the corelib's one-shot decode against the type's flat visitor, which is a module
  internal and not a member of the class). What is
  *still* not uniform is narrower and listed per backend in §10: `try_decode` is
  absent from C, Go, Python, TypeScript and Zig.
  - **`serialize` vs `encode_to`.** They are not two spellings of one thing.
    `serialize` writes *this object's fields and nothing else*, so a nested
    message can be written into a frame its parent already opened; it is the
    primitive the corelib and the parent both call. `encode_to` is the entry
    point for a caller who owns the stream: it serialises **and flushes the
    tail** the last write left in the buffer. Calling only `serialize` on a
    top-level message and forgetting the flush truncates the output, which is
    the whole reason the second name exists.
  - **Where `feed()` is absent, the corelib is why.** A generated `feed(chunk)`
    is a handle on a *resumable* corelib decoder; it cannot be synthesised over
    one that is not. Go and Python stream **pull**-shaped (the caller supplies a
    reader, the corelib pulls), so they expose that shape instead —
    `EncodeTo(io.Writer)` + `Decode<Msg>From(io.Reader)` /
    `deserialize(Decoder(reader))` — and gain `feed()` only once their corelibs
    carry a resumable decoder. **The absence of `feed()` is not an excuse for an
    unbounded decode**: §5.6 asks for the memory bound, not for a particular
    shape, and a reader-driven entry point delivers it. Go had the encode half
    only until generator#312 added `Decode<Msg>From`, which drives corelib-go's
    `Decoder.FeedFrom` — a wrapper over `Feed`, so the reader path and the
    one-shot path are the same state machine (corelib-go#130, generator#403).
    That corelib now has the resumable push decoder a generated `feed()` needed,
    so Go's missing `feed()` is a backend task rather than a blocked one.
    TypeScript's generated `<Msg>Decoder` drives corelib-ts's
    resumable `IStream` with the same flat visitor `decode(bytes)` uses, so the
    streaming and one-shot paths cannot disagree — there is one implementation of
    every rule, which is what §5.3.1 is for.
- **Decode by `switch` on field id**, not an if-chain — compilers build a jump
  table; unknown ids fall through to the corelib's skip path, giving
  forward/backward compatibility for free.
- **A schema bound is checked in exactly one place: wherever it is decided
  earliest.** For every bound the corelib reader accepts as an argument — an
  array `count`, an element's declared width, a string/blob `maxlen` — that place
  is *inside the reader*, at the count or length word, before the payload. The
  reason is a verdict, not a saving: §5.2 makes INVALID dominate INCOMPLETE, and
  a check the generated code runs after the read cannot fire for a value the read
  never finished assembling, so it loses the verdict on exactly the truncated
  input the bound exists to reject (generator#216, #267).
  A second copy of that check at the call site is therefore **not** defense in
  depth — it is strictly later and strictly weaker than the one that already
  decided, and it is not free: re-walking every decoded string and every decoded
  array was 10% of the TypeScript decode (generator#339). Keep a call-site check
  only where the corelib has no argument to take the bound — on a visitor surface
  that is every bound, which is why `arrayBegin` / `fixlenBegin` exist and why the
  verdict is taken there rather than in the value callback behind it. What guards against a
  corelib that stops honouring an argument is the conformance suite, which
  exercises each reject and its truncated twin — not a duplicate branch on the
  hot path.
- **Resolve everything at generation time.** Field ids, type mappings, enum
  backing widths, array element kinds/counts, `maxlen` — all known statically, so
  bake them in as constants/literals; nothing is computed at runtime.
- **No reflection / no runtime schema** — all dispatch is concrete generated
  code. (The sole exception is C, which *deliberately* uses a static descriptor
  table for footprint.)
- **Static helpers belong in the corelib; generated code stays thin.** A corelib
  knows no schema — only what a call hands it, and it has no compile-time
  reference to any message. That is precisely why a *static* helper belongs
  there: one whose code has the **same shape for every schema**, with its schema
  dependence carried entirely by arguments and type parameters. A `count`,
  `maxlen`, element width or capacity is then a runtime value like any other.
  `sofab::StringSeq` / `sofab::MessageSeq`, `sofab.arrays.*` and corelib-ts's
  `PayloadAcc` / `StringSeq` / `BlobSeq` / `decodeUtf8` / `elementsEqual` are the
  shape to copy — one collector in the corelib, the bounds passed per field; and
  `corelib-go/arrays.go` states the same test in its own file comment.
  The generator emits only what has a **different shape per schema** — the field
  arms, the id routing, the per-field guards, the declared types, the
  sized-array descriptors of §11 — and what **names a generated symbol** (Go's
  `_isDefaulter` requires the generated `isDefault()`, so it stays). Reaching a
  generated type through a *type parameter* is not naming it.
  *Thin* means lines of logic, not lines of comment: a helper's rationale moves
  with it and is then written once in the corelib instead of re-emitted into
  every user's source tree.
  Three things override this, each **measured, never asserted** (§15): a move
  that costs `.text`/`.data` on a footprint target (`c`, `rs-no-std`, `c-cpp`); a
  move that costs instructions/op on a maxspeed target (the corelib boundary can
  defeat inlining); and output-buffer **allocation**, which CORELIB_PLAN §5.1
  assigns to the generated layer — the corelib may offer the mechanism, never the
  policy (`growingOStream` and `Encoder.encodeToBytes` allocate inside their
  corelibs and are therefore off-limits to generated code).
  A helper that moves makes generated code require a corelib new enough to have
  it, and while SofaBuffers is `0.x` that needs no machinery: `API_VERSION` marks
  the *wire/API contract*, is 1 in every corelib, and is bumped only for a
  breaking contract change — so it cannot express "new enough to have
  `StringSeq`", and a floor built on it would never fire. An out-of-date corelib
  fails at compile time on the missing symbol, which is immediate and precise
  enough. Tracked as generator#345.
- **Pick the narrowest correct type** — map each integer to its exact width;
  enum → smallest *signed* backing, bitfield → smallest *unsigned* backing; avoid
  widening on the hot path (§11 natural-width writes).
- **Heap-free field storage is its own axis — offer it wherever the language can
  express it.** A schema bound (`maxlen`, `count`) is a *compile-time* size, so a
  target whose language can hold a bounded field inline — a fixed-capacity
  container, an inline array, a stack buffer — exposes that as an `allow_dynamic`
  switch instead of hard-wiring one representation. Four rules keep the axis
  honest, all of them already visible in the C++ and Rust backends:
  1. **It is orthogonal to the corelib/environment.** It selects storage, never
     wire format and never API: encode output is byte-identical in both modes and
     the corelib's typed reads take either destination. So the switch belongs on
     the **maxspeed** corelib as much as on the footprint one — a `corelib: rs`
     crate can hold its bounded fields in `heapless` while keeping serde, the heap
     decode stack and the ordinary std prelude (§10, Rust row).
  2. **The default is corelib-keyed, not global** — `false` where the profile is
     `footprint` (no heap to spare), `true` where it is `maxspeed` (allocate what
     the message carries, not its declared worst case).
  3. **It applies per field.** A bounded field goes inline, an unbounded one keeps
     its dynamic container, so enabling it never fails a build on a target that
     permits unbounded fields, and never demands a schema change (§9.3). Where the
     *profile* additionally makes bounds mandatory (`c-cpp`, `rs-no-std`) that is
     the profile's rule, not this switch's — it holds in both modes.
  4. **The container's capacity is not an enforcement path.** Exceeding a declared
     bound is a reject (`INVALID`, §7.1) in both modes; a fixed container must not
     silently truncate or drop where the dynamic one rejects.
  Where the language cannot express it the axis simply does not exist: Java, C#,
  Go, Dart, Python, TypeScript and Kotlin hold objects on the heap, and the
  reachable wins there are buffer *reuse* rather than inline storage (Java's
  `ThreadLocal byte[MAX_SIZE]`) and holding each value at its *declared* width
  rather than a widened one, which C# and Kotlin both do (§10). Where a target
  is statically bounded by construction it is not a switch but the only mode — C rejects unbounded fields
  outright and has no dynamic fallback to select (`generators/c/bounded.go`).
  Both of those are legitimate answers; what is not legitimate is leaving the
  question unstated, so `docs/generator/<lang>.md` says which case the target is.
  And the claim is **measured, not asserted**: a target carrying the switch
  carries a `tests/bench/rows.json` row for *each* side of it (§15) — `cpp` and
  `rust` each have four. Zig is the open case (generator#323): its `count: N`
  native arrays already lower to a stack `sofab.FixedArray(T, N)`, but bounded
  strings/blobs are still `[]const u8` views, so a schema whose every field is
  bounded still needs an allocator to decode.
- **Validate cheaply or not at all on the hot path** — bounds checks (`maxlen`,
  array `count`) are debug-only assertions, so release builds pay nothing.
  (There is no config knob for this today; a `validation` key existed in the
  schema but was never consumed and has been pruned.)
- **Escape reserved-word field names.** A schema field name may collide with a
  target-language keyword (`where`, `class`, `int`, …); the backend must make it a
  valid identifier — *escape* where the language allows (Rust `r#name`, C#
  `@name`, Kotlin `` `name` ``), *mangle* otherwise (C/C++/Java/Python trailing
  `_`), or be keyword-safe by construction (Go exports/capitalises; TS allows
  keyword member names). A few
  words can't be escaped at all (Rust `self`/`Self`/`crate`/`super`) and must be
  mangled. The **wire is unaffected** (keyed by id) and the **JSON name stays the
  original** — keep the raw name for JSON keys, and add a rename when the
  identifier was mangled (escapes like `r#`/`@` are serializer-transparent). The
  `keywords.yaml` corpus compiles a keyword-heavy schema in every backend to guard
  this (and any new backend). Per-backend helpers: `cIdent`/`cppIdent`/`csIdent`/
  `javaIdent`/`ktIdent`/`pyIdent`/`rustIdent`. Kotlin carries a second rule beside
  the escape, and it generalises: a name colliding with a **generated member**
  (`encode`, `reset`, …) is *mangled*, because an escape answers the grammar and
  not another declaration.
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
    field-level metadata above;
  - field **bounds** — an array's `count`, a string/blob's `maxlen`, and an array
    element's `maxlen` → a `Schema bound: …` line on the **field's own** doc. The
    bound is enforced in every target and used to be stated only in internal decode
    plumbing (the array collector, the visitor's cap parameter), which is not where
    a caller assigning to the field is looking; `count: N` then reads as "N
    elements" rather than as a capacity over a container that starts empty. The
    text is rendered by one shared helper (`internal/generator.BoundDoc`) and says
    the two things the type cannot: where the LENGTH starts, and that exceeding the
    bound is a rejection (`INVALID`) rather than a truncation. It is phrased per
    **storage**, which is a per-field property, not a per-target one:
    *dynamic* (the bound is nowhere in the type), *fixed* (a capacity-carrying
    container states it, the length still does not), and *companion* (C: the length
    is a second member, so the note names it — the one shape where forgetting it
    encodes a silently empty field). A field with no bound gets no note, so an
    unbounded schema's output is unchanged. The `docs` target renders the bound in
    its Type column (`u32[3]`, `string (maxlen 8)`) and puts the same statement in
    a legend under the field table instead.

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
  deprecation marker for the deprecated field; `TestBoundsReachTheFieldDoc`
  (`testdata/bounds.yaml`) does the same for the bound note, and additionally
  pins that an unbounded field receives none; each backend's own unit test covers
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
flush sink when full (so a message can exceed RAM). That property is only worth
anything if generated code *exposes* it, which is what `encode_to(ostream)` is
for — `encode()` is the same path with a buffer sized to hold the whole message
(`MAX_SIZE`, or the configured `max_message_size` ceiling when a field is
unbounded), so it is the convenience, not the capability. The generated
`serialize` writes each field in schema/id order via these operations (names per
corelib; canonical set):

`write_unsigned(id, v)` · `write_signed(id, v)` · `write_boolean(id, v)` ·
`write_fp32(id, v)` · `write_fp64(id, v)` · `write_string(id, s)` ·
`write_blob(id, ptr, len)` · `write_array_unsigned/signed(id, elems)` ·
`write_array_fp32/fp64(id, elems)` · `write_sequence_begin_lazy(id)` ·
`write_sequence_end()` · `write_sequence_end_keep()`.

**Sequence framing is lazy** (CORELIB_PLAN §6): `begin_lazy` holds the header back
until a field write proves the sequence non-default, `end` drops a frame that never
got content, `end_keep` forces it out for an array **element**. See *Lazy sequence
framing* in §11 for which closer goes where and why. The **C** backend does not use
this path at all — its descriptor decides omission per field before opening, so it
keeps the eager `sofab_ostream_write_sequence_begin`/`_end` pair.

Integers are written at their **natural width** (the varint output is
value-based, so the bytes are identical to a widened write; this lets
width-reduced corelib builds compile — §11).

### 9.3 Decode models

Decoding has **five families**; a backend picks the one its corelib exposes. All
route by `(scope, id)` and are forward-compatible (skip unknown ids).

1. **Flat visitor + location-stack** (Rust, C#, Java, Kotlin, Python, TypeScript, and
   the C++ `c-cpp` wrapper). The corelib drives a `Visitor` with flat callbacks; the generated
   visitor is a `(location, id)` state machine with a stack pushed/popped on
   sequence begin/end. Callbacks: `unsigned(id,v)`, `signed(id,v)`,
   `fp32/fp64(id,v)`, `string(id, total, offset, chunk)` and `blob(...)`
   (delivered in chunks; `total` is the full length), `array_begin(id, kind,
   count)` then element callbacks, `sequence_begin(id)`, `sequence_end()`. This
   is the **reusable template for any new flat-visitor corelib**.

   **Python is the same shape over a coarser corelib surface.** corelib-py's
   visitor delivers each array WHOLE (`on_unsigned_array(id, values)`) rather
   than element by element, so the generated visitor needs no `ai`/`askip`/
   `afill` element bookkeeping — it is the location stack and nothing else. It
   also needs no `_DEAD` location: `on_sequence_begin` returning `False` makes
   corelib-py skip the entire subtree, delivering nothing and firing no
   `on_sequence_end`, so the stack is only ever pushed for a scope the visitor
   entered. Every bound whose verdict must precede the payload moves to a header
   hook, and since generator#406 they are split by who owns the rule: the
   **schema's** `count:`/`maxlen:` is *declared* to the corelib in
   `on_schema_bound(field_id, n) -> int` and applied there (§9.5 below);
   `on_field` keeps the §7.3 tag test in front of it and the wrapper element
   index; `on_array_begin` keeps the declared element width, which the decoder
   applies per element.

   **Optional bulk element hand-off (Java and Kotlin, `Visitor.arrayBulk`/`arrayBulkEnd`).**
   A flat visitor pays its per-callback routing *per element*, which for a short
   integer array is the whole cost of decoding it. corelib-java therefore offers
   the destination once per array instead: right after `arrayBegin`, `arrayBulk(id,
   kind, count)` may return a `byte[]`, `short[]`, `int[]` or `long[]` of at least
   `count`, the decoder writes the elements straight into it (ZigZag already
   applied for a signed array) with no element callback at all, and
   `arrayBulkEnd(id, n)` closes the fill. Returning `null` — the default — decodes
   exactly as before, which is what makes it additive: generated code emits both
   the offer and the per-element arms, without `@Override`, so it compiles and runs
   against a corelib that predates the methods. Both write paths honour it, so a
   chunk boundary anywhere inside the array is invisible. **`count` is untrusted**
   (the wire's claim, bounded only by the format ceiling), so the offer is made
   only for arrays the schema already bounds with a `count: N`; an unbounded one
   keeps the capped-reservation, grow-as-you-go fill (#96).

   **The destination's width is a bound**, which is why the return type is
   `Object` rather than four overloads: handing back a narrower array than
   `long[]` says the elements are declared that wide, so a value that does not fit
   fails the decode with `INVALID_MSG` — zero-extended for an `UNSIGNED` array,
   sign-extended for a `SIGNED` one — and the decoder never truncates silently.
   That is what lets the §7.1 width check and the narrowing happen in the pass
   that decodes instead of a second one afterwards; four overloads would cost four
   virtual calls per array to find the one that is not null, which is more than
   the pass they save. It is also the one place a corelib judges a value against
   something the *consumer* supplied rather than against the format — a deliberate
   exception, paid for by the pass it removes, and the generated code keeps its
   own per-element guard for the path where the offer is declined. The same
   reasoning ports to any flat-visitor corelib; corelib-kotlin-mp is the second to
   take it, and there the hand-off costs nothing at all to arrange: Kotlin's
   unsigned arrays are inline classes over their signed peers, so the `asByteArray()`
   view a `UByteArray` field offers IS that field's backing array and there is no
   second pass to copy anything back. The remaining corelibs have not taken it.

   String/blob
   callbacks take a **single-shot fast path** — when the whole payload arrives in
   one chunk (`offset == 0 && chunk_len >= total`) they build the value straight
   from the contiguous slice, keeping the byte accumulator only for split
   payloads. Reassembly carries no schema knowledge — `(total, offset, chunk)` in,
   the field's bytes out — so by §8 it belongs to the corelib rather than to every
   generated crate; both Rust ports have taken it (`sofab::PayloadAcc`, and
   `PayloadAcc<N>` on no_std, whose storage stays in the caller and whose overflow
   is `Error::BufferFull`), and the generator emits the call, not the buffer. **That accumulator is scratch, and what reaches a destination must
   be storage of its own.** It is one buffer per visitor, reused by the next
   split payload, while destinations keep what they are handed — wrapper-array
   elements outlive the callback that stored them. On a target where the
   destination holds a *view* rather than a copy (Zig `[]const u8`, a Rust
   `&[u8]`), handing out the accumulator therefore aliases every payload
   assembled earlier onto the newest, and growth reallocates the buffer out from
   under them, leaving a length that reads past the live bytes and — under an
   allocator that releases — a freed read. Copy out at completion; the borrow of
   a *whole-in-one-chunk* payload is unaffected, since those are disjoint regions
   of the caller's own buffer. Only Zig was ever exposed here (generator#293,
   Crucible F-0058 / codegen defect G-0036); a target whose string type owns its
   bytes copies at the store anyway.

   **On a resumable path, borrowing is not available at all for such a target.**
   The accumulator is only half the problem: a corelib that stitches an item
   straddling a chunk boundary into an internal carry buffer and parses out of it
   delivers that payload as a slice into *itself*, and the callback signature
   gives generated code no way to tell it from a slice into the caller's chunk.
   The next stitched item then overwrites it. So a view-storing target must copy
   **every** payload on the streaming path, not just the split ones, and can keep
   the borrow only on the contiguous path, where the corelib is handed one whole
   buffer and never stitches (generator#295). Both corelib-rs and corelib-zig
   carry this way; only Zig is exposed, because only its generated storage is a
   view. The cost is one copy per streamed payload, in exchange for a destination
   whose lifetime is independent of both the carry buffer and the fed chunk.

   **That independence is now the spec's rule, not just this design's
   (documentation#37 → documentation#38).** CORELIB_PLAN §6 states it normatively:
   a fed chunk is borrowed **only for the duration of the `feed` call** — once
   `feed` returns the caller may reuse, overwrite or free that memory, and the
   decoded message MUST NOT be affected. The one-shot `decode(buffer)` is exempt
   in the obvious way, since the caller supplies the whole buffer and keeps it
   alive across the call, which is exactly the split `own` already draws. §7.2
   item 4 adds the oracle that makes it checkable: scrub every chunk after `feed`
   returns and assert the decoded message is unchanged. Run against generated Zig
   at family tip — the only target that stores views — a `nested.str = "hello"`
   message fed out of a buffer the harness overwrites with `0xa5` immediately
   after each `feed` decodes to `"hello"` at chunk sizes 1, 2, 3, 5 and 16. **A
   new view-storing backend inherits this obligation**: borrow on the contiguous
   path, copy on the resumable one.

   Fixed-count native arrays decode into a fixed/primitive member
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
   unbounded fields (no `maxlen`/`count`) are rejected outright — in both storage
   modes, since one schema has to stay valid for every `c-cpp` target. What
   `allow_dynamic` changes is where a *bounded* field lives: `std::string` /
   `std::vector` sized to what the message carries, with the `maxlen`/`count`
   enforced as an explicit reject instead of as the container's capacity. Encode
   output is byte-identical between the two modes. **The same switch is available
   on `corelib: cpp`**, where it defaults the other way (`true`) and applies per
   field: bounded fields go inline, unbounded ones keep their dynamic container,
   so heap-free storage is reachable at maxspeed without the embedded profile's
   demand that every field be bounded. corelib-cpp's typed reads take either
   destination, so the generated decode is identical in both of its modes. **Rust `corelib: rs-no-std`
   (`no_std`, on by default) is the direct analog** (`docs/generator/rust.md`):
   bounded strings/blobs/sequence arrays lower to `heapless::String<N>` /
   `heapless::Vec<T,N>` (the `heapless` crate; the corelib stays storage-agnostic),
   `encode` fills a fixed `heapless::Vec<u8, MAX_SIZE>`, the location stack is a
   bounded `heapless::Vec` sized from the schema (sound only because *skipped*
   scopes are depth-counted rather than stacked — see "A skip must contain the
   whole subtree" below), `serde` is gated behind a cargo feature, and the crate
   root carries `#![no_std]` — same wire bytes, same `allow_dynamic` rule for
   unbounded fields (an `alloc` fallback). A binary can't be `no_std` on a hosted
   target, so the firmware artifact is the lib (`cargo build --lib
   --no-default-features`); the JSON harness bin is a separate `std` target.
2. **Push child-visitor** (Go, Dart). The generated object drives the corelib's
   visitor; `Decode<Msg>` runs a zero-copy cursor over the in-memory buffer that
   calls a typed method per field
   (`Unsigned/Signed/Float32/Float64/String/Bytes`, `*Array` for native arrays
   delivered widened to 64-bit). Nested scopes descend via `BeginSequence(id)` /
   `onSequenceStart(id)`, which returns the child visitor: the nested object
   itself, or a small collector for a wrapper-sequence array
   (string/blob/struct/union/matrix elements). A no-op base supplies defaults so
   each type overrides only the callbacks it uses. **Go** — the generated struct
   *is* the `sofab.Visitor` (the corelib's no-op `sofab.VisitorBase`, embedded in
   every object), driven by **either** entry point: `sofab.AcceptBytes(buf, m)`
   for bytes the caller holds, or `sofab.NewDecoder(m).FeedFrom(r, scratch)` for
   an `io.Reader`, which feeds the decoder chunk by chunk so peak memory is the
   scratch buffer plus the largest single field rather than the wire image (§5.6,
   generator#312). Both are `Feed` on one resumable state machine, so they are
   **event-equivalent** by construction rather than by agreement — same
   callbacks, same verdicts at every byte boundary — which is why one emitted
   visitor serves both and neither can tell which is driving it.

   Since corelib-go#130 the codec **builds no aggregate** (§6.6.3), and that
   shapes what the generated visitor looks like. A `string`/`blob` arrives as
   `String(id, total, offset, chunk)` — one call per fed piece, the chunk a
   window into the caller's bytes — and is assembled by a `sofab.PayloadAcc` the
   object carries; one accumulator per object suffices, because a fixlen payload
   is contiguous on the wire and two of an object's fields can never be in flight
   at once. The object also embeds `sofab.StringCheck`, so the decode's own
   `WithStrictUTF8` policy reaches the destination's UTF-8 check (§6.4) instead of
   only the build tag. A native array arrives as `ArrayBegin(id, kind, count)`,
   one `Array*(id, index, v)` per element, `ArrayEnd(id)`: the destination is
   opened at the header (which is also what makes a repeated id REPLACE the array,
   §7.4) and each element is width-checked as it lands. Copying is still the rule
   — a decoded message owns its bytes and outlives the buffer it was decoded from
   (§9.6) — but it is now a copy out of the assembled payload rather than out of a
   value the codec built. **Dart** — a separate `_<Msg>Visitor` class
   holds a reference to the object and a shared decode context;
   `sofab.Decoder.decode(data, visitor)`. Because Dart's corelib delivers a native
   array whole through a distinct `on*Array` callback (like Go, unlike the
   flat-visitor family that streams array elements through the scalar callbacks),
   an integer/fp array at a scalar id lands in a callback the id switch does not
   handle and skips **structurally** — no `askip` guard (§7.3). And because the
   Dart callbacks return `void`, the over-count/over-index/over-maxlen INVALID
   verdicts ride a **sticky `_inv` flag** the generated `tryDecode` converts to a
   terminal INVALID after the corelib returns (the Rust/Zig sticky-flag model);
   the receiver-side `max_dyn_*` limits are compared inside the corelib — the
   wrapper collectors take theirs per field, beside the schema bound they are
   exclusive with — and every number is supplied by generated code, corelib-dart
   holding none (§9.5).
3. **Pull-parser** — *empty*. The generated `decode` used to loop
   `Decoder.Next()` → a field `{id, wire-type}`, switch on `id`, read the typed
   value and `Skip()` unknowns.

   **Python, TypeScript and Go all left this family in 2026-08**, when corelib-py,
   corelib-ts and corelib-go removed their pull APIs (CORELIB_PLAN §5.3.1, "the
   visitor is the only decode surface", now forbids a second one). All three moved
   to family 1 above. Go was the last, and its `Decoder.Accept` / `AcceptStream` /
   `NewDecoder(io.Reader)` went with it (corelib-go#130): the reader-driven entry
   point is a wrapper over the same `Feed`, not a surface of its own.
4. **Child-visitor** (pure C++ `corelib-cpp`). Nested objects decode via
   `sofab::read(is, child)` (a child `IStreamMessage`); scalars via
   `sofab::read(is, member)`.

   **The call goes through the `sofab::` helper layer, not through the stream's
   own members** (generator#404 / corelib-cpp#125). CORELIB_PLAN §6.6 forbids the
   codec growing a caller's destination from a wire number — "it moved the
   allocator call one type away, where a source-level audit no longer sees it" —
   so `IStreamImpl::read` / `readString` / `readBlob` / `readArray` now fill the
   room the destination already holds and refuse one too short with
   `InvalidArgument` (§6.3's third tier). The sizing moved into free functions
   beside them, `sofab::read(is, dst)` and friends, which size a growable
   destination for the announced value — clamped to what the bytes in hand could
   fill, and only once the tag (§7.3), the schema bound (§7.1) and the receiver
   cap (§6.2.1) admit the field, so neither a mistyped occurrence nor a hostile
   count reaches an allocator. **Generated code calls the free functions**; for a
   heap-free destination the sizing compiles away, so `allow_dynamic: false` pays
   nothing for going through them. The two-argument `is.read(dst, maxlen)`
   raw-blob copy is not part of this: it has no growable destination and keeps its
   member form.

   **The decoder is also resumable, which decides what a decode destination may
   be.** A field split across `feed` chunks is delivered once per chunk that
   carries part of it, with the same id, and each delivery continues where the
   last stopped — into the destination it was handed. So a generated arm must not
   reset its destination inside `deserialize` (that would throw away what earlier
   chunks delivered; `is.progress() == 0` is the guard if one is ever needed), and
   must not read into a **temporary** that dies with the call: a fresh temporary
   per delivery keeps only the last chunk's elements. That is what retired the two
   arms which used to convert through one — the same two the `c-cpp` leg had
   already had to change for its deferred decoder, so both C++ legs now emit the
   same shape:

   - a **boolean array**'s member element type is the wire's `std::uint8_t` on
     every `cpp` profile, so the member is a native destination like any other.
     `std::vector<bool>` could never have been one: it is the bit-packed
     specialisation and has no `data()`;
   - an **enum array**'s member keeps its scoped enum element (the generated API
     and the JSON harness stay value-typed) and binds through
     `sofabgen::RawArray`, which reinterprets the **elements** — never the
     container — and forwards `resize()`/`size()`, so `readArray` keeps ownership
     of the tag check, the bound check and the reset, in that order.
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
**TypeScript's move into family 1 (2026-08).** corelib-ts withdrew its `Cursor`
pull decoder, its child-visitor shape and its opt-in `Visitor.longs` channel in one
refactor, leaving a single flat `Visitor` driven by `IStream`/`decode`. The generated
backend followed exactly the shape family 1 describes, with four TypeScript-specific
points worth recording:

- **The scope stack is static.** The scopes of one generated type form a *tree* — a
  location is assigned per (type, path), so a scope is only ever entered from one
  parent — and corelib-ts fires `sequenceEnd` only for a scope the visitor accepted
  (a declined subtree reports nothing at all). `sequenceEnd` therefore restores the
  parent from a `switch (this._c)`, and the visitor carries no stack: one array less
  per decode, and no push/pop per nested scope. The same reasoning applies to every
  family-1 backend; TypeScript is the first to take it.
- **64-bit values come from `lo`/`hi`, without a flag.** Every integer hook carries
  the exact wire halves the varint reader already holds, so `int64: long` and
  `int64: number` build their destination with `Long.fromBits(lo, hi)` and never
  materialise a `bigint` on the decode path. This replaces the withdrawn `longs`
  channel, which was read once from the ROOT visitor and so covered every integer
  field alike — making 64-bit fields free and narrow ones dearer, a trade the
  backend had to guess from the schema (#344). There is no trade left to guess.
- **A narrow destination reads the hook value as a `number`, not through
  `Number(v)`.** The assertion is made true by the declared-width guard on the next
  line: corelib-ts hands over a `bigint` in exactly one case, a magnitude above
  2^53-1, and every narrow width tops out at 2^32-1, so such a value throws before
  it can be stored. Where no guard follows (a `bitfield`, an `enum`) the conversion
  stays.
- **A native array is filled through a destination, not element by element.** The
  element callbacks were the single largest item in what the move cost: measured
  against the withdrawn cursor, a `u32` element read in bulk cost 177 Ir and
  1067 Ir through `arrayUnsigned`. corelib-ts's `Visitor.arrayBulk`
  (corelib-ts#163) is the `arrayBulk` hand-off this section already describes for
  Java and Kotlin, ported: generated code points one reused `ArrayTarget` at the
  destination `arrayBegin` just built and states the element's declared width, and
  the decoder fills it with no element callback at all — 425 434 → 280 215 Ir/op
  on a 200-element array, −34.1 %.
  **Gated twice, and both gates are load-bearing.** The offer is itself a call
  (~1300 Ir per array, against ~435–730 Ir saved per element), so the corelib
  refuses below `BULK_MIN = 16` elements, and the generator does not emit an arm at
  all for an array whose declared `count` — a capacity, hence an upper bound — can
  never reach it. Without the second gate a message of four-element arrays paid
  +7572 Ir to be told no; with both it pays +1850 (+0.24 %) for the branch, and
  emits no hook when nothing in the schema qualifies. An arm is offered only where
  a **declared narrow width** exists, which is exactly where the elements are plain
  JS numbers *and* there are bounds to state: `u64`/`i64` (bigint/Long), `boolean`,
  `enum` and `bitfield` (no declared width — inventing one would reject legal
  values) and the floats all keep the element callbacks. Those arms stay emitted
  regardless: they are what runs for a declined array and against a corelib that
  predates the hook, which is what makes taking it additive.
- **Only the leaf wrapper-array collectors come from the corelib.** `StringSeq` and
  `BlobSeq` own the index rules, both bounds, the payload join and the strict UTF-8
  decode for a `string`/`blob` element (ARCHITECTURE §8). A *framed* element — a
  struct, a union, a nested row — is placed by generated code instead, because
  `ElementSeq` fills a gap with one shared `def` value: right for the immutable `""`
  and empty-bytes defaults its two subclasses use, and aliasing for an element whose
  default is a fresh object. The `_ObjSeq` / `_MatSeq` / `_RowSeq` classes the
  child-visitor shape needed are gone: a flat visitor routes the element scope
  itself.

**What the move cost, decomposed** (Callgrind, subtract method, §15,
`vehicle_telemetry`). The headline is +13.8 % on decode, 684 761 → 779 440 Ir/op,
and it is worth knowing where that sits because the obvious reading — "the corelib
got slower" — is wrong:

| | old (cursor) | new (visitor) |
|---|---:|---:|
| framing only (skip the whole message) | 491 252 | 491 411 (+0.03 %) |
| full decode | 684 761 | 779 440 |
| ⇒ value delivery (full − framing) | 193 509 | 288 029 (+48.8 %) |

The parse is **identical**; the whole regression is in handing values to the
generated class. Removing the schema's eight array fields shrinks it from +94 679
to +36 894 Ir, so **61 % of it is native array elements** — 54 of them, at ~1070 Ir
each, which a 200-element micro-schema confirms independently at 1067. The
remaining 39 % is per-field visitor dispatch. On a pure-scalar schema the new path
is 24 % *faster* than the cursor was. That is what the bulk hand-off above
addresses, and why it is the array path it addresses rather than the dispatch.

**Decode outcome (MESSAGE_SPEC §7).** Every corelib reports the finish-less
three-valued outcome — COMPLETE / INCOMPLETE / INVALID — and the generated
one-shot decode must not hide it. For corelibs that surface INCOMPLETE as an
error/exception (Go, Rust, C++, C, Python, TS) the fallible decode entry
point (`try_decode`, Go's `(msg, error)`, thrown exceptions) already propagates
all three. The **status-returning** corelibs (C#, Java, Kotlin, Zig, Dart) treat
INCOMPLETE as a non-error status (C#/Java/Kotlin: `DecodeStatus` from
`Feed`/`status()`/`status`; Zig: `Status` from `feed(chunk)`; Dart: `DecodeStatus`
from `Decoder.decode`/`feed`) and leave the end-of-input verdict to the caller, so
their backends must surface it explicitly:

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
- Dart, like Zig, has no back-compat surface, but its corelib is exception-free
  for the decode path (it *returns* `DecodeStatus`), so the generated
  `static DecodeStatus tryDecode(Uint8List data, T out)` returns the corelib's
  terminal status directly — `invalid` when the sticky `_inv` flag (an
  over-count/index/maxlen violation) is set, else the corelib's verdict. The
  best-effort `static T decode(Uint8List)` discards it (the 90 % case). The
  project harness exposes `tryDecode` as a `trydecode` mode pinning the same two
  vectors; its `decode` mode sets a non-zero process exit on any non-`complete`
  status (Dart ignores an `int` returned from `main`, so the harness calls
  `exit()`).
- Kotlin has no back-compat surface either, and its corelib *does* throw for
  INVALID, so the split runs the other way from Dart's: `tryDecode(data, out)`
  returns COMPLETE/INCOMPLETE and malformed input raises (the C#/Java shape),
  while the one-shot `decode(bytes)` is **strict about both** non-COMPLETE
  outcomes — it raises `IllegalStateException` on a terminal INCOMPLETE, a
  deliberately different exception type from the corelib's `SofabException`,
  because an incomplete message is not a malformed one. That is the zig ruling
  reached through an exception language: a one-shot whole-buffer decode IS at
  end-of-input, so handing back a half-filled object would hide exactly the
  verdict §7 says generated code must not hide. `Decoder.finish()` draws the same
  line for the streaming path. The conformance harness pins all three channels —
  the two `trydecode` statuses, and a truncated `decode` that must not succeed.

#### Decode verdict: over-count scalar arrays are INVALID (all families)

MESSAGE_SPEC §3 makes a scalar-array field's schema `count` its **capacity `N`**
(the wire carries `0..N` elements and that wire count IS the length — §11
*`count` is a capacity*), and §7 classifies "a
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
  (`arrayBegin` throws `SofabException(INVALID_MSG)` wrapped unchecked), Kotlin
  (the same guard, thrown *un*wrapped — Kotlin has no checked exceptions, so the
  corelib's own `SofabException` leaves a Visitor callback directly and the
  category reaching the caller is the one the format defines), C#
  (`ArrayBegin` throws `SofabException(InvalidMessage)` — the guard also bounds
  the eager `new T[count]` allocation), Python (`raise SofaDecodeError` from
  `on_array_begin`, at the count word), TypeScript (`throw SofabError(InvalidMsg)`
  from `arrayBegin`, at the count word).

The infallible best-effort entry points kept for back-compat (Rust/C++
`decode`) still discard the verdict; the fallible path is authoritative, and
the conformance harnesses assert the reject through it (§12).

**Ordering vs truncation (§5.2, generator#216 / F-0032).** A message that is
*both* over-count *and* truncated must report **INVALID**, not INCOMPLETE —
§5.2 makes INVALID "malformed regardless of what follows," so it dominates a
truncated tail. This holds only when the over-count is decided at the **count
header** (`count > N` at `array_begin`), before the cut-off elements: a guard
that compares only after consuming the elements (or at the element store) never
fires when truncation cuts the array short, so it misreports INCOMPLETE. The
Rust backend now checks at the header (the `array_begin` `if count > N { inv }`
arm) — the same "decide at the deciding word" rule as the over-`maxlen` guard
(checked at the fixlen length word) and the try_decode ordering that reads the
sticky `inv` flag before propagating `feed`'s `Incomplete`.

Whether the fix is generator-only splits by the corelib's decode model:
- **Header-first corelibs** (Rust, Zig `arrayBegin(id, kind, count)`; Python
  `fld.count` on the delivered field) surface the count *before* the elements, so
  the generator alone moves the guard to the header. **Done: Rust, Python, Zig**
  (generator#216).
- **Whole-unit / measure-then-deliver corelibs** surface the truncation
  `Incomplete` *before* the generated whole-value guard ever runs, so the guard
  alone cannot decide at the header. Each needed a small additive **corelib header
  hook** — a count/length callback fired at the deciding word, before the
  element/payload read — which the generator then implements/passes a bound into.
  Their `try_decode`/`tryDecode` ordering already lets a sticky INVALID (or a
  header-thrown INVALID) dominate the later `Incomplete`, so only the header wiring
  was missing. **Done via that hook:**
  - **Go** — corelib `sofab.HeaderVisitor` (optional interface: `ArrayBegin(id,
    kind, count)` at the array header, `FixlenHeader(id, subtype, length)` at the
    length word, both before the truncation check; for a fixlen array `ArrayBegin`
    fires *after* the `fixlen_word`, so `kind` names the element subtype — see
    §4.8 below). The generator emits these methods on a
    type whenever a field declares a `count`/`maxlen` bound (`if count > N { return
    sofab.ErrInvalidMsg }` / `if subtype == S && length > N …`), so a bound-free type
    does not implement the interface and the max-speed decode path is unchanged.
    Both methods are emitted together — the interface is all-or-nothing (see §7.3
    below) — and the `maxlen` guard is gated on the declared fixlen subtype.
  - **Dart** — corelib `MessageVisitor.onArrayBegin(id, kind, count)` /
    `onFixlenHeader(id, subtype, length)` overrides set the sticky `e.inv`, which
    `tryDecode` reads before returning the incomplete status. Emitted only for
    bounded fields, and subtype-gated as above.
  - **TypeScript** — the verdict is taken in the header hooks the flat visitor
    receives before any payload: `arrayBegin(id, kind, count)` for a bounded
    native array, `fixlenBegin(id, subtype, total)` for a bounded scalar
    string/blob, and `StringSeq`/`BlobSeq.begin` for a bounded wrapper element —
    each throwing `InvalidMsg` at the count/length word, ahead of the truncated
    field's `Incomplete` (precedence INVALID > LIMIT_EXCEEDED > INCOMPLETE). The
    payload callbacks re-take the same verdict against `total` rather than against
    the assembled value, so an over-bound field is never buffered. The generated
    harness has a `status` mode surfacing the `SofabError.code` so conformance can
    assert the INVALID-vs-INCOMPLETE distinction a bare non-zero exit hides.
  - **C++ `corelib-cpp`** — the pure profile *measures* a whole top-level field
    for completeness (`measureField`) before delivering it to `deserialize` where
    the `is.invalidate()` guards live, so those guards run too late once the field
    is truncated. corelib-cpp#50 added a **measure-phase schema** (`sofab::schema`:
    a static `SeqNode`/`FieldBound` tree consulted at the count varint / fixlen
    length word / element header, before truncation is surfaced) installed with
    `setSchema`. The generator emits that tree per message — one `FieldBound` per
    over-`count` native array (matched by array wire type), over-`maxlen`
    string/blob (`Fixlen` **plus the declared `subtype`**, see §7.3 below), and
    over-index wrapper array (`SequenceStart`, `wrapperArray`), plus a `child`
    descriptor for each nested struct/union field —
    and calls `in.setSchema(&…_schema)` in `decode`/`try_decode`. Emitted only when
    a message carries a bound anywhere in its tree, so a bound-free message keeps
    the corelib's schema-free (maxspeed) measure walk; element-level bounds inside a
    wrapper array are not expressible (element ids are wire indices, not fixed field
    ids), so a wrapper array carries only its over-index bound. Pure profile only:
    the c-cpp wrapper is statically schema-bounded and has no measure phase. The
    harness `status` mode surfaces the verdict, as for TypeScript.

  With C++ landed, **generator#216 is closed for every backend.**

**The same rule, in the positions #216 did not reach (generator#267).** #216 moved
the guard to the deciding word for a message's own **counted native array**. Three
neighbouring positions kept the late shape, and each is the identical defect —
a bound established by a word, checked after the bytes that word describes:

- **Wrapper-array elements.** The element collectors (`_strSeq`/`_bytesSeq` in go,
  since moved to corelib-go as `sofab.StringSeq`/`sofab.BlobSeq`; `_StrSeq`/
  `_BlobSeq` in dart) carried the over-index *and* element-`maxlen`
  checks in the payload callback, not in the header hook their enclosing message
  already implemented. Both corelibs offered the hook; only the scalar fields used
  it. Fixed by emitting `FixlenHeader`/`onFixlenHeader` on the collectors too
  (generator#277). Go needs `ArrayBegin` **alongside** `FixlenHeader` here for the
  all-or-nothing reason recorded under §7.3 below.
- **A scalar `blob`'s `maxlen` in python.** The string arm already peeked
  `d.fixlen_len()` before the read; the blob arm did not (generator#277).
- **A wrapper-array `blob` ELEMENT's `maxlen` in python** — the same arm one
  position deeper, and the site the bullet above did not carry over. It stayed
  late for another year of Crucible runs before F-0062 hit it with
  `ce 0c 22 e3 30`: element index 4 declares 780 bytes against a `maxlen` of 64
  and the message ends at the `fixlen_word`, so `d.bytes()` reached end-of-input
  and answered INCOMPLETE where the fourteen other drivers — the `string_array`
  element **one field earlier in the same generated file** included — answered
  INVALID. It was the one site of five in that `message.py` not using
  `fixlen_len()`. The late guard also left the element unbounded in the interval
  it mattered: the emitted `d.schema_bounded()` turns the receiver-side
  `max_blob_len` cap off (§6.2.1) on the promise that generated code enforces the
  schema bound instead, and until #377 nothing did so before the payload wait.
  Fixed by emitting the string arm's peek verbatim (generator#377). This entry is
  history: `fixlen_len()` and `schema_bounded()` are gone with corelib-py's pull
  API, and both sites are `on_field` arms now, so the two can no longer drift.
- **An array element's declared width in typescript.** A `u8[]`/`i16[]` element
  outside its type's range was found by a scan over the *assembled* array, which
  cannot fire for an array that never assembles. The bound now sits on the element
  callback, so it fires as each element arrives and a truncation behind an
  out-of-range element cannot downgrade the verdict. (It briefly lived inside the
  cursor readers, `readUnsignedArray(count, max)` / `readSignedArray(count, min,
  max)`, corelib-ts#90; that surface is withdrawn.) A **dynamic** array carries the
  width bound too and no count bound: width is a property of the element *type*,
  not of the array *length*.

TypeScript additionally had no counterpart to `arrayBegin` for a **scalar**
string/blob, so its visitor's `maxlen` check had nowhere earlier to live than the
payload callback. corelib-ts#89 added `Visitor.fixlenBegin(id, subtype, total)` and
the generator emits into it, **testing the announced subtype** — one callback
serves both kinds, so ignoring the subtype would measure a blob field's `maxlen`
against a string arriving at that id, a §7.3 skip rather than a bound
(generator#303, closing #300). With the cursor path withdrawn this is no longer one
of two surfaces to keep in step; it is the only place the verdict is taken.

**The remaining five were a corelib gap first, and that is worth keeping on the
record because it decided the sequencing.** `rust`, `rust-no-std`, `java`, `csharp`
and `zig` all announce an array at its count word (`array_begin`/`arrayBegin`) and
all five backends emitted against it, so the **count** position was already
latched. The **fixlen** position could not be, from the generator: none of those
five corelibs had a fixlen-header hook, and their only callback carrying `total`
sat inside the payload loop. Measured against corelib-rs with a recording visitor
— a `write_str` message truncated to end exactly at its length word (`1a 52`, tag
+ length word, nothing more) produced **no visitor event at all**, whole or fed
one byte at a time, while the untruncated message reported `(id 3, total 10,
offset 0, len 10)`. `deliver_payload` guarded the callback with `if avail > 0`;
java/csharp/zig/rs-no-std had the identical shape. That put them in the
**whole-unit corelib** category above, needing the same small additive hook
go/dart/py/ts already had.

**All five hooks landed** (corelib-rs#47, corelib-rs-no-std#68, corelib-java#62,
corelib-cs#53, corelib-zig#37), with one signature across the family:
`fixlen_begin(id, subtype, total)` — fired after the length word is read and
validated, before any payload byte, `total == 0` included. The generator emits
into all five, latching **both** bounds a fixlen header decides: a scalar's
`maxlen`, and for a wrapper element its array's `id >= count` **before** the
element `maxlen` — an element that is not this array's element at all must not
have its length measured against the element bound.

Two per-target details are load-bearing rather than incidental:

- **Zig's hook is the only fallible one in the family.** It returns
  `sofab.Error!void`, so the reject is `return error.InvalidMessage` rather than
  a sticky `inv` flag. Zig also rejects an unused function parameter, and not
  every schema reads both: an array with a `count` but no element `maxlen` reads
  `id` and never `total`, so each parameter is named only when some arm below
  actually reads it — the rule `emitPayloadVisit` already followed for its own
  `total`.
- **Java and C# surface INVALID as a throw, not as a returned status.** Their
  generated `tryDecode`/`TryDecode` return COMPLETE/INCOMPLETE and malformed input
  raises; the conformance legs therefore assert the two verdicts on *different
  channels* — the over-bound message must throw, the in-bound control must return
  INCOMPLETE. Reading that as a single status word is what a first attempt at
  those legs got wrong.

The payload-side guards stay in every backend. They are unreachable for a message
that reaches the header hook, and they are the only thing still bounding a
consumer built against a corelib predating it.

**One position deeper: an array ELEMENT's declared width, in the four pull
corelibs.** With the fixlen positions closed, one row of Crucible F-0043 stayed
red — `width_elem_trunc.bin` (`a6 06 0c 05 b0 51`): `arrays.i8` declares five
elements, the first carries **5208**, and the message ends there. The element is
*fully on the wire*; only later elements are missing. `rust`, `java`, `csharp`,
`zig`, `c` and `cpp` reported INVALID, and `go`, `dart`, both python engines and
`typescript` reported INCOMPLETE.

The camps split exactly along the **decode model**, and this is the third time
that line has decided a #267 position. The push/visitor corelibs deliver an
element at a time, so generated code sees the offending value before anything
else happens. The four **pull** corelibs materialise the whole array and hand it
over in one callback, so the emitted `for _, x := range v` / `for (final _v in
values)` / `any(... for _v in ...)` scan is *exact for an array that arrives* and
**never runs for one that does not** — and §5.2 requires the verdict to be
INVALID precisely for the array that does not arrive. As with the fixlen header,
the decoder is the only party that sees the element in time and the schema the
only party that knows the bound, so the bound has to travel into the decoder:

- **typescript** already did (`readUnsignedArray(count, max)`, generator#304 — the
  bound now travels on `arrayBegin` and the element callback instead), but the
  cursor's `arrayCount` rejected a count larger than the bytes remaining as
  INCOMPLETE *before* the element loop — an allocation guard (corelib-ts#38)
  deciding the verdict from the count word alone. It is now a cap on the
  **allocation** (`min(count, bytes remaining)`) instead of a rejection, which is
  all it was ever for; the elements decide (corelib-ts#99). `skipArrayCount` had
  reached the same conclusion one path over and said so in its doc comment.
- **go** gained `ElemBoundVisitor.ArrayElemBound(id, kind) (min, max, ok)` — a
  **separate** optional interface, not a third method on `HeaderVisitor`, for the
  all-or-nothing reason recorded below: a new method there would silently switch
  `ArrayBegin`/`FixlenHeader` off for every visitor generated before it existed.
  **Both are retired** (corelib-go#130, generator#403). The elements go past one
  at a time now, so the width is checked in the `ArrayUnsigned`/`ArraySigned` arm
  where it lands, and `ArrayBegin`/`FixlenBegin` are ordinary `Visitor` methods —
  which also removes the all-or-nothing trap rather than working around it.
- **dart** gains `MessageVisitor.onArrayElemBound(id, kind) -> ElemRange?`, a
  virtual with a `null` default, so it is additive by construction.
- **python** took it as reader arguments, where the schema count and a blob's
  `maxlen` already went: `read_unsigned_array(elem_max)` /
  `read_signed_array(elem_min, elem_max)`. **That seam is gone** — corelib-py
  removed the pull API (§5.3.1), and its visitor surface has no per-element hook
  to replace it, so Python is the one backend where this bound is currently
  stated at the array header instead: corelib-py's `on_array_begin(id, wtype,
  count)` returns `(dst, elem_min, elem_max)`, and the decoder applies the width
  AT each element. `on_*_array` hands over a list that has already fully
  arrived, so a check there could only ever decide an array that *arrives* —
  which is the one case §5.2 is about. The same hook carries the schema count on
  its `count` argument and can hand over a destination buffer, so an integer
  array need never build a Python object per element. Its `count` argument is no
  longer *checked* there — generator#406 moved the schema count into
  `on_schema_bound`, which the decoder asks one hook earlier.

`kind` is what the **wire** declares and the bound applies only in the arm
matching the declared element type — the §7.3 rule `ArrayBegin` already carries,
one level down.

**Where each corelib applies it is a cost decision, and they differ on purpose.**
The bound only changes an outcome where the whole-array callback does not fire,
so three of the four apply it on the failure path and leave their element loops a
pure decode: go walks the prefix already filled (in `scanTruncatedArray` when the
count exceeds the bytes left, and in `fillUnsigned`/`fillSigned` when the count
fits but the elements do not — **both** branches, which is why the conformance
vector deliberately cuts at the second), dart walks the decoded prefix when the
contiguous array fails, and python-pure turns its `SofaIncompleteError` into a
`SofaDecodeError` at that point. The two that can afford exactness check at the
element: corelib-ts at the element callback, and python's Cython engine with
two typed compares before the value is boxed. Dart's *streaming* path also checks
at the element, because its state machine already decodes one at a time. The
verdicts are identical, which is what the shared vectors and the chunk-invariance
axis pin.

Measured after: `width_elem_trunc` is unanimous across the family, its control
(`ctl_width_elem_inrange_trunc`, an in-range element cut at the same offset)
stays unanimously INCOMPLETE, and TypeScript's chunk-invariance mismatches over
10 442 truncations × 6 chunk sizes went **100 → 8** — the whole
`whole=INCOMPLETE / chunked=INVALID` direction of generator#300, gone. go, dart
and both python engines measure 0 on the same axis.

**What is still open, and why it is not fixed here.** One F-0043 row remains: an
over-index wrapper element truncated *inside* its `fixlen_word`
(`overindex_trunc_in_fixlen_word.bin`, `c6 0c 2a c2`). It once split the family:
TypeScript's withdrawn cursor read the subtype from the low three bits of the
word's **first** byte, so the element had passed the §7.3 type test and its id was
over `count` before the length was known, and it rejected where the other drivers
reported INCOMPLETE. With that surface gone TypeScript takes the verdict where
every other driver does — at the completed word, in `fixlenBegin`, which
CORELIB_PLAN §4.1.1 makes normative ("the low 3 bits of an unfinished varint must
not influence an outcome even though they are already arithmetically fixed") — so
the family is unanimous and the divergence is closed by the spec having settled
rather than by a corelib callback (documentation#43, corelib-ts#97 withdrawn).

#### Decode verdict: over-index wrapper-array elements are INVALID (all targets)

The **sequence-form analogue** of the over-count scalar rule (generator#142).
A `string`/`blob`/`struct`/`union` element array with a schema `count: N` lowers
to a wrapper sequence whose child ids are the 0-based element index (§4, §9.2). An
element whose wire id is `≥ N` is a schema-bound violation: MESSAGE_SPEC §5.1
recovers a wrapper array's length as *highest present id + 1* and `count` bounds
it without ever adding elements the decoder did not receive, so §7 makes an
element id `≥ N` **`INVALID`**, *never silently truncated to `N`*. The
generated decoder therefore **rejects** an over-index element **before** growing
the container — which also bounds the fill: the id is an unbounded varint, so an
unguarded id-keyed grow materialised `id+1` elements and turned a ~9-byte message
into an arbitrarily large allocation (a heap-amplification DoS).

**§7.3 is decided first, and for a fixlen element that needs the fixlen word.**
An element header whose wire type — or, for a `string`/`blob`/fp element type,
whose fixlen subtype — contradicts the declared element type is *skipped* exactly
as an unknown id is (§7.3), so it never becomes an element and its id is not an
array index this bound could measure (§7.4: "an occurrence skipped under §7.3 is
not an occurrence"). For a fixlen element type the subtype is known only at the
fixlen word, so a message ending **between** the element header and that word is
`INCOMPLETE`, not `INVALID` — the analogue of §4.8's ruling for a fixlen array's
two words. From the fixlen word on the reject is immediate and never waits for
payload bytes. Format-level rejects still fire on a skipped element's own
metadata; §7.3 subordinates the *schema* bound only.

Who enforces it splits exactly like the scalar case:

- **Heap families reject** — the heap backends (`go`, `rust` std, `cpp`
  `corelib-cpp`, both Python, `java`, `kotlin`, `typescript`, `csharp`, `zig`,
  `dart`) emit a per-element `id >= N` guard using the same INVALID channel as
  the scalar over-count guard (`is.invalidate()` / sticky `inv` / `ErrInvalidMsg` /
  thrown `SofabException`/`SofabError` / `SofaDecodeError` / Dart's sticky
  `_inv`). A dynamic wrapper
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
  Why this needs a corelib affordance at all (and the over-`maxlen` case did not):
  an over-index is an element-*count* bound, not a byte bound. The over-`maxlen`
  reject (corelib-c-cpp#90) rides the C runtime's existing capacity check — a
  `maxlen` maps to the read's *buffer capacity*, and the core already rejects a wire
  `length > target_len` (`istream.c`), so the generated code just passes the bound
  as the capacity. But a fixed-count `string`/`blob` array lowers to a **wrapper
  sequence**, whose elements the core delivers one at a time by `id`; it never
  learns the schema `count`, so no capacity check fires. Only the callback knows
  `id >= N`, and it needs a channel to turn that into an `INVALID` verdict — the
  `sofab_istream_invalidate` abort primitive added in **corelib-c-cpp#92**.
- **C++ `c-cpp` now rejects** — with the #92 abort channel in place, the
  `_FixedStrSeq`/`_FixedBlobSeq` capacity guard calls `is.invalidate()` (in place of
  issue#126's silent `return`), so an over-index element is `INVALID`, converging
  with pure `cpp` and the heap families (generator#149). The reject still returns
  before the fill loop, so the issue#126 no-hang guarantee is preserved.
- **C now rejects** — the pure `c` target is descriptor-driven: its decode loop is
  the corelib's `object.c: sofab_object_field_cb`, which matches an element by
  scanning the descriptor's `field_list` for the id. A message skips an unmatched
  (unknown, forward-compat) id, but a fixed-count sequence **holder** — whose
  fields are exactly the element slots `0..N-1` — must reject an unmatched id as
  over-index. **corelib-c-cpp#94** added a `fixed_seq` descriptor flag (macro
  `SOFAB_OBJECT_DESCR_SEQ`) that makes `object.c` call `sofab_istream_invalidate`
  on an unmatched id; the generator now emits that macro for every holder
  (`buildHolder` sets `objectPlan.fixedSeq`), while messages and the elements' own
  struct/union type descriptors keep the plain `SOFAB_OBJECT_DESCR` skip form. So
  an over-index element is `INVALID`, converging the last F-0013 profile
  (generator#149). The reject is the corelib's, before any slot grows, so the
  issue#126 no-hang property is unaffected.

#### Decode verdict: over-`maxlen` strings/blobs are INVALID (every target)

The length axis of the same rule (generator "Option B"). MESSAGE_SPEC §7 + **§7.1
("a declared bound binds every target")** make a `maxlen: L` on a `string`/`blob`
a **wire-validity bound**, not a sizing hint: a value whose wire byte length
exceeds `L` is malformed input and **MUST** be reported as `INVALID` on *every*
target, **never silently truncated to `L`** — "two conformant implementations
MUST agree on which messages are valid," regardless of allocation strategy.

- **Heap families reject** — the heap backends now emit a per-field guard at
  the length header (`wire byte length > L → INVALID`) for every bounded
  string/blob, scalar field *and* wrapper-array element, using the same INVALID
  channel as the over-count/over-index guards (Dart reads `bytes.length` off the
  raw wire payload, which *is* the byte length, so nothing is re-transcoded to
  measure it). It is the **bounded-field twin**
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

#### Decode verdict: an over-width integer is INVALID (every target but `c`/`cpp-c-cpp`)

The **value** axis of the same rule, and the newest of the three. MESSAGE_SPEC
§1:45 used to call the declared integer width a *"storage hint"* and put
value-range *"outside this clause"* in §7, which left reject / mask / keep as
three defensible readings. `documentation` commit `70f9123` (documentation#32)
closed that hole: the declared width is now a **normative validity bound**, and
§7/§7.1 name an over-width scalar alongside `M > N` and `maxlen` as a
**schema-bound INVALID**. It **MUST NOT** be masked to the width and **MUST NOT**
be kept.

The check cannot live in the corelib. CORELIB_PLAN §4.1 accumulates every integer
into a ≥64-bit accumulator and delivers *that*; only the schema knows the
destination was declared `u8`. So this is §7's own division of labour — "the
corelib cannot know the schema, so schema-bound violations are detected, and
reported, by generated code" — and the guard belongs in the emitted store arm,
beside the `count`, wrapper-element-id and `maxlen` guards already there.

- **The narrow kinds only.** `u8`/`u16`/`u32` and `i8`/`i16`/`i32` are bounded;
  `u64`/`i64` span the delivery accumulator itself, so no reachable value can
  breach them and **no guard is emitted** for them. The one shared source of the
  ranges is `ir.NarrowRange` — backends import `ir` and nothing else, so the
  bound cannot drift between targets.
- **Every store, not just the scalar field.** A declared width binds wherever the
  schema declares it: message fields, struct/union members, and native array
  elements alike.
- **Placement inside an array arm is normative, not cosmetic.** The guard goes
  *after* the fill guard, never before. An over-width scalar arriving at an array
  id with no `array_begin` in front of it is a **§7.3 skip**; rejecting it ahead
  of the fill check would convert that skip into a spurious INVALID and make the
  two clauses contradict each other.
- **`c` and `cpp-c-cpp` were already conformant** and are deliberately untouched.
  Their descriptor-driven store path has the declared width at exactly the right
  point and already answers `INVALID`; adding a second guard would only duplicate
  it. This is the one decode verdict where the footprint pair led and the other
  eleven profiles followed.
- **Enums and bitfields are out of scope.** Their backing width is a property of
  the named type, not of the field, and an out-of-range *enum* value is a
  different question (unknown-variant handling) from an over-width integer.
  Kotlin is the one target where the enum question answers itself and so is
  covered: it stores an enum as an `Int`, which IS the signed 32-bit range
  MESSAGE_SPEC §1 binds an enum to, so the bound and the storage are the same
  fact and letting an out-of-range value through would be the silent truncation
  §7.1 rules out. Its bitfield is a `ULong`, i.e. the whole unsigned domain, so
  there is nothing to guard.
- **`cpp` needed a different shape from the rest.** corelib-cpp's typed `read()`
  ends in `value = static_cast<T>(raw)` — the mask itself, applied where
  generated code cannot see the raw value. A narrow destination therefore reads
  through a 64-bit temporary and range-checks before the store. §7.3 is
  unaffected: `read()` derives its expected wire type from signedness alone, so
  `u64` and `u8` frame identically and a contradicting tag still stores nothing.

**`cpp` array elements are covered by arming the corelib, not by inlining.**
`IStream::readArray` converts elements *inside* corelib-cpp
(`sp[i] = static_cast<Elem>(raw)`), so the raw value never reaches generated code
and the scalar temporary does not carry over; routing arrays through a wide
temporary would defeat the bulk/zero-copy path the maxspeed profile exists for.
corelib-cpp therefore takes the bound as an argument and the generator hands it
in (`ElemBound::of<E>()`), so the check runs at the point of conversion. Left at
its default the argument is unarmed and the unbounded decode runs — the defect
this closed. Floating-point elements are excluded by construction: instantiating
the helper for a `float` would cast `numeric_limits<float>::max()` to `int64_t`
in a constexpr context, and the corelib ignores the argument for a non-integral
element in any case.

This is the shape worth reaching for whenever a bound cannot be observed from
generated code: hand the schema fact to the corelib rather than re-deriving the
wire in the backend. It is what `c` and `c-cpp` have always done through their
descriptors, and why those two were conformant from the start.

Reproducers (Crucible F-0033, codegen defect G-0026, generator#266): `00 ff 7f`
is id 0 / `u8` / value 16383 and must be `INVALID`, as must 256 into a `u8` and
70000 into a `u16`; the in-range control `00 ff 01` (value 255) must still decode
and round-trip unchanged.

#### Decode verdict: a contradictory wire type is SKIPPED, not fatal (§7.3)

MESSAGE_SPEC **§7.3** requires a field whose header wire type is not the one its
declared type maps to (§1) — for `fixlen`, including the **subtype** — to be
**skipped**, exactly as a field with an unknown id is skipped. It is a framing
mismatch, not a malformed message: the header itself is well-formed, so the
decoder stays synced and simply does not deliver that field. Note this is a
*type* check, never a value-range check — `u8`/`u16`/`u32`/`u64`/`boolean`/`enum`/
`bitfield` all map to the unsigned-integer wire type, so a header carrying it is
well-formed for any of them. The **value** carried by such a header is a separate
question, decided later and by a different rule: see the over-width verdict
above, whose guard deliberately sits *behind* this skip so that a §7.3 mismatch
never reaches it.

The generated readers are **schema-typed**: each case arm calls the reader for the
field's declared type. That reader assumes it is only invoked for its matching
wire type, so the dispatch must check before reading. Who does the check differs
by family (generator#174, Crucible F-0020):

- **Python** — the generated dispatch compares the delivered header against the
  expected wire type **plus the fixlen subtype** where the wire type alone is
  ambiguous (`fp32`/`fp64`/`string`/`blob` all share `Fixlen`, and the fp arrays
  share `ArrayFixlen`), and skips on a mismatch. Without it the *whole* decode
  fails, because corelib-py rightly raises `SofaStateError` when a caller asks
  for a type the field does not carry — a caller error the generated code should
  never provoke.
- **Both C++ paths** — the comparison lives in the **corelib**, inside the typed
  read itself: `is.read(x)` knows both the tag it declares and the one that was
  delivered, so a contradicting field is skipped there and the generated
  `deserialize` carries no wire comparison at all. `corelib-cpp` decides inside
  every read; the `c-cpp` wrapper's C decoder unbinds a contradicting read and
  skips the field like an unknown id. Where an arm must touch its destination
  *before* binding it — sizing a string or blob, resetting an array, emptying a
  wrapper sequence — that preparation moved into the read too
  (`readString`/`readBlob`/`readArray`/`readSequence`), because a check is only
  a check if it precedes the side effect it protects. §6.6 later moved the
  **sizing** half back out of the codec, into the `sofab::` helper the generated
  arm actually calls (§9.3 family 4), and the ordering survived the move intact:
  the helper consults the tag, the schema bound and the receiver cap itself before
  it sizes anything, so a mistyped or over-long occurrence still reaches no
  allocator and leaves the destination as it was. This is the "seam"
  (`docs/models/type-reconciliation.md`); it replaced a generated guard per field
  arm, whose earlier failure mode in C++ was *silent*: `read<T>` zig-zags on
  `T`'s signedness rather than the wire type, so a `Signed` header on a `u8`
  field yielded the raw un-zig-zagged varint.
- **C** — the C object API is descriptor-driven, so the corelib makes the
  decision: `sofab_object_field_cb` compares the descriptor's expected wire opt
  against the delivered one and leaves `target_ptr` NULL on a mismatch, letting
  the istream skip the field (corelib-c-cpp#101). No generated guard, and none
  possible — the generator emits descriptors, not dispatch. Note this covers the
  **C** target only: C++ over `c-cpp` drives `istream.c` directly, not the object
  API, so it does not inherit the fix (see the gap below).
- **TypeScript** — settles it *structurally*, like the group below. corelib-ts
  resolves the fixlen word itself and dispatches to distinct typed callbacks, so a
  contradictory header lands in a callback whose id switch has no case for that
  field and evaporates. Two cases the wire type alone does not settle keep an
  explicit test: an ARRAY header, where `arrayBegin`'s `kind` separates the four
  element kinds that share one wire type, and a bounded fixlen SCALAR, where
  `fixlenBegin`'s `subtype` keeps a `string` arriving at a `blob` id from being
  bounded by that field's `maxlen`.
- **Go, Rust (std + `no_std`), C#, Java, Zig** — conformant *structurally* for
  every mismatch but one. Their corelibs resolve the fixlen word themselves and
  dispatch to *distinct typed callbacks* (`Float64` vs `String`, …), each
  defaulting to a no-op. A contradictory header therefore lands in a different
  callback, whose id switch has no case for that field, and evaporates — no
  generated check, nothing to get wrong. The **one** exception is an integer
  **array** delivered to a **scalar**-declared id of the same signedness, which
  needs an explicit guard on five of the six (see below).

#### A skip must contain the whole subtree, and leave no residue behind it

§7.3 says a contradicting field is skipped "exactly as a field with an unknown id
is skipped", and CORELIB_PLAN §5.2/§4.9 say what skipping a *sequence* means:
the entire sub-sequence is consumed and discarded, descending into nested
sequences. A skip is therefore not just "do not store this header" — it has two
further obligations that the flat-visitor backends each got wrong in a different
way (Crucible F-0044/F-0045/F-0046/F-0047, generator#268/#270/#271/#272).

**Containment: the children go with the parent.** A flat visitor tracks its
position in a `cur` scope variable, and its `sequence_begin` dispatch had no
default arm — an id the schema does not declare here left `cur` on the enclosing
scope, so the skipped sequence's *children* bound into it. A child id 3 inside an
unknown sequence set the root's own field 3; a sequence opened at a string-array
element position bound its string as that element. The fix is one shared shape: a
**dead scope** (`_Loc::Dead` in rust, `.dead` in zig, `_DEAD` in java/csharp,
`DEAD` in kotlin) that no callback arm matches, so the whole subtree is discarded — a nested sequence
inside a dead subtree matches no arm either, so it stays dead, and the live scope
is restored at the matching end. Dart reaches the same result one level up: its
collectors inherit a shared base whose `onSequenceStart` returns `null` instead
of the corelib's descending `this`, beside the `onStringBytes` no-op that is there for exactly the
same reason.

This has to hold **even for a message that declares no sequence at all**. Neither
corelib skips the subtree on its own — corelib-rs's trait default is a no-op and
corelib-zig only checks `@hasDecl` — so a visitor that omitted the override let an
unknown sequence's children arrive with `cur` still on root. Both backends now
emit `sequence_begin`/`sequence_end` unconditionally.

**No residue: the counters must not stay armed.** `array_begin` armed its §7.3
discard and fill counters on the wire kind *family* (`Unsigned | Signed` in one
arm), so an `ArrayUnsigned` header at a declared `i8[]` was skipped but left the
fill counter armed — and the next bare scalar was absorbed into that array. The
same collapsing let a schema `count` be applied through a wildcard-kind arm, so an
`ArrayFixlen` header at an integer id was measured against a bound belonging to a
field it is not. Both are fixed by keying **one arm per wire kind**, which makes
the §7.3 check decide in the match itself — before any counter is armed and before
any bound is applied, the order CORELIB_PLAN §4.8 requires.

**And it must cost nothing to come back from.** A skipped subtree is the one
place where wire depth is unbounded by the schema: `MAX_DEPTH` is 255
(CORELIB_PLAN §4.9/§6.2) and an unknown sequence — which forward compatibility
*requires* a decoder to accept — may nest arbitrarily inside a known one. A
backend whose scope stack is fixed-capacity and sized from the **schema** (the
reachable frame count) therefore had a bound that the **wire** could exceed:
`rust` `no_std` stacked every opened sequence into a `heapless::Vec<_Loc, N>` and
dropped the push's `Result`, so past `N` the push did nothing, the matching pop
restored the *wrong* scope, and a field written after the unwind bound nowhere —
accepted, and silently missing a field (generator#283 / Crucible F-0055).

The rule that removes the mismatch: **only live scopes are stacked.** Every scope
inside a dead subtree is dead, so the stack was recording nothing but the level to
return to; a `u16` counter (`dead`) records that for any depth, and what stays on
the stack is one entry per live scope entered — a chain the frame count bounds.

No other backend had the mismatch, for three different reasons: `c`/`cpp-c-cpp`
never push for a skipped subtree at all (an unknown id matches no descriptor
field, so no object decoder is taken and the istream skips the subtree itself),
`zig` sizes its stack by *wire* depth (`[256]_Loc`), and `java`/`csharp`/`kotlin`
grow theirs. The generalization for any new flat-visitor backend: if the scope
stack is bounded by the schema, skipped levels must not be stacked — and a bounded push
must never have its overflow discarded.

The lesson generalizes: a skip is only correct if it is *scoped* (children go with
it), *inert* (it arms nothing that a later field will read), and *free to unwind*
(it consumes no schema-sized resource).

#### The one mismatch structural skip cannot catch: an array at a scalar id

For `rs`, `rs-no-std`, `cs`, `java`, `kotlin` and `zig` the structural skip has a
blind spot (generator#183, Crucible F-0021). Those corelibs stream an integer
array's elements **one by one through the very `unsigned(id, v)` / `signed(id, v)`
callbacks a lone scalar uses**, announcing `arrayBegin(id, kind, count)` first as
context. This is a deliberate zero-extra-allocation streaming design —
`corelib-rs-no-std` and `corelib-zig` depend on it for heap-free decode — but it
means an `ArrayUnsigned` header at an id declared `u8` delivers its elements into
the *matching* callback, where the id dispatch stores them. Every other mismatch
skips because it arrives at a *different* callback; an array of the same
signedness has no different callback to fall through to. (`go` is unaffected: its
corelib hands arrays to a distinct array-shaped visitor entry point.)

The fix is **generator-only** — all five corelibs already announce `arrayBegin`
with the count before any element, so the generated visitor only has to consult
it. Every backend emits the same two-part shape:

- in the generated `arrayBegin`, arm a per-visitor counter: `askip = count`
  unless the `(scope, id)` pair declares a native array **of the announced
  `kind`**; `0` otherwise. Every wire array kind is armed, not just the integer
  ones: an fp array's elements reach the `fp32`/`fp64` callbacks, which a scalar
  `fp32`/`fp64` field shares (generator#193), so the same blind spot exists
  there.
- in `unsigned` / `signed`, discard while armed: `if askip > 0 { askip -= 1;
  return; }`.

It self-terminates on the announced count, so no array-end callback is needed; it
survives a feed chunk boundary because the counter lives in the generated visitor
object; a legitimately declared array is disarmed and stores normally; and a real
scalar at the same id *after* the array still decodes, because the counter has
reached zero by then. Two profile-specific wrinkles: under `rs-no-std` the guard
is emitted only when the schema turns on the `array` Cargo feature (without it
that corelib cannot decode an array wire type at all, so no element can reach a
scalar callback, and referencing the feature-gated `ArrayKind` would not
compile); and Zig rejects unused function parameters, so `arrayBegin` takes `id`
only when the message has an integer array to disarm for. On the four backends
whose `arrayBegin` was previously emitted only for messages that *have* a native
array, it is now emitted whenever the guard needs somewhere to arm itself.

#### A fixlen array's element subtype decides before any schema bound (§4.8)

An array header carries a wire *kind*; for the fixlen array wire type it carries
a second word, the `fixlen_word`, which says whether the elements are `fp32` (4
bytes) or `fp64` (8 bytes). CORELIB_PLAN §4.8 fixes the order in which those are
consumed: **count** (a format ceiling only) → **`fixlen_word`** → a §7.3 skip on
a contradicting subtype, *without* applying the schema `count` → the schema bound
only for a field that survives that test.

Generated code could not express that, because the array-header hook fired
before the `fixlen_word` and reported a collapsed `Fixlen` kind — which says the
array is a fixlen array but not whether its elements are `fp32` or `fp64`. An
`fp64` array arriving at a declared `fp32[N]` slot was therefore indistinguishable
from the real thing: it was measured against `N`, and an over-count turned a
*skippable* contradiction into `INVALID`. Sizing the declared container from it
was the same error in the other direction.

The push-API corelibs changed to make the rule implementable (Crucible F-0042,
generator#259). The hook now fires **after** the `fixlen_word`, and `ArrayKind`
is `{Unsigned = 0, Signed = 1, Fp32 = 2, Fp64 = 3}` — the collapsed `Fixlen`
member is gone. The ordinals are normative and shared family-wide; `corelib-ts`
is the reference. Two corelibs gained a `kind` parameter they never had
(`corelib-go`'s `ArrayBegin(id, kind, count)`, `corelib-dart`'s
`onArrayBegin(id, kind, count)`).

Every push-API backend therefore emits **one arm per wire array kind**, and a
declared fixlen array appears only under the arm for its *own* element subtype.
Three things live inside that arm and nowhere else: the schema `count > N`
reject, the receiver-side `max_dyn_array_count` reject, and any sizing or
clearing of the declared container. A header of a non-matching kind falls through
to the discard counter and is consumed without touching the field. Integer arrays
are unaffected — that path has no second word.

`c`, `cpp` and `python` need no change: their descriptor and pull APIs already
deliver a field after both words, which MESSAGE_SPEC §7.3 explicitly blesses, and
they are the differential control for this behaviour.

One gap is deliberately left open: **rust still keys its integer-array arms
kind-agnostically**, so an fp header arriving at a declared *integer* array id
still applies that field's bound and clears it. That is the generator#254 /
Crucible F-0039 face, fixed for `java` and `csharp` only and never for `rust`; it
is a different codegen path and is tracked on its own. The other five backends
key their integer arms too.

#### Who can satisfy §7.3, and why (audited)

The obligation lands differently depending on **who decides a field's type**, and
that is a property of the corelib, not of the backend. Three models exist:

| model | corelibs | who checks | §7.3 status |
|---|---|---|---|
| **corelib dispatches by type** — the corelib resolves wire type *and* fixlen subtype, then calls a distinctly-typed callback | go, dart, rs, rs-no-std, cs, java, kotlin, zig | nobody, *except* for an integer array at a scalar id: the six that stream array elements through the scalar callbacks need the generated `askip` guard (generator#183). `go` and `dart` are exempt even there — their corelibs deliver a native array *whole* through a distinct `on*Array` callback, so an array at a scalar id skips structurally too | structural ✅ (+ `askip` on rs/rs-no-std/cs/java/kotlin/zig) |
| **descriptor / object API** — the generator hands the schema to the corelib as a table | c (`c-cpp` object API) | corelib, against the descriptor (mask `0x3F` = wire type + subtype) | ✅ |
| **generated code pulls by id** — the corelib delivers `(id, …)` and generated code chooses the reader | python, typescript | **generated code**, so the corelib must expose the delivered type at the decision point | ✅ all |
| **corelib decides inside the typed read** (the seam) — generated code names the type it wants and the corelib compares before it acts | cpp, cpp-over-`c-cpp` | corelib, in `read`/`readString`/`readBlob`/`readArray`/`readSequence` | ✅ |

Only the third model puts the burden on the generator, and it is the only one
where a gap was possible — the corelib must surface enough type metadata for the
guard to be written at all. CORELIB_PLAN §5.2 already requires exactly that
("the decoder notifies the **field handler** … with the field's *identity and
type metadata*", with **Skip** as a first-class handler outcome), so the two
gaps that once existed here were shortfalls against that contract, not missing
features. Both are now closed:

- **python** — `Field.type` + `Field.subtype`. ✅
- **cpp** (`corelib-cpp`) — was `wire()` + `fixType()` (corelib-cpp#43); the
  decision has since moved inside the reads themselves, so generated code needs
  neither. ✅
- **typescript** — was `Cursor.wire` + `Cursor.fixSub` (corelib-ts#58); with the
  cursor withdrawn the decision moved into the visitor's own header hooks, which
  carry the announced `subtype` (`fixlenBegin`) and the announced element `kind`
  (`arrayBegin`, where `ArrayKind` names `Fp32`/`Fp64` directly). ✅
- **cpp over `c-cpp`** — likewise: `wire()` + `fixType()` were added with the
  same `sofab::Wire`/`sofab::Fix` surface as `corelib-cpp` (corelib-c-cpp#104),
  and are now used by the corelib's own reads rather than by generated guards.
  The C decoder underneath skips a contradicting read instead of reporting a
  usage error (corelib-c-cpp#111), which is what made that possible. ✅

All twelve target/corelib combinations now pass the four §7.3/§7.4 vectors (a
`fixlen`-subtype mismatch and its control, plus the two §7.4-interaction cases
below) — `dart` joins the group that settles them structurally (its corelib
dispatches by resolved type to distinct callbacks, including a distinct `on*Array`
for native arrays, so no generated guard is needed; §9.3 family 2). The two combinations that previously failed — TypeScript on the subtype
vector, and `cpp`/`c-cpp` on *any* contradictory wire type — are covered by the
corelib accessors above and the generator guards keyed off them; the conformance
harnesses assert all four on every target, no longer gated. The backends carrying
the `askip` guard (`rust`, `rust`/`no_std`, `csharp`, `java`, `kotlin`, `zig`)
additionally assert the array-at-a-scalar-id pair — an unsigned array at a `u8`
id and a signed array at an `i8` id, each with a correctly-typed control that
pins the counter's self-termination.

##### The §216 header hooks re-opened §7.3 for the "structural" backends

"The corelib dispatches by type, so §7.3 is structural" holds for the *value*
callbacks — it does **not** hold for the header hooks generator#216 added, because
those fire **before** dispatch, at the count/length word, which is the whole point
of them. A `maxlen` guard written there sees only `(id, subtype, length)`: the
corelib has resolved the wire subtype but cannot know the **declared** one, which
is schema knowledge only the generated code has. Comparing `length` alone
therefore measures a *contradicting* fixlen value against the declared field's
bound and rejects it, where §7.3 requires it be **skipped** (generator#224: an fp64
— 8 bytes — landing on a `blob` with `maxlen: 4` came back `INVALID`, and the same
un-gated shape rejects any string↔blob mismatch longer than the bound).

The rule for any header-hook guard: **gate the bound on the declared subtype**
(`subtype == string && length > N`), never on length alone. Applied to:

- **dart** — `if (subtype == sofab.FixlenType.string && length > N) e.inv = true;`
- **go** — `if subtype == 2 && length > N { return sofab.ErrInvalidMsg }`
  (corelib-go keeps its fixlen-subtype constants unexported, so the generated
  guard spells out the §4.6 wire values, which the format fixes).
- **cpp** — the descriptor row carries the subtype rather than the guard:
  `{.wire = sofab::Wire::Fixlen, .subtype = sofab::Fix::Blob, .bound = N, …}`,
  and `measureField` matches it alongside the wire type (generator#229, below).

The array hooks (`onArrayBegin`/`ArrayBegin`) get no analogue: an array's wire
type already selects the callback, and the fixlen-array *element* subtype is
checked by the element callbacks the corelib dispatches to.

That last point is a **known, deliberate asymmetry**, uniform across the family
rather than a per-backend gap. For a fixlen array the `count` word precedes the
`fixlen_word` on the wire, so at the moment the count bound is decided the element
subtype is not yet known — dart/go's hooks are handed `(id, count)` and nothing
else, and cpp's `measureField` would have to defer its `ArrayFixlen` count check
past the element-size word to see it. So an fp64 array arriving at a declared
`fp32[] count: N` is **skipped** when its count is within `N` (§7.3, measured
identical on cpp and go) but reported **INVALID** when its count is above `N` —
the over-count guard fires first, on a field §7.3 says is not that field's value
at all. Deciding whether §5.2 should defer here (with the subtype unknown, more
bytes genuinely could still make the field skippable) is a MESSAGE_SPEC question,
not a codegen one; until it is settled the family stays uniform. Tracked with the
measured vectors in **#232**.

Three adjacent findings from the same audit:

- **go, HeaderVisitor is all-or-nothing.** `sofab.HeaderVisitor` declares *both*
  `ArrayBegin` and `FixlenHeader`, and the cursor reaches them through a single
  `v.(HeaderVisitor)` assertion. Emitting only the method a message happens to
  need left that assertion **failing**, silently disabling every header reject —
  so a message with a `maxlen` but no counted array (or the reverse) still folded
  over-bound+truncated to `INCOMPLETE`, the generator#216 defect it was supposed
  to fix. A type with any bound now gets both methods; the one with no arms is an
  empty switch. A bound-free type still implements neither, so the max-speed path
  is unchanged.
- **cpp (pure profile) had the same §7.3 hole, in the measure-phase schema rather
  than a header hook — fixed in generator#229 + corelib-cpp#51.** `FieldBound`
  carried `id` + `wire` + `bound` and no subtype, and fp32/fp64/string/blob all
  share `Wire::Fixlen`, so the measure check (`fb->wire == Wire::Fixlen && len >
  fb->bound`) matched a *contradicting* subtype and rejected it — an fp64 landing
  on a `maxlen: 4` blob came back `INVALID` where the deliver path (which does
  gate on `is.fixType()`) and every other backend skip it. `FieldBound` gained a
  `subtype` member that `measureField` matches alongside the wire type, and the
  generator emits it on every `Wire::Fixlen` row. The member defaults to
  `Fix::Fp32` — a subtype no *bounded* fixlen field can declare, since only
  string/blob carry a `maxlen` — so an unset subtype disables that row's bound
  instead of misapplying it, and the array/sequence rows that never read it are
  unchanged. This is the whole §5.2-vs-§7.3 interaction: a skipped field carries
  **no** bound, so it is measured for completeness only (a truncated skipped field
  is `INCOMPLETE`, never `INVALID`), while a *matching* subtype keeps the full
  anti-folding order.
- **typescript is clean by construction** — the `maxlen` verdict lives in
  `fixlenBegin` and is gated on the announced `sub`, so a mismatched subtype is
  skipped and never measured against this field's bound at all.

#### Decode verdict: a repeated field id — scopes merge, wrappers replace (§7.4)

MESSAGE_SPEC **§7.4** defines what a decoder does when a field id repeats within
one scope: the **last occurrence wins, per field id**. The consequence differs by
what the field *is*:

- A re-opened **sequence continues its scope** — `struct` and `union` members
  therefore **merge**, and children set by an earlier opening whose ids do not
  recur are **retained**.
- An **array wrapper is the exception**: it *is* the array's value (§5), so a
  later occurrence **replaces it whole**.

Both halves are decode-side only; encode never emits a repeated id. Who enforces
them (generator#175, Crucible F-0019):

- **Nested `struct`/`union` must decode *into* the existing member.** Most
  backends already did (C++ `is.read(nested)`, Go `return &m.Nested, nil`).
  **TypeScript** did not: its case arm assigned a *fresh* object built by the
  per-type from-decoder, so the earlier opening's children were discarded. The
  decode loop now lives in `_decodeInto<T>(c, o)` — `_decodeFrom<T>(c)` is the
  fresh-object entry point that delegates to it — and the nested case calls
  `_decodeInto<Child>(c, o.member)`. The member is always constructed (the class
  declares it with a `new T()` default), so nothing is allocated on that path.
  (Both were `static` methods on the generated class until generator#384 moved
  them to module level; the §7.4 shape is unchanged.)
- **Array wrappers must be cleared before filling.** The **C++** collectors
  (`_StrSeq`/`_BlobSeq`/`_MsgSeq` and their fixed `InlineVector` variants) place
  by element index or emplace in arrival order and never reset, so a second
  opening merged into the first one's elements; the generated case arm now emits
  a `clear()` before the read. Go already emitted the equivalent
  (`m.Field = m.Field[:0]`). Native scalar arrays need no clear — they read the
  whole array in one call, which already replaces.
- **C** distinguishes the two kinds by the `fixed_seq` flag the generator already
  emits for every wrapper array (`SOFAB_OBJECT_DESCR_SEQ`, required since
  corelib-c-cpp#96). That flag is what lets `object.c` reset a wrapper's slots on
  open while structs and unions keep merging (corelib-c-cpp#101) — so this target
  needed **no generator change**, only the descriptor kind it already emits. This
  supersedes the "C needs a new descriptor kind" reading in generator#175.

**§7.4 interacts with §7.3, and the ordering is load-bearing.** The spec closes
the clause with:

> An occurrence skipped under §7.3 is **not** an occurrence for this clause: a
> correctly typed earlier occurrence survives a mis-typed later one.

For a struct this is free — a skipped occurrence simply never opens the scope.
For an **array wrapper it is a trap**, because the §7.4 fix is a destructive
`clear()`: if generated code clears the target *before* consulting the wire type,
a mis-typed later occurrence silently wipes a valid earlier array — turning a
loud failure into silent data loss. Every backend therefore places the reset
behind the type decision, though by different means:

- **cpp** (both corelibs) emits the §7.3 guard *above* the clear in the same case
  arm, so the guard's `break` skips it. This is why the c-cpp accessor and the
  guard had to land together (corelib-c-cpp#104): emitting the guard above the
  clear fixes the mis-typed-INVALID *and* the interaction at once — fixing only
  the INVALID would have converted a loud failure into the silent-data-loss
  variant.
- **typescript** collects into a fresh local and only publishes it (`o.f = arr`)
  after the loop, so a skipped occurrence never touches the member.
- **go, rust, zig, cs, java, kotlin** put the reset *inside* the sequence-begin
  callback, which the corelib only invokes for an actual sequence header — so the
  wire-type dispatch shields it structurally.
- **c** resets in `object.c`'s `FIELDTYPE_SEQUENCE` case, which sits after the
  descriptor wire-type check.

#### Decode verdict: invalid-UTF-8 strings are INVALID (strict, config-gated)

MESSAGE_SPEC §8 + CORELIB_PLAN §6.4 make a `string` **UTF-8 text** (`blob` is the
type for opaque bytes): an invalid-UTF-8 `string` that is *materialized* is the
`INVALID` decode outcome, enforced symmetrically (encode refuses non-UTF-8 with
`InvalidArgument`). The check is gated behind the corelib option **`SOFAB_STRICT_UTF8`
(default ON)** — a *validation policy, never a wire-format switch*, so peers with
different settings still interoperate on all valid data. **No silent U+FFFD in any
mode**: lossy replacement mutates the payload and breaks the byte-exact round-trip,
so it is forbidden. Skipped fields are never validated (validation runs only where a
string is read into a destination) — see *Placement* below, which is where the
codegen half of that rule lives. Where the string is materialized decides where
the generator carries responsibility:

- **Codegen-materialized Unicode targets (Rust, Java, C#, Kotlin) are always
  strict** — a Unicode string type cannot hold non-UTF-8 bytes, so its only non-mutating option
  is the strict constructor and the option is a documented no-op (always ON). The
  generator emits the strict path directly: Rust `core::str::from_utf8` (Err → the
  sticky `inv` flag → `InvalidMsg`; the two Rust profiles now agree, **subsuming
  #80**), Java an allocation-free `Utf8.valid` scan followed by the JVM-intrinsic
  `new String(…, UTF_8)` (the platform constructor alone is lossy), Kotlin the same
  pair against the corelib's `Utf8.valid` scanner and `ByteArray.decodeToString`
  (whose default settings substitute `U+FFFD`, which §8 forbids in every mode), C# a
  `UTF8Encoding(throwOnInvalidBytes: true)` (`Encoding.UTF8.GetString`
  is lossy) — invalid bytes throw the same `INVALID_MSG` channel as the over-count
  guards. No config key is threaded into generated code.
- **Codegen-materialized byte-container target (Zig)** — the borrowed `[]const u8`
  slice is zero-copy, so the corelib exposes a `utf8Valid(bytes)` primitive and the
  generator emits an **unconditional** call at the materialization site
  (`!sofab.utf8Valid(chunk) → self.inv`); the `SOFAB_STRICT_UTF8` gate lives
  inside the primitive (folds to `true` when compiled off), so generated code is
  identical across build configs and flipping the flag never regenerates it.
  `blob` elements are stored verbatim — the wrap is emitted only for `string`.
- **Corelib-materialized targets (c, cpp, py, ts)** build the string inside the
  corelib, so the check is corelib-internal; the generator emits no UTF-8 code for
  them. (`go` and `dart` were in this group until the skip-placement work moved
  them out — see *Placement* below.) Encode-side strictness is corelib-side for **every** target (the generator
  encodes via `os.writeString(id, value)` into the corelib's OStream).

**Placement — the destination is resolved first (normative, generator#257).** For
every codegen-materialized target the corelib delivers **all** string payloads to
the generated `string()` callback, an unknown id and a §7.3 wire-type contradiction
included: the push visitor, not the corelib, is what decides whether a payload is
materialized. So the callback must resolve the destination **before** it does
anything else — a `(scope, id)` match over the string-declaring fields and the
string wrapper-sequence rows, returning immediately when nothing matches. Only
inside a matched arm may it accumulate, transcode, validate, or set a sticky
invalid flag. Emitting the UTF-8 check ahead of that dispatch validates payloads
the message never reads, which is exactly what CORELIB_PLAN §6.4 forbids, and it
also lets a skipped payload's bytes enter the shared chunk accumulator where a
later declared field would inherit them. The schema-`maxlen` and receiver-limit
pre-checks are themselves destination-scoped, so they sit behind the guard and
§5.2's INVALID-over-INCOMPLETE ordering is unaffected. Zig had this order from the
start (as does kotlin, which was written to it); **rust, rust-no-std, java and
csharp were fixed to match** (Crucible F-0038, codegen defect G-0024). `blob` needs no such guard — it carries no encoding, so
there is nothing to validate on the way past.

The degenerate case is part of the rule: a message that declares **no string at
all** still gets the callback (the visitor interface declares it, and the corelib
still routes string fields at unknown ids to it), and every string reaching it is
skipped by definition — so its **body is empty**, not guarded. Decoding one only
to drop it is the same §6.4 violation with every string skipped instead of some.
Rust gets this for free (the callback is emitted only when the schema uses
strings); java, csharp and kotlin emit the empty body explicitly.

**go and dart moved too, as a two-half change.** Both corelibs used to hand the
visitor a *finished* language string, so the check sat inside the corelib and the
generator had no seam to guard. Each corelib gave up that check and exposed what
the destination needs instead, and each backend picked it up:

- **go** — `sofab.UTF8Valid(bytes) bool`, called in every arm that stores a
  string: the scalar fields. A wrapper-array element is materialized by the
  collector, which is corelib-go's `sofab.StringSeq` since generator#345 and
  validates it there. The primitive carries its own compile-time gate, so
  generated code calls it unconditionally and never depends on the corelib's
  build configuration.
- **dart** — `MessageVisitor.onStringBytes(id, bytes)` delivers the **raw wire
  bytes**. The generated visitor overrides that instead of `onString`, so the arm
  resolves the destination first and only then calls `sofab.decodeUtf8Strict`,
  which validates and materializes in one pass and answers `null` for anything
  malformed. A pleasant side effect: a schema `maxlen` is a *byte* bound, and the
  raw bytes are the wire length, so the guard no longer re-encodes to measure.

Neither half is correct alone: the corelib half by itself accepts invalid UTF-8 at
a *declared* field, so the two land together.

Overriding the hook where a destination exists is still only half the property, as
dart showed (#265). A corelib's string hook has to keep a **validating default**
for hand-written visitors — they carry no schema, so every string they are handed
is one they wanted. Generated code is the opposite case, and a scope with *no*
string destination emitted no override at all and inherited that default: an
undeclared string reaching the top-level visitor of a string-free message was
rejected instead of skipped. The rule for any push backend is therefore that the
skip is emitted for **every** visitor scope, not only the ones with somewhere to
put a string — dart carries it on a shared base every generated visitor extends
(the corelib's `sofab.VisitorBase` since corelib-dart#65, emitted before that),
java/csharp/kotlin emit the callback unconditionally with a
resolve-then-leave prologue (#258). Where a backend can make the property
structural rather than per-emission-site, it should: that is what stops the next collector class from
silently re-inheriting the default.

The validator is a real UTF-8 validator (rejects overlong forms incl. `C0 80`,
surrogates `U+D800`–`U+DFFF`, and code points above `U+10FFFF`; permits embedded
`U+0000`), and validity is a property of the **complete** payload — a multi-byte
sequence split at a chunk boundary stays `INCOMPLETE`, only a truncated-at-end or
malformed payload is `INVALID` (§5.2 anti-folding). This is tracked family-wide as
**issue #85**; conformance and the differential fuzzer run with the check ON.

**That timing is now normative, and the exception that allowed the other reading
is gone (documentation#33 → documentation#40).** CORELIB_PLAN §6.4 used to say a
decoder **MAY** report `INVALID` mid-payload for a byte that can never appear —
the one place §5.2's INVALID-dominates-INCOMPLETE precedence was allowed to pull a
verdict forward. It now says the opposite: a decoder **MUST NOT** report `INVALID`
before the declared length is reached; that input is `INCOMPLETE` until the payload
ends, and `INVALID` once it does. The reason is that this check is not a property
of the wire — `SOFAB_STRICT_UTF8` has a normative OFF mode, and validation runs
only where a string is *materialized* — so letting its timing decide the verdict
would make two conformant decoders disagree on accept-vs-reject.

**Nothing changes in the generator, and this is measured rather than assumed.**
Every backend's validation already sits behind reassembly: the check runs on the
value the accumulator returns, and the accumulator returns nothing while the
payload is short. Zig was the one implementation the finding named, so it is the
one worth pinning — generated `decode()` *and* the streaming `Decoder`, against
corelib-zig `main` with strict UTF-8 on:

```
mid-payload 0xa2, payload declares 2 bytes, 1 arrives
  whole -> INCOMPLETE      chunk 1/2/3/5/8 -> INCOMPLETE
control: the SAME byte in a payload that COMPLETES
  whole -> INVALID         chunk 1/3       -> INVALID
```

The control is what makes the first line mean something: without it, "INCOMPLETE
everywhere" is equally consistent with validation being compiled off.

### 9.4 Capability / value-width model

Footprint-tunable corelibs gate wire types behind build switches; the generator
must (a) only emit calls for the wire types a message uses, and (b) surface a
guard so a stripped corelib + a message needing a missing feature fails loudly.
The authoritative switch lists live in each corelib's README — the generator
only needs to mirror their *names* and gate on the schema's used features:

- **corelib-rs-no-std** — Cargo features (`fixlen`, `array`, `sequence`, `fp64`,
  `value64`); see its [README](https://github.com/sofa-buffers/corelib-rs-no-std).
  The generated crate sets `default-features = false` + the **full** wire-type set
  (not a schema-derived subset — see the §7.3 caveat below), emits the `Visitor`
  callbacks those types need, and a `require!` guard asserts the set.
- **corelib-c-cpp** — `SOFAB_DISABLE_*` macros (`FIXLEN`, `ARRAY`, `SEQUENCE`,
  `FP64`, `INT64`); see its [README](https://github.com/sofa-buffers/corelib-c-cpp).
  Generated C emits per-feature `#error` guards (only for features it uses); the
  C++ wrapper hard-requires FIXLEN+SEQUENCE and gates ARRAY/FP64/INT64.
- **Value width** — disabling 64-bit integers narrows the value type to 32-bit;
  a schema with no `u64`/`i64` field then builds against the smaller corelib.

> **§7.3 skip caveat (generator#215 / Crucible F-0027).** Point (a) — "gate on
> the schema's used features" — holds only where a feature gates *field storage /
> encode*. Where a corelib gates wire-type **parse/skip** behind the same switch
> (as corelib-rs-no-std and corelib-c-cpp do), a **decoder** must provision the
> full wire-type set regardless of the schema: §7.3 requires skipping any wire
> type an unknown id may carry (array, fp64, 64-bit value), so a schema-derived
> subset yields a decoder that *rejects* a well-formed skippable field instead of
> skipping it. The rust backend therefore emits the full feature set for
> corelib-rs-no-std.
>
> The feature-independent skip path this note previously named as the
> footprint-preserving alternative is **settled and not available**: corelib-rs-no-std
> built and measured it (corelib-rs-no-std#103/#104) and it grew the integer-only
> Cortex-M0 build from 614 B to 986 B (+61 %) and `IStream` from 12 B to 28 B — an
> unconditional parser for every wire type is most of what the flags save, so
> keeping it defeats their purpose. Both footprint corelibs therefore define a
> disabled flag as a **subset of the wire format**, not as narrower storage: a
> message carrying the construct is INVALID, terminally (corelib-c-cpp#145/#146
> made the verdict sticky, having found that feeding on past a rejection could
> resynchronize and report OK for a message already refused). Interop narrows to
> peers that never send the construct in *any* field, including ids the receiver
> does not know — which is exactly why a **generated** decoder, which cannot know
> its peers' schema, provisions the full set.

### 9.5 Decode resource bounds (receiver-side limits)

MESSAGE_SPEC §5.4 bounds the decode *stack* (`MAX_DEPTH`); this is the **heap
analogue** (generator#102). Schema `count`/`maxlen` are optional — a field
without one is dynamic/unbounded, and its decoder would otherwise allocate
whatever the wire claims (heap-exhaustion DoS; count-prefixed arrays are the
sharp *amplification* vector: a ~10-byte message claiming `count = 2^31`).

Three **sofabgen config** keys — `max_dyn_array_count`, `max_dyn_string_len`,
`max_dyn_blob_len` (`generic:`, per-target overridable) — bake receiver-side caps
into the generated code as named constants. **Every target MUST have a finite
default for all three**; there is no unset state and no unlimited mode. The
*values* are deliberately left to the target: an element count that is trivial on
a server is brutal in C, so a sensible cap is a per-language, per-use-case
judgement and never a family-wide constant.

**The defaults** (generator#385) live in one place, `internal/generator/dynlimits.go`,
as two tiers a backend picks from — the *values* are per-target, the *reasoning*
is not, and duplicating a number across ten backends is how they drift:

| Tier | Targets | `max_dyn_array_count` | `max_dyn_string_len` | `max_dyn_blob_len` |
|---|---|---|---|---|
| Server / native | `go`, `java`, `csharp`, `python`, `rust` (std), `cpp` (`corelib: cpp`), `zig` | 65536 | 1 MiB | 4 MiB |
| Client | `typescript`, `dart`, `kotlin` | 16384 | 256 KiB | 1 MiB |
| Statically bounded | `c`, `cpp` (`corelib: c-cpp`), `rust` (`rs-no-std`) | inert, provably | | |

The magnitudes are an **amplification barrier, not application policy**. The
attack is the one this section opens with — a ~10-byte message claiming
`count = 2^31`, i.e. 2 GB from nothing — and 65536 takes five orders of magnitude
off it. Going much lower buys almost nothing more against the DoS while rejecting
a great deal of ordinary traffic, and an application-tight bound belongs in the
**schema**, where it is wire-visible to both peers and enforced as `INVALID`
everywhere rather than as a receiver-local policy rejection. 4 MiB for a blob is
gRPC's default max receive message size, deliberately a number operators already
know as "the one you raise for large payloads"; a string is text and is kept an
order below it. The client tier sits one order down because a browser tab or a
phone process has a memory budget well under a server's — and TypeScript pays 2×
on strings, a byte cap on the wire becoming UTF-16 in memory.

There is deliberately **no third, smaller "firmware" tier**, because it would
apply to nothing: the statically bounded profiles reject an unbounded field at
schema-validation time (`checkBounded` in the `rust`, `c` and `cpp` backends, and
`rust` skips `resolveLimits` outright unless `std()`), so a field a cap could
govern cannot exist there. Their finite default satisfies "no unset state" on
paper and changes zero generated bytes in fact.

**A cap being set is not a cap being live.** Liveness is a property of the
*schema*: a backend emits the constant and the guard only where the message
actually reaches an unbounded field of that kind (`ir.Bounds`). That is why a
fully bounded schema still generates no limit plumbing at all.

An unset schema `count`/`maxlen` still means the *message* declares no bound — it
does not mean the receiver has none. The schema bound and the receiver cap answer
different questions (validity vs. capacity), and a field with neither is capped by
the target default rather than by `ARRAY_MAX`. The rules, normative for every
backend:

- The caps govern **only** fields the schema left unbounded. A schema-bounded
  field is governed by its own bound (#100); a field that legitimately needs
  more than the cap gets an explicit schema bound (the escape hatch).
- Exceeding a cap is a decode **error** in the corelib's `LimitExceeded`
  category — a *policy* rejection, deliberately distinct from `INVALID` (the
  bytes may be perfectly well-formed), and **never a clamp** (the #100 lesson:
  silent clamping is data corruption).
- The check runs at the count/length **header**, before any allocation or
  buffering — a claimed oversize fails fast even if the payload never arrives.
- The corelib surfaces what arrives — the count at the header, the index of the
  element in hand — and the number it is measured against always comes from
  generated code. **Which layer performs the comparison is a separate question**,
  answered per field kind and per port below ("Where the comparison runs"): a
  corelib may take the cap as an argument and check it itself, and most now do,
  but none holds, defaults or clamps to a limit of its own.
  A wrapper-sequence array carries no count *header*, but its elements
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
    id + 1* (§5.1), so the element **index is the array length** and therefore the
    amplification vector: two elements at id `0` and id `16383` are a 16384-slot
    container. `max_dyn_array_count` therefore binds the **index** here, not a
    count header: an element whose `id >= max_dyn_array_count` is `LimitExceeded`,
    checked **before** the container grows. Capping the index rather than the
    number of elements delivered is what closes this — a sparse array allocates by
    its highest id, not by how many elements arrived.

    **Landed in generator#387** for the six backends whose gap-fill is generated
    code: **Java**, **Kotlin**, **C#**, **Zig**, **Rust** (std) and **Python** —
    and in **Dart** since, where the gap-fill is the corelib's but the bound is
    not: `corelib-dart`'s collectors all take an `rcap` beside the schema `cap`,
    consulted only where the schema declared no `count:`, so generated code passes
    the deployment's number and the two can never both be in play. It travels
    **unraised**, as its own constant where the raise made one differ: a per-decode
    `DecoderLimits` had to clear the largest schema bound in the message or it
    rejected a field §6.2.1 forbids it to touch, while a per-array cap that cannot
    reach a bounded field needs no such loosening — and raising it would hand an
    unbounded array a looser cap because some sibling declared a large `count:`.
    The
    check goes in the same place as the `count: N` reject of #142, because the two
    are one decision with two answers — the bound (schema `count` vs. the cap) and
    the category (INVALID vs. `LimitExceeded`) — so each backend has ONE helper
    returning both, rather than a second guard beside the first. That the two must
    not be folded together is CORELIB_PLAN §6.2.1: the same bytes decode under a
    looser cap, so an over-index element is a policy rejection and not malformed
    input. A native matrix **row** takes the index cap too, ahead of the row's own
    element count from the A-shape work above — id first, then count, in every
    backend.

    Two points the implementation settled:

    - **Liveness now has a second source.** A cap's state is emitted only where the
      schema actually reaches an unbounded field of its kind, and three backends
      decided that by walking for an unbounded *count* — a native array with no
      `count`, or a matrix row with none. A message whose only unbounded array is a
      string wrapper has no count header anywhere, so nothing was emitted while the
      new guard named it: Java and Kotlin did not emit `MAX_DYN_ARRAY_COUNT`, and
      Zig did not declare the sticky `lim` field. All three now count a `cap < 0`
      wrapper frame as making the array cap live, and thread that per-message
      decision down to the guard, which sits too deep to re-derive it. **Both were
      caught by conformance and by neither unit test** — the generated source did
      not compile, which a codegen assertion on substrings cannot see. C#, Rust and
      Python were already right, deriving liveness from `ir.Bounds`, which marks
      any array without a schema count.
    - **Zig refuses in three different ways** — an error return from `fixlenBegin`,
      the sticky `self.lim` in the payload callback, a break to the dead scope in
      `sequenceBegin` — so its helper returns the *test* and the *category* and
      each site spells its own refusal.

    **Three backends could not take it generator-side at all**, because their
    gap-fill is not generated: **Go**, **Dart** and pure **C++** hand the whole
    wrapper array to a corelib collector, so neither an element's index nor its
    length word ever reaches generated code. The `cap`/`ElemMax` those collectors
    already took mean the *schema* bound and answer `INVALID`, so routing a
    receiver cap through them would have produced the category §6.2.1 forbids.
    Each corelib therefore grew a **second, separate** field for the policy bound —
    `Caps{ArrayCount, StringLen, BlobLen}` beside `Bounds` in corelib-go, `rcap`
    and its element-length sibling in corelib-dart, `indexCap`/`elemLenCap` on
    `sofab::StringSeq`/`BlobSeq` in corelib-cpp — and that is what turned that
    blocker into the split the family now has, rather than a compromise.

**Where the comparison runs (normative).** §6.2.1 separates two questions that had
been treated as one, and the answers are not the same:

- **Provenance — always generated code.** The *numbers* come from the layer that
  knows the schema and the target. A codec **MUST NOT** hold a limit of its own,
  supply a default for one it was not given, read an omitted argument as
  *unlimited*, or clamp to one. A format ceiling (§6.2 `ARRAY_MAX` / `FIXLEN_MAX`)
  reached because no cap was stated is the **format's** bound and **MUST NOT** be
  presented as a receiver cap — reporting `LimitExceeded` against a ceiling nobody
  configured promises a limit to raise that never existed. A **missing** cap is a
  caller defect and belongs in §6.3's `InvalidArgument` category for the same
  reason. The strictest form of that, and the one the corelibs took, is to make the
  argument **required**: a struct literal's zero value is exactly the omission the
  API must not accept.
- **Site — wherever a call already exists to carry it.** §6.2.1 is explicit that a
  corelib **MAY** take a limit as an argument and perform the check itself. Where
  generated code already calls the corelib at the count/length header — the point
  the check must happen at anyway — the number rides that call and the comparison
  folds in beside a bound test already there. Where no such call exists for a field
  kind, the check stays in generated code.

**One implementation, wherever it runs.** A port whose corelib offers the check for
a kind **MUST NOT** also emit it into the generated layer, and vice versa. Two
routes to one rule is the divergence §5.3.1's one-implementation test is written
over, and the receiver caps are named in it.

**Moving the check does not move where it fires.** It stays at the count/length
header, *before* the allocation it exists to prevent, and *behind* the MESSAGE_SPEC
§7.3 tag test — a field whose wire type contradicts the declared one is skipped, and
a skipped field is never capped (§6.2.1). Riding an existing call is what makes both
conditions structural instead of a discipline every call site has to keep: on most
surfaces only the codec can see the tag before the destination is bound, so a caller
checking *in front of* its own read caps exactly the field it was required to skip.
The hazard to watch for is the destination being **sized** before the check runs —
corelib-cpp had that defect hidden behind the generated guard, its free
`readString` calling `fitDest()` on the announced length before any cap was
consulted, so an over-cap string was materialised and only then rejected. The Zig
backend had the same shape at its own layer: the string/blob cap sat in the payload
callback, *behind* the generated reassembly helper that returns nothing until the
whole payload is in hand, so a chunked sender could stream an arbitrarily long
over-cap payload into the accumulator and only the last byte triggered the
rejection. It now rides `fixlenBegin`, the hook the schema `maxlen` already used.

**A truncated over-cap header reports `LimitExceeded`, never `INCOMPLETE`
(normative).** Bytes that declare a count or length above the cap and then end
before the field completes are a **policy rejection**, not a truncation. §6.2.1
puts the check at the count/length header *for the same reason `INVALID` is
decided there* (§5.2.3): the describing bytes have arrived, so the verdict is in,
and a decoder that waits for the payload reaches end-of-input first and reports
the wrong thing. §6.3 then makes the rejection **terminal** — no continuation can
lift it — while `INCOMPLETE` means precisely "more bytes can change this" and is
read by a streaming caller as *feed me the next chunk* (§5.2.4). Answering
`INCOMPLETE` therefore loses the category **and** tells a hostile sender its
connection is still wanted: a 4-byte message holds a receiver open indefinitely,
which is the amplification this whole section exists to close. The rule holds on
the streaming surface too — a crossed cap is surfaced by the `feed` that crossed
it, not deferred to a `finish`. §5.2.3's own precedence is unaffected: a
**structural `INVALID`** still dominates a crossed cap, because those bytes are
malformed regardless of what follows.

The answer is not a matter of taste per port: §6.2.1 says conformance compares
implementations configured with **identical** limits, so two ports disagreeing on
the same bytes under the same numbers is a conformance defect by that sentence
alone. Nine of the eleven ports already answered `LimitExceeded` — everywhere the
guard sits in a callback that can abort the feed (an error return, a raised
exception, an `exceedLimit()` latch), it terminates on the spot and the question
never arises. The two that answered `INCOMPLETE` are the ones whose callback is
infallible and whose generated code therefore has to **choose** an order: Rust std
(all three kinds, `try_decode` surfaced `fed?` ahead of the sticky `lim`) and Zig
(string/blob only, via the reassembly hazard above). Both now report the cap
first. Where a port has an existing conformance case for the schema-bound twin
("over-count + truncation must be INVALID, not INCOMPLETE", generator#216/#267),
the cap case belongs beside it — it is the same decision one category over.

**The split is per field kind AND per port, deliberate, and this is the list.**
"In the corelib" below always means *passed in by generated code, compared there*;
never a number the corelib knows:

| target | compared in the corelib, on this existing call | compared in generated code |
|---|---|---|
| **C++** (`corelib: cpp`) | all three kinds: `readString`/`readBlob`/`readArray`, plus `indexCap`/`elemLenCap` on the `StringSeq`/`BlobSeq` collectors | — |
| **Zig** | array counts and wrapper element indices: `arrays.allocNCapped` / `growCapped` / `setElemCapped` | string and blob lengths |
| **Go**, **Dart** | wrapper arrays — the element index and the element length — through the collectors' receiver-cap fields | scalar string/blob lengths, native array counts |
| **Java**, **Kotlin**, **C#** | string and blob lengths, in `PayloadAcc`, which the payload already passes through | array counts and wrapper element indices |
| **Python** | all three kinds, in the corelib's own header walk: `Decoder(max_dyn_*=…)` takes the three numbers as **required** arguments and the schema bounds are *declared* to it (`on_schema_bound`, or a destination map's entry), so a bounded field is never capped — §9.5.1 | — |
| **TypeScript** | a wrapper array's element index and element length, on the `StringSeq`/`BlobSeq` collectors that receive those two headers instead of the visitor (`receiverCap`/`receiverElemMax`) | the other kinds, in the flat visitor's own `fixlenBegin`/`arrayBegin` — the hooks that already carry the schema bound, so the cap is its `else`; corelib-ts is handed no limits object at all |
| **Rust** (std) | — | all three kinds (§9.5.2: there is no call to hang a cap on, and the collector shape the others use is two overlapping mutable borrows) |
| **C**, **C++** `corelib: c-cpp`, **Rust** `no_std` | inert — the profile rejects an unbounded field at schema validation, so no field a cap could govern exists | inert |

**Which halves have landed.** The corelib side is done in every port that takes an
argument (corelib-cpp, -go, -dart, -zig, -java, -kotlin-mp, -cs, -py, -ts): each
made the cap a **required** parameter and holds none of its own. The generator side
lands per port, one PR each, in the generator#402 series — so a port whose PR is
still open keeps its previous shape until it merges, and the `-unbounded` bench rows
below are where that shows.

A deliberate split *within* one port is the normal case, not an anomaly: Zig's
arrays go one way and its strings the other because `arrays.allocNCapped` exists at
the array's count header and nothing equivalent exists at a string's length word.
Inventing a new helper purely to hold a check is what this design rejects — it
costs 1–2.5 percentage points of decode and buys nothing that the guard in place
did not already give.

**Measured**, callgrind Ir/op on the decode path, identical generated source and one
corelib build per pair. Each pair was taken in its own port's PR rather than from
`results.txt`, because until the `-unbounded` rows below existed the committed file
could not see any of it:

| port | shape | decode |
|---|---|---:|
| C++ | guard emitted in front of `readString` | +3.39% |
| C++ | the cap passed into `readString` | **+0.11%** |
| Zig | generated guards | +5.10% |
| Zig | the cap on `arrays.allocN` / `grow` / `setElem` | **+1.00%** |
| Go | decoder-level `WithMax*` options | +3.14% |
| Go | the cap as a per-field collector argument | **~0%** |
| Rust std | generated inline guard (the §9.5.2 exemption) | +0.77% |

The pattern is the same everywhere: the cost is not the comparison, it is the
*place*. A check bolted in front of a call the code already makes duplicates that
call's setup and defeats its inlining; the same check inside it is a compare
against a register the callee already holds.

**The raise is gone, so a configured cap is binding rather than advisory.** This is
the user-visible half of the change. A decoder-level cap — Go's `WithMax*` options,
Dart's and TypeScript's module-wide limits object — was tested against *every*
field, including the schema-bounded ones §6.2.1 forbids it to touch. Keeping those
decodable meant raising the number generated code passed to at least the largest
schema bound of its kind in the message, which loosened it for exactly the
unbounded fields it existed to protect: a `maxlen: 4096` element in the module
lifted a configured 1024-byte string cap for every unbounded string beside it, and
`max_dyn_array_count: 64` could not coexist with a sibling's `count: 100000` at
all. Per-field enforcement needs no such loosening, so **every cap is now emitted as
configured**. `ir.Bounds.Max*` survives only as an input to the worst-case size walk
(§9.6), not as a floor under any cap.

**And it is measured rather than argued.** The bench schema
(`vehicle_telemetry`) bounds every array and every string/blob, so no row measured
on it emits a single `max_dyn_*` constant, cap argument or guard: for the whole life
of this section its cost was invisible to `tests/bench/results.txt` in both
directions. Four `-unbounded` rows — `cpp-cpp-unbounded`, `zig-unbounded`,
`go-unbounded`, `rust-rs-unbounded`, one per enforcement shape above — measure a
deliberately unbounded schema instead (§15).

Pure C++ additionally derives a
streaming reassembly cap (`sofab::Limits{max_buffered_field}`) for its `acc_`
buffer — the largest **byte span** one top-level field can legitimately reach, from
the same worst-case walk that sizes `_maxSize` with the configured caps standing in
for the missing schema bounds, so no message the per-field guards accept can trip
it. A count is an element count, never a byte budget: deriving the cap from counts
rejected a valid at-cap array — and even a fully schema-bounded wrapper array —
through `try_decode` where a bare `feed` accepted it (#228). A field left both
unbounded and uncapped yields no cap at all rather than one that would reject valid
traffic. **Statically bounded profiles** (C, C++ `corelib: c-cpp`,
Rust `no_std`) are capacity-bound by construction — the keys are inert.

**Allocation shape: check first, then allocate once.** No generated decoder may
allocate from an untrusted wire count *before* checking it, and an over-cap count
is `LimitExceeded` — never a clamp to the cap, which is the #100 lesson again.
What follows the check depends on whether the size is known up front, and only two
shapes are conformant:

- **A — the size arrives before the payload** (native integer arrays §4.7, fixlen
  arrays §4.8, and `string`/`blob` lengths). Check the count/length against the
  schema bound or the cap, then allocate **exactly that count**, once. No growth,
  no reserve-and-extend: the wire already said how big it is, and the cap already
  bounded what that may be.
- **B — the size is only known at the end** (sequence/wrapper arrays, whose length
  is *highest present id + 1*). There is no count to allocate from and no second
  pass available in a streaming decode, so the container **grows** as elements
  arrive. Growth is bounded by construction: the index check above rejects
  `id >= max_dyn_array_count` before the grow. Grow geometrically to at least
  `id + 1` rather than exactly `id + 1`, so a sparse array does not cost O(n²)
  copies.

Growth belongs to **B alone**. Its earlier appearance in the A shape (#96/#98 —
Java's lazy array growth, merged 2026-07-08) was a heap-exhaustion mitigation
written the day *before* the config caps of #102 existed, and the caps replaced the
need for it: once an untrusted count is checked against a finite bound before the
allocation, allocating it exactly is both safe and cheaper than growing into it.
Statically bounded profiles are unaffected — they were never growing.

**The A shape landed in generator#386**, in the four backends that had copied the
#96/#98 pattern: **Java**, **Kotlin**, **C#** and **Zig**. Each reserved a small
fixed capacity (`Seq.ARRAY_INIT_CAP` / `Seq.ArrayInitCap` = 16,
`sofab.arrays.ARRAY_INIT_CAP` = 1024) and grew toward the announced count as the
elements arrived; each now allocates the checked count exactly, and the per-element
store is a plain indexed write with no growth call, no per-element reference store
back into the message object, and no per-array growth ceiling to carry. Rust and Go
were already in this shape. Three things are worth recording about the conversion:

- **The A/B split is decided by the wire, not by the schema.** A native array
  *without* a schema `count` still has a wire count **header**, so it is shape A —
  the caps are what bound it, and the earlier wording ("count-less array arms")
  read as if it were B. Only a wrapper/sequence array, which has no count header at
  all, is B.
- **A row's own element count was unbounded in all four.** The outer array's
  `count: N` bounds a native matrix row's **id**; nothing bounded how many elements
  the row itself claimed unless the row was schema-unbounded and picked up the cap
  by accident. A row now takes both verdicts, id first, then count: INVALID against
  the inner schema `count`, `LimitExceeded` against the cap. This is not a cost
  change — it is the §7.1 bound the row was missing, and it is what makes the row's
  exact sizing safe.
- **Java and Kotlin's bulk offer widened as a consequence.** `Visitor.arrayBulk`
  needs a destination sized to `count` up front, which is why it was restricted to
  schema-bounded arrays: theirs was the only count that had been checked. Every
  native integer array is now sized that way, so the offer reaches the unbounded
  ones too — the untrusted-count objection is answered by the check rather than by
  the reservation.

One call site survives the conversion as a no-op: Zig's dynamic slices still store
through `sofab.arrays.putGrowing`, whose growth branch is now unreachable (the
slice is already `count` long, so `i >= s.len` implies `i >= n` and the helper
returns on its first line). Reducing it to the bounded put it has become is
corelib-zig's half, filed as corelib-zig#67.

#### 9.5.1 Python: declaring the schema bound, and what the declaration costs

corelib-py owns the schema-bound rule in one place (`Decoder._settle_bound`) and
reaches it from two routes: a destination map's entry, and
`Visitor.on_schema_bound(field_id, n) -> int`, asked at the count/length header
for a `string`, a `blob` or an array the handler accepted. Returning `n >= 0`
does two things — a wire count/length above it is INVALID (§7.1), and the
receiver-side cap stops applying to the field (§6.2.1). The `python` backend
declares through the hook (generator#406) instead of carrying a generated copy of
the comparison.

**The §7.3 tag test cannot go with it.** The hook is told the id and the
announced count/length and nothing else, so it cannot tell this field's value
from a header that merely reuses its id with a contradicting wire type — and
MESSAGE_SPEC §7.3 is explicit that against a schema bound the skip wins ("the
subtype is therefore decided first and the schema bound applied only to a field
that survives it"). Generated code therefore keeps the tag test one hook earlier,
in `on_field`, and **declines** a mis-tagged field outright. Without that, a
40-byte `blob` arriving at the id of a `string, maxlen: 32` is reported
`SofaDecodeError` where the spec says skip. The map route needs no such guard:
the corelib runs the tag test ahead of a table entry itself.

**Measured, and it is a loss.** Ir/op for `decode`, `tests/bench` row
`python-native`, `vehicle_telemetry`, one corelib-py build:

| generated handler shape | Ir/op | vs before |
|---|---:|---:|
| bound chain in `on_field`, count in `on_array_begin` | 522,503 | — |
| bound declared in `on_schema_bound`, tag test in `on_field` | 574,968 | **+10.0%** |
| *(the same without the §7.3 tag test — not conformant)* | 565,054 | +8.1% |

The cost is one **Python call per aggregate field** (+37,397 Ir on its own,
~1,300 Ir per call), which the two-integer signature reduces but cannot remove;
the attribute reads it replaces are worth a tenth of that, and the §7.3 tag test
is worth 9,914. `on_array_begin`'s count check was only 2,105 Ir to begin with.
The change is kept for what it fixes, not for what it costs: the §6.2.1 defect
above, and one implementation of the rule instead of two.

**The shape that would pay is the table.** A destination map declares its bounds
**once**, at construction, so no hook is called per field *and* the corelib's own
§7.3 tag test sits ahead of the bound — both problems above disappear together.
Adopting it means keying the flat visitor's `(location, id)` dispatch onto
corelib-py's `Binding`, which is a larger change than this one and is not in
generator#388's path either. Until then Python pays the call.

#### 9.5.2 Rust: why the cap stays in the generated visitor

CORELIB_PLAN §6.2.1 (doc PR #86) settles that a corelib **MAY** take a receiver cap
as an argument and run the comparison itself, and that the cheap way to do it is to
hang the number on a call generated code already makes — the compare then folds in
beside a bound test already there. Every other target has such a call: C++
`readString`/`readArray`, Zig `arrays.allocN`/`grow`/`setElem`, Go's, Dart's and
TypeScript's collectors, Java's, Kotlin's and C#'s `PayloadAcc`, Python's decode
entry — see the table in §9.5 for which kinds each one carries.

**Rust std has none, and cannot be given one by adding a parameter.** The generated
visitor holds the whole message mutably:

```rust
struct V<'a> { m: &'a mut VehicleTelemetry, acc: sofab::PayloadAcc, lim: bool, ... }
```

The collector shape Go and Dart use hands the corelib the *destination* —
`&sofab.StringSeq{Out: &m.Warnings, RCap: ...}`. Its Rust equivalent would be
`&mut self.m.warnings` alongside the `&mut self.m` the visitor already holds: two
overlapping mutable borrows, which the language rejects. That is an aliasing rule,
not an omission, and it is why corelib-rs has no collectors, no `Seq` types and no
allocation helpers — its entire decode surface is `decode(buf, visitor)` and
`feed(chunk, visitor)`. The one corelib call on the path, `acc.feed(total, offset,
chunk)`, sits *after* the point a cap must fire at; moving the check there would
buffer the payload first and reject it after, which is the eager-allocation §6.2.1
exists to prevent.

**The one route that stays open is not worth taking.** Limits handed in per decode,
with the corelib checking in its own header walk, borrows nothing and would work —
but the corelib knows no schema, so it would cap schema-bounded fields too unless a
`schema_bound` callback is added for it to consult. That is exactly the shape of
Go's retired `WithMax*` options: a global bound tested against every field, with a
dynamic call on breach. Measured, that shape costs **+3.14%** of decode in Go
against **+0.77%** for Rust's present inline guard — so the move would very likely
make Rust slower while adding a corelib feature and a generated bound table. (The
3.14% is measured in Go, not Rust; the transfer is reasoned, and no Rust
measurement was taken.)

**Rust std is therefore exempt**: its `max_dyn_*` guards stay in the generated
visitor, per field, at the count/length header, ahead of any accumulation. This
concedes nothing to §6.2.1 — the numbers still come from generated code and
corelib-rs holds no limit of its own, which is the rule the section is about, and
it **MUST NOT** gain one. `rs-no-std` is out of scope for a different reason: its
profile rejects an unbounded field at schema-validation time, so no field a cap
could govern exists there.

**An exemption is a claim about where the guard sits, so conformance pins where it
sits.** The other ports inherit the placement from the corelib call they hang the
cap on; Rust's is generated, and only a decode of real bytes can show it is in the
right place. `tests/conformance/rust/run.sh` builds one project with all three
`max_dyn_*` keys set and asserts, per hand-built message:

* the cap fires on each unbounded kind and answers **`LimitExceeded`** — the §6.3
  category for a policy refusal of well-formed bytes — while a value at the cap
  still decodes;
* a **MESSAGE_SPEC §7.3 skip is never capped** (generator#410): a signed array at
  an unsigned-declared id, a blob at a string id, a string at a blob id, an
  unknown id, and the wrapper-array twins of all four. Every row is over its
  kind's cap and every row must decode **`COMPLETE`** with the declared field left
  at its default — a receiver that refuses a message whose only offence is a field
  it was never going to read is the defect the rule exists to prevent;
* a **schema-bounded field is governed by its own bound alone**, and the schema
  bound is deliberately set *above* the cap so the two cannot be confused:
  `maxlen: 32` under a string cap of 8 accepts 20 bytes and answers `InvalidMsg`
  at 40, `count: 6` under an array cap of 4 accepts 5 elements and rejects 7 the
  same way. Neither ever answers the cap's category, which is the precision a
  decoder-level cap cannot have (§6.2.1) and the reason §9.5's second family is
  being retired.

All of it passed unchanged — the rows were written against the backend, not for a
fix — and the reason is structural: every guard is a match arm keyed by `(wire
callback, location, id)`, so a field the dispatch skips reaches no arm at all. The
arms are generated, though, and one widened to `_` would cap a skip silently with
no unit test able to see it. That is what the rows are for.

### 9.6 Worst-case message size (one walk, all backends)

Most targets emit a `MAX_SIZE` constant and size their encode buffer from it.
**That number is a property of the schema, not of the target.** The wire format
is language-agnostic — the same definition encodes to the same bytes everywhere —
so the walk that computes it lives **once**, in `internal/ir/wiresize.go`
(`ir.MaxWireSize`), alongside `ir.Bounds`. A backend must not compute it itself.

This is a correction, not a preference. Seven backends previously each carried
their own copy and they disagreed with each other:

| defect | scope | effect |
|---|---|---|
| every integer charged the full 64-bit varint width | 6 of 7 | a `u32` cost 10 bytes instead of 5 — 82 where the answer is 49 |
| a surplus framing byte per wrapper array | 7 of 7 | the field header **is** the `sequence_begin`; only the terminator is extra |
| the `fp32`/`fp64` array `fixlen_word` uncharged | C | 1 byte short per fixlen array |
| an array without `count` charged **zero** payload | Rust | a schema-unbounded `Vec<u32>` reported as bounded, at 2 bytes |

Only the last is unsafe, and only one backend had it — but no test could see any
of them, because each backend was only ever compared against itself.

**Every per-type maximum is exact; nothing is estimated.** `bool` 1, `u8`/`i8` 2,
`u16`/`i16` 3, `u32`/`i32`/`enum` 5, `u64`/`i64`/`bitfield` 10 (signed kinds are
ZigZag-mapped onto their unsigned peer's width, so they cost the same); `fp32`
1+4, `fp64` 1+8; `string`/`blob` the fixlen word plus `maxlen`. Plus each field's
`(id<<3)|wiretype` header, an array's element-count varint, a sequence's
one-byte terminator, and one `fixlen_word` per `fp32`/`fp64` array.

**Encode size and decode span are different questions.** A second entry point,
`ir.MaxFieldDecodeSpan`, sizes a *reassembly window* and differs in two ways
forced by what a decoder must tolerate:

- it charges every varint its **widest** form (10 bytes) regardless of declared
  type, because CORELIB_PLAN §4.1 obliges a decoder to accept a non-minimal
  encoding — a conformant peer may legally pad a `u32` to ten bytes;
- it substitutes the receiver-side `max_dyn_*` caps (§9.5) for missing schema
  bounds, because those caps are exactly what this peer will accept.

Those caps must **never** reach the encode walk: a limit on what I accept says
nothing about what I may legitimately send, and sizing an encode buffer by it
would refuse a message the schema permits.

**When the schema cannot bound a message** — an array without `count`, a
string/blob without `maxlen` — there is no worst case, and the walk says so
rather than inventing one. The `max_message_size` config key then supplies a
ceiling, and generated code names the distinction so a reader can see which kind
of number they have:

| case | emitted |
|---|---|
| schema-bounded | `MAX_SIZE = <computed>` |
| schema-bounded, `max_message_size` set | `MAX_SIZE = <computed>`; generation **fails** if it exceeds the limit |
| unbounded | `MAX_SIZE_LIMIT = <limit>`, `MAX_SIZE = MAX_SIZE_LIMIT` |
| unbounded, statically-bounded target | rejected at generate time (C/`c-cpp`/`no_std` already reject unbounded fields) |

The explicit-limit check is the more useful half: a message that cannot fit the
target transport is caught while generating, not on the device. The default
(4096) never triggers it — it only fills the unbounded case.

**Why the number matters: the corelib never allocates.** CORELIB_PLAN §5.1 makes
every output buffer the **caller's**, and a corelib neither allocates nor grows
one. Generated code is that caller, so the allocation belongs in generated code —
and it needs a size, which is what `MAX_SIZE` supplies. The two cases therefore
produce two different generated shapes, and conflating them is a truncation bug:

- **bounded** — one exactly-sized buffer holds the whole message, handed to the
  corelib's buffer constructor (Rust `OStream::new`, Java `new OStream(buf)`, Go
  `sofab.NewEncoderBuffer`, Python `Encoder.over_buffer(buf, 0)`, TypeScript
  `new OStream(buf)`, Dart `Encoder.overBuffer(buf)`, Kotlin `OStream(buf)`).
  A value the caller filled past its own declared bound does not fit, and is **reported** (buffer-full) rather than emitted short —
  §5.1 forbids returning partial output as if it were complete.
- **unbounded** — `MAX_SIZE` is an imposed ceiling, so it must not size a buffer:
  a message above it is legal and would be silently refused. The shape is a fixed
  caller scratch plus a flush sink draining into caller-owned storage (Rust
  `OStream::with_flush`, Go `sofab.NewEncoderSink`, Python
  `Encoder.over_buffer(scratch, 0, sink)`, TypeScript
  `new OStream(scratch, 0, sink)`, Dart `Encoder(sink, buffer: scratch)`, Kotlin
  `OStream(scratch, 0, FlushSink { … })`), which bounds memory by the scratch
  instead of by the message.

A streaming entry point (`EncodeTo(writer)` and peers) is the sink shape for both
cases, the writer being the drain. Where the drain only *copies* what it is handed
— `io.Writer.Write` may not retain it — the sink returns without handing a
replacement buffer back, which §5.1 spells out as the take-vs-copy distinction.

One capability is worth naming because moving a target onto caller-owned buffers
can cost it: §5.1's optional **pass-through of a divisible run**, where a payload
at least as long as the buffer is handed to the sink directly instead of being
copied through. It is opt-in *at installation* ("the caller has granted it"), so
a corelib whose caller-owned constructor takes no permission parameter cannot
offer it to generated code at all — corelib-py, corelib-ts and corelib-dart are
in that position today, which costs a large single field a copy it used to avoid
on the library-allocated path.
§5.1 permits a port to always copy, so this is a corelib capability gap and never
a conformance one; a target noticing it should report it rather than keep the
library-allocated shape.

**The other direction: a decoded field owns its bytes.** The same rule read on the
decode side settles what a generated destination may store. A corelib that hands a
`string`/`blob` payload over as a *view* into the input buffer is doing its job —
it allocated nothing — but a destination that keeps that view makes the message's
lifetime the buffer's, silently. So a generated destination **copies** into
storage the message owns, on every target whose corelib offers a borrowing read
(Go's `AcceptBytes` cursor is the clearest case). Faster borrowing modes are
deliberately not offered and not configurable: the complexity of a second lifetime
regime is not worth what it buys. One target still diverges: Zig's `decode()`
borrows strings/blobs out of the input buffer into a message the *caller* declared
(§10), so there the lifetime is at least visible in the caller's own code rather
than hidden — and its streaming path copies regardless, because a payload stitched
across a chunk boundary completes inside corelib memory. Bringing it in line is a
separate change to that backend.

**Verification.** `tests/matrix/maxsize_test.go` requires every target to emit
the *same* number and to match `ir.MaxWireSize` — the guard none of the seven
copies ever had. `tests/conformance/c/maxsize_fill.{yaml,c}` (and the shared
`tests/conformance/lib/maxsize_fill.sh` its peers reuse) closes the loop
against reality: a message with one field per wire shape, every bound exhausted
and every varint at its widest, must encode to **exactly** `MAX_SIZE`. Too small
means a legal message does not fit its own buffer; too large means every
fixed-buffer target wastes that RAM silently. The wrapper-array surplus byte
above was found by that check on its first run.

---

## 10. Per-language backend reference

| Lang | Corelib(s) | Decode model | Notes |
|---|---|---|---|
| **C** | `corelib-c-cpp` | descriptor-table callback | `object.h` struct + static descriptor; `symbol_prefix`; auto capability + API-version guards; analytic `MAX_SIZE`; project mode also emits `Makefile` + `CMakeLists.txt`, `run.sh`, and a devcontainer. |
| **C++** | `corelib-cpp` (default) / `corelib-c-cpp` (`corelib: c-cpp`) | child-visitor / flat-visitor wrapper | header-only `OStreamMessage`+`IStreamMessage`; decode on the `cpp` leg goes through the `sofab::read`/`readString`/`readBlob`/`readArray` **helper layer**, which sizes the destination the codec then fills (§6.6, §9.3 family 4); a boolean array's element is `std::uint8_t` and an enum array binds through `sofabgen::RawArray` on **both** legs, because neither a deferred nor a resumable decode may land in a temporary; `c-cpp` decode pre-sizes varlen fields + links the C sources. |
| **Rust** | `corelib-rs` (default) / `corelib-rs-no-std` (`corelib: rs-no-std`) | flat-visitor location-stack | std (throughput, no features) vs no_std (feature-gated, footprint); feature-clean codegen. Field storage is a SEPARATE axis from the environment: `allow_dynamic` selects `String`/`Vec` or fixed-capacity `heapless` on **either** corelib, so a std crate can hold its bounded fields inline while keeping serde, the heap decode stack and the ordinary std prelude. String/blob chunk reassembly is the corelib's — `sofab::PayloadAcc`, and `PayloadAcc<MAX_SIZE>` on no_std where the storage is the caller's and an over-long split payload is `Error::BufferFull` (generator#345). |
| **Go** | `corelib-go` | push child-visitor | struct implements `sofab.Visitor`; exported `Serialize(*sofab.Encoder)` + `EncodeTo(io.Writer)` — generated code owns every encode buffer (§5.1): `Encode` allocates one exactly-sized `<Msg>MaxSize` buffer (`NewEncoderBuffer`) for a bounded schema and drains a fixed 512-byte scratch into a caller `[]byte` (`NewEncoderSink`) for an unbounded one, and `EncodeTo` drains that same scratch into the writer, so a message never exists as one contiguous `[]byte`; ONE decode surface (§5.3.1) since corelib-go#130 retired the pull `Decoder` — `Decode<Msg>` via `sofab.AcceptBytes` for bytes in hand and `Decode<Msg>From(io.Reader)` via `sofab.NewDecoder(m).FeedFrom(r, scratch)` for a byte stream are both one resumable `Feed`, so the chunked and one-shot paths cannot drift (memory bounded by the 4 KiB scratch plus the largest single field, §5.6); **no generated `feed()`** yet, though the corelib now has the push decoder one needs; the codec builds NO aggregate (§6.6.3), so a string/blob arrives in pieces (`String(id, total, offset, chunk)`) and is assembled by a `sofab.PayloadAcc` the object carries — one per object, since a fixlen payload is contiguous and two fields are never in flight at once — while a native array arrives as `ArrayBegin` / one `Array*(id, index, v)` per element / `ArrayEnd`, with the destination opened at the header (a repeated id REPLACES, §7.4) and each element width-checked as it lands (§5.2: an over-width element behind a truncation stays INVALID); every generated destination COPIES, so a decoded message owns its bytes and outlives the input buffer; the object embeds `sofab.StringCheck`, so `WithStrictUTF8` reaches the destination's UTF-8 check (§6.4) rather than only the build tag; `BeginSequence` descends into nested objects / array collectors; canonical-JSON tags. |
| **Python** | `corelib-py` | pull-parser | dataclasses + `serialize`/`deserialize` (public since generator#239; `deserialize(Decoder(reader))` IS the chunk-capable path — the corelib pulls from any reader); generated code owns every encode buffer (§5.1): `encode()` allocates one exactly-sized `MAX_SIZE` `bytearray` (`Encoder.over_buffer(buf, 0)`) for a bounded schema and drains a fixed 512-byte scratch into a caller list (`Encoder.over_buffer(scratch, 0, out.append)`) for an unbounded one, so `Encoder()`/`getvalue()` — the corelib installing and growing its own storage — is gone from generated code; every decoded field COPIES, so a decoded message outlives the input buffer. |
| **TypeScript** | `corelib-ts` | flat visitor + static scope map | classes + `serialize(os)` plus `encode()` — generated code owns every encode buffer (§5.1): `encode()` allocates one exactly-sized `MAX_SIZE` `Uint8Array` (`new OStream(buf)`) for a bounded schema and drains a fixed 512-byte scratch into a caller list for an unbounded one, where the sink is handed the INSTALLED buffer plus the region's coordinates (`(_b, _s, _e) => _out.push(_b.slice(_s, _e))`, §5.1.6 — a `subarray` would be an allocation per flush) and the corelib's own `growingOStream()`, which owns and doubles a slab of its own, is emitted nowhere; ONE decode surface (§5.3.1): `decode(bytes)` runs the corelib's `decode` against a per-type flat `Visitor`, and `decoder()` → `feed`/`finish` drives the very same visitor over the resumable `IStream`, so the chunked and one-shot paths cannot drift; dispatch keys on `(location, id)` with the parent restored from a static `switch` rather than a stack (the scopes of a type form a tree, and a declined subtree fires no `sequenceEnd`); the schema-free half is the corelib's — `PayloadAcc`, `StringSeq`/`BlobSeq`, `decodeUtf8` and `elementsEqual` are called, not re-emitted (#345, on corelib-ts#151), and the `_ObjSeq`/`_MatSeq`/`_RowSeq` collectors the withdrawn child-visitor shape needed are gone; 64-bit → `bigint` by default, `int64: long`/`number` backs u64/i64 arrays with corelib `Long[]` accessors — and the scalars too, as `Long` under `long` or `number` under `number` — built from the hooks' `lo`/`hi` wire halves (`Long.fromBits`), so the hot path materialises no `bigint` and needs no opt-in channel (corelib-ts#161 withdrew `Visitor.longs`, whose per-schema trade #344 had to guess); a narrow destination reads the hook value as a `number` with no conversion call, the declared-width guard on the next line being what makes that assertion true; alloc-free `writeString`; a `number` is a 64-bit double, so an fp32 NaN keeps the hook's 32-bit wire word in a `Uint8Array \| null` companion, captured only for a NaN, to preserve a signaling NaN bit-for-bit (§4.6, #235); `recode` harness mode (wire → object → wire) exercises it; the receiver-side `max_dyn_*` caps are applied by the generated visitor, per field, at each field's own count/length header (§9.5, #388), and a wrapper array's element index and element length — the two headers the corelib's `StringSeq`/`BlobSeq` receives instead of the visitor — take theirs as the collector's own `receiverCap`/`receiverElemMax` arguments (#405), so the corelib is handed no `DecodeLimits` at all; every decoded value is COPIED out of the fed chunk (§6.7), so a message owns its bytes and outlives the input. |
| **C#** | `corelib-cs` | flat-visitor location-stack (`IVisitor`) | classes + `Serialize`/`EncodeTo`; nested `Msg.Decoder` (constructed with `new`, not a `Decoder()` factory — C# puts nested types and members in one declaration space) → `Feed`/`Finish` for chunked decode; `TryDecode(data, out msg)` returns the §7 `DecodeStatus` (#105); System.Text.Json harness. |
| **Java** | `corelib-java` (Maven) | flat-visitor location-stack | one public class per file (`<Message>.java`, one `<Type>.java` per struct/union) — schema types are public like every other target's, and a type reached from two messages is emitted once (#305); no support file beside them: `Seq`, `PayloadAcc`, `Utf8.decode`, `Sofab.invalid` and `OStream.overScratch` are corelib API (corelib-java#97 / #345); classes + `serialize`/`encodeTo`; nested `Msg.Decoder` via `decoder()` → `feed`/`finish` for chunked decode (`finish` throws `IllegalStateException`, not `SofabException`: `SofabError` has no INCOMPLETE, and an incomplete message is not a malformed one); ints → `long` (u64 via `toUnsignedString`); `tryDecode(data, out)` returns the §7 `DecodeStatus` (#105); Gson harness. |
| **Kotlin** | `corelib-kotlin-mp` (Gradle/Maven Central) | flat-visitor location-stack | Kotlin Multiplatform: the emitted message sources are plain `commonMain` (stdlib + `sofab`, no JVM API), so one source set compiles for the JVM, Node/browser and native, and only the `emit: project` scaffolding is JVM-specific. One file per declaration (`<Message>.kt` + the internal `<Message>Visitor`, one `<Type>.kt` per struct/union) and no support file of its own -- element placement, array growth, payload reassembly and UTF-8 materialisation are the corelib's `Seq`/`PayloadAcc`/`Utf8` (#345); classes + `serialize`/`encodeTo`/`encode()`; nested `Msg.Decoder` via `decoder()` -> `feed`/`finish`. Integers map to their EXACT declared width, unsigned included (`u8` is a `UByte`, `u8[]` a `UByteArray`) -- the C# position, not Java's widen-to-`long`, since Java's reason for widening does not apply. What is Kotlin-specific is that this costs nothing at the corelib boundary: the unsigned arrays are inline classes over their signed peers, so `asIntArray()` is a reinterpretation and the field's own backing array reaches `writeArrayUnsigned`, while the `arrayBulk` offer hands that same view over as the destination, whose element width IS the declared width (§7.1 checked in the pass that decodes). `enum` -> `Int` and `bitfield` -> `ULong`, the widths that cannot lose a legal value, with the declared members emitted as documented named constants in an `object` beside the field -- so per-constant metadata is rendered where C and Java have no symbol for it. `boolean[]` is a `BooleanArray` (no native array boxes). Keyword field names are BACKTICK-escaped, never mangled; a name colliding with a generated member is mangled instead. `tryDecode(data, out)` returns the §7 `DecodeStatus` and `decode(bytes)` is STRICT about both non-COMPLETE outcomes (`IllegalStateException` on a terminal INCOMPLETE, deliberately not `SofabException`). Guards throw the corelib's `SofabException` unwrapped -- Kotlin has no checked exceptions. Hand-written JSON harness (exact u64 from the literal text). |
| **Zig** | `corelib-zig` | flat-visitor location-stack (comptime duck-typed) | structs with schema defaults in the declaration + `serialize`; `decoder(out, alloc)` → `feed`/`finish` (the destination is the CALLER's: Zig moves structs by value, so a decoder owning its message would dangle its own visitor pointer); zero-copy `decode()` (strings/blobs borrow the input buffer, arrays from a caller allocator) — but the STREAMING path borrows nothing: `feed` copies every string/blob into `alloc`, because a payload stitched across a chunk boundary completes inside the corelib's reused carry buffer and is delivered as a slice into the decoder itself, indistinguishable in the callback from one into the caller's chunk (generator#295); fixed `[N]T` for counted native arrays; hand-rolled JSON harness (exact u64). |
| **Dart** | `corelib-dart` | push child-visitor (`MessageVisitor`) | classes with per-field defaults + `serialize`/`encodeTo`/`encode()` — generated code owns every encode buffer (§5.1): `encode()` allocates one exactly-sized `maxSize` `Uint8List` (`Encoder.overBuffer(buf)`, returning the `written` view over it) for a bounded schema and drains a fixed 512-byte scratch into a caller `BytesBuilder(copy: true)` (`Encoder(sink, buffer: scratch)`) for an unbounded one; the corelib's `Encoder.encodeToBytes` — the one place that package allocates output storage — is emitted nowhere; `decoder(out)` → `feed`/`finish` for chunked decode (`finish` returns `null` rather than throwing — this backend's decode path is deliberately exception-free; the corelib reassembles split payloads into storage of its own, so nothing is borrowed from a fed chunk); `onSequenceStart(id)` returns a child visitor (nested object / array collector), native arrays arrive whole via `on*Array` (S7.3/S7.4 structural, like Go); `int` is 64-bit so a u64 >= 2^63 is emitted as its signed/hex bit pattern; a `double` is 64-bit so an fp32 NaN routes through the corelib raw-bits API (`onFp32Bits`/`writeFp32Bits` with a companion `int?` slot for a scalar, a bit-exact `Float32List` copy for an array) to preserve a signaling NaN bit-for-bit (§4.6, #226); `tryDecode` -> `DecodeStatus` (INVALID rides a sticky flag; `decode` is the best-effort convenience); the corelib hands a string/blob over as a view into the decode buffer on the ONE-SHOT path (it allocates the container itself for an array on either path, and reassembles a split payload while streaming), but every generated destination COPIES (`Uint8List.fromList`, `sofab.decodeUtf8Strict`), so a decoded message owns its bytes and outlives the input buffer regardless of which side allocated; the schema-free half of the emitted prelude is the corelib's (`sofab.VisitorBase`, `sofab.elementsEqual`, `sofab.decodeUtf8Strict`, `sofab.utf8Length` — §8, #345); JSON harness carries u64 as a string. |
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
`Deprecated:` paragraph (Go), a Sphinx `.. deprecated::` directive (Python), a
`/// Deprecated.` note (Zig), and `@Deprecated("…")` (Kotlin). Because the
generated encode/decode still touches a deprecated field, C/C++/C#/Rust/Kotlin locally suppress the resulting self-use
warning so generated code stays warning-clean. **C and Java lower enum/bitfield
fields to a raw integer** and emit no named constants, so they carry only the
field-level metadata above. **Kotlin lowers them to a raw integer too and still
carries the constants**: the field stays an `Int`/`ULong` so an undeclared wire
value survives a decode, while the declared members are emitted as documented
`const val`s in an `object` beside it — a closed `enum class` could not represent
the first and would have been the only reason to give up the second. The `docs`
target renders the same metadata as HTML page content
(dedicated Unit column, `deprecated` badge). Both corelib variants of C++
(`cpp`/`c-cpp`) and Rust (`rs`/`rs-no-std`) render metadata identically.

---

## 11. Cross-cutting design decisions

- **`count` is a capacity, so the wire carries the length** (MESSAGE_SPEC §3,
  documentation#29 `af536c4`; supersedes the trailing-default-run rule of
  documentation#18 / generator#136 / Crucible F-0010). A field declared
  `count: N` may carry **`0 .. N` elements**. `N` is the maximum, it **never
  appears on the wire**, and it exists so a heap-less target can pre-size a
  buffer and so an over-long array is `INVALID`. The wire count `M` **is** the
  array's length.
  - **Encode.** Every element the value holds is written, trailing element
    defaults included — `[1,2,0,0]` in a `count: 4` u32 field is four elements,
    not two, because `[1,2,0,0]` and `[1,2]` are different values. There is no
    trailing-run elision. The ordinary §2 whole-field ≠-default test still
    applies: an array equal to the field's declared `default` (the empty
    collection when none is declared, compared element-wise, **never padded to
    `N`**) is omitted entirely.
  - **Decode.** The decoder materializes exactly the `M` elements it received.
    There is **no fill to `N`**. `M > N` is `INVALID` (§7), `M = 0` is the empty
    array.
  - **Storage that cannot express `M < N`.** A backend whose `count: N` array
    lowers to a fixed-size type (`T[N]`, `std::array<T,N>`, `[N]T`) has no
    logical length to carry, so it always encodes `N` and settles a received
    `M < N` at the element default. That is a conformant encoding of an
    `N`-length value, but such a target cannot *send* a shorter array. Where
    that matters the length has to be carried explicitly — the C backend does it
    with a companion member (`SOFAB_OBJECT_FIELD_ARRAY_SIZED`, and
    `SOFAB_OBJECT_DESCR_SEQ_SIZED` for a wrapper holder), following the
    sized-blob convention that already paired a buffer with its length. The
    length must be at least as wide as the element's alignment or it is padded
    away from the slot; corelib-c-cpp asserts the adjacency at compile time.
  - **The bit-pattern rule survives its cause.** The trim it protected is gone,
    but "equals the element default" is still decided on **bits**, not `==`,
    wherever the predicate is used (element sparsity below): `-0.0 == 0.0` holds
    in every target language, so a numeric compare would drop a `-0.0` element
    and rebuild it as `+0.0`. The shared vectors treat the two as distinct
    (`array_fp32_specials`).
- **Sparse-canonical encoding** — encoding is **always** sparse (no config
  toggle, MESSAGE_SPEC §2): a field equal to its effective default (schema
  `default:`, else type-zero) is skipped on encode and reconstructed on decode.
  The `!= default` test is applied **per field, and a `sequence`-typed field is
  no exception** (a `struct`/`union`, and the wrapper form of
  composite/dynamic-element arrays): an all-default sequence field is **omitted**,
  not emitted as an empty wrapper — see *Lazy sequence framing* below for how the
  backends express that in one forward pass. **Within a wrapper array the same rule
  reaches the elements** (id = index, MESSAGE_SPEC §2): a `string`/`blob`
  **element** is a leaf, so it is **omitted when it equals its element default
  (empty)** — leaving an id gap the decoder fills from the default, so trailing
  default elements collapse (`["a",""]` encodes as `["a"]`, `["",""]` as the
  empty wrapper). A `struct`/`union`/nested-array element is itself a sequence and
  **stays framed even when all-default** — element presence is what carries a
  dynamic array's length (*highest present id + 1*, MESSAGE_SPEC §5.1), so dropping
  one would change the decoded length, not merely the bytes. (The compact native
  scalar arrays are exempt — they carry no
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
- **Lazy sequence framing** — the §2 rule above asks whether a sequence is emitted
  from *what its children turn out to be*, while its header must precede them. The
  backends never buffer a sub-message to find out; the corelib holds the header
  back instead (CORELIB_PLAN §6, "Sequence framing"):
  `begin_lazy(id)` pushes the id onto a pending run, the **first field write**
  emits the whole run (outermost header first), `end()` **drops** a frame that got
  no content, and `end_keep()` forces it out. Held-back ids are encoder state, not
  buffer content, so a flush cannot split a run and the bytes stay independent of
  the output-buffer size — the chunked-encode guarantee is untouched.
  The closer is chosen **statically**, from the position in the schema:
  | position | closer |
  |---|---|
  | `struct`/`union` field, array field (the wrapper) | `end` |
  | wrapper-array **element** (`struct`/`union`/nested row) | `end_keep` |
  The two failure directions are asymmetric, which makes `end_keep` the safe
  default: the wrong `end_keep` costs one non-canonical empty frame a decoder
  normalizes away, the wrong `end` silently changes an array's length. In C++ the
  distinction rides on the corelib's two message writes — `write(id, msg)` is the
  element form (keeps the frame), `writeLazy(id, msg)` the field form.
  **C is the exception**: its message layer is the `object.h` descriptor, which
  tests each field against its default *before* opening anything, so it keeps the
  plain eager `sequence_begin`/`sequence_end` pair and needs no hold-back at all.
  The guard that decides this in `sofab_object_encode` discriminates on the
  **role** (`info->fixed_seq` — a wrapper holder's "fields" are element slots),
  not on the field type: dropping the type check alone would elide interior array
  elements and break §5.1.
- **Wrapper-array elements: placement and positional sparsity** — inside a
  wrapper sequence the child id **is** the array index (MESSAGE_SPEC §5.1), and
  the array carries no length field, so the decoded length is *highest present
  id + 1*. From that single fact both rules follow: **nothing that carries the
  length may be elided, and everything else may be.**
  | rule | encode | decode |
  |---|---|---|
  | **placement** | element written at its index | element decoded **into** `dest[id]` after gap-filling with element defaults — never appended |
  | **interior sparsity** | an element before the last one that equals its element default is **omitted**, leaving an id gap — a `string`/`blob` is not written *and* a `struct`/`union`/nested-array element is **not framed** | the absent `dest[id]` is restored from the element default |
  | **the last element** | **always** written — a leaf as its value, a sequence element as an **empty frame** | its presence is what fixes the length |
  One rule governs both element kinds; the old carve-out ("a sequence element is
  never omitted") is gone, and so is any dependence on whether the field declares
  a `count`. `["a", ""]`, `["a"]` and `[]` are three distinct values with three
  distinct encodings; `[x, default, default]` is element 0 plus an empty/default
  element at id 2, with element 1 the gap.
  Placement is also what gives §7.4 struct-merge on a **reopened** element id for
  free: the second frame decodes into the element the first produced. Appending
  instead shortens the array by any interior gap *and* turns a reopened id into a
  second element (generator#247) — and once interior gaps became reachable, the
  same defect in the **row** collectors of a matrix/nested array started shifting
  every later row down by one, which is why those place by id too.
  The closer choice is therefore **positional in the value** (last vs. interior),
  not static from the schema position, and each backend generates the writer and
  its all-default predicate from **one** expression: a predicate that narrows
  where the writer does not, or the reverse, omits a field that is on the wire or
  keeps one that is not.
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
8. **Performance & footprint** (`tests/bench/`, §15) — the committed
   `tests/bench/results.txt` records instructions/op (Callgrind) per (language ×
   corelib) row and the `.text`/`.data`/`.bss` of the generated code
   cross-compiled to the embedded targets the `footprint` profiles ship to.
   Regenerated with `tests/bench/run.sh`; the **diff** is the gate — a generator
   change that costs or saves shows up in the PR next to the code that caused it.
   Unlike gates 1–7 this is read by a human rather than being red/green, and it is
   deliberately **not** merged with gate 2: conformance builds unoptimized against a
   moving corelib, the bench builds `-O3`/`-Os` (§8 makes bounds checks debug-only
   assertions, so a debug build measures code that never ships).

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
                         gen-artifacts.sh builds the per-language CI artifact bundle;
                         bench/ Ir/op + footprint of the generated code (§15; committed results.txt)
.github/workflows/       ci.yml (hermetic + lang-<x> jobs), release.yml (binaries +
                         npm-publish + pypi-publish + verify-published),
                         action.yml + npm.yml + pypi.yml (distribution smoke
                         tests), verify-published.yml (post-publish: install from
                         the registries and use it)
.github/actions/         setup-sofabgen/ composite action (installs the CLI in CI;
                         thin wrapper over install.sh)
install.sh               one-line installer: OS/arch detect + release download +
                         SHA-256 verify (curl|sh); the action reuses it
packaging/               per-package-manager distribution recipes, one directory
                         per channel, all fed from the same release assets
packaging/npm/           npm distribution: bin/sofabgen.js launcher + per-platform
                         optional-dependency packages built from the release
                         binaries (scripts/build-platform-packages.js); packages/
                         is git-ignored (built at publish time)
packaging/pypi/          PyPI distribution: build-wheels.py turns the nine release
                         binaries into thirteen platform wheels (stdlib only, no
                         build backend), check-wheels.py is the pre-upload gate;
                         wheels/ is git-ignored (built at publish time)
```

**Distribution.** The release workflow (`release.yml`, PLAN §1/M8) is the source of
truth: on a `v*` tag it cross-compiles the static, CGO-free binary for the nine
supported OS/arch pairs and attaches each plus a `.sha256` to the GitHub release.
Everything else is a thin consumer of those assets so there is one artifact set and
one checksum to trust.

**The `v*` tag is the single source of truth for the version**, injected into every
artifact at build/publish time — never hand-maintained in a committed file, so it
cannot drift:

- The **Go binary** carries a `main.version = "0.0.0-dev"` placeholder; the build
  step injects the tag via `-ldflags "-X main.version=<tag>"`, so a release binary
  self-reports the exact tag. Non-tag (`workflow_dispatch`) builds keep the placeholder.
- The **npm package** carries a `0.0.0-dev` placeholder in
  `packaging/npm/package.json`; the `npm-publish` job injects the tag with
  `build-platform-packages.js --version <tag>`, rewriting the version and every
  `optionalDependencies` pin in lockstep.
- Two guards back this up: `check-version` fails the release early if the tag is not
  a well-formed `vMAJOR.MINOR.PATCH[-prerelease]`, and after injection the
  `npm-publish` job asserts every package's version equals the tag before publishing.

The consumers of the release assets:

- **`install.sh`** — `curl … | sh` picks the matching asset by `uname`, verifies its
  checksum, and installs it. Honors `SOFABGEN_VERSION` / `SOFABGEN_INSTALL_DIR`.
- **`.github/actions/setup-sofabgen`** — a composite action that runs the *same*
  `install.sh` (from the action's own checked-out ref) and adds the binary to
  `$GITHUB_PATH`, so downstream CI can `uses:` it instead of hand-rolling downloads.
- **`go install github.com/sofa-buffers/generator/cmd/sofabgen@vX.Y.Z`** — builds from
  source; the CLI reports the module version via `runtime/debug.ReadBuildInfo()`
  (`cmd/sofabgen`), so an install-by-version self-reports that version. It falls back
  to `main.version` only when no module version is present — the release workflow
  overrides that fallback with `-ldflags "-X main.version=<tag>"`; a plain local
  `go build` reports the `0.0.0-dev` placeholder.
- **`npm i -D @sofa-buffers/generator`** (`packaging/npm/`) — the per-platform
  optional-dependency pattern (esbuild/swc model): a tiny launcher package pulls in
  one `@sofa-buffers/generator-<os>-<arch>` (matched by npm via `os`/`cpu`) that
  ships the corresponding release binary. No download, no `postinstall`; the binary
  is lockfile-hashed. The `npm-publish` job in `release.yml` builds these from the
  released binaries (`--version <tag>` keeps the version + optionalDependencies pins
  in lockstep) and publishes via **trusted publishing (OIDC, no token)** with
  automatic provenance; `npm.yml` smoke-tests the launcher on every runner OS against
  the latest published release (the committed version is a placeholder). The published
  package `version` always equals the release tag because it is injected from it, and
  the `npm-publish` guard asserts this before publishing. OIDC cannot *create* a
  package, so each package's first-ever version is bootstrapped once by hand
  (`packaging/npm/PUBLISHING.md`); the workflow publishes all versions after that.
- **`pip install sofabgen` / `uv tool install sofabgen`** (`packaging/pypi/`) — where
  npm needs a launcher plus nine optional dependencies, PyPI resolves the host
  natively through wheel tags: one project name, and `build-wheels.py` turns the nine
  release binaries into thirteen `py3-none-<platform>` wheels (Linux twice — the same
  static binary serves glibc and musl, but `manylinux`/`musllinux` are distinct tags).
  Each wheel holds nothing but the binary, at `sofabgen-<version>.data/scripts/`,
  which pip unpacks straight onto `PATH`: no shim, no Python code, no entry point.
  Only wheels are published — a source distribution would make pip on an unsupported
  platform attempt a build instead of reporting no matching distribution. The wheels
  are written with stdlib `zipfile` (no build backend), so their RECORD/tags/layout
  are guarded by unit tests in the `packaging` CI job rather than by a toolchain, and
  by `check-wheels.py` as the last gate before an upload (a PyPI version is
  immutable). The `pypi-publish` job in `release.yml` builds them from the released
  binaries and uploads via **trusted publishing (OIDC, no token)** with automatic
  PEP 740 attestations; `pypi.yml` smoke-tests the real install — venv, `PATH`,
  executable bit — on every runner OS against the latest published release. Two
  things differ from npm and both are load-bearing: the publisher's **environment**
  is a matched OIDC claim here (`pypi`, where npm's is blank), and the tag needs a
  **PEP 440** spelling — npm publishes `v0.21.0-rc1` verbatim, PyPI calls it
  `0.21.0rc1`, so `build-wheels.py` translates the a/b/rc forms and refuses the rest
  rather than publishing a version the tag does not name. PyPI OIDC *can* create the
  project, so there is no hand-bootstrapped first version: an org-level pending
  publisher covers it (`packaging/pypi/PUBLISHING.md`).

Both registry channels are verified twice, and the two tests answer different
questions. The smoke workflows (`npm.yml`, `pypi.yml`) build the artifact locally
from the release assets and use it — that proves the *recipe*, on every PR that
touches it. `verify-published.yml` runs after the publish jobs and starts from
`npm install` / `pip install` against the real registries on all three OSes — that
proves the *upload*. It also queries both registry APIs for the whole artifact set
(nine npm platform packages, thirteen wheels, provenance on each), because a host
only ever installs its own: a platform package or wheel that failed to publish is
invisible to an install test that just passed on a different platform.

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
4. **Decide the storage axis explicitly.** If the language can hold a
   schema-bounded field inline (fixed-capacity container, inline array, stack
   buffer), expose it as `allow_dynamic` with a corelib-keyed default and a bench
   row per side (§8, §15); if it cannot, or if the target is statically bounded by
   construction, say which in `docs/generator/<lang>.md`. Do not leave it unstated.
5. Add a project/harness template, corpus coverage, and a `tests/conformance/<lang>/run.sh`
   (generate → build → round-trip → byte-exact vectors) plus a gated unit test.
6. Add a `lang-<x>` CI job running the harness.
7. Add the `bench` verb to the project harness and a row to `tests/bench/rows.json`
   + a `tests/bench/lang/<x>.sh` recipe, then regenerate `tests/bench/results.txt`
   (§15).

A language milestone lands on `main` only when its tests + CI job are green, and
this document is updated to match.

---

## 15. Benchmark harness — instructions/op & footprint

`tests/bench/` measures what a generator change costs. The artifact is the
**committed** `tests/bench/results.txt`: change the generator, run
`tests/bench/run.sh`, and `git diff` shows the impact next to the code that caused
it. Gate #8 of §12. Full detail — including the traps — is in `tests/bench/README.md`.

**What is measured** is the *whole package*: generated code plus the corelib it
calls, as it ships. Not the corelib alone (each corelib benches itself), and not the
generator's own runtime.

**Ir, not wall-clock.** Instructions retired under Callgrind are independent of CPU
clock and OS scheduling, which is what makes a number stable enough to commit to a
file at all. Determinism is therefore a hard requirement here, not a nicety: a file
that wobbles when nothing changed is one nobody regenerates.

**The corelib is deliberately not pinned.** It is cloned from its default branch, as
conformance does, because a corelib must match the generated code built against it —
pinning would break the bench on exactly the commits that adopt a new corelib API.
Provenance replaces pinning: corelib SHAs and toolchain versions live in the
`results.txt` header. Header unchanged + a number moved ⇒ the generator did it. The
cost is that absolute numbers are not comparable across days; this is a diff tool.

**`results.txt` is regenerated by hand in the devcontainer**, the same way the
benchmark arena is driven — never by CI. Ir/op depends on the compiler that produced
the binary, so a runner with its own toolchain pins is a second measuring device, and
two devices disagree on unchanged code: the bench workflow on `ubuntu-24.04` with Go
1.26 read one row at 56,237 Ir/op that the devcontainer (Go 1.24) read at 24,698.
Neither is wrong; they are different scales. One environment owns the file.

`.github/workflows/bench.yml` still exists but is **`workflow_dispatch` only** — a
second opinion on demand, for a number that looks implausible locally or a toolchain
not available in the devcontainer. It is not a PR gate, because it would be loud on
every PR for reasons no PR caused. Its report (`tests/bench/lib/report.py`) leads
with the toolchain comparison for that reason, then separates failed measurements
from outliers from ordinary movement; it fails the job only on a failed measurement.

**The `zig` and `csharp` rows carry runner-specific pins** so that the "second
measuring device" merely *drifts* rather than *fails* — for two different reasons.

The zig row builds `-Dcpu=baseline`. `b.standardTargetOptions` defaults to the host
CPU, so an unpinned `--release=fast` build on a runner with AVX-512 (the `ubuntu-24.04`
Ice Lake / EPYC images) emits 512-bit instructions, and that runner's Callgrind (3.22,
older than the devcontainer's 3.26) cannot decode them: the run SIGILLs under Valgrind
and the row measures as `!`. Baseline is generic x86-64 — the same ISA gcc/rustc/go
already default to, which is why the other native rows measure clean on the older
Valgrind. It also makes zig reproducible across machines (the runner and devcontainer
now read the row identically), which is the point of Ir/op.

The csharp row runs the dll under `*/bin/Release/*`, `--roll-forward Major`, and
`DOTNET_EnableAVX512F=0` `DOTNET_PreferredVectorBitWidth=256`. The load-bearing fix is
the path anchor: a Release build leaves four `harness.dll` under the project (`bin/…`
plus three under `obj/…`, including the `refint` reference assembly), and `find | head
-1` returns them in *directory order*, which is not stable across filesystems. The
devcontainer yielded `bin/` first; the runners yielded `obj/…/refint/` first — a
metadata-only reference assembly with no `runtimeconfig.json`, so the host aborts with
"`libhostpolicy.so` not found" (~2.5M Ir, identical at every rep count) → zero slope →
`!`. Anchoring to `bin/` picks the one runnable app regardless of order. `Major` then
lets that `net9.0` app run on a newer major runtime if that is all the runner has, and
the AVX-512 knobs are the same guard as zig's baseline (512-bit RyuJIT codegen would
SIGILL under the runner's older Callgrind). All are no-ops on the devcontainer, so
`results.txt` (generated there) is unchanged but for the one zig row baseline moved.

A failed measurement is no longer silent: the harness greps the offending Callgrind
log for the tell-tale lines and prints them next to the `!` — which is what finally
surfaced the `refint` dll in the run command after two wrong hypotheses (a Valgrind
AVX-512 decode failure, then a missing runtime) had been chased on the ~2.5M-Ir
signature alone.

The same trap exists *inside* the devcontainer: `PATH` decides whether `cargo` is
apt's or rustup's, and the two rustc versions move the Rust rows about 8%. So the
file carries a **`## toolchain` table** — every compiler that built a row, its
version, and which rows it built, derived from `rows.json` so the mapping cannot
drift from the rows actually present. `(not found)` is recorded rather than omitted:
a tool that vanished from the environment is exactly the kind of thing that moves a
number quietly. This is what makes "header unchanged ⇒ the generator did it" true for
every row rather than only the ones gcc and rustc build.

**Two isolation methods**, split by whether a native symbol exists to collect on.
Both mirror the corelibs' own `bench/run_callgrind.sh`, which every corelib ships:

* **`toggle`** (c, cpp, rust, go, zig) — `--collect-atstart=no
  --toggle-collect=run_<workload>` around a single op. The `run_<w>` wrapper is
  `noinline` with external linkage. The barrier is on the **wrapper only**, so the
  corelib still inlines into the generated code — that inlining is the cost being
  measured. **Go additionally warms up**, alone among the `toggle` rows: the op
  collected is the *first* one, and Go's runtime builds itabs and resolves
  type/name offsets lazily on first use, charging those one-time costs to it (18k
  of a 55k decode; 5.5k of a 25k encode). Worse, they scale with how many types the
  generated code converts to interfaces, so adding itabs reads as a per-op
  regression that is not there. The generated harness runs an uncollected
  `warmup_<w>` first, with the body duplicated rather than delegating to `run_<w>`
  — toggling keys on entering the symbol whoever the caller is. One op suffices:
  AOT, so no tiers to climb, and each warmed cost is a global first-touch cache.
  c/cpp/rust/zig have no lazy runtime metadata and need none.
* **`subtract`** (java, kotlin, python, ts, csharp) — no native symbol (JIT'd/interpreted),
  so run at two rep counts and subtract: `Ir/op = (Ir(R2) − Ir(R1)) / (R2 − R1)`,
  cancelling startup, class loading and JIT compilation exactly. Needs a fixed
  warmup in the harness plus per-runtime pinning (EpsilonGC, `-XX:-TieredCompilation`,
  `-XX:hashCode=2`, …) so the two runs differ in nothing but the rep count.

**The `bench` verb is generated**, part of the `emit: project` backend contract (§8),
IR-driven like `encode`/`decode` and needing no new config key. Hand-written drivers
were rejected: one cannot compile against two generator revisions, and the
API-changing commits are precisely the ones worth measuring.

**Validity is enforced, not assumed.** A `--toggle-collect` matching no symbol is not
an error — callgrind silently reports `Ir = 0`, which reads as an infinite speedup —
so `ir_toggle` refuses zero. And two rep points cannot distinguish a real slope from a
JIT tier transition, so `ir_subtract` takes three and refuses unless the slopes agree
to 1%.

**A row is a `(config, corelib)` pair, so every axis that changes generated code
needs its own row.** Three are covered: the corelib choice (`cpp-cpp`/`cpp-c-cpp`,
`rust-rs`/`rust-rs-no-std`), the TypeScript `int64` mode (`ts-bigint`/`ts-long`), and
`allow_dynamic` (`cpp-c-cpp-dyn`, `rust-rs-no-std-dyn`, `cpp-cpp-static`,
`rust-rs-static`). The last axis now spans BOTH profiles: it chooses *where* a
bounded field's bytes live, and that is a question on a server as much as on a
microcontroller — the maxspeed rows toggle it the other way (static ON against a
heap-backed default), which is why they are named `-static` rather than `-dyn`.
Read every one of them as a pair; the flag has no absolute number, only a
difference against the row it toggles. On the footprint side turning it on trades
static bytes for an allocator, which on `c-cpp` drags in newlib's malloc (`.text`
6589 → 14287) and on bare-metal Rust needs one supplied by the footprint driver at
all. On the maxspeed side the trade runs the other way — fewer per-field
allocations on the decode path, paid for in `sizeof`, since a message then holds
its declared worst case. What no static-section
measurement can show is the heap the dynamic build then needs at runtime.
A fourth is the corelib's own engine where it has one: `python` / `python-native`
(below). Uncovered axes are named in `tests/bench/README.md`: the corelib build
switches (`SOFAB_DISABLE_*`, cargo features, `sofab_no_strict_utf8`) — which is the
footprint story itself — and corelib-ts's `setKernel` seam.

**A fifth axis is not a config at all: the SCHEMA.** Whether a field is bounded is
a property of the message definition, and one whole subsystem is live only where it
is not — the receiver caps of §9.5 govern schema-**unbounded** fields, so on
`vehicle_telemetry`, which bounds every array and every string/blob, no target
emits a single `max_dyn_*` constant, cap argument or guard. The bench measured that
subsystem's cost as zero for as long as it existed, in both directions. `rows.json`
therefore takes a per-row `schema`/`payload`/`message`, and four rows use it:
`cpp-cpp-unbounded`, `zig-unbounded`, `go-unbounded` and `rust-rs-unbounded`, one
per enforcement shape in §9.5's table (into the reads; split; via the collectors;
entirely generated), each with the same config as its bounded sibling and all four
`toggle` rows, i.e. one op apiece. **The pair is not a difference**: the two rows
measure different messages, so the sibling is a control that must stay still, not a
baseline to subtract. The footprint rows have no unbounded twin because their
profiles reject an unbounded field at schema-validation time — the same fact as
§9.5's "their caps are inert". `tests/matrix/bench_rows_test.go` asserts on the IR
that the default schema stays fully bounded and that a row-level schema is unbounded
in all three kinds *and* keeps a bounded twin of each, so the rows cannot quietly
stop measuring what they are named for.

**What ran is recorded, not assumed.** The header carries the corelib SHAs and the
`## toolchain` table the compiler versions, because each moves numbers with the
generator unchanged. `sofab-engine` is the same idea one level up: corelib-py picks
between its pure-Python classes and its Cython accelerator at *import* time, silently,
and the two turn out to be **7.2× (encode) / 4.8× (decode)** apart — far too much for
one row to stand for both, and for years only the fallback was measured (an
accelerator perf release moved the row by zero instructions). Both are measured now,
`python` and `python-native`, each pinning its own engine because they share a corelib
checkout, and each row's actual engine is written into the table. `python-native`
verifies `sofab.IMPL` before reporting: the extension is `optional=True`, so a failed
compile is not a failed build, and a row that silently degraded would report the pure
cost under the native name. See `tests/bench/README.md`.

**Not measured / known gaps** (properties of the targets, not the harness): the C++
`footprint` profile cannot build freestanding (the generated header pulls in
`<string>`/`<vector>`), and AVR cannot host any fp64 schema. Both are recorded in
`tests/bench/README.md`.
