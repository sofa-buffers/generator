# C++ target — `targets.cpp`

Target-specific options, accepted under `targets.cpp`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `corelib` | `cpp` \| `c-cpp` | `cpp` | Which C++ corelib the generated code targets — the runtime only. |
| `allow_dynamic` | bool | **depends on `corelib`**: `true` for `cpp`, `false` for `c-cpp` | Storage for schema-bounded fields. `true` = `std::string`/`std::vector`; `false` = heap-free `FixedString<N>`/`FixedBytes<N>`/`InlineVector<T,N>`. Works on **both** corelibs. Never changes the wire or the API. |
| `namespace` | string | `message` | C++ namespace wrapping the generated types. Also settable in `generic`. |

### What the two profile keys combine into

The two keys are **orthogonal**: `corelib` picks the runtime, `allow_dynamic`
picks the storage, and every combination is usable.

| `corelib` | `allow_dynamic` | Storage | Unbounded field | Heap |
|---|---|---|---|---|
| `cpp` | `true` (default here) | `std::string` / `std::vector` | fine; `max_dyn_*` caps what the schema leaves open | yes |
| `cpp` | `false` | `FixedString<N>` / `FixedBytes<N>` / `InlineVector<T,N>` **where the schema bounds the field** | keeps its dynamic container | only for the fields that stayed dynamic |
| `c-cpp` | `false` (default here) | `FixedString<N>` / `FixedBytes<N>` / `InlineVector<T,N>` | **rejected at generate time** | none on the message path |
| `c-cpp` | `true` | `std::string` / `std::vector` | **rejected at generate time** | yes |

`allow_dynamic` defaults differently per corelib because the two start from
opposite ends: an embedded target has no heap to spare, a server target would
rather allocate what a message carries than its declared worst case.

**Where the two corelibs genuinely differ is the unbounded field, and that
follows from the profile rather than from the switch.** `c-cpp` has no fallback:
`maxlen`/`count` are mandatory in *both* its storage modes, so one schema stays
valid for every `c-cpp` device. `cpp` applies static storage **per field**,
wherever a bound exists, and leaves the rest dynamic — so `allow_dynamic: false`
never fails a build and never forces a schema migration. A schema simply gets
faster where it is already bounded.

A declared bound is honoured **whatever its size**. There is deliberately no
threshold above which a large `count` silently falls back to the heap: the schema
states the intent, and a hidden byte budget would make the member type
unpredictable from the schema.

The `c-cpp` rows accept exactly the same schemas and produce the same `_maxSize`,
so the switch is a per-device decision and never a schema change. What differs is
where a field's bytes live, and therefore what a message costs to hold and to
move.

**Cost, measured** (`examples/messages/realworld/vehicle_telemetry.yaml`,
callgrind toggle, `-O3 -g -DNDEBUG`, wire byte-identical in both modes):

| `corelib: cpp` | encode Ir/op | decode Ir/op | `sizeof` |
|---|--:|--:|--:|
| `allow_dynamic: true` | 13672 | 31043 | 672 B |
| `allow_dynamic: false` | 12943 (−5.3 %) | **22014 (−29.1 %)** | 1360 B |

The gain is the eliminated heap-container work and is therefore schema-shaped:
count the arrays, blobs, and strings with `maxlen >= 16` (below that,
`std::string`'s small-string optimisation already keeps it off the heap). A
scalar-heavy message with one short bounded string measures under 5 %. The price
is `sizeof`, which grows with the declared bounds rather than with the payload.

Encode output is byte-identical too, and so is what a decode reconstructs — for
every field kind, with no exception. That holds because **every array member
carries a logical length**, whichever container it lands in: `std::vector<T>` on
the heap profiles, `sofab::InlineVector<T, N>` (inline slots plus a `len_`) on
the heap-free one. Since `count` is a capacity and the wire count is the array's
length (MESSAGE_SPEC §3), a member that could not be shorter than N could not
express what the wire can say: `[1, 2]` on a `count: 4` field would be four
elements in one storage mode and two in the other, and the wire `7b 02 01 02`
would come back as `[1, 2]` on one leg and `[1, 2, 0, 0]` on the other. A
`std::array<T, N>` was exactly such a member, and it is no longer used for one.
See *`count` is a capacity* below.

### `encode()` and `encodeTo()`

Every message gets both. `encode()` returns a `std::vector<std::uint8_t>`: one
allocation at `_maxSize`, serialized into directly, then shrunk to the bytes
actually written — no staging buffer and no copy. `encodeTo(dst, cap)` writes
into storage the caller already owns and allocates nothing at all; it returns the
byte count, or `0` if the message does not fit in `cap`, in which case `dst`
holds however much was written before that was discovered.

### `reset()` and decoding a second message

Every generated message and nested type also gets a public `void reset() noexcept`
that puts each field back to its **declared default, in place** — containers are
cleared rather than reallocated, so a destination reused across messages keeps the
capacity it has already paid for, and a struct or union member recurses into its
own `reset()` instead of being assigned a fresh temporary.

It exists because a field whose value equals its default is **absent** from the
encoded bytes (MESSAGE_SPEC §2), a sequence-typed one included. An absent field
delivers no `deserialize()` callback, so nothing on the callback side can clear a
destination that is decoded into twice: the "a later occurrence replaces the array
whole" clear inside `sofab::StringSeq` / `BlobSeq` / `sofabgen::WrapperSeq` hangs
off the sequence header, which an omitted field never sends. Clearing has to happen where
absence is still observable — at the start of the decode.

So: **drive a stream yourself and `reset()` is yours to call** between messages,
alongside the corelib's own `IStreamImpl::reset()` for the decoder state (the two
halves: the destination is yours, the decoder state is the stream's).

```cpp
sofab::IStreamObject<Telemetry> in;
in.feed(first.data(), first.size());
in.reset();                       // decoder state AND the message it owns
in.feed(second.data(), second.size());

Telemetry dst;                    // a destination you own
sofab::IStreamInline *sp = nullptr;
sofab::IStreamInline s{[&](sofab::id id, std::size_t sz, std::size_t ct) {
    dst.deserialize(*sp, id, sz, ct);
}};
sp = &s;
s.feed(first.data(), first.size());
s.reset(); dst.reset();           // both halves, or the first message survives
s.feed(second.data(), second.size());
```

`try_decode` handles this for you, differently per profile:

| profile | shape | `out` on a rejected input |
|---|---|---|
| `corelib: cpp` | `out.reset()`, then an `IStreamInline` decodes into `out` directly — no second instance, no copy, no buffer handed back | the fields decoded before the error |
| `corelib: c-cpp` | decodes into a fresh `IStreamObject` and copies the result over `out` | untouched |

The fixed profile keeps the copy on purpose. Its containers are inline, so there
is no allocation to hand back and nothing to reuse, and the instance it decodes
into starts at the declared defaults every time — while its `IStreamObject`
dispatches through a C-ABI function pointer where `IStreamInline` holds a
`std::function`. Routing the decode through a callback there would put that
machinery in `.text` on the targets that have the least of it.

Setting any of the three `max_dyn_*` keys also derives a streaming reassembly cap, passed as
`sofab::Limits{max_buffered_field}` into the one-shot decode entry points, that
bounds how much the corelib buffers for a single incomplete field. It is a **byte**
budget, so it is the largest byte *span* any one top-level field can legitimately
reach — the same worst-case walk that sizes `_maxSize`, with each configured
`max_dyn_*` cap standing in for the missing schema bound. An array charges its
count times the worst-case element size plus framing; a count is never itself a
byte budget (#228). No message the per-field guards accept can therefore trip the
cap.

A field that is neither schema-bounded nor covered by a configured cap has no
legitimate maximum, and the cap is one number for the whole stream — so none is
emitted and reassembly stays uncapped, exactly as with no limits configured. Cap
every dynamic field kind your schema uses if you want the bound.

### `corelib`

Both corelibs expose the same `sofab::` interface and produce **byte-identical
wire output**; they differ only in the decode of variable-length fields.

- **`cpp`** (default) — the pure-C++20, header-only [`corelib-cpp`]. `read()`
  resizes string/blob targets for you. Build with
  `make SOFAB_CPP_DIR=/path/to/corelib-cpp SOFAB_C_DIR=/path/to/corelib-c-cpp`.
- **`c-cpp`** — the C++ wrapper over the C library in [`corelib-c-cpp`]. The
  wrapper binds a decode target by address and fills it after the field
  callback, so the generated decode pre-sizes strings/blobs and reads
  blobs/sequences via the wrapper's native overloads. The generated `Makefile`
  compiles and links the corelib's C sources, so only
  `make SOFAB_C_DIR=/path/to/corelib-c-cpp` is needed.

```yaml
targets:
  cpp:
    namespace: myproj
    corelib: c-cpp     # default: cpp
```

[`corelib-cpp`]: https://github.com/sofa-buffers/corelib-cpp
[`corelib-c-cpp`]: https://github.com/sofa-buffers/corelib-c-cpp

### `corelib: cpp` = dynamic containers

The default heap profile maps every **schema-unbounded** field to a growable
container: a string with no `maxlen` → `std::string`, a blob with no `maxlen` →
`std::vector<std::uint8_t>`, and an array with no `count` → `std::vector<T>` —
including a **native scalar array** (e.g. an `array` of `u32` with no count is
`std::vector<std::uint32_t>`, not `std::array<T, 0>`). A **bounded** native array
(count present) is a `std::vector<T>` too: `count` is a capacity, so the member
still has to hold anything from 0 to N elements, and only a length-carrying
container can. Decode sizes a native vector to the wire element count before
filling it, and the
[`max_dyn_array_count`](#options) cap (when set) rejects an over-cap count as
`Error::LimitExceeded` before any allocation.

### `corelib: c-cpp` = fixed-capacity (embedded) containers

`corelib: c-cpp` targets real embedded devices, so it **always** uses fixed-capacity,
heap-free containers — there is no separate knob. This removes hidden dynamic
allocation from the generated message code. If a target has the resources for a
heap, use `corelib: cpp` (which uses `std::vector`/`std::string`). Wire output is
identical either way for every value both representations can hold — this is an
in-memory representation change, so the shared conformance vectors and every
sha256 stay the same. The one value that only one of them can hold is a
`count: N` array of native scalars shorter than N; see
[`count` is a capacity](#count-is-a-capacity).

What `c-cpp` produces vs `cpp` (all sized from the schema's `maxlen`/`count`):

| Field kind | `corelib: cpp` (dynamic) | `corelib: c-cpp` (fixed) |
|---|---|---|
| string (`maxlen N`) | `std::string` | `sofab::FixedString<N>` (inline, no heap) |
| blob (`maxlen N`) | `std::vector<std::uint8_t>` | `sofab::FixedBytes<N>` (inline, no heap) |
| string array (`count N`, elem `maxlen M`) | `std::vector<std::string>` | `sofab::InlineVector<sofab::FixedString<M>, N>` |
| blob array (`count N`, elem `maxlen M`) | `std::vector<std::vector<std::uint8_t>>` | `sofab::InlineVector<sofab::FixedBytes<M>, N>` |
| struct / union / matrix array (`count N`) | `std::vector<T>` | `sofab::InlineVector<T, N>` |
| native numeric/enum/bool/bitfield array (`count N`) | `std::vector<T>` | `sofab::InlineVector<T, N>` |

A **boolean** array's element type differs between the two corelibs:
`bool` on `corelib: cpp`, **`std::uint8_t`** on `corelib: c-cpp` (in both storage
modes). corelib-c-cpp's decoder is *deferred* — `read()`/`readArray()` record the
destination's **address** and the C runtime writes the element bytes after the
field callback has returned — so the destination has to be the member itself and
needs one addressable byte per element. `std::vector<bool>` is the bit-packed
specialisation: it has no `data()` and no byte per element, so it cannot be a
decode destination at all. Both c-cpp storage modes take the same element type so
that `allow_dynamic` stays a storage switch and not an API change. The wire is
unaffected — a boolean array has always travelled as an unsigned array of 0/1
bytes, which is now exactly what the member holds.

All three fixed-capacity containers — `sofab::FixedString<N>`,
`sofab::FixedBytes<N>` and `sofab::InlineVector<T,N>` — live in the corelib-c-cpp
wrapper (`sofab.hpp`) as a single source of truth; the generator only references
them (nothing container-shaped is emitted into the generated headers, so a fix to
a container is a corelib change, not a codegen change — the one generated block,
`namespace sofabgen`, holds the wrapper-array element collector described
[below](#wrapper-arrays-element-placement-and-the-sparse-element-rule), which is
bounded by the schema `count` and so cannot live in a corelib, plus
`sofabgen::RawArray` — a *view*, not a container: it owns no storage and only
lets the corelib see an **enum** array member's elements as the enum's backing
integer, which is what the wire carries. It exists because the deferred decoder
must bind the member itself, so the temporary the `corelib: cpp` leg converts
through would dangle. It reinterprets the *elements*, never the container:
casting a `std::vector<T>` to a `std::array<T, N>` would make the vector's own
begin/end/capacity words its first N elements, so wire bytes would overwrite the
begin pointer and the destructor would free a pointer assembled from the
message).

`sofab::FixedString<N>` is a heap-free, `std::string`-friendly fixed-capacity
string (implicit construct/assign from `std::string`/`std::string_view`/`const
char*`, implicit `operator std::string_view` view, `c_str()`, comparisons, `str()`
to go back to an owning `std::string`); the decoder fills it in place via the same
`read_string_noterm` path as `std::string`. `sofab::FixedBytes<N>` is the same
idea for a blob.

**Why a custom container and not plain STL?** Each of these tracks a **logical
length distinct from the capacity `N`** — a blob shorter than its `maxlen`, an
array shorter than its `count`. `std::array<T,N>` is always exactly length `N`, so
it cannot represent "3 of a possible 5"; `std::vector` would represent it but
reintroduces the heap this profile exists to avoid. So a purpose-built inline
container (inline `std::array` storage + a separate `len_`) is the only fit —
which is what makes every array express the 0..N that §3 and §5.1 ask of it.
That now includes the **native scalar** arrays, which used to be the one
exception: they were plain `std::array<T,N>` and so had no length, which is the
gap [`count` is a capacity](#count-is-a-capacity) used to record and no longer
does. `InlineVector` gained a `resize()` for it (corelib-c-cpp), which is what
lets `IStreamImpl::readArray` keep ownership of the tag / bound / reset / bind
order for inline storage too — without it, readArray took its fixed-extent
branch, set the logical length to 0 and dropped the array silently.
`InlineVector`'s inline storage also never
reallocates, so a bound-then-filled element is address-stable — strictly safer
under the corelib-c-cpp deferred decoder than a `std::vector` + `reserve()`.
The per-element collectors (`sofab::FixedStringSeq` / `FixedBlobSeq` for the
leaves, `sofabgen::WrapperSeq` for structs, unions and rows) place an element at
its wire index `id` by growing the `InlineVector` up to that slot; because
`emplace_back()` is a no-op once the vector is full, an untrusted element index
`id >= N` is **dropped** (the fill loop is guarded by the container capacity, and
`WrapperSeq` rejects the index outright before the loop) rather than spun on
forever — the corelib skips the element's payload since the callback binds no
destination, mirroring the native-array over-capacity drop (MESSAGE_SPEC §5.1).
Without the guard a 4-byte message could hang the decoder (issue #126, DoS).
Because they are non-aggregates with `initializer_list` constructors, a brace-init
like `msg.field = {"a", "b"}` sets the logical length correctly rather than
silently leaving it at zero (which would drop the field from the wire). A
non-allocating `encodeTo(dst, cap)` is also emitted alongside the convenience
`encode()`.

**Unbounded fields.** A string or blob without `maxlen`, or an array without
`count`, cannot be sized, so on the `c-cpp` path such a field fails generation
with an error naming the field and the missing attribute. That holds in **both**
storage modes — `allow_dynamic` picks the container, never whether a bound is
needed — so one schema stays valid for every `c-cpp` target. The `count`
requirement covers **every** array element kind, including a plain numeric array:
a count-less native scalar array (e.g. `array` of `u32`) is rejected too, rather
than silently lowering to a zero-length `std::array<T, 0>` (generator#104). For
genuinely unbounded fields, use `corelib: cpp`.

**Storage mode (`allow_dynamic`).** With every field bounded, the switch chooses
where those fields live. The same mapping applies on `corelib: cpp`, where it is
the other way round by default and only reaches the fields the schema bounds:

| schema | `allow_dynamic: false` (inline) | `allow_dynamic: true` |
|---|---|---|
| `string, maxlen 8` | `sofab::FixedString<8>` | `std::string` |
| `blob, maxlen 8` | `sofab::FixedBytes<8>` | `std::vector<std::uint8_t>` |
| `array u32, count 3` | `sofab::InlineVector<std::uint32_t, 3>` | `std::vector<std::uint32_t>` |
| `array string, count 2, maxlen 4` | `sofab::InlineVector<sofab::FixedString<4>, 2>` | `std::vector<std::string>` |

Inline is the default and the one that guarantees no allocation at all: the
worst case is the object's size, known at compile time. Dynamic suits a target
that has a heap and the C++ stdlib — a field then holds what the message
actually carries rather than its declared worst case, and a message moves
instead of copying, which matters once a bound is large enough that the inline
object no longer fits comfortably on a stack.

The bounds do not weaken. What was the inline container's capacity becomes an
explicit check on the decode path (`_size > maxlen` / `_count > count` →
`invalidate()`, and `cap`/`elemMax` on the sequence collectors), placed after the
§7.3 wire-type guard and before the resize — so an over-long field is rejected as
`INVALID` rather than allocating what the bound exists to prevent. `_maxSize` is
identical in both modes, and so is encode output — for every value, with no
exception: both containers a bounded field can land in carry a logical length, so
there is no value one mode can hold and the other cannot. See
[`count` is a capacity](#count-is-a-capacity).

The `encode()` convenience method still returns a `std::vector<std::uint8_t>`
(heap) for host-side use; embedded callers use the non-allocating
`encodeTo(dst, cap)`. Because `encode()` — and, under `allow_dynamic`, the field
storage itself — uses `std::string`/`std::vector`, the `<string>`/`<vector>`
header includes are retained.

Note: the `-Os -ffunction-sections -fdata-sections -fno-exceptions -fno-rtti`
compile flags and `-Wl,--gc-sections` link flag ship in the generated `c-cpp`
`Makefile` (all generated + corelib code is `noexcept` and uses no RTTI) — a
`.text` win with no wire/API change.

```yaml
targets:
  cpp:
    namespace: myproj
    corelib: c-cpp        # embedded profile; every field must be bounded
    allow_dynamic: true   # optional: std::string/std::vector storage (needs a heap)
```

## Nested arrays (arrays of arrays)

An array of arrays lowers to a wrapper sequence whose elements are the **rows**
(MESSAGE_SPEC §5.1: an element id IS its index). How a row is read depends on
what the row holds, and there are exactly two cases:

- **A row of native scalars** (`array<array<u32>>`, `array<array<fp32>>`) is a
  span of trivially-copyable values, so `sofabgen::WrapperSeq` places the row at
  its element id and hands it to `is.read(row)`.
- **A row of strings, blobs, structs/unions or further arrays** is itself a
  wrapper *sequence*. It is neither a span of scalars nor an `IStreamMessage`, so
  `is.read(row)` has nothing to do with it — handing the row container to
  `MessageSeq<T>` fails the corelib's `static_assert("Unsupported span element
  type in IStream::read()")` and the header does not compile (generator#250).
  These rows get a small **generated row collector** instead: it places the row
  at its element id and then reads the row with exactly the emission the first
  level uses — `sofab::StringSeq` / `BlobSeq` / `sofabgen::WrapperSeq` on the
  `cpp` leg, `sofab::FixedStringSeq` / `FixedBlobSeq` / `sofabgen::WrapperSeq` on
  the `c-cpp` leg.

The collector is generated rather than shipped by the corelib because what a row
costs to read is the *schema's* business (its element bounds and element type),
not the wire format's — the corelib still owns every byte that is actually read.
It is emitted as a local struct at the point of use, and it nests: depth 3
(`array<array<array<string>>>`) wraps the depth-2 collector, so there are no
per-shape special cases. The two spec rules it carries are the same ones the
corelib collectors carry: §5.1 placement plus the over-index reject at the schema
`count` (the fixed profile reads that bound off the `InlineVector` capacity,
which also stops a saturating `emplace_back()` from spinning — issue #126), and
§7.4 replace-whole (`prepare()` on the `cpp` leg, `readSequence()`'s own clear on
the `c-cpp` leg). Both legs produce byte-identical wire output for these shapes,
as for every other.

> **Gap.** A nested row of `enum` or `boolean` elements
> (`array<array<enum>>`, `array<array<boolean>>`) still does not compile: those
> element kinds are value-converted through a native-typed temporary rather than
> read in place, which the row path does not do yet. First-level `array<enum>` /
> `array<boolean>` are unaffected.

## `count` is a capacity

A schema `count: N` is a **capacity**, never a length (MESSAGE_SPEC §3). It never
reaches the wire, it bounds the array — an element count or an element id past
`N` fails the decode as `INVALID` — and it lets the embedded profile pre-size
its storage. It never adds an element.

What follows from that, in C++:

- A fresh `count: N` **wrapper** array (string/blob/struct/union/row elements) is
  **empty**, in all three storage kinds — `std::vector`, and
  `sofab::InlineVector<T, N>`, whose N inline slots are a capacity and whose
  *logical* length starts at 0. A declared `default` shorter than `N` is
  materialized exactly as written and never tail-padded.
- Encode writes **every** element the container holds. `{1, 2, 0, 0}` and
  `{1, 2}` are different values with different bytes; there is no
  trailing-default-run elision on either element kind any more.
- Decode yields exactly the elements the wire carried. Nothing is filled in past
  the highest element the wire named, so `size()` after a round trip equals
  `size()` before it.
- A field is omitted only when it **equals its default** — for an array with no
  declared default, only when it is empty.

### Every profile agrees, because every array member has a length

A `count: N` array of **native scalars** used to be stored in `std::array<T, N>`
on the `cpp` profile and on `c-cpp` with `allow_dynamic: false`. That
container has no logical length — its value is always N elements — so those two
profiles could not express a shorter array at all, and the *same schema* then had
two different wire images and two different decode results. That broke the
byte-identity promise this document makes at the top and the one `checkBounded`
enforces ("the storage switch never changes the wire").

Every array member is length-carrying now, so for a `count: 4` `u32` array all
four profiles agree, in both directions:

| value | wire (all four profiles) | decodes back to |
|---|---|---|
| `[1, 2, 0, 0]` | `7b 04 01 02 00 00` | `[1, 2, 0, 0]` |
| `[1, 2]` | `7b 02 01 02` | `[1, 2]` |
| `[]` | `7b 00` | `[]` |
| equal to the declared default | field omitted | the declared default |

What makes it true: `std::vector<T>` under `allow_dynamic: true` on either
corelib; `sofab::InlineVector<T, N>` under `allow_dynamic: false` on either —
inline slots plus a `len_`, so a heap-free build stays heap-free while still
expressing 0..N. `InlineVector` needed one addition
for this, a `resize()`, so that `IStreamImpl::readArray` recognises it as a
*resizable* destination and sizes it to the wire count; before that it fell to
readArray's fixed-extent branch, which set the length to 0 and dropped the array.

**Cost.** On the `maxspeed` profile this replaces an inline `std::array` member
with a heap `std::vector`, and that is not free: on the
`examples/messages/realworld/vehicle_telemetry.yaml` bench row (`cpp-cpp`) it is
**+5.5% encode and +21.7% decode** Ir/op. It is paid because there is no
alternative that keeps the wire honest: `sofab::InlineVector` lives in
corelib-c-cpp only, corelib-cpp ships no length-carrying inline container, and
generated headers deliberately define no containers of their own. The heap-free
row (`cpp-c-cpp`) is unaffected on encode (-0.02%) and +1.1% on decode, for
+3.2%/+4.4% `.text`.

## Wrapper arrays: element placement and the sparse element rule

An array of strings, blobs, structs, unions or rows travels as a **sequence whose
child id is the element's index** (MESSAGE_SPEC §5.1), and its decoded length is
*highest present id + 1*.

The generated header carries a small `namespace sofabgen` block for the element
collector, emitted once per header behind `#ifndef SOFABGEN_WRAPPER_SEQ_HELPERS`
at global scope, so several generated headers — even with different `namespace`
settings — can be included into one translation unit. It is the only thing the
generator emits outside its configured namespace, and it holds no state.

**1. An element is placed at its id, never appended.** `sofabgen::WrapperSeq`
gap-fills the destination with default elements up to `id` and then decodes into
`dest[id]`. Appending would shorten the array by the size of every interior id
gap — and rule 2 makes such gaps ordinary input — and would decode a **reopened**
element id as a second element instead of continuing the first (§7.4
struct-merge, which placement gives for free). An element id at or past `count`
is rejected as `INVALID` **before** the fill, which also bounds it
(generator#247). The leaf collectors stay in the corelib —
`sofab::StringSeq`/`BlobSeq` and `FixedStringSeq`/`FixedBlobSeq` always placed;
this is the object path agreeing with them. The corelib collectors that append
id-blind (`MessageSeq` / `FixedMessageSeq`) are not used by generated code.

**2. The interior is sparse; the last element is always written.** One rule, both
element kinds, decided from the position in the **value** at run time:

- an element **before the last one** that equals its element default is omitted
  and leaves an id **gap** — a `string`/`blob` leaf is simply not written, and a
  struct/union/row element is **not framed** either (`os.writeLazy`, whose
  lazily-held frame vanishes when the nested serialize writes no child);
- the **last** element is always written — a leaf as its value, a sequence
  element as an **empty frame** (`os.write`, the frame-keeping form) — because
  its presence is what carries the length.

So `{"a", ""}`, `{"a"}` and `{}` are three distinct values that encode and decode
distinctly, and `{"", ""}` travels as its final element alone, at id 1. A
declared `count: N` changes none of it: N is a capacity and can never restore an
elided tail, so the counted and count-less arrays emit the identical loop.

**3. The field wrapper itself still closes with the dropping end.** A
sequence-typed *field* — as opposed to an *element* — is omitted when it is
empty, and absence reconstructs it (§2). An all-default message still encodes to
zero bytes.

Every generated struct, union and message also carries
`bool _isDefault() const noexcept`: the explicit form of the "was any child
written?" test the lazy framing answers implicitly. It and the serialize loop are
generated from **one** expression per field, so the writer and the predicate
cannot drift — a predicate that called a field default which the writer puts on
the wire would omit it.

Two consequences worth knowing:

- A mistyped child inside a wrapper array is skipped with **no container
  mutation** — the wire-type decision precedes the gap-fill, so it cannot leave a
  phantom default element behind (generator#249).
- An array of **rows** (`array` of `array u32, count 3`) applies rule 2 to the
  row's own default value, which — now that a row carries a length like any other
  array — is the EMPTY row: an empty interior row is omitted and leaves an id gap,
  and the last row is always written.

## Struct member order (widest-first)

The members of a generated message struct are declared **widest-first**
(8→4→2→1-byte alignment; strings, blobs, containers and nested types rank
as 8), not in schema order, so the compiler inserts less padding between them.
Members of equal alignment keep their schema order. This affects **declaration
order only** — encode iterates the schema/field-id order, so the wire bytes are
byte-identical to every other target. Initialize members by name (designated
initializers or assignment), not with positional aggregate initialization.

## Benchmark row

Row `cpp-cpp` (corelib `cpp`) and `cpp-c-cpp` (corelib `c-cpp`) in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **toggle** method. Tracked: Ir/op for both; `cpp-c-cpp` also `.text`/`.data`/`.bss` on ARMv6-M and ARMv7-M+fp.dp.

`cpp-c-cpp-dyn` is the same row with `allow_dynamic: true`, and the two are meant to
be read as a pair — `allow_dynamic` only does anything on `c-cpp`, since `corelib:
cpp` is heap-backed either way. Switching it on moves variable-length fields off
inline, schema-bound storage and onto the heap, which is not a saving: the inline
build never calls `operator new`, so the dynamic one drags in newlib's malloc and
`.text` goes 6589 → 14287 on ARMv6-M while the objects get smaller. Neither number
captures the heap the dynamic build then needs at runtime.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.

## §7.1: the declared integer width is a validity bound (issue #266)

A `u8`/`u16`/`u32`/`i8`/`i16`/`i32` destination rejects a value outside its
declared range with `is.invalidate()`. The width is a normative bound, not a
storage hint (MESSAGE_SPEC §1/§7.1).

This backend needed a different shape from the others. corelib-cpp's typed
`read()` ends in `value = static_cast<T>(raw)` — that IS the mask §7.1 forbids,
and it happens where generated code cannot see the raw value. So a narrow
destination reads through a 64-bit temporary and range-checks before the store:

```cpp
case 0: { std::uint64_t _v; if (is.read(_v)) {
            if (_v > 255) { is.invalidate(); return; }
            a_u8 = static_cast<std::uint8_t>(_v); } } break;
case 3: is.read(d_u64); break;   // u64: the direct typed read, nothing to bound
```

§7.3 is unaffected by the wider temporary: `read()` derives its expected wire
type from signedness alone (`Wire::Unsigned` for every `u*`, `Wire::Signed` for
every `i*`), so `u64` and `u8` frame identically. A contradicting tag still
returns `false` and the arm stores nothing — the skip, not a reject.

**The `c-cpp` profile is exempt and untouched.** It was already conformant: its
deferred descriptor carries the declared width to the corelib, which rejects
there. Adding a generator-side guard would only duplicate it.

### Array elements: the bound is armed, not inlined (issue #279)

`is.readArray(dst, count)` converts the elements *inside* corelib-cpp
(`sp[i] = static_cast<Elem>(raw)`), so the raw value never reaches generated code
and the scalar trick above does not carry over. Routing arrays through a wide
temporary would defeat the bulk/zero-copy path this profile exists for.

corelib-cpp closed that by taking the bound as an argument, so the generator hands
it in and the corelib enforces it at the point of conversion:

```cpp
is.readArray(u8s, 5, -1, sofab::ElemBound::of<std::uint8_t>());
```

Left at its default the argument is **unarmed** and the unbounded decode runs —
which is what masked 5208 into a `u8` array down to 88 (Crucible F-0052). Two
details of the emission are deliberate:

- **64-bit elements get the argument too.** `ElemBound::of` returns unarmed for
  them, so the corelib's own helper decides rather than a special case here.
- **Floating-point elements do not.** `ElemBound::of<float>()` would cast
  `std::numeric_limits<float>::max()` to `int64_t` inside a `constexpr` function
  — out of range, so a hard compile error rather than a wrong bound. The corelib
  ignores the argument for a non-integral element anyway.

`c-cpp` keeps its own shape: it was already conformant through its descriptor
path, and its `readArray` has a different signature.
