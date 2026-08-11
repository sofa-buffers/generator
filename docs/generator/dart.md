# Dart target — `targets.dart`

Target-specific options, accepted under `targets.dart`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size. It reaches generated Dart only for a message the schema cannot bound, where it is emitted as `maxSizeLimit`; set explicitly it is also a budget a computed worst case may not exceed. Full semantics in the [generic config](README.md) and ARCHITECTURE §9.6. |

Apart from that the Dart target takes no options of its own — everything else is
set in the [generic config](README.md).

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

- `void serialize(sofab.Encoder e)` — sparse-canonical field writes into any
  caller-configured `Encoder` (fixed buffer, or a flush sink for streaming).
- `Uint8List encode()` — the one-shot entry point. It **allocates the output
  storage itself** and hands it to the corelib; see
  [The caller owns the encode buffer](#the-caller-owns-the-encode-buffer).
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
- `serialize` writes **every** element the list holds. `<int>[1, 2, 0, 0]` and
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
exception**, so `serialize` never opens a frame eagerly: every sequence is opened
with `Encoder.beginSequenceLazy(id)`, which holds the header back until a child
field is actually written. Because the nested `serialize` already omits each child
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
e.beginSequenceLazy(_i0); objs[_i0].serialize(e);
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

#### Header hooks, and why each bound sits inside a kind test (issue #259)

A schema bound is rejected at the **header word**, before the corelib's
truncation check, so a field that is both over-bound and truncated is INVALID
rather than INCOMPLETE (MESSAGE_SPEC §5.2, generator#216). Two hooks carry it:

| hook | fires at | guard |
| --- | --- | --- |
| `onArrayBegin(id, kind, count)` | the array header | `count > N` for a `count: N` native array |
| `onFixlenHeader(id, subtype, length)` | the fixlen length word | `length > maxlen` for a bounded `string`/`blob` |

Both fire for **whatever** arrived at a field id. The corelib resolves the wire
kind/subtype but cannot know the *declared* one — that is schema knowledge only
the generated code holds — so both guards are written as a conjunction, the
declared kind first:

```dart
@override
void onArrayBegin(int id, sofab.ArrayKind kind, int count) {
  switch (id) {
    case 0:
      if (kind == sofab.ArrayKind.fp32 && count > 3) e.inv = true;   // fp32[3]
      return;
    case 1:
      if (kind == sofab.ArrayKind.fp64 && count > 5) e.inv = true;   // fp64[5]
      return;
  }
}
```

The nesting is the point. An array whose element kind contradicts the
declaration **was never this field's value** (§7.3): it is a skipped field, so
its element count is not this field's count and must not be measured against
`N`. Bounding first would turn a skippable contradiction into INVALID — an
`fp64` array header announcing 8 elements landing on the declared `fp32[3]`
above must be *skipped* and the message *accepted*.

That is also why `sofab.ArrayKind` keeps `fp32` and `fp64` apart instead of one
collapsed `fixlen` (CORELIB_PLAN §4.8): a fixlen array's `count` word precedes
its `fixlen_word`, so a hook fired between the two words could only report "some
fixlen array" and the test above could not be written at all. `corelib-dart`
fires `onArrayBegin` **past** the `fixlen_word` for `arrayFixlen`, carrying the
real element subtype; integer arrays have no second word and keep firing on the
count word. A message ending *between* the two words is INCOMPLETE, not INVALID
— the decoder cannot yet know which field it is looking at.

Only the guard needs the explicit test. The **skip** stays structural, as
everywhere else in this backend: the whole-array callbacks
(`onUnsignedArray` / `onSignedArray` / `onFp32Array` / `onFp64Array`) are already
kind-dispatched by the corelib, so a contradicting array lands in a callback with
no arm for that id and evaporates — leaving a correctly typed earlier occurrence
of the same id intact (§7.4). The ports whose corelibs stream array *elements*
(C#, Rust, Zig) need an explicit discard counter here; Dart does not. Nothing in
`onArrayBegin` sizes, clears or allocates the declared field either, so a skipped
header cannot disturb it.

Wrapper-sequence arrays (`string`/`blob`/`struct`/`union`/nested-array elements)
fire no array header at all — they descend through `onSequenceStart` and are
bounded at the collector's `cap` instead. The collector still needs the *fixlen*
header for the same reason the message does: `_StrSeq`/`_BlobSeq` override
`onFixlenHeader`, so an over-index element (`id ≥ cap`) or an over-`maxlen`
element sets `e.inv` at the length word rather than once the payload arrives, and
`tryDecode` reads that sticky flag before returning the incomplete status
(generator#267/#277). The guards sit inside the declared-subtype test, exactly as
the message-level ones above. The payload-side checks stay as defense.

#### An array element's declared width (`onArrayElemBound`, issue #267)

One position deeper, the same shape again. `onUnsignedArray`/`onSignedArray`
hand over the whole list, so the emitted `for (final _v in values)` scan is exact
for an array that **arrives** and never runs for one that does not — while §7.1
makes an out-of-width element invalid and §5.2 makes that outrank the truncation
behind it. So the bound goes to the decoder, which is the only party that sees
the element in time:

```dart
@override
sofab.ElemRange? onArrayElemBound(int id, sofab.ArrayKind kind) {
  switch (id) {
    case 16:
      if (kind == sofab.ArrayKind.signed) {
        return const sofab.ElemRange(-2147483648, 2147483647);
      }
      return null;
  }
  return null;
}
```

Asked **once per array field**, at the count word, never per element — the range
is resolved there and the decoder applies it as the elements go past. `const`, so
answering costs no allocation. Gated on `kind` for the reason `onArrayBegin` is
(§7.3), and emitted for every narrowed integer element, a **dynamic** array
included: width is a property of the element *type*, not of the array *length*.
`u64`/`i64`, enums, bitfields and `bool` return nothing — their range is already
the callback parameter's.

Unlike Go's, this needs no separate-interface dance: `MessageVisitor` is a class
of virtual no-ops, so a new callback with a `null` default is additive by
construction. The list scan stays as defense for a consumer built against a
corelib that does not fire it.

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
  §7.4, wins). `serialize` re-emits `writeFp32Bits` when the value is a NaN **and**
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

## The caller owns the encode buffer

CORELIB_PLAN §5.1: a corelib never allocates or grows an output buffer — the
caller does, and generated code **is** that caller. `encode()` therefore
allocates the storage and hands it to the corelib's `Encoder`, together with the
number it is sized from.

corelib-dart's `Encoder.encodeToBytes` is exactly the shape this replaces: it
builds its own `Uint8List(bufferSize)` and its own `BytesBuilder` inside the
package — its doc comment calls it "the ONE place the package allocates output
storage". Nothing this backend emits calls it any more, in the library, the
harness or the bench body: `TestDartCallerOwnsTheEncodeBuffer` generates a whole
project and fails if the name reappears in *any* emitted file — the conformance
run cannot catch that one, because the helper produces exactly the same bytes and
would sail through every byte-exact leg.

**Bounded** — every field carries a `count`/`maxlen`, so the schema has a worst
case and one exactly-sized buffer holds any conformant value:

```dart
static const int maxSize = 49;        // derived: no value can encode to more

Uint8List encode() {
  final buf = Uint8List(maxSize);
  final e = sofab.Encoder.overBuffer(buf);   // no sink: nothing can be drained
  serialize(e);
  e.flush();
  return e.written;
}
```

No flush sink means `MIN_OUTPUT_BUFFER` does not apply (corelib-dart imposes the
floor only when one is installed), so a field-less message legitimately encodes
through a 0-byte buffer.

**Unbounded** — one field has no bound, so there is no worst case. `maxSize` is
then the configured *ceiling* (emitted as `maxSizeLimit`, with `maxSize` aliasing
it) and must **not** size a buffer: a larger message is legitimate and would be
silently refused. A fixed 512-byte scratch drains into caller-owned storage
instead, so what an encode holds resident is the scratch, not the message:

```dart
Uint8List encode() {
  final out = BytesBuilder(copy: true);
  final e = sofab.Encoder(out.add, buffer: Uint8List(512));
  serialize(e);
  e.flush();
  return out.toBytes();
}
```

`copy: true` is not decoration: the sink is handed a **view** the encoder
overwrites the moment the callback returns. Copying and returning without
installing a replacement is §5.1's copy-and-continue case — the encoder keeps the
scratch and resumes at offset 0, with no take-and-replace handover.

Four consequences worth knowing before you rely on them:

- **A value filled past its own schema bound is refused, not truncated.** It no
  longer fits the exactly-sized buffer, and `SofabException(SofabError.bufferFull)`
  propagates out of `encode()` with nothing returned. Such a message used to be
  encoded and handed back — bytes every conformant receiver rejects as INVALID
  anyway (MESSAGE_SPEC §7.1) — and §5.1 forbids returning partial output as if it
  were complete. This is the *only* encode-side bound the Dart backend has: it
  emits no `maxlen`/`count` validation of its own, and a Dart `List` is not
  capacity-limited at run time.
- **`encode()` reports through the exception channel.** It returns `Uint8List`
  with no error return, which is deliberate and not a swallowed error:
  `serialize` already throws for an out-of-range field id, for `MAX_DEPTH` and
  for an unpaired surrogate in a string, so this adds no new *kind* of failure
  path, and the Dart profile is `maxspeed`. The exception-free promise this
  backend does make is about **decode** (`finish()` returns `null` rather than
  throwing) and is untouched.
- **The bounded arm allocates the schema's worst case, not the value's,** and
  returns `e.written` — a view over that buffer, whose `.buffer` is `maxSize`
  bytes even when the message is shorter (`BytesBuilder.toBytes()` used to hand
  back an exactly-sized list). `array<u64, count: 10000>` means a 90 KB
  `Uint8List` per `encode()` call even for a ten-element value. Worth weighing
  before declaring an aspirational bound; the escape hatch is `encodeTo(e)` with
  an `Encoder` you construct over a buffer — or a sink — of your choosing. There
  is deliberately no cached or module-level scratch: a nested type's `serialize`
  can re-enter `encode()`, and a shared buffer would corrupt the outer message.
- **`maxSize` is a static class member**, so a schema field literally named
  `maxSize` (or `maxSizeLimit`, or `encode`) collides with it. Java, C#, Python
  and TypeScript carry the same exposure.

## A decoded message owns its bytes

`decode()`/`tryDecode()` return a message that outlives the buffer it was decoded
from: the input may be reused or mutated the moment they return. The streaming
`decoder()` is the same — a fed chunk may be refilled as soon as `feed` returns.

corelib-dart does hand views over, but only for a `string`/`blob` on the
**one-shot** path: `_ContiguousDecoder._fixlen` delivers them as
`Uint8List.view(_buf.buffer, …)` straight into the decode buffer. That is correct of the corelib, which allocated
nothing. Owning the bytes is therefore the generated destination's job, and every
one of them copies — `utf8.decode(bytes)` builds a fresh `String`,
`Uint8List.fromList(value)` a fresh blob, and the array collectors (`_StrSeq`,
`_BlobSeq`, `_IntMat`, `_DblMat`, `_BoolMat`) do the same per element. The
`maxlen`/`count` guards run on the view, **before** the copy, so nothing
over-bound is duplicated first.

The other decode paths cannot alias today, because the corelib allocates the
container itself. That is a corelib-side allocation this backend cannot suppress
(there is no caller-owned decode-storage API in corelib-dart), so treat it as a
fact about today's corelib rather than a guarantee to lean on:

| decoded as | one-shot | streaming (`feed`) |
|---|---|---|
| `string`, `blob` | **view into the input buffer** | corelib `Uint8List(length)` — a split payload is reassembled in corelib storage |
| integer array | corelib `Int64List(count)` | corelib `Int64List(count)` |
| fp array | corelib `Float32List`/`Float64List(count)` | corelib `Float32List`/`Float64List(count)` |

So the generated copy is *load-bearing* for a one-shot string/blob and
*defence-in-depth* everywhere else, where it doubles as the conversion into the
field's declared type (`List<int>`, `Float32List`). Keep it on both: which side
allocates is the corelib's choice to change, and the destination must own its
bytes either way.

The property held from the start but was asserted nowhere, so
`tests/conformance/dart/ownership_check.dart` now pins it by behaviour rather than
by shape: it decodes, overwrites the whole input buffer (and, for the streaming
leg, each chunk's scratch right after feeding it), and re-encodes. **Read its
reach off the table above**: mutating a generated destination to keep the
corelib's value instead of copying it fails the check for a one-shot
string/blob, and passes for the array fields, whose copies the check cannot
currently pin because there is no view to expose. A borrowing mode is
deliberately not offered and not configurable (ARCHITECTURE §9.6).

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

The measured encode body is `obj.encode()`, whose *inside* changed with the
caller-owned buffer: the workload (`vehicle_telemetry`) is fully bounded, so it
now allocates one 971-byte `Uint8List` and returns a view over it, where it used
to fill a 4096-byte corelib scratch through a `BytesBuilder` and copy the result
out with `toBytes()`. The encode figures currently in `results.txt` therefore
predate the change and are not comparable across it; the next full run resets
them.

## Strict UTF-8 — validated at the destination (issues #85, #257)

Encode-side strictness is corelib-side (`Encoder.writeString` never substitutes
`U+FFFD`, MESSAGE_SPEC §8). **Decode is not**, and that changed: the corelib used
to hand the visitor a finished `String`, which forced it to validate and transcode
before the consumer could say whether it even wanted the field — so a `string` the
decoder was *skipping* got validated too, which CORELIB_PLAN §6.4 forbids.

`MessageVisitor.onStringBytes(int id, Uint8List bytes)` now delivers the **raw wire
bytes**, and the generated visitor overrides that instead of `onString`. Each arm
resolves its destination first, then calls `sofab.utf8Valid(bytes)` and
`utf8.decode(bytes)`. A skipped field reaches no arm and is never inspected. Invalid
bytes at a materialized position set the sticky `e.inv`, the same channel as the
schema-bound rejects; `blob` is never validated.

Two consequences worth knowing:

- A schema `maxlen` is a **byte** bound, and the raw bytes *are* the wire length,
  so the guard reads `bytes.length` instead of re-encoding the decoded string.
- The generated module imports `dart:convert` **only when it decodes a string**.
  `dart analyze` reports an unused import, and the corpus sweep builds definitions
  that have no string at all.

### Every visitor extends `_Visitor` (issue #265)

Overriding `onStringBytes` in the scopes that *have* a string field is only half
the property. `sofab.MessageVisitor`'s **default** for that method validates the
payload and flags the decode INVALID — correct for a hand-written visitor, which
has no schema and therefore wanted every string it is handed, and wrong for
generated code, where the id decides. A scope with no string field emitted no
override at all and so inherited it: an undeclared string reaching the top-level
visitor of a string-free message was rejected instead of skipped. Three bytes
(`4a 0a 8a` — unknown id 9, a lone continuation byte) were enough, and dart was
alone against twelve implementations that accept them.

The prelude therefore emits one base, and **every** generated visitor extends it —
the per-type visitors and all wrapper-array collectors alike:

```dart
abstract class _Visitor extends sofab.MessageVisitor {
  @override
  void onStringBytes(int id, Uint8List bytes) {}
}
```

A scope that declares strings overrides it again with its id switch and falls out
of that switch — into this same no-op — for every id it does not match. Putting
the no-op on a shared base rather than at each emission site is deliberate: it
makes the property hold by construction, including for collectors added later,
which is exactly what the per-site shape failed to do. Schema names cannot begin
with `_`, so the name cannot collide with a generated type.

## §7.1: the declared integer width is a validity bound (issue #266)

A `u8`/`u16`/`u32`/`i8`/`i16`/`i32` destination rejects a value outside its
declared range by setting the sticky `e.inv` — the same INVALID channel as the
maxlen and count rejects. Dart never masked the value (its `int` holds the whole
64-bit range), so the defect here was that an out-of-range value was simply
**kept**.

```dart
case 0:
  if (value < 0 || value > 255) { e.inv = true; return; }
  o.a_u8 = value;
case 3:
  o.d_u64 = value;   // u64: nothing narrower to bound
```

**The `value < 0` term is load-bearing.** Dart's `int` is a 64-bit SIGNED
integer with no unsigned counterpart, so an unsigned wire value at or above 2^63
arrives negative and `value > 255` alone would wave through exactly the largest
values. Java needs the same term for the same reason; C# does not (`Unsigned`
delivers a `ulong`).

A native array arrives whole as a `List<int>`, so one scan over the elements
decides it: a single out-of-range element makes the message INVALID.

## §7.3: a mistyped sequence element is skipped, not entered (issue #272)

A wrapper-array element position opened as a sequence must be skipped whole. The
message visitors always overrode `onSequenceStart` and returned `null` for an
unmatched id — but the leaf element collectors (`_StrSeq`, `_BlobSeq`) declare no
sequence of their own and so never overrode it at all, inheriting
`sofab.MessageVisitor`'s **descending** default, which returns `this`. A sequence
arriving at an element position therefore descended into the collector itself and
its child string bound as that element.

The fix sits on the shared `_Visitor` base, beside the `onStringBytes` no-op that
is there for exactly the same reason:

```dart
abstract class _Visitor extends sofab.MessageVisitor {
  @override
  void onStringBytes(int id, Uint8List bytes) {}
  @override
  sofab.MessageVisitor? onSequenceStart(int id) => null;
}
```

Both defaults are corelib behaviour that is right for a hand-written visitor —
which has no schema, so everything it is handed is something it wanted — and wrong
for generated code, where the id decides. Putting them on the base rather than at
each class keeps it true for collectors added later. The object and row
collectors, whose elements genuinely *are* sequences, override it back to a
descent.

## §6.5: the fp32 raw-bits companion is public (issue #275)

A scalar `fp32` field carries a companion `int? <name>Fp32Bits` holding the exact
32 wire bits when the decoded value is a NaN, because a Dart `double` cannot
carry an fp32 NaN's payload or signaling bit (MESSAGE_SPEC §4.6).

**It is public, and that is the requirement rather than a convenience.**
CORELIB_PLAN §6.5 obliges a double-only target to provide the raw-wire path *"for
bit-exact consumers"* — a transcoder, a comparator, a materialized walk — not
merely for the type's own re-encode. Dart privacy is per **library**, so a
leading underscore put the bits out of reach of everything outside the generated
file: the round-trip stayed bit-exact (which is why a round-trip-only test never
saw it) while an external walk got the widened double, whose quiet bit is already
set and whose signaling NaN is therefore unrecoverable.

```dart
class Probe {
  double f32 = 0.0;
  int? f32Fp32Bits;          // public: consumers read it, and callers may set it
  ...
  void serialize(sofab.Encoder e) {
    if (f32 != 0.0) {
      if (f32.isNaN && f32Fp32Bits != null) { e.writeFp32Bits(0, f32Fp32Bits!); }
      else { e.writeFp32(0, f32); }
    }
```

A **field** rather than a getter, matching the typescript backend's
`<name>Fp32Raw`: a getter would close the *encode* side, and a caller who wants to
emit a signaling NaN has no other way to say so — the `double` cannot carry it.

Only the scalar position was ever affected. A decoded `fp32` **array** is a
`Float32List` whose byte buffer is public, so a consumer could always read the
untouched wire bits there; this is about field visibility, not Dart's float model.
