# C++ target — `targets.cpp`

Options accepted under `targets.cpp`. For shared options (`emit`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. |
| `corelib` | `cpp` \| `c-cpp` | `cpp` | Which C++ corelib the generated code targets. This also picks the container representation: `cpp` = dynamic (`std::vector`/`std::string`), `c-cpp` = fixed-capacity/heap-free (see below). |
| `allow_dynamic` | bool | `false` | `corelib: c-cpp` only. Keep a `std::vector`/`std::string` heap fallback for genuinely unbounded fields instead of failing generation. |
| `namespace` | string | `message` | C++ namespace wrapping the generated types. Also settable in `generic`. |
| `max_dyn_array_count` | integer | unset = unlimited | See [generic config](README.md). `corelib: cpp` only (`c-cpp` is statically schema-bounded). Emits a per-field `is.exceedLimit()` guard on unbounded count-prefixed arrays; `feed()`/`try_decode` then return `Error::LimitExceeded`. |
| `max_dyn_string_len` / `max_dyn_blob_len` | integer | unset = unlimited | See [generic config](README.md). `corelib: cpp` only. Per-field `_size` guard on unbounded strings/blobs, plus a **derived** streaming reassembly cap passed as `sofab::Limits{max_buffered_field}` into the one-shot decode entry points — `max(string cap, blob cap, largest schema maxlen, largest schema count * 10)`, so legitimately schema-bounded fields always still fit when fed in chunks. |

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
a container is a corelib change, not a codegen change).

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
The generated per-element collectors (`_FixedStrSeq` / `_FixedBlobSeq`) place a
string/blob element at its wire index `id` by growing the `InlineVector` up to
that slot; because `emplace_back()` is a no-op once the vector is full, an
untrusted element index `id >= N` is **dropped** (the fill loop is guarded by the
container capacity) rather than spun on forever — the corelib skips the element's
payload since the callback binds no destination, mirroring the native-array
over-capacity drop (MESSAGE_SPEC §5.1). Without the guard a 4-byte message could
hang the decoder (issue #126, DoS).
Because they are non-aggregates with `initializer_list` constructors, a brace-init
like `msg.field = {"a", "b"}` sets the logical length correctly rather than
silently leaving it at zero (which would drop the field from the wire). A
non-allocating `encodeTo(dst, cap)` is also emitted alongside the convenience
`encode()`.

**Unbounded fields.** A string or blob without `maxlen`, or an array without
`count`, cannot be sized, so on the `c-cpp` path such a field fails generation with
an error naming the field and the missing attribute — unless `allow_dynamic: true`
keeps a `std::string`/`std::vector` fallback for it (bounded fields still go
fixed). This makes "no hidden allocation" the default guarantee: size your schema,
or consciously opt a field into a heap fallback. The `count` requirement covers
**every** array element kind, including a plain numeric array: a count-less native
scalar array (e.g. `array` of `u32`) is rejected too, rather than silently
lowering to a zero-length `std::array<T, 0>` (generator#104).

The `encode()` convenience method still returns a `std::vector<std::uint8_t>`
(heap) for host-side use; embedded callers use the non-allocating
`encodeTo(dst, cap)`. Because `encode()` and the `allow_dynamic` fallbacks may
still use `std::string`/`std::vector`, the `<string>`/`<vector>` header includes
are retained.

Note: the `-Os -ffunction-sections -fdata-sections -fno-exceptions -fno-rtti`
compile flags and `-Wl,--gc-sections` link flag ship in the generated `c-cpp`
`Makefile` (all generated + corelib code is `noexcept` and uses no RTTI) — a
`.text` win with no wire/API change.

```yaml
targets:
  cpp:
    namespace: myproj
    corelib: c-cpp        # fixed-capacity, heap-free containers
    allow_dynamic: true   # optional: keep std::vector/std::string for unbounded fields
```

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
