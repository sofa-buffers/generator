# Dart target — `targets.dart`

Options accepted under `targets.dart`. For shared options (`emit`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. `emit: project` scaffolds a buildable package (`pubspec.yaml` + a JSON encode/decode harness). |
| `max_dyn_array_count` | integer | unset = unlimited | Receiver-side decode limit (generator#102): caps the wire element count of arrays the schema left unbounded (no `count`). Baked as `maxDynArrayCount` and passed to the corelib decoder as a `sofab.DecoderLimits`; exceeding it fails decode with `DecodeStatus.limitExceeded` at the count header, before allocation — never a clamp. Raised to the largest schema `count`, so schema-bounded arrays stay governed by their own bound. |
| `max_dyn_string_len` | integer | unset = unlimited | Same, for strings without a schema `maxlen` (`maxDynStringLen`). |
| `max_dyn_blob_len` | integer | unset = unlimited | Same, for blobs without a schema `maxlen` (`maxDynBlobLen`). |

The Dart target has a single corelib — [`corelib-dart`], the **max-speed**
(throughput) port — so there is no `corelib` selector. `sources` emits a single
`message.dart` library; `project` additionally scaffolds `pubspec.yaml`, a
`bin/harness.dart` JSON encode/decode harness, and a `README.md`.

Set the corelib path in the generated `pubspec.yaml` (the
`${SOFAB_DART_CORELIB}` placeholder — a `path:` dependency) before
`dart pub get`. The published package is `sofabuffers`, imported aliased as
`sofab`.

[`corelib-dart`]: https://github.com/sofa-buffers/corelib-dart

## Generated shape

One `class <Message>` per object, each field initialized to its schema default
(Dart requires non-nullable fields be initialized) — a fresh object already
carries every default, which is what makes sparse-canonical decode
(MESSAGE_SPEC S2) a no-op for omitted fields. Enums and bitfields lower to an
`abstract final class` namespace of `static const int` values
(`Someenum.RED`, `Somebitfield.FLAGA`) over the raw wire integer, so negative
enum values (which a plain Dart `enum` cannot express) and 64-bit bitfields work.

| Field kind | Dart storage |
|---|---|
| numeric / enum / bitfield | `int` (Dart's single 64-bit int) |
| fp32 / fp64 | `double` |
| bool | `bool` |
| string | `String` |
| blob | `Uint8List` |
| native numeric/enum/bool/bitfield array | `List<int>` / `List<double>` / `List<bool>` |
| string / blob / struct / union / nested array | `List<String>` / `List<Uint8List>` / `List<T>` / `List<List<…>>` |
| struct / union | the generated class type |

Per message:

- `void marshal(sofab.Encoder e)` — sparse-canonical field writes into any
  caller-configured `Encoder` (fixed buffer, or a flush sink for streaming).
- `Uint8List encode()` — one-shot convenience over `Encoder.encodeToBytes`.
- `static DecodeStatus tryDecode(Uint8List data, <Message> out)` — the
  status-surfacing one-shot decode (MESSAGE_SPEC S7): fills `out` and returns the
  terminal outcome. `invalid` covers both malformed bytes and a schema-bound
  violation (over-count / over-index / over-maxlen); `incomplete` means the bytes
  end inside a field or an open sequence.
- `static <Message> decode(Uint8List data)` — the best-effort convenience (the
  90 % case): returns the message decoded so far, discarding the status.

### Decode model

`corelib-dart` exposes the **push child-visitor** decode (like Go): a
`MessageVisitor` whose `onSequenceStart(id)` returns a child visitor for a nested
scope, and whose native arrays arrive whole through a distinct `on*Array`
callback. Two consequences the generated code relies on:

- **MESSAGE_SPEC S7.3 / S7.4 are settled structurally.** A contradictory header —
  a wrong wire type, a fixlen subtype mismatch, or an integer/fp **array** at a
  scalar id — is dispatched by the corelib to a differently-typed callback the
  field's id switch does not handle, so it evaporates (no `askip` guard needed,
  unlike the C#/Rust/Zig ports whose corelibs stream array elements through the
  scalar callbacks). A re-opened `struct`/`union` scope descends into the
  **existing** member (merge); an array wrapper clears its list inside
  `onSequenceStart` (replace) — and because that clear lives in the sequence-only
  callback, a mis-typed later occurrence can never wipe a valid earlier value.

- **INVALID verdicts ride a sticky flag.** The corelib's visitor callbacks return
  `void`, so a generated visitor cannot fail the decode mid-stream. The over-count
  (generator#100), over-index (generator#142) and over-`maxlen` (S7.1) rejects set
  a sticky `_inv` flag shared across the decode; `tryDecode` converts it to a
  terminal `DecodeStatus.invalid` after the corelib returns — the Rust/Zig
  "generated guard, sticky flag" model. The receiver-side `max_dyn_*` limits are
  enforced by the corelib itself (a `DecoderLimits`), the Go/Python/TS family.

### 64-bit integers

Dart's `int` is a signed 64-bit value, and a decimal literal outside
`[-(2^63-1), 2^63-1]` is a compile error. A u64 default `>= 2^63` (and `int64`
min) is therefore emitted as its 64-bit **bit pattern** — the signed-decimal form
(`2^64-1` becomes `-1`, which `writeUnsigned` re-expands to the same bits), or a
hex literal for `int64` min. On the wire this is identical to every other port.
The `project` JSON harness carries u64 values as decimal **strings** for the same
reason (`jsonDecode` reads a large number as a lossy `double`).

## Reserved-word and type-name field names

A schema field whose name is a Dart reserved word (`class`, `for`, `return`, …)
**or a core type name** (`int`, `double`, `String`, `List`, …, which would shadow
the type the generated code references) is mangled with a trailing underscore
(`int_`). The wire is id-keyed and the JSON name stays the original, so the
mangling is source-only. The `keywords.yaml` corpus exercises this.

## Benchmark row

Row `dart` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured
with the **subtract** method (the Dart VM JITs the hot path, so there is no native
symbol to toggle on). Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.

## Strict UTF-8 (issue #85)

A `string` is materialized inside the corelib (`corelib-dart` validates strictly
and never substitutes `U+FFFD`, MESSAGE_SPEC S8), so the generator emits no UTF-8
code for strings — the check is corelib-side, both encode and decode.
