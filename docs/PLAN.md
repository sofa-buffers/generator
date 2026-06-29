# SofaBuffers Code Generator — Implementation Plan

> A new, from-scratch code generator that turns a YAML object/message definition
> (validated against `sofabuffers-schema-v1.json`) into typed, ready-to-use source
> code in every supported language, where the generated code calls into the
> highly-optimized **corelib** for that language.

**Status:** Proposal / design plan
**Author context:** Replaces the existing TypeScript POC in `generator-old/`
**Date:** 2026-06-28

---

## 0. Repositories

GitHub organization: **<https://github.com/sofa-buffers>**

| Repo | Link | Role |
|---|---|---|
| `generator-old` | <https://github.com/sofa-buffers/generator-old> | The **existing TypeScript POC** (renamed from `generator`); hosts the JSON Schema, examples, and the example/test artifacts under `test/`. The new generator described by this plan supersedes it. |
| `corelib-c-cpp` | <https://github.com/sofa-buffers/corelib-c-cpp> | Embedded C (`object.h`) + C++ wrapper (`sofab.hpp`), minimal footprint. |
| `corelib-cpp` | <https://github.com/sofa-buffers/corelib-cpp> | High-speed, header-only C++20. |
| `corelib-rs-no-std` | <https://github.com/sofa-buffers/corelib-rs-no-std> | `no_std` Rust, minimal footprint. |
| `corelib-go` | <https://github.com/sofa-buffers/corelib-go> | Go, high throughput. |
| `corelib-py` | <https://github.com/sofa-buffers/corelib-py> | Python, high throughput. |
| `corelib-java` | <https://github.com/sofa-buffers/corelib-java> | Java, high throughput. |
| `corelib-cs` | <https://github.com/sofa-buffers/corelib-cs> | C# / .NET, high throughput. |
| `corelib-ts` | <https://github.com/sofa-buffers/corelib-ts> | TypeScript, high throughput. |

---

## 1. Goal & Scope

Build a standalone code generator that:

1. Reads an **object definition** written in **YAML** (JSON also accepted).
2. **MUST validate the input against the JSON Schema before doing anything else.** This is a hard, non-optional gate: every input definition is validated against `schema/sofabuffers-schema-v1.json` (JSON Schema draft-07, including the custom keywords `uniqueIds` and `defaultMatchesEnum`). If validation fails, the generator **emits a clear, located error and aborts with a non-zero exit code, producing no output** — invalid input is never code-generated. (See §8.1, Parser; the config file is likewise validated against its own schema, §7.1.)
3. Produces **idiomatic, typed source code** for one or more target languages.
4. Generated code depends on, and is optimized for, the matching **corelib** runtime.
5. Ships as a **single, minimal-dependency, statically-linked executable** that cross-compiles to Windows / Linux / macOS on x86, x86-64, ARM, ARM64, etc.

### Per-target optimization mandate

| Corelib | Target profile | Generated-code mandate |
|---|---|---|
| `corelib-c-cpp` (C) | Embedded, minimal footprint | Use the descriptor-driven `object.h` API; no heap; static descriptors; honor `SOFAB_DISABLE_*` feature flags. |
| `corelib-c-cpp` (C++) | Embedded | Use the `sofab.hpp` wrapper (`OStreamMessage`/`IStreamMessage`, inline/stack buffers). |
| `corelib-cpp` | High performance | Optimize for max speed: header-only C++20, `std::span`/`string_view` zero-copy, `if constexpr` dispatch, no allocations on hot path. |
| `corelib-rs-no-std` | Embedded, no_std | Generated code **must be `no_std`-compatible**; no `alloc` unless gated; emit `sofab::require!(...)` feature guards. |
| `corelib-go` / `-py` / `-java` / `-cs` / `-ts` | High throughput | Optimize for throughput: minimal allocations, primitive scalars, visitor-based decode. |

---

## 2. Decisions (please confirm / override)

These two decisions shape the rest of the plan. The plan below assumes the **recommended** option; flag if you want the alternative.

### 2.1 Generator implementation language — **Recommended: Go**

- **Go** gives the strongest match to the "single minimal-dependency executable, many OS + many arch" requirement: `GOOS`/`GOARCH` cross-compiles static binaries from one machine with no C toolchain, and the stdlib + a couple of mature modules (`gopkg.in/yaml.v3`, `santhosh-tekuri/jsonschema`, `text/template`) cover everything.
- **Rust** is a fully viable alternative (static musl binary, `serde_yaml`, `jsonschema` crate, `minijinja`/`askama` templating) but multi-target cross-compilation needs more setup (`cross`/`cargo-zigbuild`).
- *Either way, the generator binary itself is a normal hosted program — it does not need to be `no_std` or embedded-friendly. Only the **emitted** Rust/C code carries those constraints.*

### 2.2 Backend implementation order — **Recommended: embedded-first**

Tackle the hardest constraints first so the IR and emitter architecture are proven against the worst case:

1. `corelib-c-cpp` C (`object.h`) — static descriptors, no heap.
2. `corelib-rs-no-std` — `no_std`.
3. `corelib-c-cpp` C++ (`sofab.hpp`).
4. `corelib-cpp` — max speed.
5. Throughput languages: `go`, `ts`, `py`, `java`, `cs` (they share an almost-identical `OStream`/`IStream`+Visitor API, so they go fast once one is done).

---

## 3. The Object Definition Format (input)

Authoritative source: [`schema/sofabuffers-schema-v1.json`](../schema/sofabuffers-schema-v1.json) (in this repo), documented in [`schema/README.md`](../schema/README.md).

### 3.1 Top-level shape

```yaml
version: 1
$defs:        # optional: reusable struct/union/enum/bitfield definitions, referenced via $ref
  struct: { ... }
  union:  { ... }
  enum:   { ... }
  bitfield: { ... }
messages:     # each key = a message name (PascalCase/identifier), each with a payload
  MyMessage:
    summary: "..."
    payload:
      fieldName:
        id: 0          # unique numeric id within parent, 0 .. 2^31-1
        type: <type>
        ...constraints...
```

A file must contain `$defs` and/or `messages`. Field **ids must be unique** within their parent payload/struct/union (enforced by the `uniqueIds` custom keyword).

### 3.2 Field types

| Type | Notes / constraints from schema |
|---|---|
| `u8 u16 u32 u64` | unsigned ints; optional `default` (range-checked per width) |
| `i8 i16 i32 i64` | signed ints; optional `default` (range-checked per width) |
| `fp32` `fp64` | floats; optional `default`, optional `decimals` (0–15) |
| `boolean` | optional `default` |
| `string` | optional `maxlen`, optional `default`; see §5.7 for the per-target `maxlen` requirement |
| `blob` | optional `maxlen`, `default` is base64; see §5.7 |
| `array` | fixed-length; `items: {type, count}`; element type is a **primitive, `string`, or `blob`** |
| `enum` | inline map or `$ref`; values may be negative; `default` must match a value (`defaultMatchesEnum`) |
| `bitfield` | inline `bits` map or `$ref`; each flag has `pos` 0–63 + optional `default` |
| `struct` | nested; `fields:` inline or `$ref`; **recursive** (structs in structs) |
| `union` | `oneof:` inline or `$ref`; optional `default_id` |

Common optional attributes on every field: `description`, `unit`, `deprecated`.

> This table is a summary. The **authoritative** type rules, plus the validation
> that the bare JSON Schema cannot express — the `$data` cross-field rules and the
> six custom keywords (`uniqueIds`, `uniquePositions`, `defaultMatchesEnum`,
> `defaultIdMatchesUnion`, `blobDefaultLength`, `int64Range`) — and the
> dereference-then-validate contract, are documented in
> [`schema/README.md`](../schema/README.md). A
> reimplementation must follow that document, not this summary.

### 3.3 `sequence` is a wire type, not an authoring type

`sequence` is **not** a field type a user writes — it is a **wire encoding** (`sequence_begin` / `sequence_end`, wire types `0b110`/`0b111`). The definition format therefore never needs a `sequence` keyword:

- `struct`, `union`, and any nested structure are serialized **as a sequence** — each opens a fresh, isolated id scope (so nested ids never collide with the parent's);
- `array` of `string` or `blob` (dynamic-length elements) is **not** an array wire type — it is encoded as a **sequence** of string/blob fields;
- `array` of a numeric type uses the real fixed array wire type.

So the fixed-count `array` plus `struct`/`union` already cover every case; the variable-length / dynamic-element forms all lower onto sequences in the corelib at encode time. The generator must route `struct`/`union`/`array-of-string`/`array-of-blob` through the corelib's `sequence_begin/end` API and require the `sequence` capability for them. Full detail: [`schema/README.md` → "How definition types map to the wire format"](../schema/README.md) and the [wire-format documentation](https://github.com/sofa-buffers/documentation/blob/main/README.md).

### 3.4 `$ref` handling

The POC dereferences `$ref` for validation but keeps the `$ref` structure for generation (so a referenced `$defs/struct` becomes a single shared generated type, not duplicated). The new generator should preserve this: **resolve refs into a shared-type graph**, not by inlining/duplicating.

---

## 4. The corelib Runtime Contract (output target)

The generator treats each corelib as an **opaque codec**. The wire format — how field ids, varints, floats, strings, arrays and nested sequences are laid out on the wire, byte order, forward/backward-compatible field skipping — is entirely the **corelib's** concern and is **not** the generator's responsibility. The generator only needs each corelib's **public encode/decode API**, and emits a thin typed wrapper that calls it. Because every corelib already speaks the same format, code generated for one language interoperates with code generated for any other for free.

### 4.1 Encoder/decoder API per language (what generated code calls)

The shapes below are the verified public surfaces. Generated code is a thin, typed wrapper over them.

**C — `corelib-c-cpp` `object.h` (descriptor-driven, no heap):**
- Generated output per object = a `struct`, a static `sofab_object_descr_field_t[]` table (via `SOFAB_OBJECT_FIELD*` macros), a `sofab_object_descr_t`, plus optional default-value instance.
- Encode: `sofab_object_encode(&ostream, &descr, &obj)`. Decode: init `sofab_object_decoder_t`, `sofab_istream_init(&is, sofab_object_field_cb, &dec)`, `sofab_istream_feed(...)`.
- Nested structs referenced via `nested_list` + `SOFAB_OBJECT_FIELD_SEQUENCE(..., idx)`.
- Honor feature flags: `SOFAB_DISABLE_{FIXLEN,ARRAY,SEQUENCE,FP64,INT64}_SUPPORT`.

**C++ embedded — `corelib-c-cpp` `sofab.hpp`:** subclass `OStreamMessage` (`serialize(OStreamImpl&)`) / `IStreamMessage` (`deserialize(IStreamImpl&, id, size, count)`); use `OStreamInline<N>` / `IStreamObject<T>`; `os.write(id, value)` chains, `is.read(field)` dispatch.

**C++ high-speed — `corelib-cpp` (`include/sofab/sofab.hpp`):** same `OStreamMessage`/`IStreamMessage` pattern, header-only C++20, `OStreamInline<N>`/`IStreamObject<T>`, `std::span`/`string_view` zero-copy reads, `Result` chaining. `_maxSize` constexpr per message drives stack buffer sizing.

**Rust — `corelib-rs-no-std` (no_std):**
- Encode: `OStream::new(&mut buf)` (or `with_offset`/`with_flush`), `write_unsigned/signed/boolean/fp32/fp64/str/blob`, `write_array_*`, `write_sequence_begin/end`.
- Decode: implement the `Visitor` trait (`unsigned/signed/fp32/fp64/string/blob/array_begin/sequence_begin/sequence_end`), drive with `IStream::new().feed(&buf, &mut visitor)`.
- Emit `sofab::require!(fixlen, array, sequence, fp64, value64)` guards matching the types used. No `alloc` in generated code by default.

**Throughput backends** — all surfaces below were verified against the repos (file:line in the team notes). The naming and the decode model **differ per language**, so the generator's backends are not interchangeable templates:

| Lang | Encoder type / ctor | Scalar writers | Array writers | Sequence | Decode model |
|---|---|---|---|---|---|
| **Go** | `sofab.NewEncoder(io.Writer)` (buffered, **sticky error**, `Flush()`) | `WriteUnsigned/WriteSigned/WriteBool/WriteFloat32/WriteFloat64/WriteString/WriteBytes` | generic funcs `sofab.WriteUnsignedArray[T]/WriteSignedArray[T]`, methods `WriteFloat32Array/WriteFloat64Array` | `WriteSequenceBegin(id)`/`WriteSequenceEnd()` | **pull-parser** `Decoder.Next()/Skip()` + typed readers (also a `Visitor`/`AcceptBytes`) |
| **Python** | `Encoder()` / `Encoder.over_buffer(buf,…)` (`sticky=`) | `write_unsigned/write_signed/write_bool/write_float32/write_float64/write_string/write_bytes` | `write_unsigned_array/write_signed_array/write_float32_array/write_float64_array` | `write_sequence_begin/end` | **pull-parser** `Decoder.next()/skip()` + typed readers (also `Visitor`+`drive()`) |
| **Java** | `new OStream(byte[]{,offset,FlushSink})` | `writeUnsigned/writeSigned/writeBoolean/writeFp32/writeFp64/writeString/writeBlob` | `writeArrayUnsigned/Signed` (byte/short/int/long overloads), `writeArrayFp32/Fp64` | `writeSequenceBegin/End` | **Visitor** (`IStream.feed(bytes,Visitor)`; default no-op methods) |
| **C#** | `new OStream(byte[]{,offset,FlushSink})` | `WriteUnsigned/WriteSigned/WriteBoolean/WriteFp32/WriteFp64/WriteString/WriteBlob` | `WriteArrayUnsigned/Signed` (sbyte/short/int/long…), `WriteArrayFp32/Fp64` | `WriteSequenceBegin/End` | **Visitor** (`IStream.Feed(bytes,IVisitor)`) |
| **TS** | `new OStream()` (grow) / `new OStream(buf,off,flush)` | `writeUnsigned/writeSigned/writeBoolean/writeFp32/writeFp64/writeString/writeBlob` (`number\|bigint`) | `writeUnsignedArray/writeSignedArray/writeFp32Array/writeFp64Array` | `writeSequenceBegin/End` | **Visitor** (`decode(bytes,visitor)` / `IStream.feed`); `sequenceBegin` may return a child visitor |

Notes that shape the backends:
- **Go & Python decode are pull-parsers** (a `Next()/next()` loop with typed readers and `Skip()`), not visitors — generated `Unmarshal` reads a field header, `switch`es on `(id,type)`, and calls the matching reader. Java/C#/TS/Rust decode is **visitor-based**; C/C++ use a field callback / object descriptor. So §5 step 3's "visitor/callback/descriptor/pull-parser" really is per-language.
- Go uses `Bool`/`Bytes`/`Float32` (not `Boolean`/`Blob`/`Fp32`); Python is snake_case. Don't assume one casing across backends.
- Field-id max is `INT32_MAX` (`0x7fffffff`) in every backend (`IDMax`/`ID_MAX`).

### 4.2 Nesting & depth

- `struct`, `union`, and variable `sequence` are emitted using the corelib's `sequence_begin(id)` / `sequence_end()` API calls (the corelib handles the on-wire representation).
- **Max nesting depth = 256 — a hard limit from the SofaBuffers spec.** Every generator carries the same `MAX_NESTING_DEPTH = 256` **constant** and validates depth ≤ 256 at generation time, emitting a hard error past it. 256 is already an enormous nesting depth in practice, so the cap is never a real-world constraint; using one shared constant keeps every backend portable and the embedded **C** decoder (a `uint8_t depth` counter in `object.h` / `istream.h`) within its natural bound.

---

## 5. How the Generated Code Should Look

**One generated type per object.** Because any `struct`/`union`/`$defs` entry can be referenced as a nested member elsewhere, the generator emits a standalone type (class/struct/record) for **every** object — top-level messages and every named nested struct/union — and composes them by reference.

For a message `MyMessage` the generator emits, per language:

1. A **type** holding all members with language-appropriate field types:
   - integers → native sized ints,
   - `enum` → a generated enum type backed by the smallest **signed** integer width that fits its **signed-32-bit** value set (enums are signed zig-zag varints on the wire),
   - `bitfield` → an integer with named bit accessors / flag constants,
   - `array` of a **numeric** element (`u*`/`i*`/`fp32`/`fp64`) → fixed-size array/`std::array`/`[N]T`, encoded with the corelib's array writers;
   - `array` of **`string`** → **special case: there is no string-array wire type.** It is modelled as a fixed-length **sequence of string fields** (confirmed by `corelib-c-cpp`'s object test and the `examples/messages/example.yaml` note "string arrays are internally represented as a sequence of strings"). So generated code wraps it in `sequence_begin/end` and writes one string per element — it requires the **sequence** (and fixlen) capability, *not* the array capability;
   - `sequence` → growable list/`Vec`/slice (or, in `no_std`/C, a caller-provided buffer + count),
   - `struct`/`union` → a reference to that object's generated type,
   - `string`/`blob` → language string/byte type, bounded by `maxlen`.
2. A **serialize** method that writes each field via the corelib encoder in id order, applying defaults/`deprecated` policy.
3. A **deserialize** path (visitor/callback or descriptor) that dispatches on field `id` and fills the type, **skipping unknown ids** for forward compatibility.
4. **Defaults** initialization from the schema `default` values.
5. A **max-serialized-size constant** (§5.5) — the default buffer size, overridable for streaming.
6. **Streaming support** (§5.6) — chunked `feed()` decode + flush-callback encode (and stream integration on non-embedded targets).

Generation must be **deterministic** (stable ordering, stable output) so output is diff-friendly and CI-checkable.

### 5.1 File header (every generated file)

Each emitted file starts with a banner comment, in that language's comment syntax, containing at minimum:

- a clear **"@generated / DO NOT EDIT"** marker (and that edits will be lost on regeneration — point readers at the source instead);
- **what generated it** — the tool name + version;
- **when** — an ISO-8601 timestamp (make it reproducible: the `generic.timestamp` config key / `SOURCE_DATE_EPOCH` give byte-stable, diff-friendly output in CI);
- the **source definition** file name (and ideally a hash of it) so the origin is traceable;
- the SPDX license line if configured.

Example (C/C++):
```c
/*!
 * @file myfirstmessage.hpp
 * @brief SofaBuffers generated message types — DO NOT EDIT.
 *
 * @generated by sbufgen 0.1.0 on 2026-06-28T00:00:00Z
 * @note  Generated from def/myfirstmessage.yaml. Regenerate instead of editing;
 *        manual changes are overwritten.
 * SPDX-License-Identifier: MIT
 */
```

### 5.2 Documentation comments (drive the language's doc tooling)

The schema carries human text the generated code must surface as **documentation comments** in the format each language's doc generator expects — so `doxygen`, `rustdoc`, `javadoc`, etc. pick them up automatically:

- A message's **`summary`** → the doc comment on the generated message type.
- Every field's optional **`description`** → the doc comment on that member/field. Append `unit` and, where useful, `default`, and a **`@deprecated`** tag when the field is marked `deprecated`.
- Enum constant **`description`** → doc comment on each enum value; bitfield flag `description` → doc comment on each flag/accessor.

Per-language comment style:

| Language | Doc style | Tooling |
|---|---|---|
| C / C++ | `/*! ... */` or `///` with `@brief` | Doxygen |
| Rust | `///` (outer) / `//!` (inner) | rustdoc |
| Go | `// Name ...` doc comments directly above the decl | godoc/pkgsite |
| Python | triple-quoted docstrings | Sphinx/pydoc |
| Java | `/** ... */` with `@param`/`@deprecated` | Javadoc |
| C# | `/// <summary>...</summary>` XML doc | DocFX/XML doc |
| TypeScript | `/** ... */` TSDoc/JSDoc | TypeDoc |

### 5.3 Namespacing (configurable)

- **All generated code lives in a dedicated namespace/module**, never the global scope — this also keeps doc generators (e.g. Doxygen) tidy by grouping everything under one namespace.
- The namespace is **configurable** via the config (`generic.namespace`, overridable per target — §7), with a sensible default (e.g. `sofabuffers`).
- Realized idiomatically per language:
  - **C++** → `namespace <ns> { ... }`; **C#** → `namespace`; **Rust** → `mod` / crate path; **Java** → `package`; **Go** → package name; **Python** → module/package; **TypeScript** → ES module (optionally a `namespace`).
  - **C has no namespaces**, so apply a configurable **symbol prefix** to every generated type and function (e.g. `myproj_MyFirstMessage`, `myproj_myfirstmessage_encode(...)`) to avoid collisions; the same prefix doubles as a Doxygen grouping (`@defgroup`). The same prefix convention applies to any language that emits free functions instead of methods.
- Nested-type names are namespaced under their owner to stay collision-free and self-documenting (e.g. `MyFirstMessage_somestruct`).

### 5.4 Compile-time capability guards (embedded backends)

The embedded corelibs can be **built with features disabled** to shrink their footprint. If a message uses a feature the linked corelib lacks, the generated code **must fail to build with a meaningful message** — never silently misbehave at runtime. The generator therefore emits, per message, a **capability guard** derived from the field types actually used.

Feature → trigger mapping (what makes a guard required):

| Generated-code requires | Triggered by a message using | `corelib-c-cpp` macro | `corelib-rs-no-std` feature |
|---|---|---|---|
| fixed-length fields | `string`, `blob`, `fp32`, `fp64` | `SOFAB_DISABLE_FIXLEN_SUPPORT` | `fixlen` |
| 64-bit float | `fp64` | `SOFAB_DISABLE_FP64_SUPPORT` | `fp64` |
| arrays | `array` of a numeric element | `SOFAB_DISABLE_ARRAY_SUPPORT` | `array` |
| nested framing | `struct`, `union`, `sequence`, **and `array` of `string`** | `SOFAB_DISABLE_SEQUENCE_SUPPORT` | `sequence` |
| 64-bit integers | `u64`/`i64` (or values/ids needing >32 bits) | `SOFAB_DISABLE_INT64_SUPPORT` | `value64` |
| wide descriptors (C object API) | field id / struct size beyond the profile | `SOFAB_OBJECT_DESCR_PROFILE` / `…_ID_MAX` | — |

> Note the asymmetry: an `array` of a **numeric** type needs the `array` capability; an `array` of **`string`** is a *sequence of strings* (§5) and therefore needs `sequence` + `fixlen` instead.

**C / C++ embedded (`corelib-c-cpp`)** — the corelib's header explicitly invites this pattern. Emit a guard header that includes `sofab/sofab.h` (to pull in the resolved config macros) then `#if defined(SOFAB_DISABLE_…) # error "…"`:

```c
#include "sofab/sofab.h"
#if defined(SOFAB_DISABLE_FP64_SUPPORT)
# error "SofaBuffers: field 'TelemetryFrame.temperature' is fp64/double, but the corelib was built with SOFAB_DISABLE_FP64_SUPPORT. Re-enable fp64 or change the field to fp32."
#endif
#if SOFAB_TELEMETRYFRAME_MAX_FIELD_ID > SOFAB_OBJECT_DESCR_ID_MAX
# error "SofaBuffers: field ids exceed the configured SOFAB_OBJECT_DESCR_PROFILE id width."
#endif
```
(C++ embedded can use the same macro `#error` guards, or `static_assert` on the equivalent condition.)

**Rust (`corelib-rs-no-std`)** — the corelib ships a purpose-built macro; emit exactly the capabilities used:

```rust
sofab::require!(fixlen, array, sequence, fp64, value64);
// fp64 off -> compile error:
//   sofab: this application requires the `fp64` feature, but it is disabled
```

Both patterns are **demonstrated end-to-end (and verified) in `generator-old/test/capability-guard-c/` and `…-rs/`** — see §10. The required capability set is **auto-derived from the definition** (the generator knows which wire features a message uses), so the guards are emitted automatically — there is no manual `features` config (§7.3). A corelib built without a needed capability is caught at compile time rather than in the field.

#### API-version guard (every backend)

Capability guards catch a *feature-stripped* corelib; an **API-version guard** catches an *incompatible* corelib — one whose encode/decode API has drifted from what the generator emitted against. Every corelib **publishes its API version** (the version of the public encode/decode surface the generated code calls — e.g. a C macro `SOFAB_API_VERSION`, `sofab::API_VERSION` in Rust, and the equivalent constant in each managed corelib). The generator is built/verified against a known API version (or a compatible range) and **bakes that expectation into the generated code**, emitting a **compile-time assertion** that the linked corelib is compatible. A future, breaking corelib then **fails to build with a clear message** instead of miscompiling or failing subtly at runtime.

This is **distinct from the wire-format version** (the schema `version: 1` and the on-wire framing the corelib owns): the API-version guard protects the **source-level contract** between generated code and the corelib it links against, whereas the wire version protects encoder/decoder interop. The two evolve independently, so both are checked.

- **Policy:** compatible = same **major** (a breaking API change bumps major); the guard accepts `major == expected` and `minor >= required`. The expected API version + policy are recorded in the file-header banner (§5.1) for traceability, and — like capability guards — the guard is emitted **automatically**, not configured.
- **Per language (compile-time where possible, else fail-fast at init):**
  - **C / C++ (embedded & max-speed):** `#if SOFAB_API_VERSION_MAJOR != N || SOFAB_API_VERSION_MINOR < M\n# error "SofaBuffers: generated against API vN.M, but the linked corelib is vX.Y — regenerate or update the corelib."\n#endif` (C++ may use `static_assert`).
  - **Rust:** a const assertion against the corelib constant — `const _: () = assert!(sofab::API_VERSION_MAJOR == N);` — or a `sofab::require_api!(N, M)` macro if the corelib provides one.
  - **Go / Python / Java / C# / TS:** prefer a static/compile-time check where the language allows (Go: a build-time `const` comparison; C#: analyzer/`#error`; TS: a type-level check against the corelib's declared `API_VERSION` literal); where none exists, emit a **one-time init/load-time assertion** that throws on mismatch — fail fast, never silently.
- **Corelib requirement:** each corelib must expose its API version as a compile-time-visible constant/macro (split into major/minor) for the above to work. (Action item: confirm/standardize the constant name across all eight corelibs.)

### 5.5 Maximum serialized size constant (default buffer size)

Every generated message type **MUST expose a compile-time constant** holding the **theoretical maximum serialized length** of that message — the worst case where every field is present, every `string`/`blob`/`array` is at its declared `maxlen`/`count`, and every varint is at its maximum width.

- **Realized per language:** C++ `static constexpr std::size_t _maxSize` (already used by `OStreamInline<_maxSize>`); C `#define <PREFIX>_<MSG>_MAX_SIZE`; Rust `pub const MAX_SIZE: usize`; Go `const <Msg>MaxSize`; Java `public static final int MAX_SIZE`; C# `public const int MaxSize`; Python class attr `MAX_SIZE`; TS `static readonly MAX_SIZE`.
- **It is the default buffer size** for every message buffer the generated code allocates, stack or heap — this is what `buffer.size: auto` (§7.3) resolves to. A buffer of this size guarantees a single `serialize` call always fits with no reallocation and no flush needed.
- **The user MUST be able to override it** with a smaller buffer when they intend to chunk/stream (§5.6): C++ via the template parameter (`OStreamInline<N>` / `OStreamObject<Msg, N>`); Rust by passing a smaller slice to `OStream::new(&mut buf)`; C by passing a smaller `uint8_t buf[K]`; managed languages by passing a smaller buffer or using the streaming sink. A too-small buffer simply forces the corelib's flush/chunk path.
- **Boundedness:** the constant is finite only for fully-bounded messages (fixed `array`, `maxlen` `string`/`blob`, nested `struct`/`union`). A message containing a variable-length **`sequence`** has **no finite maximum** — for those the generator omits the constant (or emits a documented "unbounded" sentinel) and the streaming path (§5.6) is mandatory.

#### How the maximum is computed

The generator owns a small **wire-format cost model** and computes the bound **analytically from the IR** — it does *not* run each target corelib (that would break the single, cross-platform `sbufgen` binary and need every language's toolchain at generation time). This is safe because the **wire format is identical across all corelibs**, so one language-independent calculation serves every backend. The model sums an **upper bound** per field (it bounds the encoder; it does not re-implement it):

- **field header** `(id<<3)|wiretype` → `varintLen(id<<3 | 7)` bytes;
- **unsigned/signed scalar** → header + max value varint for that width (`u8`→2 … `u64`→10; signed zig-zag same widths);
- **bool** → header + 1; **fp32** → header + 1 (fixlen subheader) + 4; **fp64** → header + 1 + 8;
- **string/blob `maxlen N`** → header + `varintLen(N<<3|sub)` + N;
- **numeric `array` count C** → header + `varintLen(C)` + C × (element max varint / fixed width);
- **`array` of `string`** → sequence framing + C × (string field cost) + 1 (it is a sequence of strings, §5);
- **`struct`** → `sequence_begin` header + Σ(children) + 1 (`sequence_end`); **`union`** → framing + **max** over options; recurse for nesting.

> *Keeping it honest against the real libs (the "use the corelib" half of the requirement):* the cost model is versioned with the wire format, and the **conformance suite (§9) asserts that the corelib's actual encoded size for worst-case data never exceeds the generated constant** (and is reasonably tight). So the size is *computed* analytically but *verified* against the genuine corelib output on every CI run — if a corelib ever changed its encoding, the assertion would catch the drift.

### 5.6 Streaming — chunked send & receive

Generated code **MUST support processing a message in small chunks**, not only whole-buffer-at-once, so large messages work on memory-constrained devices and over byte streams. The corelibs already provide the primitives; generated code exposes them:

- **Receive (decode) — incremental `feed()`:** the corelibs' `IStream` is a resumable decoder; generated decode wraps `feed(bytes)`, which may be called repeatedly with **arbitrary chunk sizes (down to 1 byte)** — decoder state persists across calls until the message completes (verified for C/C++/Rust; same for Java/C#/TS). Go/Python decode by pull-parsing over an `io.Reader`/reader, which is itself a stream.
- **Send (encode) — flush callback / sink:** when the output buffer is smaller than the message, the corelib invokes a **flush callback** as the buffer fills, emitting bytes in chunks and reusing the buffer (`buffer_set`). Generated encode surfaces this: C `sofab_ostream_init(…, flush_cb, usr)`, Rust `OStream::with_flush`, Java/C# `FlushSink`, Python `Encoder.over_buffer(…, flush=)`, TS `new OStream(buf, off, flush)`; Go encodes straight to an `io.Writer`.
- **Two mechanisms, by target class (as required):**
  - **Embedded-friendly (C, C++ embedded, Rust no_std):** *only* the **`feed()` + flush-callback** model — no heap, fixed small buffers, callbacks push/pull the bytes.
  - **Non-embedded targets (Go, Python, Java, C#, TS, and max-speed C++):** additionally offer idiomatic **stream** integration — encode to a language `Writer`/`OutputStream`/stream and decode from an input stream/reader — layered on the same feed/flush primitives.
- The §5.5 max-size constant is the default for the simple "one buffer, one shot" path; the streaming path is what a user opts into by deliberately supplying a **smaller** buffer.

### 5.7 Bounded storage & required `maxlen` per target (generator-side check)

`maxlen` on `string`/`blob` is **optional in the JSON Schema** — a definition may omit it, meaning "no declared upper bound." That is fine for targets that can allocate dynamically (Go/Python/Java/C#/TS, and any backend configured to use heap/growable storage: `std::string`, `Vec<u8>`, `heapless` with a runtime cap, etc.). It is **not** fine for targets where the receive/field buffer must exist as fixed, statically-sized storage and dynamic allocation is forbidden or disabled.

The canonical case is embedded **C**: a string field becomes `char name[N];` and a blob becomes `uint8_t data[N];` — **the generator must know `N`**, and `N` comes from `maxlen`. With no `maxlen` there is no `N`, so the field cannot be emitted as fixed storage at all. An **array of `string`/`blob`** extends this one dimension: its `items.maxlen` (also optional) supplies the per-element bound, so a fixed-storage target emits a **2-D buffer** — e.g. C `char data[count][maxlen];` — and the same presence check applies to `items.maxlen` for those targets. The same applies to:

- **C** (`corelib-c-cpp`, `object.h`) with `string_storage: fixed_inline` (the default) — `maxlen`-sized `char[]`/`uint8_t[]`.
- **`no_std` Rust** (`corelib-rs-no-std`) with `string_storage: fixed` / `heapless` — bounded inline capacity needs the bound.
- **C++ embedded** when using inline/stack storage sized from `maxlen`.
- Any target whose `buffer.mode: stack` / fixed-capacity storage is selected.

Because this requirement is **per-target and per-config** (it depends on the chosen storage strategy, not on the definition alone), it **cannot** be expressed in the shared JSON Schema — making `maxlen` schema-required would wrongly reject definitions that are perfectly valid for the dynamic-allocation backends. Instead it is a **generator-side, language-specific semantic check**, run after IR construction, in the affected backend (or its Strategy):

> **Requirement:** when a backend (or its active config) uses fixed/static storage for `string`/`blob`, the generator **MUST** verify that **every** `string`/`blob` field reachable in the IR (including nested structs/unions and `array`-of-`string`/`blob` elements) has a `maxlen`. If any lacks one, the generator **emits a clear, located error naming the field and the target, and aborts non-zero with no output** (same hard-gate semantics as schema validation, §1) — e.g. *"field `TelemetryFrame.name` (string) has no `maxlen`, required by target `c` with `string_storage: fixed_inline`."* The dynamic-allocation backends skip the check.

This belongs to the analysis/semantic layer as a **target-aware validation Strategy** (§8.5): the language-independent IR stays permissive (`maxlen` optional), and each backend contributes the constraints its storage model demands. It also ties into §5.5: a `string`/`blob` **without** `maxlen` is **unbounded**, so — like a variable `sequence` — it has no finite max-serialized-size contribution and forces the streaming/dynamic path; a fixed-storage target rejects it for exactly that reason.

---

## 6. Optimization Strategy for Generated Code

The corelib owns the wire codec, so the generator's job is to emit a wrapper that adds **zero overhead** and steers each backend into its corelib's fast/small path. Strategy splits into cross-cutting rules and per-profile rules.

There are really only **two optimization axes**, and every backend sits on one of them:

1. **Minimal footprint** — the embedded targets (`corelib-c-cpp` C and C++ wrapper, `corelib-rs-no-std` no_std). Optimize for code/RAM size and no heap; speed is secondary.
2. **Maximum speed / throughput** — everything else. *"Max speed" (C++) and "high throughput" (Go/Python/Java/C#/TS) are the **same goal**, not two different ones* — go as fast as the language allows. The label differs only because the **ceiling** differs: header-only C++20 can reach the bare-metal limit (full inlining, zero-copy `string_view`, stack buffers, no GC), whereas the managed/GC or interpreted runtimes optimize for speed *within their runtime's constraints* (minimize allocations/GC pressure, avoid boxing/reflection). Same intent, different achievable peak. The techniques below are shared; only how close each gets to the metal varies.

### 6.1 Cross-cutting (all backends)

- **Stay on the corelib's typed fast path.** Always call the dedicated typed writers/readers (`write_unsigned`, `write_fp32`, `write_array_*`, …). Never touch the wire format from generated code — encoding (varints, byte order, framing) is the corelib's job and its hand-tuned path.
- **Emit fields in ascending id order.** Monotonic ids let the decoder dispatch (and, where applicable, the encoder layout) optimally and keep output deterministic.
- **`switch` on field id for decode**, not a chain of `if`s — lets compilers build a jump table. Unknown ids fall through to the corelib's skip path (forward/backward compatibility for free).
- **Resolve everything at generation time.** Field ids, type mappings, enum backing widths, array element kinds/counts, and string/blob `maxlen` are all known statically — bake them in as constants/literals so nothing is computed at runtime.
- **No reflection / no runtime schema.** All dispatch is concrete generated code; there is no descriptor interpretation at runtime (except C, which deliberately uses a static descriptor table — see below).
- **Pick the narrowest correct type.** Map each integer to its exact width. Enum values are **signed 32-bit** and encoded as **signed** (zig-zag varint) on the wire, so back each enum with the smallest **signed** integer width (`i8`/`i16`/`i32`) that covers its value range. Avoid widening on the hot path.
- **Validate cheaply or not at all on the hot path.** Bounds checks (`maxlen`, array `count`) are generated as `debug`-only assertions (or an opt-in `--validate` mode), so release builds pay nothing.
- **Honor defaults & `deprecated` without branches** where possible: initialize members to schema defaults at construction; emit deprecated fields behind a generation flag.

### 6.2 Embedded / minimal-footprint backends

> All three embedded backends must also emit the **compile-time capability guards** of §5.4 so a feature-stripped corelib build fails loudly instead of silently.

**C (`corelib-c-cpp`, `object.h`):**
- **Use the `object.h` API exclusively** — the static descriptor table + object encode/decode API — for *every* construct, including **unions** and **variable sequences of structs**. The `corelib-c-cpp` repo ships **examples and unit-tests covering exactly these cases**; treat them as the reference shape for the generated C code. There is no separate hand-rolled `ostream`/`istream` path to maintain.
- Emit a `struct` + a **static `const` descriptor table** (`SOFAB_OBJECT_FIELD*` macros) per object; descriptors live in flash/`.rodata`, not RAM.
- **No heap, no recursion on data**: nested structs via the `nested_list` index; fixed arrays sized from `count`; strings/blobs into caller-provided fixed buffers sized from `maxlen`.
- Generate `#if` guards / respect `SOFAB_DISABLE_{FIXLEN,ARRAY,SEQUENCE,FP64,INT64}_SUPPORT` so unused wire paths compile out — pay only for types a message actually uses.
- Select the smallest descriptor profile (`SMALL`/`MEDIUM`/`BIG`) that fits the message's ids/offsets/sizes to shrink the tables.

**C++ embedded (`corelib-c-cpp`, `sofab.hpp`):** `OStreamInline<N>` stack buffers (no heap), `_maxSize` computed from a static upper bound; `IStreamObject<T>` decode; bounded inline storage for strings/arrays.

**Rust (`corelib-rs-no-std`, no_std):**
- Generated code is `#![no_std]`-clean: no `alloc` by default; sequences/strings use caller buffers or `heapless`-style bounded storage from `maxlen`.
- Emit `sofab::require!(...)` for exactly the capabilities a message needs (auto-derived from the definition — `fixlen`/`array`/`sequence`/`fp64`/`value64`) so a mismatch with the linked `sofab` build is a **compile error**, not a runtime surprise.
- `#[inline]` on the small serialize/visit shims; emit **no `value64` requirement when every field fits in 32 bits**, so a 32-bit `sofab` build (≈20% smaller on 32-bit MCUs) remains usable.

### 6.3 Max-speed backend (`corelib-cpp`)

- Header-only C++20: keep serialize/deserialize **`noexcept` and trivially inlinable**; let the compiler fuse the generated wrapper into the corelib's `if constexpr` paths so the abstraction vanishes.
- **Stack buffers via `OStreamInline<_maxSize>`** — compute `_maxSize` as a tight compile-time upper bound (sum of per-field header + worst-case payload incl. `maxlen`) so no heap and no reallocation.
- **Zero-copy decode** where the field's lifetime allows: read strings as `std::string_view` into the source buffer instead of copying; arrays into `std::span`/`std::array`.
- Chain writes through the corelib's `Result` so the first error short-circuits without per-call branching in user code.
- Build the example/tests at `-O2`/`-O3` (the library targets speed).

### 6.4 High-throughput managed backends (Go / Python / Java / C# / TS)

- **Minimize allocations:** reuse encoder buffers; prefer primitive locals over boxed types; in decode, fill fields directly from visitor callbacks rather than building intermediate maps.
- **Visitor `switch` on id** with default no-op methods (override only handled ids); unknown fields skipped by the corelib.
- Language-specific: Go — avoid `interface{}`/reflection, use the generic array writers; Java/C# — primitive arrays (`int[]`, `long[]`) not boxed collections on the hot path; TS — `bigint` only where 64-bit range is required, `number` otherwise.
- Provide a streaming flush path (callback/sink) for messages larger than a single buffer without growing unboundedly (the §5.6 streaming requirement).

### 6.5 Measuring it

Each corelib ships benchmarks (`bench/`, `benches/`, `perfbench.py`, …). Wire generated-code round-trips into those harnesses and track encode/decode throughput + (for C/C++/Rust) instruction counts via Callgrind and binary size, so the "optimized" claim is regression-tested in CI, not assumed.

---

## 7. Configuration

Output is driven by an optional **config file** (YAML *or* JSON) in addition to the definition file(s). The config splits into a **generic block** (options shared by every backend) and **per-target-language config objects** (each language has specialities, so each gets its own object that may add language-specific keys and override the generic ones).

### 7.1 Sources & precedence

The config file is the single source of truth; the CLI (§8.8) stays tiny on purpose.

- Config file passed with `--config <file>` (the only way other than convenience path flags to influence output).
- **Precedence (highest wins): per-target config → generic config → built-in default.** So a per-language `namespace` overrides the generic `namespace`.
- The **only** CLI overrides are the input/output folders (`--in`/`--out`), which — when given — take precedence over `generic.input_dir`/`output_dir`. There are no other per-option flags; everything else is a config key, so a run is fully reproducible from the committed config file.
- Only the target sections you actually generate need to be present; everything else falls back to generic/defaults.

#### Config JSON Schema (required)

The config file format **MUST be described by its own JSON Schema**, shipped in the repo as `schema/sbufgen-config-schema.json` (alongside the message schema, `sofabuffers-schema-v1.json`). It is a first-class, maintained artifact:

- **The generator MUST validate every config against this schema before use** — same hard-gate semantics as the input-definition validation (§1): on any violation it emits a clear, located error and **aborts non-zero with no output**. The merge order in this section runs only *after* the config validates.
- **`additionalProperties: false`** at every level (generic block, each `targets.<lang>` object, nested objects like `buffer`) so typos and unknown keys are rejected instead of silently ignored.
- The schema encodes the allowed keys, types, and enums for everything in §7.2–§7.3 — e.g. `emit ∈ {sources, project}`, `buffer.mode ∈ {stack, heap}`, `validation ∈ {debug, always, off}`, C `descriptor_profile ∈ {auto, small, medium, big}`, etc. — and may carry defaults/descriptions for tooling and editor IntelliSense. (Capabilities are auto-derived, so there is no `features` key to describe — §7.3.)
- **Sync requirement: the schema must be updated in the same change as any config addition/rename/removal.** Config keys and their schema are kept in lockstep; a CI check fails the build if they drift — e.g. assert every documented/handled key is described by the schema and vice-versa, and validate the committed example configs (`generator-old/test/.../sbufgen.yaml`, the §9.4 config matrix) against it.

### 7.2 Generic block

Options common to all languages, e.g.:

| Key | Meaning |
|---|---|
| `namespace` | Base namespace/module for generated code (per-language overridable). |
| `emit` | What to produce: `sources` (just the message code, default) or `project` (a full buildable **root project** — build manifest + devcontainer wiring + encode/decode harness, §9.1). |
| `input_dir` | Folder of definition files to read (overridable by `--in`). |
| `output_dir` | Where files are written (overridable by `--out`). |
| `timestamp` | Emit the generation timestamp in the file header (`true`/`false`). Set `false` — or honor `SOURCE_DATE_EPOCH` — for byte-reproducible, diff-friendly output. |
| `timestamp_format` | e.g. ISO-8601 (default). |
| `tool_banner` / `license` | Header banner text and SPDX id. |
| `emit_deprecated` | Include or omit fields marked `deprecated`. |
| `validation` | Generated range/`maxlen` checks: `debug` (asserts only) / `always` / `off`. |
| `naming` | Case policy for type/field names if you want to override per-language defaults. |

### 7.3 Per-target config objects

Each backend gets its own object under `targets:`; it inherits the generic block and adds language-specific knobs. The full set below is grounded in each corelib's actual build/config surface (defaults shown match the corelib's own conventions). Every key here is described by the config JSON Schema (§7.1).

**Common keys (all targets, override the generic block):** `namespace`/module/package · `buffer {mode,size}` (§7.3 buffer subsection) · `emit` (`sources`/`project`, §9) · `file_layout` (`one_file` | `file_per_message`) · `decode_style` where the corelib offers a choice (see per-target) · `validation` · `naming`.

#### `c` — corelib-c-cpp, `object.h` (embedded C)

| Key | Allowed / type | Default | Notes |
|---|---|---|---|
| `symbol_prefix` | string | `sofab_` | namespacing for a language without namespaces (prefixes every type/function). |
| `c_standard` | `c99` \| `c11` | `c99` | corelib is C99. |
| `header_extension` / `source_extension` | string | `.h` / `.c` | |
| `include_guard` | `ifndef` \| `pragma_once` | `ifndef` | |
| `descriptor_profile` | `auto` \| `small` \| `medium` \| `big` | `auto` | → `SOFAB_OBJECT_DESCR_PROFILE`; `auto` = the narrowest that fits the message's ids/sizes (override only to force wider). The C backend always uses the `object.h` API (§6.2); there is no alternative API-style knob. |
| `string_storage` | `fixed_inline` \| `caller_buffer` | `fixed_inline` | `maxlen`-sized `char[]` vs pointer+len (no heap either way). |
| `buffer` | see below | `{stack, auto}` | |

> **Capabilities are auto-derived, not configured.** The generator computes the required capability set (`fixlen`, `array`, `sequence`, `fp64`, 64-bit `int`, …) directly from the definition — it knows which wire features a message needs — so there is **no `features`/`value_width` knob**. It emits the §5.4 compile-time guards for exactly those capabilities (the build fails clearly if the linked corelib was built with the matching `SOFAB_DISABLE_*`), and selects the narrowest integer width that fits. `integer_overflow_check` / `object_api` are corelib build choices the generator does not need from config.

#### `cpp-embedded` — corelib-c-cpp, `sofab.hpp` (embedded C++)

| Key | Allowed / type | Default | Notes |
|---|---|---|---|
| `namespace` | string | `sofab` | |
| `cpp_standard` | `c++17` \| `c++20` | `c++17` | wrapper minimum (verify when wiring the backend). |
| `buffer` | see below | `{stack, auto}` | `OStreamInline<N>`/`OStreamObject<T>` vs `OStream`. |
| `max_size_strategy` | `upper_bound` | `upper_bound` | how `_maxSize` (§5.5) is computed. |

> Capabilities are **auto-derived** (as for `c` above) — no `features`/`value_width` knob; the wrapper builds on the C core, so the §5.4 guards cover the same `SOFAB_DISABLE_*`.

#### `cpp` — corelib-cpp (max-speed, header-only C++20)

| Key | Allowed / type | Default | Notes |
|---|---|---|---|
| `namespace` | string | `sofab` | |
| `cpp_standard` | `c++20`+ | `c++20` | corelib requires C++20. |
| `header_only` | bool | `true` | one header per message vs a single bundle (`file_layout`). |
| `buffer` | see below | `{stack, auto}` | `OStreamInline<N>` vs `OStream` (`shared_ptr` buffer). |
| `max_size_strategy` | `upper_bound` | `upper_bound` | drives `OStreamInline<_maxSize>`. |
| `zero_copy_strings` | bool | `true` | decode `std::string_view` into the source buffer vs copy to `std::string`. |

#### `rust` — corelib-rs-no-std (no_std)

| Key | Allowed / type | Default | Notes |
|---|---|---|---|
| `module` | crate path | crate root | |
| `edition` | `2021`+ | `2021` | |
| `no_std` | bool | `true` | `#![no_std]`; set `false` for server builds. |
| `alloc` | bool | `false` | allow `alloc` (`Vec`/`String`) when not on bare metal. |
| `string_storage` | `fixed` \| `heapless` \| `alloc_string` | `fixed` | bounded inline (no_std) vs `heapless::String` vs `alloc::String`. |
| `derives` | list | `[Debug, Clone, Default, PartialEq]` | extra `#[derive(...)]` on generated types. |
| `buffer` | see below | `{stack, auto}` | stack `[u8; N]` vs `Vec`/boxed slice. |

> Capabilities are **auto-derived** — no `features`/`value_bits` knob. The generator emits `sofab::require!(…)` for exactly the capabilities the message needs (incl. `value64` only when a 64-bit field is present), and — when `emit: project` — enables exactly those features on the `sofab` dependency in the generated `Cargo.toml`.

#### `go` — corelib-go (throughput)

| Key | Allowed / type | Default | Notes |
|---|---|---|---|
| `package` | identifier | `messages` | |
| `module_path` | string | — | for the generated `go.mod` when `emit: project`. |
| `go_version` | string | `1.21` | corelib's `go.mod` is `go 1.21`. |
| `decode_style` | `pull` \| `visitor` | `pull` | corelib offers both (`Next()/Skip()` pull parser, or `Visitor`/`AcceptBytes`). |
| `accessors` | `fields` \| `getters` | `fields` | exported struct fields vs getter methods. |

#### `python` — corelib-py (throughput)

| Key | Allowed / type | Default | Notes |
|---|---|---|---|
| `package` | dotted name | — | import package for the generated module. |
| `python_min` | string | `3.9` | corelib `requires-python >=3.9`. |
| `class_style` | `dataclass` \| `slots_dataclass` \| `plain` | `dataclass` | corelib itself uses dataclasses. |
| `frozen` | bool | `false` | frozen (immutable) dataclasses. |
| `type_hints` | bool | `true` | corelib is typed (`py.typed`). |
| `decode_style` | `pull` \| `visitor` | `pull` | corelib offers `next()/skip()` and `Visitor`+`drive()`. |

#### `java` — corelib-java (throughput)

| Key | Allowed / type | Default | Notes |
|---|---|---|---|
| `package` | dotted name | — | |
| `java_version` | int | `17` | corelib `maven.compiler.release = 17`. |
| `use_records` | bool | `false` | Java 17 `record` types vs mutable classes (records suit immutable decode results). |
| `build` | `maven` \| `gradle` | `maven` | for `emit: project` (corelib uses Maven, `org.sofabuffers:sofab`). |
| `group_id` / `artifact_id` | string | — | packaging coordinates for the generated project. |

#### `csharp` — corelib-cs (throughput)

| Key | Allowed / type | Default | Notes |
|---|---|---|---|
| `namespace` | string | `sofab` | corelib `RootNamespace = sofab`. |
| `target_framework` | string | `net9.0` | corelib targets `net9.0` (allow `net8.0`/`netstandard2.1` for wider reach). |
| `nullable` | `enable` \| `disable` | `enable` | corelib `<Nullable>enable</Nullable>`. |
| `lang_version` | string | `latest` | |
| `use_records` | bool | `false` | `record` types vs classes. |
| `generate_doc_file` | bool | `true` | XML doc output (corelib enables it). |

#### `typescript` — corelib-ts (throughput)

| Key | Allowed / type | Default | Notes |
|---|---|---|---|
| `module` | `esm` \| `cjs` \| `dual` | `esm` | corelib publishes **dual** ESM+CJS. |
| `package_name` | string | — | e.g. `@scope/messages`; corelib is `@sofabuffers/corelib`. |
| `ts_target` | string | `ES2020` | corelib `target: ES2020`. |
| `node_min` | string | `18` | corelib `engines.node >=18`. |
| `bigint_policy` | `when_needed` \| `always` \| `number` | `when_needed` | use `bigint` for 64-bit fields, `number` otherwise (corelib reads scalars as `bigint`). |
| `emit_dts` | bool | `true` | emit `.d.ts` declarations. |
| `decode_style` | `visitor` | `visitor` | corelib decode is visitor-based (`sequenceBegin` may return a child visitor). |

#### Buffer placement: stack vs heap (`buffer`)

Where the languages support choosing **where the encode buffer lives**, it is configurable per target via a `buffer` object — modelled on the C++ corelib's two encoder classes:

- `buffer.mode: stack | heap` (default per target).
- `buffer.size: <bytes>` (or `auto` → the message's computed **max-size constant**, §5.5), used for the stack case. A smaller explicit value opts into the chunked/streaming path (§5.6).

Mapping to each corelib (verified):

| Target | `stack` | `heap` |
|---|---|---|
| **C++ (`corelib-cpp`)** | `OStreamInline<N>` (in-class array) | `OStream` (owns a `shared_ptr<uint8_t[]>`) |
| **C++ embedded (`corelib-c-cpp`)** | `OStreamInline<N>` / `OStreamObject<T>` | `OStream` |
| **C (`corelib-c-cpp`)** | generated code declares a stack `uint8_t buf[N]` for `sofab_ostream_init` | caller/alloc-provided buffer |
| **Rust (`corelib-rs-no-std`)** | `OStream::new(&mut [0u8; N])` over a stack array | `OStream::new(&mut vec![..])` / boxed slice |
| **Go / Python / Java / C# / TS** | n/a — managed runtimes; buffers are heap/GC-managed. The key is accepted but only `heap` is meaningful (a stack request is ignored with a warning). |

So the same toggle that picks `OStreamInline` vs `OStream` in C++ is exposed uniformly for the native/embedded backends that can honor it, and is a no-op (documented) for the managed ones.

### 7.4 Example

```yaml
# sbufgen.yaml
generic:
  namespace: my.proj.messages
  output_dir: ./generated
  timestamp: false          # reproducible builds (or rely on SOURCE_DATE_EPOCH)
  license: MIT
  emit_deprecated: true
  validation: debug
targets:
  cpp:
    namespace: myproj::msg
    cpp_standard: c++20
    zero_copy_strings: true
    buffer: { mode: stack, size: auto }   # OStreamInline<_maxSize>
  c:
    symbol_prefix: myproj_
    c_standard: c99
    descriptor_profile: auto              # narrowest profile that fits (auto-derived)
    string_storage: fixed_inline
    buffer: { mode: stack, size: 256 }    # uint8_t buf[256]
    # NOTE: no `features` — required capabilities (fixlen/array/sequence/fp64/int64)
    # are auto-derived from the definition and enforced by the §5.4 compile-time guards.
  rust:
    module: myproj::messages
    edition: "2021"
    no_std: true
    string_storage: heapless
    derives: [Debug, Clone, Default, PartialEq]
    buffer: { mode: heap }                # OStream over a Vec/boxed slice
    # no `features`/`value_bits` — auto-derived; the generated Cargo.toml enables
    # exactly the sofab features this message needs.
  go:
    package: messages
    go_version: "1.21"
    decode_style: pull
  python:
    package: myproj.messages
    class_style: dataclass
    frozen: true
  java:
    package: com.myproj.messages
    java_version: 17
    use_records: true
    build: maven
  csharp:
    namespace: MyProj.Messages
    target_framework: net9.0
    nullable: enable
    use_records: true
  typescript:
    module: dual
    package_name: "@myproj/messages"
    bigint_policy: when_needed
```

The same document is accepted as JSON. A built-in `sbufgen config --print-defaults` (or similar) emits the fully-resolved effective config for a given target so users can see exactly what was applied.

---

## 8. Generator Architecture

**Requirement:** the generator uses a **modular architecture built on established design patterns** — **Composite** (model), **Visitor** (generation), **Builder** (source construction), and **Strategy** (configurable behaviour) — to ensure maintainability, extensibility, and support for multiple output targets. Parser, model, visitors, and builders are **independent components with well-defined interfaces**; a new target or behaviour is added by writing a new Visitor or Strategy, **never by modifying the core model**.

Generation pipeline:

```
config (§7) ┐ (resolved: defaults → generic → per-target; --in/--out override paths only)
            ▼
YAML / JSON ─▶ [1] Parser            parse + JSON-Schema validate (hard gate, §1)
            ─▶ [2] Generic model     language-independent domain model (Composite)
            ─▶ [3] Analysis          $ref / dependency resolution + semantic checks
            ─▶ [4] IR                language-independent Intermediate Representation (Composite)
            ══ Language Selection Point ══  ← the ONLY place a target language is chosen
            ─▶ [5] Generator backend Visitor (traverse IR) + Builder (emit files) + Strategies
            ─▶ [6] Formatter         deterministic formatting → generated source
```

**Stages [1]–[4] are entirely language-independent;** a specific generator is selected only **after** the IR exists. The core pipeline never depends on any target language (§8.6, §8.7). The brief's example shows Go output, but the same pipeline emits every target in §1 — the output language is a Strategy (§8.5).

### 8.1 Input model & parser

- The input model is a **YAML file** (JSON accepted too — same schema).
- The Parser reads it and, as a **hard gate (core requirement, §1)**, validates it against `sofabuffers-schema-v1.json` (incl. the custom keywords `uniqueIds`, `defaultMatchesEnum`) **before any model is built**. On violation: a clear, located error, abort non-zero, **no files written** — nothing downstream sees unvalidated input.
- The validated document is then lowered into the internal object model (§8.2).

### 8.2 Generic model & IR — *Composite pattern*

Two language-independent layers, both **trees of objects in which every code element implements a common `Node` interface** (`accept(visitor)`, `children()`) for uniform recursive traversal:

- **Generic domain model** (`internal/model`) — the direct, validated lowering of the parsed definition. **Node kinds**, one per definition concept: `Package`/`Module`, `Message`, `Struct`, `Union`, `Enum`, `Bitfield`, `Field`, `EnumConst`, `BitfieldFlag`, `ArrayType`, `SequenceType`, `Primitive`, `Ref`, …. Composite nodes (message/struct/union) hold children; leaves (field/primitive) don't.
- **Analysis** (`internal/analysis`) — resolves `$ref` / dependencies into a **shared-type graph** (not duplicated, §3.4), assigns canonical names, and runs the **semantic checks**: unique ids, nesting depth ≤ 256, default-in-range, enum default matches, array element primitive/string, name collisions, reserved-word handling.
- **IR** (`internal/ir`) — the analyzed, normalized, **language-neutral Intermediate Representation** that backends consume. Typical leaf data: `Field { name, id, kind, typeRef, constraints, default, deprecated, description, unit }`; `Kind ∈ {U8..U64, I8..I64, FP32, FP64, Bool, String, Blob, Array(elem,count), Sequence(elem), Enum(ref), Bitfield(ref), Struct(ref), Union(ref)}`.
- The IR is the **freeze point** (settle `sequence`, §3.3, before locking it) and is **independent of any output language** — visitors, builders and strategies depend on it, never the reverse, and **must not modify it** (§8.6). New node kinds extend the model/IR without touching visitors that don't care about them.

### 8.3 Visitors — *Visitor pattern*

- Code generation is implemented as **Visitors that traverse the Composite model**; each visitor (or visitor set) produces one output. One backend = one visitor family: C, C++ embedded, C++ max-speed, Rust, Go, Python, Java, C#, TS.
- A visitor walks the tree, consults the active **Strategies** (§8.5) for decisions, and drives a **Builder** (§8.4) to emit — it decides *what* to generate; the Builder decides *how* it is written.
- Because generation lives in visitors (not in the nodes), **new outputs are added by writing a new visitor** — a docs visitor, a **test/harness visitor** (the §9.1 root-project harness is generated exactly this way), an OpenAPI visitor — **without changing the model or the YAML schema**.
- Interface sketch: `interface Visitor { visitMessage(Message); visitStruct(Struct); visitField(Field); … }` with a default recursive walk a backend overrides only where it cares.

### 8.4 Builders — *Builder pattern*

- Source is **never produced by ad-hoc string concatenation.** A **Builder constructs each source file incrementally** via a structured, intent-level API, managing:
  - package / namespace / module declarations,
  - imports / `#include` / `use`,
  - type definitions, fields, functions/methods,
  - indentation and formatting.
- Builder operations express intent (`addImport`, `beginType`, `addField`, `beginMethod`, `addStatement`, …) and track structural state (current scope, pending imports, used names) so the result is well-formed by construction.
- **Formatting is separated from generation logic:** the Builder/Formatter own layout, indentation and blank-line/brace policy; visitors own content. Any templating, if used at all, is an *internal* detail of a builder for a fixed snippet — never the structural mechanism, never raw cross-file concatenation.

### 8.5 Strategies — *Strategy pattern*

- Generation behaviour is **configurable through interchangeable strategies**, selected from the config (§7) and injected into visitors/builders:
  - **naming convention** (per-language case policy, C symbol prefix),
  - **serialization mapping** (which corelib calls; decode model — pull vs visitor, §4.1),
  - **template / snippet selection**,
  - **output language / framework** (which Visitor + Builder pair runs),
  - **formatting rules** (indent width, brace style),
  - and the per-target knobs of §7.3 (buffer mode, naming, decode style, …). *(Capability features are not a strategy — they are auto-derived from the IR, §5.4/§7.3.)*
- Swapping a strategy changes behaviour **without editing the model or the core visitor logic** — e.g. a different naming strategy, or a `pull` vs `visitor` decode strategy for Go/Python.

### 8.6 Component boundaries, extensibility & determinism

Consolidated **additional requirements & design rules** (enforced by the package boundaries in §8.7):

- **Language-independent before the IR:** everything in stages [1]–[4] (parser, generic model, analysis, IR) is target-agnostic; **the switch to a specific generator happens only after IR creation** (the Language Selection Point).
- **The core pipeline must not depend on any target language.** Generator *contracts* live in the core (`internal/generator`, interfaces only); concrete backends live outside it (`generators/<lang>`). The core imports no backend.
- **Generators must not modify the generic model / IR** — they consume it read-only and emit output; the IR is effectively immutable to backends.
- **Adding a new target language requires only a new generator package** (`generators/<lang>/`) implementing the generator interface — no edits to the core model, pipeline, or YAML schema. Future extensions explicitly in scope: **tests, documentation, OpenAPI specs, additional languages**.
- **Independent components / well-defined interfaces:** Parser, Model, Analysis, IR, Generators, Builders and Strategies are separate; each depends only on published interfaces.
- **Visitor + Builder:** backends traverse the IR with the **Visitor** pattern and produce files with the **Builder** pattern (§8.3–§8.4). **Formatting is separated** from generation (§8.4).
- **Deterministic output:** identical input ⇒ **byte-identical** output (stable node ordering, sorted imports, fixed formatting, reproducible headers via `timestamp:false` / `SOURCE_DATE_EPOCH`) — golden-diff tested in CI (§9).
- Note: varint/zig-zag and the wire format are **not** the generator's concern (the corelib owns them, §4); visitors emit typed calls into the corelib only.

### 8.7 Project structure (generator repository layout)

A modular, compiler-like Go layout (confirms the §2.1 choice) with a **strict separation between generic processing (`internal/`) and language-specific generation (`generators/`)**:

```
codegen/
├── cmd/
│   └── codegen/              # CLI entrypoint (the sbufgen binary, §8.8)
│
├── internal/                 # GENERIC, language-independent core (imports no backend)
│   ├── pipeline/             #   orchestrates stages [1]–[6]
│   ├── parser/               #   YAML/JSON parsing + JSON-Schema validation (hard gate)
│   ├── model/                #   generic domain model (Composite)
│   ├── ir/                   #   Intermediate Representation (language-independent)
│   ├── analysis/             #   $ref/dependency resolution + semantic checks
│   ├── generator/            #   generator interfaces (CONTRACTS ONLY — no language code)
│   └── config/               #   config loading + config-schema validation (§7)
│
├── generators/               # LANGUAGE-SPECIFIC backends (one package per target)
│   ├── golang/               #   each: generator.go · visitor.go · builder.go · templates/
│   ├── typescript/
│   ├── python/
│   ├── c/  · cpp/ · cpp_embedded/ · rust/ · java/ · csharp/   # (full target set, §1)
│   └── …
│
└── tests/                    # cross-cutting tests; per-language root projects live in §9.1
```

- **Package ↔ stage mapping:** `parser`→[1], `model`→[2], `analysis`→[3], `ir`→[4], `generators/<lang>` (Visitor+Builder+templates)→[5], with the Builder/Formatter→[6]; `pipeline` wires them; `config` feeds all.
- **The Language Selection Point is the `internal/generator` interface:** `pipeline` builds the IR, then dispatches to the `generators/<lang>` package registered for `--lang`. The core depends on the *interface*, never on a concrete `generators/*` package (dependencies point inward only).
- A backend package implements the contract and is otherwise free to organize its visitor/builder/templates; templates are an internal builder detail (§8.4), not the structural mechanism.
- *(Repo naming: this is the **new** generator. `generator-old` (§0) holds the superseded TS POC + schema + examples.)*

### 8.8 CLI

The CLI is deliberately **tiny** — everything configurable lives in the config file (§7). The only required arguments are the config path and the target language:

```
sbufgen --config <file> --lang <c|cpp-embedded|cpp|rust|go|python|java|csharp|ts>
        [--in <dir>] [--out <dir>]
```

| Argument | Required | Purpose |
|---|---|---|
| `--config <file>` | yes | Path to the YAML/JSON config (§7). Carries **all** other options. |
| `--lang <target>` | yes | Which backend to generate. Selects the matching `targets:` block. |
| `--in <dir>` | no | Input folder of definition files. Convenience override of `generic.input_dir`. |
| `--out <dir>` | no | Output folder. Convenience override of `generic.output_dir`. |

- That's the whole surface. **No** per-option flags (`--namespace`, `--feature`, package names, descriptor profile, …) — those are config keys only, so behaviour is reproducible from a file checked into the repo rather than scattered across a shell invocation.
- `--in`/`--out` are the only convenience overrides because paths are the one thing that legitimately varies between machines/CI; when present they win over the config's `input_dir`/`output_dir`, otherwise the config (or its defaults) applies.
- Generating for several languages = run once per `--lang` (e.g. in a small script/Make loop), each reading the same config.
- Exit non-zero with a clear, located error on schema-validation failure (definition *or* config).

---

## 9. Testing & Conformance Architecture

Testing is structured around **one buildable "root project" per language** that wraps the generated code, plus a **large, auto-generated matrix of definition files × config files** exercised through those root projects. The whole thing runs **inside the `.devcontainer` each `corelib-*` repo provides**, so every language is built and tested with exactly the toolchain its corelib targets.

### 9.1 Root projects (per-language conformance harness)

For every supported language there is a small, self-contained project under `generator-old/test/<lang>/` that:

- **builds the generated code** for the case under test against that language's corelib, and
- exposes a **uniform CLI harness** with two modes, so the same data can be round-tripped and cross-checked between languages:
  - **decode**: read **serialized bytes** on stdin → decode with the generated type → write the field values as **canonical JSON** on stdout;
  - **encode**: read **canonical JSON** field values on stdin → build the generated message → **serialize** → write bytes on stdout.

That two-way contract is exactly "accept serialized data and decode it, and accept data to serialize it." Because the interchange is language-neutral JSON ⇄ bytes, the harnesses compose:

- **Round-trip** (single language): `json → encode → bytes → decode → json'`, assert `json == json'`.
- **Cross-language interop**: `encode` in language A → bytes → `decode` in language B → JSON, assert equal (proves the shared wire format).
- **Golden vectors**: compare produced bytes to committed vectors (reuse the corelibs' existing shared vectors where they exist, e.g. `corelib-cpp/test/test_vectors.cpp`, `corelib-rs-no-std/tests`, `corelib-py` `vectors.py`).

Suggested layout (the existing `test/codegen-examples/`, `test/example-cpp/`, `test/capability-guard-*` are the seeds):

```
generator-old/test/
  <lang>/                 # root project, builds in corelib-<lang>'s devcontainer
    <build files>         #   C/C++: CMakeLists.txt · Rust: Cargo.toml · Go: go.mod
                          #   Java: pom/gradle · C#: .csproj · Python: pyproject · TS: package.json
    harness/              # the encode/decode CLI — ALSO generated (wired to the message fields)
    generated/            # the generated message sources for the case under test
  corpus/
    defs/                 # the corner-case .yaml definitions (9.3)
    configs/              # the config matrix (9.4)
    vectors/              # golden bytes + expected JSON per case
  runner/                 # orchestrator: for each (def × config) generate→build→run→assert
```

- **Build via devcontainers:** the harness for language X is built/run inside `corelib-X`'s devcontainer image (C/C++ → `corelib-c-cpp`/`corelib-cpp` gcc+cmake; Rust → cargo; Go → go; Java → JDK+maven; C# → .NET; Python → its venv; TS → node). CI uses those images directly so "works in the devcontainer" is what is actually tested.

#### The root projects are GENERATED, not hand-written

The generator is responsible for **scaffolding each root project** so there is always a ready-to-build starting point for both the unit-test matrix (§9.2) and the final conformance tests. Generating it (rather than maintaining N hand-written harnesses) is also *necessary*: the encode/decode harness must map each message's fields ⇄ canonical JSON, which is **message-specific** — so it has to be emitted from the same IR as the message code. When asked to emit a root project, the generator produces, per `(definition-set, language)`:

1. **the message sources** (the normal generated output);
2. **a build manifest** for that language — `CMakeLists.txt` (C/C++), `Cargo.toml` (Rust), `go.mod` (Go), `pom.xml`/Gradle (Java), `.csproj` (C#), `pyproject.toml` (Python), `package.json`/`tsconfig.json` (TS) — wired to depend on the matching corelib;
3. **devcontainer wiring** — a `.devcontainer` that references/reuses `corelib-<lang>`'s image so the project builds with the corelib's own toolchain;
4. **the encode/decode harness `main`** — the uniform CLI (§9.1) whose body is generated from the IR: for each message it knows the fields, so it can build the message from canonical JSON (encode) and serialize a decoded message back to JSON (decode);
5. a small **README**/run script.

The static parts (2–3, and the harness skeleton) come from per-language **project templates** in the generator; the message-aware parts (1, and the JSON⇄type mapping inside 4) are rendered from the IR. Emitting a root project is **config-controlled** (a `project`/`harness` emit option — see §7), so the same minimal CLI (`sbufgen --config … --lang …`) produces either just the sources or a full buildable project. The existing hand-written `test/codegen-examples/`, `test/example-cpp/`, and `test/capability-guard-*` are the **reference shape** these generated projects must match (and the seeds for the templates).

### 9.2 Test driver (the matrix runner)

For every `(definition, config)` pair the `runner/`:

1. runs the generator with an `emit: project` config to **generate the whole root project** (`sbufgen --config <cfg> --lang <lang> --in corpus/defs --out <lang>/`) — message sources + build files + devcontainer wiring + harness;
2. builds the generated root project in the matching devcontainer;
3. **positive cases** → runs encode/decode round-trip + cross-language + golden-vector checks;
4. **negative cases** (a config that disables a capability the def needs) → asserts the **build fails** with the expected capability-guard message (§5.4) — the guard demos in `test/capability-guard-*` are the template;
5. records pass/fail; the full matrix runs automatically in CI.

### 9.3 Definition corpus — every corner case of the schema

Auto-generate / hand-curate a broad set of `.yaml` definitions covering each schema construct at its boundaries:

- **Scalars:** every integer width (`u8…u64`, `i8…i64`) at its type minimum, maximum, 0, and `default`, signed/unsigned boundary values; `fp32`/`fp64` incl. 0, ±max, subnormals, NaN/±Inf, `decimals`; `boolean` true/false/default.
- **Strings/blobs:** empty, length 1, `maxlen`, multibyte UTF-8; blob with base64 default.
- **Arrays:** every numeric element type at `count` 1 and large; **array-of-`string`** (the sequence-of-strings special case, §5).
- **Enums:** contiguous, non-contiguous, **negative** values, explicit vs shorthand, `default`, and `$ref` to `$defs`.
- **Bitfields:** flags at `pos` 0 and 63, defaults true/false, `$ref`.
- **Structs:** flat, nested, **nesting depth 1, 2, … up to the 256 cap and one past it (must fail)**; `$ref` and shared/reused structs; recursive references.
- **Unions:** multiple options, `default_id`, `$ref`.
- **Wire sequences:** `struct`/`union` nesting and `array`-of-`string`/`blob`, exercised at depth (§3.3).
- **Field ids:** `0`, large, non-contiguous, and **duplicate-id (must fail validation)**.
- **Metadata & evolution:** `deprecated` fields; `unit`/`description`/`summary` (assert they reach the doc comments); **forward/backward compat** — decode a message that has extra/unknown ids and assert they are skipped.
- **Negative defs:** out-of-range `default`, a `string`/`blob` `default` longer than `maxlen`, duplicate ids, bitfield `pos` collisions, enum `default` not in set, union `default_id` with no matching option — assert the generator rejects them.

### 9.4 Config matrix — every combination

Auto-generate config files spanning every meaningful combination so coverage is exhaustive, not sampled:

- **target language** × **buffer mode** (`stack`/`heap`, where supported) × **validation** (`debug`/`always`/`off`) × **timestamp** on/off.
- **Capability variants (corelib build, not generator config):** build the corelib/dependency with each capability disabled — C `SOFAB_DISABLE_{FIXLEN,ARRAY,SEQUENCE,FP64,INT64}_SUPPORT`, Rust `sofab` feature off / 32-bit value build — against a message that needs it, to confirm the **auto-emitted** guard (§5.4) fails the build. (These drive the negative cases in 9.2.)
- **C object API:** descriptor profile `small`/`medium`/`big`, incl. a case where ids/sizes exceed a profile (must fail the descriptor-width guard).
- **Buffer / streaming:** `buffer.size: auto` (one-shot) **and** a deliberately **undersized** buffer that forces the chunked **flush-callback / `feed()` streaming path** (§5.6) — the round-trip must still pass.
- **Namespacing:** custom `namespace` / C `symbol_prefix`.

Two cross-cutting assertions the runner adds for every positive case: (a) the **actual encoded byte length ≤ the generated max-size constant** (§5.5), kept tight; and (b) the same message **decodes correctly when fed one byte at a time** (streaming, §5.6).

The matrix is produced programmatically (a small generator that enumerates `defs × applicable configs` and emits the `corpus/configs/` files + the expected outcome — build-ok vs build-fail), so adding a new def or flag automatically expands coverage. Reproducible output (`timestamp:false`) lets golden generated files be diffed in CI.

---

## 10. Phased Roadmap

### Milestones (outcome checkpoints)

High-level, demonstrable checkpoints — each has a single clear "done when" criterion. The detailed task lists per phase follow below.

| # | Milestone | Done when | Phase |
|---|---|---|---|
| **M0** | **Foundations** | The CLI loads a YAML/JSON definition + config, validates against the schema (incl. the six custom keywords, §3.2 / `schema/README.md`), resolves `$ref`, and builds a validated IR for `example.yaml` — no backend yet. **Initial `docs/ARCHITECTURE.md` created.** | 0 |
| **M1** | **Format finalized** | The schema (`v1.x`) + IR are frozen. No further input-format churn expected. | 0 |
| **M2** | **First backend emits compiling code** | `sbufgen --lang c` turns `example.yaml` into C (`object.h`) sources that compile against `corelib-c-cpp`, **with capability guards** (§5.4). | 1 |
| **M3** | **Root-project generator works** | `emit: project` scaffolds a buildable C root project (build files + devcontainer wiring + encode/decode harness, §9.1) that builds **inside `corelib-c-cpp`'s devcontainer**. | 1 |
| **M4** | **Conformance backbone green (C)** | The matrix runner drives the C harness: round-trip + golden shared-vector checks pass automatically. | 1 |
| **M5** | **Embedded + max-speed backends** | `corelib-rs-no-std` (no_std), `corelib-c-cpp` C++ (`sofab.hpp`), and `corelib-cpp` (max-speed) each generate + build + pass round-trip/golden via their root projects. | 2 |
| **M6** | **Throughput backends + interop** | `go`, `python`, `java`, `csharp`, `typescript` backends generate + build in their devcontainers; **cross-language interop** (encode in A, decode in B) is green across all 8 languages. | 2 |
| **M7** | **Exhaustive automated coverage** | The full corner-case corpus (§9.3) × complete config matrix (§9.4) — incl. negative capability-guard and invalid-definition cases — runs across all languages in CI. | 3 |
| **M8** | **1.0 release** | Reproducible/deterministic output is golden-tested; docs + per-language quick-starts published; the release pipeline ships cross-platform static `sbufgen` binaries (win/linux/mac × x86-64/arm64). | 3 |

Status: the **proof-of-concept artifacts below already demonstrate slices of M2/M3/M5** (hand-written but verified: C/C++/Rust/Python generated code builds and round-trips, and the capability guards fire). Turning those into the generator's templates is the M2→M3 work.

**Definition of done for *every* milestone includes updating `docs/ARCHITECTURE.md`** (below).

#### Living architecture doc (`docs/ARCHITECTURE.md`)

The generator repo ships a maintained **`docs/ARCHITECTURE.md`** that is the single, clean, up-to-date description of the generator and how it works — the **first thing a maintainer or a new-language contributor reads**. **Updating it is part of the "done when" criterion of every milestone (M0–M8)**, so the doc never drifts from the code.

It must cover at least:

- **Overview & responsibilities** — what the generator does (definition → typed code), and the firm boundary that the corelib owns the wire format (§4).
- **Architecture & patterns** — the Composite model/IR, Visitor generation, Builder source construction, Strategy configurability (§8.1–§8.5), with a short rationale for each.
- **Pipeline / flows** — the end-to-end stages `Parser → Generic model → Analysis → IR → Language Selection Point → Visitor+Builder → Formatter` (§8 diagram), what each stage consumes/produces, and where the language-independent core ends.
- **Project structure** — the `cmd/` · `internal/` · `generators/` · `tests/` layout and the package↔stage mapping (§8.7), plus the dependency rule (core imports no `generators/*`).
- **How to add a new target language** — a concrete checklist: create `generators/<lang>/` (generator + visitor + builder + templates), implement the generator interface, add the per-target config keys (§7.3) + config-schema entries (§7.1), add the root project/harness + corpus entries (§9), wire it into the matrix runner. This section is the doc's most important deliverable for long-term maintenance.
- **Config & extension points** — config model (§7), capability guards (§5.4), max-size/streaming (§5.5–§5.6), and the planned future outputs (tests, docs, OpenAPI).

`docs/ARCHITECTURE.md` is generated-by-hand prose (not auto-generated) but kept honest by the per-milestone update rule and by code review; diagrams may be ASCII (as in §8) or linked images.

#### Git workflow during development

This is a long, multi-phase effort — **frequent git commits are welcome throughout** (small, descriptive commits as work progresses, on the working branch). In addition, **commit to `main` after each milestone (M0–M8)**: when a milestone's "done when" criterion is met, land that work on `main` as a checkpoint (ideally tagged, e.g. `m3`), so `main` always reflects the last completed milestone and history shows clear progress. Day-to-day commits stay on the development branch; the milestone commit is the integration point onto `main`.

### Proof of concept — already built ✅

A full, buildable example executable lives at **`generator-old/test/example-cpp/`**. It is hand-written but mirrors exactly the generated-code shape this plan specifies for the **max-speed C++ backend** (`corelib-cpp`), and it compiles and round-trips today using the same toolchain the corelib's `.devcontainer` provides (Ubuntu 24.04, `build-essential`, CMake/Ninja):

```
generator-old/test/example-cpp/
├── CMakeLists.txt                       # finds corelib-cpp via SOFAB_CPP_DIR
├── def/myfirstmessage.yaml              # the SofaBuffers definition (schema v1)
├── include/sofabuffers_generated/
│   └── myfirstmessage.hpp               # one type per object: message + 2 nested structs
├── src/main.cpp                         # encode → wire bytes → decode → verify
└── README.md
```

Build & run (verified):
```sh
cd generator-old/test/example-cpp
cmake -S . -B build && cmake --build build && ./build/example
# → encodes 62 bytes, all 9 round-trip checks OK, exit 0
```

It exercises scalars, a negative-valued enum (forcing a signed backing type), a fixed array, and a **two-level nested struct** — validating the per-object-type + `serialize`/`deserialize` pattern and the §6 optimization rules (stack-only `OStreamInline<_maxSize>`, ascending-id writes, `switch`-on-id decode). This is the seed for the Phase-1 conformance harness: once the generator emits this header instead of it being hand-written, the same `main.cpp` becomes a golden round-trip test.

**Capability guards — also built & verified ✅** (§5.4). Against the real embedded corelibs:
- `generator-old/test/capability-guard-c/` — generated `#error` guard header + `run.sh`; compiles clean by default and **fails with a meaningful `#error` for each of** `SOFAB_DISABLE_{FP64,FIXLEN,ARRAY,SEQUENCE,INT64}_SUPPORT`.
- `generator-old/test/capability-guard-rs/` — `#![no_std]` crate with `sofab::require!(...)` + `run.sh`; builds with all features, and with `fp64` disabled fails: *"sofab: this application requires the `fp64` feature, but it is disabled."*

### Phase 0 — Foundations & format finalization
- [ ] Pick generator language (Go recommended) — §2.1.
- [ ] Stand up the **project skeleton per §8.7** (`cmd/codegen`, `internal/{pipeline,parser,model,ir,analysis,generator,config}`, `generators/<lang>`), CLI parsing, CI with cross-compile matrix (win/linux/mac × x86-64/arm64 at minimum). Enforce the dependency rule: the core imports no `generators/*` package.
- [ ] **Parser** + JSON-Schema validation (hard gate) incl. the six custom keywords and the `$data` rules (§8.1, `schema/README.md`).
- [ ] **Config system** (§7): YAML/JSON config loader; author **`schema/sbufgen-config-schema.json`** (`additionalProperties:false`) and **validate every config against it before use** (hard gate, §7.1); generic+per-target merge (+ `--in`/`--out` path overrides); `--print-defaults`; CI check that the schema and the handled config keys stay in lockstep and that all committed example/matrix configs validate.
- [ ] Minimal CLI (§8.8): `--config`, `--lang`, optional `--in`/`--out` — no other flags.
- [ ] **Composite model / AST** (§8.2): `Node` interface + node kinds, `$ref` resolution into a shared-type graph, canonical naming.
- [ ] **Semantic validation** over the model (depth ≤ 256, ranges, enum-default-matches, name/reserved-word checks).
- [ ] **Visitor / Builder / Strategy scaffolding** (§8.3–§8.5): the visitor traversal interface, a Builder API (no string concat), and the strategy hooks — so the first backend slots in cleanly.

### Phase 1 — Reference backend & conformance harness
- [ ] First backend end-to-end (embedded-first ⇒ **C `object.h`**), generating the full `example.yaml`.
- [ ] **Root-project generator (`emit: project`)** (§9.1): per-language project templates (build manifest + devcontainer wiring + harness skeleton) + IR-driven harness body; implement the **C** template first so `sbufgen … --lang c` emits a buildable `generator-old/test/c/` root project (encode/decode CLI) that builds in `corelib-c-cpp`'s devcontainer. Use the existing hand-written `test/codegen-examples/`, `test/example-cpp/`, `test/capability-guard-*` as the reference shape.
- [ ] **Matrix runner skeleton** (§9.2) + seed `corpus/defs` and `corpus/configs`, plus golden `vectors/`.
- [ ] **Shared test-vector conformance:** several corelibs ship cross-language vectors (e.g. `corelib-c-cpp/test/shared`, `corelib-cpp/test/test_vectors.cpp`, `corelib-rs-no-std/tests`, Python `vectors.py`). Wire the harness into a round-trip test that encodes with generated code and checks bytes against these shared vectors. This is the correctness backbone for every later backend.

### Phase 2 — Remaining backends (embedded-first order)
- [ ] `corelib-rs-no-std` (no_std) + `require!` guards.
- [ ] `corelib-c-cpp` C++ (`sofab.hpp`).
- [ ] `corelib-cpp` (max-speed C++20).
- [ ] Throughput langs: `go`, `ts`, `py`, `java`, `cs` (re-verify each encoder API against its repo as you go — §4.1 note).
- [ ] **Root-project template per language** (§9.1) so `emit: project` scaffolds a buildable root project for each, building in its corelib's devcontainer; add each to the matrix runner.
- [ ] Each backend: round-trip + cross-language interop + shared-vector conformance, run automatically.

### Phase 3 — Hardening & DX
- [ ] **Full corner-case corpus** (§9.3) and **complete config matrix** (§9.4), auto-generated; expand the runner to the whole `def × config` product across all languages in their devcontainers.
- [ ] **Negative tests:** capability-guard build-failure cases (§5.4) and invalid-definition rejection cases run as expected-fail in the matrix.
- [ ] Deprecated-field policy; default handling; doc-comment assertions.
- [ ] Deterministic-output golden tests (regenerate to a temp `--out` and diff in CI — no dedicated flag needed).
- [ ] Docs + per-language quick-start; example outputs committed; **`docs/ARCHITECTURE.md` complete** (incl. the "add a new target language" guide) and kept current per the per-milestone rule (§10).
- [ ] Release pipeline producing the cross-platform static binaries.

---

## 11. Key Risks & Open Questions

1. **Decode model differs per language** — visitor (Java/C#/TS/Rust), field-callback/descriptor (C/C++), pull-parser (Go/Python). Backends are not a single template.

---

## 12. Summary

The corelibs already own the hard part — a fast, portable, footprint-tuned wire codec with a uniform streaming API across all eight languages. The generator is therefore a **definition → typed-wrapper** compiler: load + validate YAML against the v1 schema, resolve refs into a shared-type graph, lower to a validated IR, and emit one idiomatic type-per-object with serialize/deserialize glue per language, each tuned to its corelib's profile (embedded/no_std minimal footprint vs. max-speed/throughput). Build it in Go for frictionless cross-platform single-binary distribution, prove it embedded-first against shared conformance vectors, then fan out to the remaining backends.
