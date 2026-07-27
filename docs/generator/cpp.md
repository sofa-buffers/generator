# C++ target — `targets.cpp`

Target-specific options, accepted under `targets.cpp`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `corelib` | `cpp` \| `c-cpp` | `cpp` | Which C++ corelib the generated code targets. This also picks the container representation: `cpp` = dynamic (`std::vector`/`std::string`), `c-cpp` = fixed-capacity/heap-free (see below). |
| `allow_dynamic` | bool | `false` | `corelib: c-cpp` only. Store bounded fields in `std::string`/`std::vector` instead of inline containers, for a target with a heap. Bounds stay mandatory either way. |
| `namespace` | string | `message` | C++ namespace wrapping the generated types. Also settable in `generic`. |

### What the two profile keys combine into

`corelib` picks the runtime; `allow_dynamic` picks the storage inside the
embedded profile. The three usable combinations:

| `corelib` | `allow_dynamic` | Storage | Bounds | Heap |
|---|---|---|---|---|
| `cpp` (default) | *ignored* | `std::string` / `std::vector` | optional; `max_dyn_*` caps what the schema leaves open | yes |
| `c-cpp` | `false` (default) | `FixedString<N>` / `FixedBytes<N>` / `std::array` / `InlineVector<T,N>` | **mandatory** | none on the message path |
| `c-cpp` | `true` | `std::string` / `std::vector` | **mandatory** | yes |

The two `c-cpp` rows accept exactly the same schemas and produce byte-identical
encode output and the same `_maxSize`, so the switch is a per-device decision and
never a schema change. What differs is where a field's bytes live, and therefore
what a message costs to hold and to move.

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
(count present) stays a fixed `std::array<T, N>`. Decode sizes a dynamic native
vector to the wire element count before filling it, and the
[`max_dyn_array_count`](#options) cap (when set) rejects an over-cap count as
`Error::LimitExceeded` before any allocation.

### `corelib: c-cpp` = fixed-capacity (embedded) containers

`corelib: c-cpp` targets real embedded devices, so it **always** uses fixed-capacity,
heap-free containers — there is no separate knob. This removes hidden dynamic
allocation from the generated message code. If a target has the resources for a
heap, use `corelib: cpp` (which uses `std::vector`/`std::string`). Wire output is
identical either way — this is purely an in-memory representation change, so the
shared conformance vectors and every sha256 stay the same.

What `c-cpp` produces vs `cpp` (all sized from the schema's `maxlen`/`count`):

| Field kind | `corelib: cpp` (dynamic) | `corelib: c-cpp` (fixed) |
|---|---|---|
| string (`maxlen N`) | `std::string` | `sofab::FixedString<N>` (inline, no heap) |
| blob (`maxlen N`) | `std::vector<std::uint8_t>` | `sofab::FixedBytes<N>` (inline, no heap) |
| string array (`count N`, elem `maxlen M`) | `std::vector<std::string>` | `sofab::InlineVector<sofab::FixedString<M>, N>` |
| blob array (`count N`, elem `maxlen M`) | `std::vector<std::vector<std::uint8_t>>` | `sofab::InlineVector<sofab::FixedBytes<M>, N>` |
| struct / union / matrix array (`count N`) | `std::vector<T>` | `sofab::InlineVector<T, N>` |
| native numeric/enum/bool/bitfield array | `std::array<T, N>` | `std::array<T, N>` (already fixed) |

All three fixed-capacity containers — `sofab::FixedString<N>`,
`sofab::FixedBytes<N>` and `sofab::InlineVector<T,N>` — live in the corelib-c-cpp
wrapper (`sofab.hpp`) as a single source of truth; the generator only references
them (nothing container-shaped is emitted into the generated headers, so a fix to
a container is a corelib change, not a codegen change — the one generated block,
`namespace sofabgen`, holds the wrapper-array element helpers described
[below](#wrapper-arrays-element-placement-refill-trailing-run-trim), which are
decided by the schema `count` and so cannot live in a corelib).

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
container (inline `std::array` storage + a separate `len_`) is the only fit — and
where a field really *is* fixed-length (the native numeric arrays), the generator
does use plain `std::array<T,N>`. `InlineVector`'s inline storage also never
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
where those fields live:

| schema | default (inline) | `allow_dynamic: true` |
|---|---|---|
| `string, maxlen 8` | `sofab::FixedString<8>` | `std::string` |
| `blob, maxlen 8` | `sofab::FixedBytes<8>` | `std::vector<std::uint8_t>` |
| `array u32, count 3` | `std::array<std::uint32_t, 3>` | `std::vector<std::uint32_t>` |
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
`INVALID` rather than allocating what the bound exists to prevent. Encode output
and `_maxSize` are identical in both modes, so the two interoperate byte for
byte.

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
  span of trivially-copyable values, so the corelib's own collector reads it:
  `sofab::MessageSeq<std::array<T,N>>` / `sofab::FixedMessageSeq<...>` places the
  row and hands it to `is.read(row)`.
- **A row of strings, blobs, structs/unions or further arrays** is itself a
  wrapper *sequence*. It is neither a span of scalars nor an `IStreamMessage`, so
  `is.read(row)` has nothing to do with it — handing the row container to
  `MessageSeq<T>` fails the corelib's `static_assert("Unsupported span element
  type in IStream::read()")` and the header does not compile (generator#250).
  These rows get a small **generated row collector** instead: it places the row
  at its element id and then reads the row with exactly the emission the first
  level uses — `sofab::StringSeq` / `BlobSeq` / `MessageSeq<Element>` on the
  `cpp` leg, `sofab::FixedStringSeq` / `FixedBlobSeq` / `FixedMessageSeq` on the
  `c-cpp` leg.

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

## Wrapper arrays: element placement, refill, trailing-run trim

An array of strings, blobs, structs, unions or rows travels as a **sequence whose
child id is the element's index** (MESSAGE_SPEC §5.1). Three rules govern that
shape, and all three are decided by the schema `count` — which only the generator
knows (CORELIB_PLAN §7), so they live in generated code rather than in either
corelib.

The generated header carries a small `namespace sofabgen` block for them, emitted
once per header behind `#ifndef SOFABGEN_WRAPPER_SEQ_HELPERS` at global scope, so
several generated headers — even with different `namespace` settings — can be
included into one translation unit. It is the only thing the generator emits
outside its configured namespace, and it holds no state: a collector
(`WrapperSeq`), the refill (`fillTo`) and the two trims (`trimObjs`,
`trimEmpty`).

**1. An element is placed at its id, never appended.** `sofabgen::WrapperSeq`
gap-fills the destination with default elements up to `id` and then decodes into
`dest[id]`. Appending would shorten the array by the size of every interior id
gap, and would decode a **reopened** element id as a second element instead of
continuing the first (§7.4 struct-merge, which placement gives for free). An
element id at or past `count` is rejected as `INVALID` **before** the fill, which
also bounds it (generator#247). The leaf collectors stay in the corelib —
`sofab::StringSeq`/`BlobSeq` and `FixedStringSeq`/`FixedBlobSeq` always placed;
this is the object path agreeing with them.

**2. A `count: N` array decodes back out to N.** `sofabgen::fillTo` pads the
destination once the sequence is settled, because §5.1 makes the length N *for
every target* — "a growable-list target MUST default-fill to N exactly like a
pre-sized one". It is gated on the field actually having been a sequence, so a
§7.3 skip never materialises N elements out of nothing. A count-less array has no
N to pad to and is left at highest-present-id + 1.

**3. The encoder stops at M.** A `count: N` array's canonical wire carries only
`[0, M)`, M being one past the last element differing from the element default —
"even for sequence-form elements" (§3/§5.1). Interior all-default elements keep
their frame (element presence is what carries the length); only the trailing run
goes. `M == 0` writes no child at all, so the lazily-opened wrapper is dropped by
its closer and the whole field is omitted (§2). A dynamic array is never narrowed
— it has no N to refill from, so a trailing default element is significant
(generator#248).

Rule 3 is only lossless *because* of rule 2: without the refill, the elision
would shorten the array on every decode/encode cycle instead of normalising it.

Every generated struct, union and message therefore also carries
`bool _isDefault() const noexcept` — the explicit form of the "was any child
written?" test the lazy framing already answers implicitly for a *field*, needed
here because an *element* must be judged before the loop opens. The serialize
loop and `_isDefault` are generated from **one** expression per field, so the
writer and the predicate cannot drift: a predicate that narrowed a field the
writer does not (or the reverse) would either omit a field that is on the wire or
keep one that is not.

Two consequences worth knowing:

- A mistyped child inside a wrapper array is skipped with **no container
  mutation** — the wire-type decision precedes the gap-fill, so it cannot leave a
  phantom default element behind (generator#249).
- An array of **fixed-length rows** (`array` of `array u32, count 3`) trims a
  trailing run of *empty* rows only. A fixed row is never empty (its size is its
  count), so such an array is not narrowed — the same behaviour as the other
  targets.

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

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.
