# Zig target — `targets.zig`

Target-specific options, accepted under `targets.zig`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

The Zig target takes no options of its own — everything is set in the
[generic config](README.md).

The Zig target has a single corelib — [`corelib-zig`], the **max-speed** port
of the family (allocation-free streaming encoder, zero-copy contiguous
decoder, comptime duck-typed visitor) — so there is no `corelib` selector.
`sources` emits `src/message.zig`; `project` additionally scaffolds
`build.zig`, `build.zig.zon` and a JSON encode/decode harness
(`src/main.zig`).

Set the corelib path in the generated `build.zig.zon` (the
`${SOFAB_ZIG_CORELIB}` placeholder) before building; `build.zig.zon` path
dependencies must be **relative to the project root**. Build with
`zig build --release=fast` (Zig 0.16+) — the corelib is tuned for
ReleaseFast and the generated `build.zig` prefers it under `--release`.

## Receiver-side decode limits

The `max_dyn_*` caps are [generic options](README.md); what is specific to this
target is how they land in the generated code — as comptime constants checked at
the length/count header, before the payload is buffered. A violation returns
`error.LimitExceeded`.

## Generated shape

One `pub const <Message> = struct { … }` per object, with the **schema
defaults in the field declarations** — a plain `.{}` value carries every
default, which is what makes sparse-canonical decode (MESSAGE_SPEC §2) a
no-op for omitted fields. Enums and bitfields become `pub const` integer
namespaces (`someenum.RED`, `somebitfield.FLAGA`) over the narrowest backing
integer, shared with the Rust backend's rules so all ports agree.

| Field kind | Zig storage |
|---|---|
| numeric / bool / fp | `u8`…`u64`, `i8`…`i64`, `bool`, `f32`, `f64` |
| enum / bitfield | narrowest backing integer (`i8`/`i16`/`i32`, `u8`…`u64`) |
| string / blob | `[]const u8` |
| native numeric/enum/bool/bitfield array (`count N`) | `FixedArray(T, N)` — `[N]T` inline capacity **plus** `len` (stack, allocation-free) |
| native array without `count` | `[]const T` |
| string/blob/struct/union/nested array | `[]const T` (a `count: N` bounds it; it is not materialized) |
| struct / union | the generated struct type |

### `count: N` is a capacity, so the storage carries a length

`count: N` is a **capacity**, never a length (MESSAGE_SPEC §3): a field holds
0..N elements and the wire count `M` **is** the length. A bare `[N]T` can only
ever *be* `N` long, so it cannot represent that value — this backend's count:N
native arrays are therefore `FixedArray(T, N)`, a small generated type holding
`items: [N]T` plus `len: usize`:

```zig
nums: FixedArray(u32, 3) = .{},                                   // no default: the EMPTY array
seed: FixedArray(u32, 5) = .{ .items = .{ 1, 2, 3, 0, 0 }, .len = 3 }, // default: [1, 2, 3] — 3 long, not 5
```

The value is `items[0..len]` (`.slice()`); `items[len..]` is spare capacity that
never reaches the wire. `.init(&.{ 1, 2 })` / `.set(&.{ 1, 2 })` build one. This
keeps a bounded array **allocation-free on both encode and decode**, which is the
point of `count` on this max-speed target — turning it into a heap slice would
make a bounded schema allocate. A declared `default` stands exactly as written
and is never padded out to `N`, so a fresh count:N array is the *empty* array
(it used to be `N` element defaults).

Per message:

- `marshal(self, os: *sofab.OStream) sofab.Error!void` — sparse-canonical
  field writes into any caller-configured `OStream` (fixed buffer, or a flush
  sink for streaming).

  Sequences are opened with the corelib's **`writeSequenceBeginLazy(id)`**,
  which holds the header back until a child field actually appears — so the
  `≠ default` test of MESSAGE_SPEC §2 applies to a sequence-typed field for
  free, with no whole-object comparison and no buffering: "the nested
  `marshal` wrote no child" *is* "the value equals its declared default".
  Which **closer** follows is decided per position — statically for a *field*,
  and from the index in the **value** for an *element*:

  | position | closer |
  |---|---|
  | `struct` / `union` field | `writeSequenceEnd` — an all-default nested object is **omitted** |
  | wrapper-array field (the array itself) | `writeSequenceEnd` — an empty array is omitted; absence reconstructs it |
  | wrapper-array **element**, last index | `writeSequenceEndKeep` — the empty frame stays |
  | wrapper-array **element**, interior | `writeSequenceEnd` — the frame drops, leaving an id **gap** |

  That element split is the whole of the rule. An array carries no length field:
  its decoded length is *highest present id + 1* (MESSAGE_SPEC §5.1), so the
  element at the highest index is the only one whose **presence** carries the
  length, and nothing that carries the length may be elided. Everything before it
  may be: an interior element equal to the element default is indistinguishable
  from an absent one, because the decoder restores an absent id from that same
  default. So: **interior sparse, last always written** — as its value for a
  leaf, as an empty frame for a sequence element. Sequence-form elements used to
  have a carve-out (framed unconditionally); they no longer do, and one rule now
  covers both element kinds. Consequently an all-default message encodes to zero
  bytes.

  A declared `count: N` changes none of it. `N` is a capacity, not a length
  (§3), so it can never restore an elided tail: **no trailing run is elided**, of
  either element kind, with or without a count. `["a", ""]` stays distinct from
  `["a"]`, `["", ""]` is written as its final element alone at id 1, and
  `["", "x", ""]` elides element 0 into a gap while keeping element 2. The
  compact form follows the same principle from the other side: the wire count IS
  the length, so `[1, 2, 0, 0]` keeps its trailing zeros and stays distinct from
  `[1, 2]`.

  A **native** nested row (an array-of-native-array element) has no frame of its
  own, so the rule lands on the write itself: an interior empty row is not
  written at all, and the last row always is (as a count-0 array if that is what
  it is).

  Every generated `struct`/`union` carries `pub fn isDefault()`: the explicit
  form of the "no child was written" test the lazy framing encodes implicitly for
  a *field*. Its per-field terms are generated from the very expressions
  `marshal` writes each field under, so the predicate and the writer cannot drift
  apart — one that disagreed would omit a field that is on the wire, or keep one
  that is not. An array field's own omit test is now simply emptiness: the writer
  emits a child for every element it holds, so "no child is written" is exactly
  "the array is empty".

  A wrapper array's declared `default` is not materialized in the
  generated field initializer today (it is the empty collection), so absent
  and explicitly-empty denote the same value and the plain dropping closer is
  correct; closing that gap would require an
  `if (value != default) { …; writeSequenceEndKeep(); }` guard on the field
  wrapper.
- `encode(self, alloc) ![]u8` — convenience wrapper: streams through a stack
  scratch buffer into an allocated byte slice via the corelib flush sink.
- `decode(alloc, data) DecodeError!Message` — one-shot decode on the
  corelib's zero-copy fast path. **The returned message borrows string/blob
  bytes from `data`** (keep the buffer alive as long as the message); array
  storage is allocated from `alloc` — pass an arena and free the whole
  message at once. `MAX_SIZE` bounds the encoded size (schema-sized, capped
  for unbounded fields).

  The module-level `DecodeError` set is `sofab.Error || error{IncompleteMessage}`
  and keeps the MESSAGE_SPEC §7 tri-state distinct: malformed bytes fail with
  the corelib's `error.InvalidMessage`, while input that merely *ends* inside
  a field or an open sequence — the corelib's non-error `.incomplete` decode
  `Status` from `feed()` — fails with `error.IncompleteMessage`
  (generator#120). The corelib leaves the end-of-input verdict to the caller;
  a one-shot decode over a whole buffer is at end-of-input by definition, so
  a trailing `.incomplete` is a truncated message, never silently accepted.
  Streaming callers that want to keep feeding chunks drive `sofab.IStream`
  directly.

The decoder is the same flat-visitor `(location, id)` state machine as the
Rust backend, monomorphized by the corelib's comptime duck typing (no
vtable). Element stores are bounds-checked explicitly — ReleaseFast compiles
without implicit bounds checks, so hostile counts/ids degrade to dropped
elements, never out-of-bounds writes.

### Array elements are placed by id, and nothing is filled in

A wrapper array's element **id IS the array index** (MESSAGE_SPEC §5.1). Every
element kind honours that, not just the `string`/`blob` leaves that go through
`sofab.arrays.setElem`:

- `struct`/`union` elements and wrapper nested rows: `sequenceBegin` grows the
  destination to `id + 1` — default-filling the id gaps an omitted element
  leaves — records the index in a per-frame `ei_*` register, and descends into
  `_at(path, ei)`. Appending instead (generator#247) shortened the array by the
  size of any interior id gap and decoded a **reopened** element id as a second
  element rather than merging into the first; placement gives the §7.4
  struct-merge for free.
- **native rows** (an array whose elements are native arrays): `arrayBegin`
  used to *append* a row per header, ignoring the element id. That was
  unreachable while every row was framed; the §2 interior-sparse rule makes an
  omitted empty row reachable, and an appending collector then shifts every
  later row down by one. Rows are now placed at `_at(path, id)` too, with the
  same `ei_*` register carrying the index into the element stores.

The `count: N` over-index reject runs first in every case (an id `≥ N` is
INVALID, §5.1/§7), which also bounds the id-keyed gap-fill against an over-index
amplification.

**Nothing is filled in on the way out.** The wire count `M` IS a compact array's
length and the highest present element id IS a wrapper array's last index
(§3/§5.1), so what arrived is the whole value: no `sequenceEnd` refill to `N`,
no `[M, N)` tail on a native array, and no `count: N` array materialized to `N`
element defaults at construction — a fresh one is empty, wrapper and native
alike. A count:N native array's `len` is reset at its array header (an
explicitly empty array decodes to the *empty* array) and advanced by the element
stores; the reset is gated on the announced wire **kind**, so a header
contradicting the declared element type is skipped like an unknown id and leaves
a correctly typed earlier occurrence intact (§7.3/§7.4).

`sequenceBegin` still resets a wrapper array field to `&.{}` before its elements
land: an array wrapper IS the array's value, so a later occurrence replaces it
whole rather than merging by index (§7.4).

## Unbounded fields

There is no `no_std`-style sizing gate: `corelib-zig` is the family's
max-speed port, and the generated code takes an allocator on the decode
path, so a string/blob without `maxlen` or an array without `count` is fine —
bounded native arrays still lower to inline `FixedArray(T, N)` stack storage and
skip the allocator entirely.

Two receiver-side protections cover those unbounded fields on decode:

- **`max_dyn_*` decode limits** (generator#102, opt-in, see the options
  table): enforcement lives entirely in the generated decoder — the corelib
  only defines `error.LimitExceeded`. Guards are per-field, emitted only for
  schema-unbounded fields, and feed a sticky `lim` flag checked after the
  generator#100 `inv` flag (`InvalidMessage` takes precedence). The same `inv`
  flag carries the **wrapper-array** over-index reject (generator#142): a
  `string`/`blob`/`struct`/`union` element array with `count: N` sets `inv` when
  a wire element id is `≥ N`, before the slice grows.
- **Capped eager allocation** (always on): a dynamic native array's wire
  count is untrusted until its elements actually arrive, so `decode()`
  allocates at most 1024 elements up front and grows geometrically (never
  past the announced count) as elements land — a lying count cannot force a
  huge allocation. Honest messages decode identically.

## Struct field order

Generated struct fields stay in **schema order** — like Rust, Zig reorders
the fields of a default-layout struct itself, so no widest-first reordering
is applied.

[`corelib-zig`]: https://github.com/sofa-buffers/corelib-zig

## Benchmark row

Row `zig` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **toggle** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.

## Strict UTF-8 (issue #85)

A `string` is a borrowed `[]const u8` slice (byte-container), materialized in
generated code — so the corelib exposes a `utf8_valid(bytes)` primitive and the
string visitor emits an **unconditional** call at the store site
(`if (!sofab.utf8_valid(chunk)) { self.inv = true; } else { … }` → `INVALID`,
surfaced as `error.InvalidMessage`). The `SOFAB_STRICT_UTF8` gate lives **inside**
the primitive (a Zig build feature; folds to `true` when compiled off), so generated
code is identical across build configs and flipping the flag never regenerates it
(MESSAGE_SPEC §8 / CORELIB_PLAN §6.4). The wrap is emitted for `string` only —
`blob` is opaque bytes, stored verbatim. Skipped fields hit the switch `else` arms
and are never validated. Encode-side strictness is corelib-side (`OStream.writeString`).

## §7.3: an integer array at a scalar id (issue #183)

MESSAGE_SPEC **§7.3** skips a field whose header wire type contradicts its
declared type. This backend's corelib settles almost every case *structurally* —
a mismatched header lands in a differently-typed visitor callback with no case for
that id — but not one: it streams an integer array's elements through the **same**
`unsigned()/signed()` callbacks a lone scalar uses, so an integer array header at a
scalar-declared id of the same signedness would be stored element by element.

The generated visitor therefore carries a skip counter. `arrayBegin` arms
`askip = count` when the announced kind is the unsigned or signed integer kind
and the `(scope, id)` pair is **not** a declared integer-element native array;
the two scalar callbacks then discard while armed. It self-terminates on the
announced count (no array-end callback needed), survives a chunk boundary (the
counter lives in the visitor), leaves legitimate arrays untouched, and still
decodes a real scalar arriving at that id after the array. Fixlen arrays are armed
the same way (issue #193): their elements stream through the `fp32()/fp64()`
callbacks a lone float scalar uses, so they need the identical guard.

The corelib calls `arrayBegin` through `@hasDecl`, so emitting it for the guard
alone is enough — it is now emitted for messages that have no native array at all.
Zig rejects unused function parameters, so it takes `id` only when the message
declares an integer array to disarm for; otherwise the parameter stays `_`.

## §4.8: the fixlen array arm is keyed by element subtype (issue #259)

`sofab.ArrayKind` is `{ unsigned = 0, signed = 1, fp32 = 2, fp64 = 3 }` — the fp
members name the **element subtype**, not merely "some fixlen array". That is
forced by the wire layout: a fixlen array's `count` word comes *before* its
`fixlen_word`, so at the moment the count is known nobody can yet say whether the
array that arrived *is* the declared field's value. corelib-zig therefore fires
`arrayBegin` only past the `fixlen_word` and reports the subtype it just read
(CORELIB_PLAN §4.8; the collapsed `fixlen` member was removed).

The generated arms follow the same key. A declared `array<fp32, count: N>` is
listed **only** under `.fp32`, a declared `array<fp64, count: N>` **only** under
`.fp64`:

```zig
self.askip = switch (kind) {
    .unsigned, .signed => switch (self.cur) { ... },
    .fp32 => switch (self.cur) {
        .root => switch (id) { 2 => 0, else => count },
        else => count,
    },
    .fp64 => switch (self.cur) {
        .root => switch (id) { 3 => 0, else => count },
        else => count,
    },
};
switch (self.cur) {
    .root => switch (id) {
        2 => if (kind == .fp32) { if (count > 5) { self.inv = true; return; } self.m.a32.len = 0; },
        3 => if (kind == .fp64) { if (count > 7) { self.inv = true; return; } self.m.a64.len = 0; },
        ...
```

Three prongs cover all four members, so there is no `else` prong — Zig rejects an
unreachable one.

**Why the schema bound sits inside the kind test.** §7.3 decides the wire-type
contradiction *first*; a schema bound applies only to a field that survives that
test. Reading the `count > N` guard before the kind test would measure an fp64
array against the fp32 field's `N` and flag the message INVALID, when the correct
verdict is to skip the array and keep whatever a correctly typed earlier
occurrence left in the field (§7.4). Behind the test, an `fp64` header at the
`fp32` slot matches nothing: it is neither bounded nor sized nor cleared, and its
id falls to `else => count` in the `.fp64` skip arm, so its elements are discarded
exactly like an array at an unknown id.

Integer arrays are untouched by all of this — there is no second word on the
`.unsigned`/`.signed` path, so the count header already carries everything the arm
needs and the two kinds stay a single prong.

## §7.1: the declared integer width is a validity bound (issue #266)

This is the backend where the defect was written down as intent. `storeCast`
used to emit `@truncate(value)` with the comment *"the declared width is a
storage hint"* — precisely the masking MESSAGE_SPEC §1/§7.1 now forbids.

```zig
0 => { if (value > 255) { self.inv = true; return; } self.m.a_u8 = @intCast(value); },
3 => self.m.d_u64 = value,   // u64: range is sofab.Unsigned's own, no guard
```

Two deliberate changes:

- `self.inv` is the same sticky INVALID flag the over-count and over-index
  guards set, surfaced by `decode()` as `error.InvalidMessage`.
- The cast is now `@intCast`, not `@truncate`. It is only ever reached for a
  value the guard has already let through, and `@intCast` is checked in safe
  build modes — so a guard that ever failed to precede a store becomes a panic
  in Debug/ReleaseSafe rather than a silently masked value.

In an array arm the guard sits INSIDE the fill guard: an over-width scalar at an
array id with no `arrayBegin` in front of it is a §7.3 skip, not an INVALID.
