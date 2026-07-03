# C++ target — `targets.cpp`

Language-specific options for the C++ backend. For shared options (`emit`,
`file_layout`, `buffer`, …) see the [`generic`](README.md)
config.

## Honored options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `corelib` | `cpp` \| `c-cpp` | `cpp` | Which C++ corelib the generated code targets (see below). |
| `containers` | `dynamic` \| `fixed` | `fixed` on `c-cpp`, else `dynamic` | `fixed` = the heap-free embedded profile: blobs and struct/union/matrix/blob sequences become fixed-capacity inline storage sized from the schema. Requires `corelib: c-cpp` (see below). |
| `allow_dynamic` | bool | `false` | Under `containers: fixed`, keep a `std::vector`/`std::string` fallback for genuinely unbounded fields instead of failing generation. |
| `namespace` | string | `messages` | C++ namespace wrapping the generated types. Also settable in `generic`. |

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

### `containers: fixed` (embedded footprint profile)

The heap-free profile, and the **default for `corelib: c-cpp`** — embedded is the
regular use of that target, so fixed containers are what you get unless you set
`containers: dynamic`. (On the pure `corelib: cpp` max-speed path the default is
`dynamic`, and `fixed` there is a generate-time error.) It removes hidden dynamic
allocation from the generated message code, for embedded targets with very
constrained memory. Wire output is **unchanged** — this is purely an in-memory
representation change, so
the shared conformance vectors and every sha256 stay identical.

What changes (all sized from the schema's `maxlen`/`count`):

| Field kind | default `c-cpp` | `containers: fixed` |
|---|---|---|
| blob (`maxlen N`) | `std::vector<std::uint8_t>` | `FixedBytes<N>` (inline, no heap) |
| blob array (`count N`, elem `maxlen M`) | `std::vector<std::vector<std::uint8_t>>` | `InlineVector<FixedBytes<M>, N>` |
| struct / union / matrix array (`count N`) | `std::vector<T>` | `InlineVector<T, N>` |
| native numeric/enum/bool/bitfield array | `std::array<T, N>` | unchanged (already fixed) |
| string (scalar or array element) | `std::string` / `std::vector<std::string>` | **unchanged — see below** |

`FixedBytes<N>` / `InlineVector<T,N>` are emitted header-only into the generated
prelude (no extra files ship). `InlineVector` separates capacity (`N`) from
logical length, and its inline storage never reallocates, so it is strictly safer
under the corelib-c-cpp deferred decoder than the `std::vector` + `reserve()` it
replaces. A non-allocating `encodeTo(dst, cap)` is also emitted alongside the
convenience `encode()`.

**Strings are not yet fixed.** A `FixedString<N>` needs a decode entry point in
corelib-c-cpp for a non-`std::string` character buffer (the scalar read is
hard-gated on `std::is_same_v<T, std::string>`, and `IStreamImpl`'s stream
context is not reachable from generated code, so no interim bridge is possible).
Until that lands, strings stay `std::string` even under `containers: fixed`.
Because `encode()`'s `std::vector` return and strings both remain, the `<string>`
and `<vector>` header includes are also retained for now.

**Unbounded fields.** A blob without `maxlen`, or an array without `count`, cannot
be sized. Under `containers: fixed` such a field fails generation with an error
naming the field and the missing attribute, unless `allow_dynamic: true` keeps a
`std::vector` fallback for it (bounded fields still go fixed). This makes "no
hidden allocation" an explicit, opted-into guarantee rather than a silent
per-field fallback.

Note: the `-Os -ffunction-sections -fdata-sections -fno-exceptions -fno-rtti`
compile flags and `-Wl,--gc-sections` link flag now ship in the generated
`c-cpp` `Makefile` **unconditionally** (all generated + corelib code is `noexcept`
and uses no RTTI), independent of `containers` — a `.text` win with no wire/API
change.

```yaml
targets:
  cpp:
    namespace: myproj
    corelib: c-cpp            # containers: fixed is implied (the default here)
    allow_dynamic: true       # optional: keep std::vector for unbounded fields
    # containers: dynamic     # set this to opt back out to std::vector/std::string
```

## Reserved options

Accepted by the schema validator but not yet honored by the generator:

`cpp_standard` · `header_only` · `max_size_strategy` · `zero_copy_strings`
