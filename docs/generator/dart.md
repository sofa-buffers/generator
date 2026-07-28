# Dart target — `targets.dart`

Target-specific options, accepted under `targets.dart`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

The Dart target takes no options of its own — everything is set in the
[generic config](README.md).

The Dart target has a single corelib — [`corelib-dart`], the **max-speed**
(throughput) port — so there is no `corelib` selector. `sources` emits a single
`message.dart` library; `project` additionally scaffolds `pubspec.yaml`, a
`bin/harness.dart` JSON encode/decode harness, and a `README.md`.

Set the corelib path in the generated `pubspec.yaml` (the
`${SOFAB_DART_CORELIB}` placeholder — a `path:` dependency) before
`dart pub get`. The published package is `sofabuffers`, imported aliased as
`sofab`.

[`corelib-dart`]: https://github.com/sofa-buffers/corelib-dart

## Receiver-side decode limits

The `max_dyn_*` caps are [generic options](README.md); what is specific to this
target is how they land in the generated code — as `maxDynArrayCount`,
`maxDynStringLen` and `maxDynBlobLen` constants. A violation reports the
corelib's `LimitExceeded` outcome.

## Generated shape

One `class <Message>` per object, each field initialized to its schema default
(Dart requires non-nullable fields be initialized) — a fresh object already
carries every default, which is what makes sparse-canonical decode
(MESSAGE_SPEC S2) a no-op for omitted fields (a *reused* object gets there via
`reset()`, below). Enums and bitfields lower to an
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
- `void reset()` — every field back to its declared default, **in place**. See
  [Reusing a destination](#reusing-a-destination); `tryDecode` calls it for you.

### Arrays — `count` is a capacity

Every array field maps to a Dart `List`, and the list's length is the array's
length. A schema `count: N` is a **capacity**, not a length: it never reaches the
wire, it bounds the array (an element count or element id past `N` fails the
decode as INVALID), and it lets fixed-storage targets pre-size — but it never
adds elements.

The consequences you can observe from Dart:

- A fresh `<Msg>()` leaves a `count: N` array **empty** unless the schema
  declares a `default`, and a declared default shorter than `N` is materialized
  exactly as written (never tail-padded to `N`). `reset()` restores the same
  thing. This holds for native and wrapper element kinds alike.
- `marshal` writes **every** element the list holds. `<int>[1, 2, 0, 0]` and
  `<int>[1, 2]` are different values with different bytes.
- Decode yields exactly the elements the wire carried: `length` after a round
  trip equals `length` before it, for both the compact scalar form and the
  wrapper form.
- A field is omitted only when it **equals its default** — for an array with no
  declared default, only when it is empty. An all-zero `<int>[0, 0, 0, 0]` is a
  four-element value and stays on the wire.

Inside a wrapper-sequence array (string/blob/struct/union/nested-array elements)
the **interior is sparse**: an element equal to the element default is dropped
and leaves an id gap, which decode restores from that same default. The **last**
element is always written — as its value, or as an empty frame for a
struct/union/nested element — because its presence is what carries the length.
So `<String>['a', '']`, `<String>['a']` and `<String>[]` are three distinct
values that encode and decode distinctly.

### Encode model — lazy sequence framing (MESSAGE_SPEC §2)

The `≠ default` omit test is per field and a **sequence-typed field is no
exception**, so `marshal` never opens a frame eagerly: every sequence is opened
with `Encoder.beginSequenceLazy(id)`, which holds the header back until a child
field is actually written. Because the nested `marshal` already omits each child
equal to its default, "no child was written" *is* "the value equals its declared
default", evaluated per field and recursively — no buffering and no runtime
whole-object compare.

The **closer** is what decides whether a contentless frame survives:

| position | closer emitted |
|---|---|
| `struct` / `union` field | `e.endSequence()` — frame dropped when empty |
| array field (the wrapper) | `e.endSequence()` — frame dropped when empty |
| wrapper-array **element** (`struct`/`union`/nested row) | `endSequenceKeep()` at the **last** index, `endSequence()` in the interior |

A sequence-typed **field** always drops: an all-default one is omitted and
absence reconstructs it. Consequence: an all-default message encodes to **zero
bytes**.

An **element** is the one place the choice is positional, decided from the index
in the *value* at run time — the schema cannot answer it:

```dart
e.beginSequenceLazy(_i0); objs[_i0].marshal(e);
if (_i0 == objs.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }
```

#### One sparse rule for both element kinds (§2)

An array element **before the last one** that equals its element default is
omitted, leaving an id **gap** the decoder restores from that same default — a
`string`/`blob` leaf is simply not written, a `struct`/`union`/nested-array
element is **not framed** either. The **last** element is always written: a leaf
as its value, a sequence element as an **empty frame**, because a wrapper array
recovers its length as *highest present id + 1* (§5.1) and nothing that carries
the length may be elided.

A leaf element expresses it on the write (`lastElemExpr`):

```dart
if (fstrs[_i0].isNotEmpty || _i0 == fstrs.length - 1) e.writeString(_i0, fstrs[_i0]);
```

a sequence element on the closer (above), and a **native nested row** — which has
no frame of its own, being a single count-prefixed value — on the write again,
with `isNotEmpty` as its element-default test.

A declared `count: N` changes none of this: `count` is a capacity, so it can
never restore an elided tail. The same test applies with or without one.

| value | wire | decodes as |
|---|---|---|
| `['a', '']` | `06 02 0a 61 0a 02 07` | `['a', '']` |
| `['a']` | `06 02 0a 61 07` | `['a']` |
| `['', '']` | `06 0a 02 07` | `['', '']` — final element alone, at id 1 |
| `['', 'x', '']` | `06 0a 0a 78 12 02 07` | `['', 'x', '']` |
| `[]` | *(nothing)* | `[]` |
| `[{k:1}, {k:0}, {k:3}]` | `06 06 00 01 07 16 00 03 07 07` | same — id 1 is a gap |
| `[{k:0}, {k:0}]` | `06 0e 07 07` | same — the last frame survives alone |

The array wrapper may use the dropping closer because a wrapper array's declared
`default` is not materialized by this backend (the generated field starts empty),
so *absent* and *explicitly empty* denote the same value. If that gap is ever
closed, the wrapper needs an `if (value != default) { …; e.endSequenceKeep(); }`
guard so an explicitly-empty value differing from a non-empty default still
reaches the wire as the empty wrapper (§2, §3).

### Decode model

`corelib-dart` exposes the **push child-visitor** decode (like Go): a
`MessageVisitor` whose `onSequenceStart(id)` returns a child visitor for a nested
scope, and whose native arrays arrive whole through a distinct `on*Array`
callback. Three consequences the generated code relies on:

- **MESSAGE_SPEC S7.3 / S7.4 are settled structurally.** A contradictory header —
  a wrong wire type, a fixlen subtype mismatch, or an integer/fp **array** at a
  scalar id — is dispatched by the corelib to a differently-typed callback the
  field's id switch does not handle, so it evaporates (no `askip` guard needed,
  unlike the C#/Rust/Zig ports whose corelibs stream array elements through the
  scalar callbacks). A re-opened `struct`/`union` scope descends into the
  **existing** member (merge); an array wrapper clears its list inside
  `onSequenceStart` (replace) — and because that clear lives in the sequence-only
  callback, a mis-typed later occurrence can never wipe a valid earlier value. It
  is also why it cannot serve a *reused* destination, whose stale value must be
  cleared before the decode starts (see [Reusing a
  destination](#reusing-a-destination)).

- **A wrapper element's id IS its array index (§5.1).** *Every* collector —
  `_ObjSeq` / `_StrSeq` / `_BlobSeq` for the leaf and object kinds, and
  `_IntMat` / `_DblMat` / `_BoolMat` / `_SeqSeq` for the row kinds — gap-fills
  with default elements up to the child id and then decodes **into** `out[id]`,
  never appends. Appending would shorten the array by the size of any interior
  id gap — and an omitted all-default interior element is exactly such a gap —
  and would decode a *re-opened* element id as a second element instead of
  merging into the first (§7.4, which placement gives for free). The over-index
  guard (`id >= cap` is INVALID) also bounds the gap-fill; the row collectors
  take the **outer** array's `cap`, since a row's element id is its index in
  that array.

- **Nothing is filled in after the wire.** The wire count M *is* a compact
  array's length and *highest present id + 1* is a wrapper array's, so the
  elements that arrived are the whole value. A `count: N` bounds M (over-count /
  over-index → INVALID) but never adds elements, so a decoded array has exactly
  the length the sender gave it.

- **INVALID verdicts ride a sticky flag.** The corelib's visitor callbacks return
  `void`, so a generated visitor cannot fail the decode mid-stream. The over-count
  (generator#100), over-index (generator#142) and over-`maxlen` (S7.1) rejects set
  a sticky `_inv` flag shared across the decode; `tryDecode` converts it to a
  terminal `DecodeStatus.invalid` after the corelib returns — the Rust/Zig
  "generated guard, sticky flag" model. The receiver-side `max_dyn_*` limits are
  enforced by the corelib itself (a `DecoderLimits`), the Go/Python/TS family.

### Reusing a destination

`tryDecode(data, out)` lets the caller supply the destination, so the same object
can absorb a stream of messages without reallocating. That entry point starts
with **`out.reset()`**, and it has to: since MESSAGE_SPEC §2 dropped the sequence
carve-out, a `struct`/`union` member or an array field equal to its default is
**not on the wire at all**, so it fires no callback. The §7.4 "a later occurrence
replaces the array whole" clear in `onSequenceStart` therefore cannot run for it,
and a reused `out` would keep the *previous* message's elements — the decoded
array would hold data that is not in the message. The start of the decode is the
only place absence is still observable, so that is where the clear belongs. The
§7.4 sequence-start clear stays exactly as it was: a re-opened wrapper must still
replace, not append.

`reset()` is **public** for the caller who drives the visitor directly rather
than through `tryDecode` (corelib-cpp exposes `IStreamImpl::reset()` for the same
reason), and for recycling an instance between encodes.

It restores each field in place, so a reused instance keeps its backing storage:
a nested `struct`/`union` member is `reset()` recursively rather than
reconstructed, and a list is `clear()`ed (and, when it has a materialized value,
refilled from its declared default's literal). That literal is `const`, which the
compiler canonicalizes — so the refill allocates nothing. Two
kinds are assigned instead, because Dart has no in-place form for them: a `blob`
(`Uint8List` is fixed-length) and an **fp32 array**, whose member holds the
fixed-length `Float32List` that decode installs to keep a signaling NaN's bits
(see below) and on which `clear()` would throw. For the same reason `reset()`
expects the *growable* lists the initializers and the decode path produce; a
caller who assigns a fixed-length list to a list member owns that choice.

`decode(data)` builds a fresh instance, which already carries every default, so
it skips the redundant reset (both routes share a private `_decodeInto`) — the
`bench` decode row is unchanged by this.

### 64-bit integers

Dart's `int` is a signed 64-bit value, and a decimal literal outside
`[-(2^63-1), 2^63-1]` is a compile error. A u64 default `>= 2^63` (and `int64`
min) is therefore emitted as its 64-bit **bit pattern** — the signed-decimal form
(`2^64-1` becomes `-1`, which `writeUnsigned` re-expands to the same bits), or a
hex literal for `int64` min. On the wire this is identical to every other port.
The `project` JSON harness carries u64 values as decimal **strings** for the same
reason (`jsonDecode` reads a large number as a lossy `double`).

### fp32 signaling NaN (issue #226)

A Dart `double` is 64-bit, so widening an `fp32` value through it **quiets** a
signaling NaN and drops a quiet NaN's payload bits (`0x7F800001` → `0x7FC00001`),
violating the MESSAGE_SPEC §4.6 bit-for-bit float round-trip. `corelib-dart`
delivers an fp32 **NaN** through the opt-in raw-bits callback `onFp32Bits(id,
bits)` (not `onFp32`) and re-emits it with `Encoder.writeFp32Bits(id, bits)`, so
the generated code uses that path:

- **Scalar** — each `fp32` field gets a private companion `int? _<name>Fp32Bits`.
  `onFp32Bits` captures the exact 32 wire bits there (and widens a display
  `double` for element access); `onFp32` clears it (a later non-NaN occurrence,
  §7.4, wins). `marshal` re-emits `writeFp32Bits` when the value is a NaN **and**
  bits were captured, else `writeFp32` — the `!= default` omit test is unchanged
  (a NaN never equals the default).
- **Array** — elements bind through `_f32copy`, a **raw byte copy** into a fresh
  `Float32List` (never `List<double>.from`, which widens each element) of exactly
  the wire count — a `count: N` is a capacity and adds no elements;
  `writeFp32Array` re-emits a `Float32List`'s bytes verbatim. Nested fp32 rows
  (`_DblMat`) copy the same way.

The `recode` harness mode (wire → object → wire, no JSON) round-trips a signaling,
a payload/quiet, and a negative NaN — scalar and array — byte-for-byte in
`tests/conformance/dart/run.sh`. fp32 only: an fp64 NaN already survives a
`double`. Verified cross-language by Crucible finding F-0031.

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
