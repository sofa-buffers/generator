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
| native numeric/enum/bool/bitfield array (`count N`) | `[N]T` (stack, allocation-free) |
| native array without `count` | `[]const T` |
| string/blob/struct/union/nested array | `[]const T` (with `count: N`, materialized to N element defaults) |
| struct / union | the generated struct type |

Per message:

- `marshal(self, os: *sofab.OStream) sofab.Error!void` — sparse-canonical
  field writes into any caller-configured `OStream` (fixed buffer, or a flush
  sink for streaming).

  Sequences are opened with the corelib's **`writeSequenceBeginLazy(id)`**,
  which holds the header back until a child field actually appears — so the
  `≠ default` test of MESSAGE_SPEC §2 applies to a sequence-typed field for
  free, with no whole-object comparison and no buffering: "the nested
  `marshal` wrote no child" *is* "the value equals its declared default".
  Which **closer** follows is decided at generation time from the position in
  the schema, never from the value:

  | position | closer |
  |---|---|
  | `struct` / `union` field | `writeSequenceEnd` — an all-default nested object is **omitted** |
  | wrapper-array field (the array itself) | `writeSequenceEnd` — an empty array is omitted; absence reconstructs it |
  | wrapper-array **element** (struct/union element, nested array row) | `writeSequenceEndKeep` — the frame stays |

  An **interior** element keeps its frame because element presence is what
  carries a dynamic array's length (*highest present id + 1*, MESSAGE_SPEC
  §5.1): dropping an all-default element would change the decoded **length**,
  not merely the bytes. Consequently an all-default message now encodes to zero
  bytes.

  The **trailing** run is different. A `count: N` wrapper array is
  fixed-length, so its canonical wire stops at `M` — one past its last element
  differing from the element default — "even for sequence-form elements"
  (MESSAGE_SPEC §3/§5.1, generator#248). The element loop therefore runs over
  `_trimObjs(T, …)` / `_trimSlices(T, …)`, not over the whole slice, and `M == 0`
  writes no child at all, so the lazily-opened wrapper is dropped and the field
  is omitted (§2). A **dynamic** (count-less) array has no `N` to refill from, so
  a trailing default element is significant there and is never narrowed away.

  That significance also binds the **leaf** side. A `string`/`blob` element is
  not framed — it is omitted individually whenever it equals the element default
  (empty), leaving an id gap the decoder restores. At the **last** index of a
  *dynamic* array that omission is lossy: the array recovers its length as
  *highest present id + 1*, so the final element is the only one whose
  **presence** carries the length. The generated omit test therefore gains an
  `or _iN == <slice>.len - 1` disjunct there (MESSAGE_SPEC §2, "the last element
  of a dynamic array is always present"): `["a", ""]` used to encode exactly like
  `["a"]` and decode one element short, and `["", ""]` encoded to nothing at all.
  Now `["", ""]` is written as its final element **alone**, at id 1 — interior
  gaps are untouched, so `["", "b"]` still elides element 0. A `count: N` array
  gets no such guard: its length is `N` whatever the wire carries, which is why
  it elides the whole trailing run instead. For the same reason `elemTrimExpr`
  narrows a `string`/`blob` run only when the array is fixed — narrowing a
  dynamic one would make `isDefault` call `[""]` "default" and omit a field the
  marshal loop writes.

  Every generated `struct`/`union` carries `pub fn isDefault()` for this: it is
  the explicit form of the "no child was written" test the lazy framing already
  encodes implicitly for a *field*, needed because an *element* must be judged
  **before** the loop opens. Its per-field terms are generated from the very
  expressions `marshal` writes each field under, so the predicate and the writer
  cannot drift apart — a predicate that narrowed a field the writer does not
  (or the reverse) would omit a field that is on the wire, or keep one that is
  not.

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

### Wrapper-array elements are placed by id, and filled back out to N

A wrapper array's element **id IS the array index** (MESSAGE_SPEC §5.1). Every
element kind honours that, not just the `string`/`blob` leaves that go through
`sofab.arrays.setElem`: for a `struct`/`union` (and a nested-array row)
`sequenceBegin` grows the destination to `id + 1` — default-filling the id gaps
an omitted element leaves — records the index in a per-frame `ei_*` register,
and descends into `_at(path, ei)`. Appending instead (generator#247) shortened
the array by the size of any interior id gap, and decoded a **reopened** element
id as a second element rather than merging into the first; placement gives the
§7.4 struct-merge for free. The `count: N` over-index reject still runs first,
which also bounds the gap-fill.

When the array's sequence scope closes, a `count: N` array is default-filled
back out to `N`: §5.1 says the length "is N for every target — a growable-list
target MUST default-fill to N exactly like a pre-sized one". This is what makes
the encoder's trailing-run elision above **lossless**: without it, re-encoding a
decoded fixed array would not re-normalise it, it would shorten it on every
round trip. A dynamic array is never filled — its length is *highest present id
+ 1*.

That fill hangs off `sequenceEnd`, so it can only reach a sequence that was
actually **opened** — which is why the `count: N` length also has to be
established at **construction**. A fixed-count *native* array always had it: its
storage IS a `[N]T`, materialized by the field declaration
(`nums: [3]u32 = @splat(0)`). A fixed-count *wrapper* array is a slice, so its
declaration materializes the same length explicitly, as N element defaults:

```zig
strs: []const []const u8 = &([_][]const u8{""} ** 3),   // count: 3, string elements
objs: []const VecObjsElem = &([_]VecObjsElem{.{}} ** 2), // count: 2, struct elements
dyn:  []const []const u8 = &.{},                         // count-less: no N, stays empty
```

The elements are the ELEMENT default — the same value the gap-fill above writes
into an id the wire omitted; a declared per-element `default` is not
materialized anywhere today. The `**` repetition is the slice analogue of the
native array's `@splat`: the emitted source stays O(1) in `N`. Without this the
field disagreed with itself and with the native array beside it: absent decoded
at length 0 while one element on the wire, or an explicitly-empty wrapper,
decoded at N.

The literal is comptime-const, hence read-only, and decode never writes through
it: every store into a wrapper array sits behind `sequenceBegin`, which resets
the field to `&.{}` first (an explicit empty wrapper must override a non-empty
value), so the const is only ever replaced. Encoding is unaffected — an
all-default fixed array still narrows to `M = 0`, so its lazily-opened wrapper is
dropped and the field stays off the wire (§2).

## Unbounded fields

There is no `no_std`-style sizing gate: `corelib-zig` is the family's
max-speed port, and the generated code takes an allocator on the decode
path, so a string/blob without `maxlen` or an array without `count` is fine —
bounded native arrays still lower to fixed `[N]T` stack storage and skip the
allocator entirely.

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
decodes a real scalar arriving at that id after the array. The fp arrays are never
armed — their elements go to the float callbacks and cannot reach a scalar arm.

The corelib calls `arrayBegin` through `@hasDecl`, so emitting it for the guard
alone is enough — it is now emitted for messages that have no native array at all.
Zig rejects unused function parameters, so it takes `id` only when the message
declares an integer array to disarm for; otherwise the parameter stays `_`.
