# Plan: shrink the C++ `c-cpp` code + memory footprint (EmbeddedProto-style)

Status: PLAN ONLY (no generator code changed by this document).
Scope: the C++ backend `generators/cpp/` in **`corelib: c-cpp`** mode (the
embedded-friendly path — corelib-c-cpp, the C library + thin C++ wrapper).
Goal: eliminate hidden dynamic allocation and cut `.text` on the always-linked
message path, sized from the schema's `maxlen`/`count`, without changing the
wire bytes or the shared conformance vectors.

All file:function references are to this repo. All "before" C++ snippets are the
**actual generated output** for `examples/messages/example.yaml` in `c-cpp`
mode (regenerated during analysis; header = `myfirstmessage.hpp`).

> **This is now measurable.** §3's `.text` inventory below was hand-reasoned;
> [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15) now cross-compiles the
> `cpp-c-cpp` row to ARMv6-M / ARMv7-M+fp.dp and records `.text`/`.data`/`.bss` in a
> committed `results.txt`, so this plan's effect can be read off a diff rather than
> argued. Two current readings are worth having in mind as the baseline:
>
> * the row measures **`bss = 180`** — static RAM in a profile that advertises none;
> * it cannot be built `-ffreestanding` at all, because the emitted header pulls in
>   `<string>`/`<vector>` and libstdc++ rejects those on a bare-metal target
>   (*"This header is not available in freestanding mode"*). `tests/bench` therefore
>   drops `-ffreestanding` for the cpp rows and treats them as a bloat tracker
>   rather than a flash budget. **Landing this plan should let that flag come back**
>   — that is the acceptance test, and the number should step down when it does.

---

## 1. How `c-cpp` mode is selected and where it diverges

- Selected by config key `corelib: c-cpp` under `targets.cpp` (default `cpp`).
  `backend.go:Generate` sets `gen.clib = cfgString(cfg,"corelib","cpp")=="c-cpp"`
  (backend.go:36, 47-57). Conformance drives it via
  `tests/conformance/cpp/run.sh` (`corelib: c-cpp`), and the generated Makefile
  switches to `cppClibMakefile` (project.go:34-35, 61-90), which compiles+links
  corelib-c-cpp's C sources (`object.c/ostream.c/istream.c`) and needs only
  `SOFAB_C_DIR`.
- The `clib` flag changes: the header include of `cppPrelude` is **skipped**
  (backend.go:97-99 — `_StrSeq`/`_BlobSeq` are corelib-cpp only); `deserialize`
  gains a named `_size` param (backend.go:189-193); and decode of every
  variable-length field is rewritten to pre-size from `_size` and read via the
  wrapper's native overloads (backend.go:356-390, 414-462).
- Decode model (ARCHITECTURE §9.3 family 1 / §10): the corelib-c-cpp wrapper is
  a **deferred flat-visitor** decoder. `IStreamImpl::read(target)` only *binds*
  a target by address; the bytes are filled by a later `feed()` pass. Therefore
  every bound target must be **alive and address-stable** until decode of the
  field completes. This is why:
  - the `_MsgSeq<T>` element visitor is held in `static` storage on the `c-cpp`
    path and the target vector is `reserve()`d up front
    (helpers.go:391-398 `cppMsgSeqPrelude`; backend.go:453-462
    `deserializeSeqInto`; see generated `case 23/24/25` in the header) — a
    stack-local visitor would be a **use-after-return** (the documented hazard),
  - enum/bool arrays read in place via `reinterpret_cast` rather than through a
    local temp (backend.go:378-390, 414-426),
  - strings/blobs are pre-sized before the bind (backend.go:356-369).
  Any fixed-storage replacement must preserve exactly this address-stability
  contract (fixed inline storage is *strictly safer* here — it never
  reallocates — see §7).

---

## 2. STL / dynamic-memory inventory of the emitted `c-cpp` C++

Legend: **HEAP** = allocates on the free store (message path); **STACK** =
in-object, no heap; **HARNESS** = optional JSON project only (not the embedded
artifact).

| STL type / construct | Where emitted (generator) | Emitted in | Alloc |
|---|---|---|---|
| `std::string` member | `helpers.go:cppType` (L71-72), `cppArrayElem` (L114-115) | `somestring`, `nestedstring`, `option2`, `asstring`, `label`, `somemap.key`, string-array element | **HEAP** (unless SSO ≤15B) |
| `std::vector<std::uint8_t>` (blob) member | `helpers.go:cppType` (L73-74), `cppArrayElem` (L116-117) | `someblob` | **HEAP** |
| `std::vector<std::string>` (string-array) | `helpers.go:cppArrayContainer` (L106) | `somestringarray` | **HEAP** ×(1 vec + N strings) |
| `std::vector<std::vector<uint8_t>>` (blob-array) | `cppArrayContainer` (L106) | `someblobarray` | **HEAP** ×(1 vec + N vecs) |
| `std::vector<StructElem>` (struct/union/matrix/map array) | `cppArrayContainer` (L106) | `somestructarray`, `someunionarray`, `somematrix`, `somemap` | **HEAP** |
| `_MsgSeq<T>` decode collector (`std::vector<T>* out`, `emplace_back`) | `helpers.go:391-398 cppMsgSeqPrelude`; `backend.go:453-462` | decode of the 4 above | **HEAP** (`emplace_back`) |
| `encode()` returns `std::vector<std::uint8_t>` | `backend.go:162-166` | every message `encode()` | **HEAP — always-on** |
| `std::array<T,N>` (native numeric/enum/bool/bitfield array) | `cppArrayContainer` (L104) | `someuintarray`, `someintarray`, `somefloatarray`, `someenumarray`, `someboolarray`, `somebitfieldarray`, `somestructwitharray.values`, matrix inner | **STACK — already good** |
| `sofab::OStreamInline<_maxSize>` inline encode buffer | `backend.go:163` | `encode()` | **STACK — already good** |
| corelib `std::function` (`flushCallback`/`fieldCallback`) | corelib-c-cpp `sofab.hpp` | only `OStream`/`IStreamInline` ctors — **not** on the generated `OStreamInline`/`IStreamObject` path | none on hot path |
| corelib `std::shared_ptr<uint8_t[]>` | corelib `OStream` | heap `OStream` only — generated code uses `OStreamInline` | not used |
| `std::span` | corelib `read/write` array branch | native-array read/write | STACK (view) |
| `<sstream>` `std::ostringstream`, `<iostream>` `std::cin/cout/cerr` | `project.go:harnessMain` (L286-301) | `harness/main.cpp` | **HARNESS only** |
| `std::ostream`, `std::snprintf` JSON formatting | `project.go:jsonHelpers`, `emitToJSON` | `harness/json.hpp` | **HARNESS only** |

Not emitted anywhere: `std::optional`, `std::variant`, `<algorithm>`,
`std::to_string`, exceptions/`throw`. Unions are plain structs with all options
as members (see generated `MyfirstmessageSomeunion`), not `std::variant`.

**Biggest heap offenders (message path), in impact order**
1. `encode()`'s `std::vector<std::uint8_t>` return — one allocation on *every*
   encode, unconditional (backend.go:162-166).
2. String/blob **sequence** arrays — 1 + N allocations each
   (`somestringarray`, `someblobarray`).
3. Struct/union/matrix/map sequence arrays via `_MsgSeq` `emplace_back`
   (`somestructarray`, `someunionarray`, `somematrix`, `somemap`).
4. Scalar `std::string` / blob members (6 strings + 1 blob in the example).

---

## 3. `.text` bloat sources

| Source | On message path? | Note |
|---|---|---|
| `<string>` + `<vector>` includes in every message header (backend.go:79-80) | YES | Pull the full `std::string`/`std::vector` template machinery into every TU. Removable *only* if all string/blob/seq members become fixed **and** unbounded fields are rejected (§9). |
| Per-`(elem,N)` template instantiation of corelib `read/write` array branches | YES | Already `if constexpr`-gated (one body per used type). Fixed containers add instantiations but are still contiguous `.data()/.size()` → reuse the same span branch (§5). |
| Virtual dispatch (`serialize`/`deserialize` are `override`) → one vtable + RTTI per message/struct/union type | YES | Inherent to the corelib visitor model; not removable without a corelib redesign. Mitigate with `-fno-rtti` (generated code never uses `typeid`/`dynamic_cast`). |
| Exception landing pads | YES but empty | All generated + corelib code is `noexcept`; `-fno-exceptions` drops the tables. |
| `<sstream>`/`<iostream>`/`std::snprintf` | NO (harness) | Lives in `harness/main.cpp` + `harness/json.hpp` only; the embedded artifact is the `.hpp`, which never includes them. **Leave the harness as-is.** |
| Build flags | YES | `cppClibMakefile` uses `CXXFLAGS ?= -O2 -Wall` (project.go:71) — no `-ffunction-sections -fdata-sections -Wl,--gc-sections`, no `-fno-exceptions -fno-rtti`, no `-Os`. Easy win (§6). |

---

## 4. Schema-sizing opportunity (what can become fixed-capacity)

The cost model already derives byte bounds from the same IR fields we need
(`cost.go`): `fieldCost` uses `f.HasMaxlen/f.Maxlen` for string/blob
(L33-37) and `arrayCost` uses `count` + `ElemMaxHas/ElemMax` for arrays
(L80-107). So the *capacity numbers are already computable at codegen time* — the
work is emitting fixed containers instead of dynamic ones.

Map for `example.yaml` (member → sizing source → target):

| Field | maxlen / count | Today | Fixed target |
|---|---|---|---|
| `somestring` | maxlen 50 | `std::string` | `FixedString<50>` |
| `someblob` | maxlen 16 | `std::vector<uint8_t>` | `FixedBytes<16>` |
| `somestruct.nestedstring` | maxlen 32 | `std::string` | `FixedString<32>` |
| `someunion.option2` | maxlen 64 | `std::string` | `FixedString<64>` |
| `somestructwitharray.label` | maxlen 16 | `std::string` | `FixedString<16>` |
| `somestringarray` | count 5, elem maxlen 16 | `std::vector<std::string>` | `std::array<FixedString<16>, 5>` + len |
| `someblobarray` | count 3, elem maxlen 8 | `std::vector<std::vector<uint8_t>>` | `std::array<FixedBytes<8>, 3>` + len |
| `somestructarray` | count 3 | `std::vector<Elem>` | `InlineVector<Elem, 3>` |
| `someunionarray` | count 2 (elem `asstring` maxlen 16) | `std::vector<Elem>` | `InlineVector<Elem, 2>` |
| `somematrix` | count 2 × inner count 4 | `std::vector<std::array<u32,4>>` | `InlineVector<std::array<u32,4>, 2>` |
| `someunionarray.asstring`, `somemap.key` | maxlen 16 / 32 | nested `std::string` | `FixedString<N>` |
| native numeric/enum/bool/bitfield arrays | count present | `std::array<T,N>` | **already fixed — no change** |
| **`somemap`** | **no `count`** (dynamic) | `std::vector<MapElem>` | **UNBOUNDED → policy §9** |

Genuinely unbounded in the example: only `somemap` (id 29 — a map is modelled as
a count-less array of `{key,value}`). Its `key` string *is* bounded (maxlen 32),
but the collection length is not. Everything else is fully sizeable.

---

## 5. Replacement strategy (fixed-capacity containers)

Three small header-only containers, sized from the schema. Emit them as a
generator prelude (guarded like the existing `SOFABUFFERS_GEN_PRELUDE`,
backend.go:95-101) so no extra files ship and multi-message TUs don't redefine.

1. **`FixedString<N>`** — `std::array<char, N>` + `std::size_t len_`.
   - Encode: `operator std::string_view() const` → the corelib's existing write
     branch already accepts anything `convertible_to<std::string_view>`
     (corelib `sofab.hpp` L396-399), so **encode needs no corelib change**.
   - Decode: the corelib string read branch is gated on
     `std::is_same_v<T, std::string>` (corelib `sofab.hpp` L928-932) and calls
     `sofab_istream_read_string_noterm(&ctx_, value.data(), value.size())`.
     `FixedString` is not `std::string`, so **decode needs corelib support**
     (§8 — the one hard dependency): add a wrapper `read()` overload/concept for
     "contiguous mutable char buffer with `data()/size()`". Interim fallback:
     generated decode may call `sofab_istream_read_string_noterm(&is.ctx(),
     s.data(), _size)` directly (the C symbol is linked), reaching under the
     wrapper — acceptable as a bridge, preferred to be replaced by the overload.
2. **`FixedBytes<N>`** (blob) — `std::array<uint8_t, N>` + `len_`, with
   `.data()`/`.size()`.
   - Encode: current code is already pointer+size — `os.write(id, b.data(),
     size)` (backend.go:274) — works unchanged.
   - Decode: current code is `b.resize(_size); is.read(b.data(), _size)`
     (backend.go:368-369); the wrapper's `read(void*, size_t)` blob overload
     (corelib `sofab.hpp` L1038-1049) takes a raw pointer, so
     `FixedBytes` drops in with **no corelib change** (`b.set_len(_size);
     is.read(b.data(), _size)`).
3. **`InlineVector<T, N>`** — `std::array<T, N>` + `len_`, exposing exactly the
   members `_MsgSeq` and serialize use: `data()/size()/reserve()(no-op)/
   emplace_back()/back()/begin()/end()/operator[]`.
   - `_MsgSeq` decode (`out->emplace_back(); is.read(out->back())`,
     helpers.go:395-397) works verbatim once `out` is `InlineVector<T,N>*` —
     placement-construct into the next inline slot, no heap; and because inline
     storage never moves, the deferred bound element is address-stable
     (**strictly safer** than the current `std::vector` + `reserve`).
   - String/blob sequence arrays become `std::array<FixedString/FixedBytes, N>`
     filled **by index** (see §6, needs the string decode support of §8, or a
     generator-emitted element collector modelled on the removed `_StrSeq`/
     `_BlobSeq` prelude but writing to `arr[i++]` instead of `push_back`).

Prefer `std::span`-like views / `.data()+len` over owning containers wherever the
corelib delivers contiguous data — which, with the above, is everywhere on the
`c-cpp` path.

**Decision — where the containers live:** emit `FixedString/FixedBytes/
InlineVector` from the **generator** (prelude), because they are pure in-memory
representation the generator already owns (it owns `_MsgSeq`). The **only** piece
that must land in **corelib-c-cpp** is the decode-side string read for a
non-`std::string` buffer (§8). Do not put the containers in the corelib.

Also address the always-on `encode()` allocation (backend.go:162-166): add a
non-allocating `std::size_t encodeTo(std::uint8_t* dst, std::size_t cap) const`
(serialize into caller storage / an `OStreamInline<_maxSize>` and `memcpy`), and
keep the `std::vector`-returning `encode()` for source-compat but make it the
non-embedded convenience. Under the embedded profile, prefer `encodeTo`.

---

## 6. Per-file mapping (before → after on the example schema)

### `helpers.go`
- `cppType` (L47-83): string → `FixedString<maxlen>`, blob →
  `FixedBytes<maxlen>` when `f.HasMaxlen`. Needs the field to reach `cppType`
  (today it takes only `*ir.Field`, which carries `HasMaxlen/Maxlen` — OK).
- `cppArrayContainer` (L101-107): for native elems, unchanged (`std::array`);
  for string/blob/composite elems under the embedded profile return
  `std::array<Elem, count>` (fixed) instead of `std::vector<Elem>` — requires
  `count > 0` (else §9).
- `cppArrayElem` (L112-129): string → `FixedString<ElemMax>`, blob →
  `FixedBytes<ElemMax>`.
- `cppMsgSeqPrelude` (L391-398): change `std::vector<T> *out` →
  `InlineVector<T,N>* out` (template the prelude on N, or make `_MsgSeq` accept
  any container exposing `emplace_back/back`). Add the `FixedString/FixedBytes/
  InlineVector` definitions to the prelude block.
- `cppDefault` (L157-226) and `cppNativeArrayBraces` (L230-240): fixed-string
  defaults become brace/`len_` initializers; `FixedBytes` default already emits
  a byte list (L194-200) — adapt to the new type's aggregate init.

Before (member, backend.go:147-153 via `cppType`):
```cpp
std::string somestring = "";
std::vector<std::uint8_t> someblob = {0x48,0x65,0x6c,0x6c,0x6f};
std::vector<std::string> somestringarray = {};
std::vector<MyfirstmessageSomestructarrayElem> somestructarray = {};
```
After:
```cpp
FixedString<50> somestring = "";
FixedBytes<16>  someblob   = {0x48,0x65,0x6c,0x6c,0x6f};
std::array<FixedString<16>, 5> somestringarray = {};
InlineVector<MyfirstmessageSomestructarrayElem, 3> somestructarray = {};
```

### `backend.go`
- header includes (L78-82): under the embedded profile, drop `#include <string>`
  and `#include <vector>` **iff** no field falls back to them (i.e. no unbounded
  field survives — §9). Keep `<array>` `<cstdint>`.
- `encode()` (L162-166): add `encodeTo(dst,cap)`; keep vector `encode()` for
  compat (see §5).
- `emitSerialize` blob (L270-275): `someblob.data()/size()` already fine for
  `FixedBytes`; the default-compare `!= std::vector<std::uint8_t>{...}` becomes
  `!= FixedBytes<16>{...}`.
- `serializeArray` string/blob/struct/union (L329-345): iterating `for (auto&
  e : arr)` over a fixed array must iterate **only `len_` elements** (the fixed
  array is full-capacity) — emit `for (std::size_t i=0;i<arr.size();++i)` where
  `arr.size()` returns `len_`. This is the one non-mechanical change: today the
  container *is* the logical length; fixed storage separates capacity from length.
- `emitDeserialize` string (L356-357): `somestring.assign(_size,'\0'); if(_size)
  is.read(somestring)` → `somestring.set_len(_size); if(_size) is.read(...)`
  where `read` uses the §8 overload.
- `emitDeserialize` blob (L368-369): `someblob.resize(_size); is.read(
  someblob.data(), _size)` → `someblob.set_len(_size); is.read(someblob.data(),
  _size)` (no corelib change).
- `deserializeArray` string/blob seq (L427-438): today `is.read(target)` binds
  the corelib `std::vector<std::string>`/`<vector<uint8_t>>` overloads (corelib
  `sofab.hpp` L1065-1080). Under fixed storage, either use a new corelib overload
  (§8) or emit a static index-filling element collector.
- `deserializeSeqInto` (L453-462): `somestructarray.reserve(3)` becomes a no-op
  on `InlineVector`; `static _MsgSeq<...>` stays static (still required by the
  deferred decoder). Generated `case 23/24/25` are otherwise unchanged.

Before (decode, generated `case 11`/`12`/`23`):
```cpp
case 11: somestring.assign(_size,'\0'); if(_size) is.read(somestring); break;
case 12: someblob.resize(_size); is.read(someblob.data(), _size); break;
case 23: { static _MsgSeq<...Elem> _r0; _r0.out=&somestructarray;
           somestructarray.reserve(3); is.read(_r0); } break;
```
After:
```cpp
case 11: somestring.set_len(_size); if(_size) is.read(somestring); break;   // §8 read
case 12: someblob.set_len(_size); is.read(someblob.data(), _size); break;   // no corelib change
case 23: { static _MsgSeq<...Elem,3> _r0; _r0.out=&somestructarray;
           is.read(_r0); } break;                                           // inline, no reserve
```

### `cost.go`
No behavioural change needed — it already computes the exact capacities from
`maxlen/count/ElemMax` (L23-107). Reuse the same helpers to emit the `<N>`
template args (factor the per-field capacity out of `fieldCost`/`arrayCost` into
a small `capacityOf(field)` used by both `cost.go` and the container emitters).
When a field is unbounded, `fieldCost` returns `ok=false` (L34, L81) — that same
signal drives the §9 reject/fallback.

### `project.go`
- `cppClibMakefile` (L61-90): change `CXXFLAGS ?= -O2 -Wall` →
  `CXXFLAGS ?= -Os -Wall -ffunction-sections -fdata-sections -fno-exceptions
  -fno-rtti` and add `LDFLAGS ?= -Wl,--gc-sections` to the link line (L85). Same
  flags for the C objects (`CFLAGS`, L70). This is the cheapest, safest `.text`
  win and touches no emitted C++.
- Harness (`harnessMain` L286, `jsonHeader`, `jsonHelpers`): **leave as-is** —
  `<iostream>/<sstream>/<ostream>` are harness-only and not part of the embedded
  artifact. Optionally document that the harness is a host-side tool.

---

## 7. Correctness & wire-neutrality constraints

- **Wire-neutral by construction.** Every change is in-memory representation
  only. Encode still calls the same `os.write(id, ...)`/`sequenceBegin/End` in
  the same order (backend.go:249-346); `FixedString`→`string_view`,
  `FixedBytes`→`data()+len`, `InlineVector`→`data()+len` all present identical
  bytes. The sha256 conformance vectors and the sparse-omission defaults
  (`!=default` compares, backend.go:289,298-300) are unchanged — only the
  *types* of the compared members change, not the values.
- **Deferred-decode / use-after-return hazard.** The corelib-c-cpp decoder binds
  targets by address and fills them after `deserialize` returns (ARCHITECTURE
  §9.3; corelib `sofab.hpp` L774-791, L1051-1069). Fixed inline storage is
  **address-stable and never reallocates**, so it removes the hazard that forced
  `_MsgSeq` into `static` storage + `reserve()` (helpers.go:391-398;
  backend.go:453-462). Keep `_MsgSeq` `static` regardless (the visitor object,
  not the target, is what the deferred pass dereferences), but the target's
  `reserve()` becomes an unnecessary no-op. String/blob element collectors must
  likewise be `static` and write to a stable slot `arr[i]`.
- **Length vs capacity.** The single semantic subtlety: a fixed array is always
  at full capacity, so serialize/JSON iteration must bound by the logical `len_`,
  not `N` (§6, `serializeArray`). Encode of a fixed native array is already
  whole-value (`os.write(id, arr)`) and unaffected.
- **Validation plan (env-gated conformance).**
  1. Regenerate `example.yaml` + the corpus for `c-cpp` and diff wire output vs
     `main` (must be byte-identical).
  2. `go test ./generators/cpp/...` (codegen/unit).
  3. `tests/conformance/cpp/run.sh` with `SOFAB_C_DIR` pointing at a corelib-c-cpp
     checkout carrying the §8 read overload — builds the `c-cpp` variant, runs
     encode/decode round-trips, and `check_vectors.py` asserts the sha256 vectors
     (run.sh:97-101, the `c-cpp` `run_variant`). This is the gate that proves
     wire-neutrality end to end.
  4. Add a codegen golden test asserting the header emits no `std::string`/
     `std::vector` for a fully-bounded schema under the embedded profile.

---

## 8. Dependency on corelib-c-cpp (the one hard external change)

Inspected `corelib-c-cpp/src/include/sofab/sofab.hpp` (cloned read-only). It has
**no** fixed-capacity primitives — only `OStreamInline<N>` (inline encode
buffer) and span-based native-array read/write. Two decode paths block a
non-`std::string` string target:

1. Scalar string read is hard-gated on `std::is_same_v<T, std::string>`
   (L928-932).
2. String/blob **sequence** reads are hard-bound to `std::vector<std::string>`
   and `std::vector<std::vector<uint8_t>>` overloads (L1065-1080), whose private
   element callbacks `emplace_back` (L1082-1109).

**Required corelib-c-cpp addition:** a decode entry point for a contiguous
mutable char buffer, e.g. either
- generalize the scalar branch from `is_same_v<T,std::string>` to a concept
  `{ t.data() } -> char*; { t.size() } -> size_t;` (covers `FixedString` and
  keeps `std::string`), and
- add fixed-sequence element reads (a `read()` overload templated on a
  fixed-capacity sequence, or expose the existing `sofab_istream_read_sequence`
  element-callback pattern for generator-supplied storage).

Blob (`read(void*,size_t)`, L1038-1049) and native arrays (span branch) already
support fixed storage with **no corelib change**. `InlineVector` message/matrix
sequences need **no corelib change** (generator owns `_MsgSeq`).

**Bridge if corelib lags:** generated decode can call
`sofab_istream_read_string_noterm(...)` directly (the C symbol is linked in
`c-cpp` builds) to fill a `FixedString`, deferring the wrapper overload. Ship
strings-fixed only once the corelib overload lands to keep the abstraction clean.

Track as: corelib-c-cpp issue "fixed-capacity string/sequence decode reads for
the embedded C++ profile".

---

## 9. Unbounded-field policy (embedded profile)

A field is unbounded when `cost.go` returns `ok=false`: string/blob without
`maxlen` (L34), or an array without `count` (L81) — e.g. `somemap`.

**Recommended policy: reject at generate time, with an explicit opt-out.**
- Default under the embedded profile: **fail loudly** with a message naming the
  field and the missing attribute, e.g.
  `field "somemap" has no count; the fixed-capacity (embedded) profile requires
  count on every array and maxlen on every string/blob (or set
  cpp.allow_dynamic: true to keep a std::vector fallback for this field)`.
  Rationale: silent per-field heap fallback defeats the "no hidden allocation"
  guarantee an embedded user is opting into.
- Opt-out `allow_dynamic: true` (hybrid): unbounded fields keep `std::vector`/
  `std::string` (today's code path, already correct on `c-cpp`), bounded fields
  go fixed. Then `<string>`/`<vector>` includes are retained only when a dynamic
  fallback actually survives (§6 header-include gating).
- A global cap config (`max_dynamic: N`) is **not** recommended as the default —
  a wrong global cap silently truncates; prefer per-field `maxlen/count` in the
  schema. (Could be added later as an explicit knob.)

`_maxSize` note: with `somemap` present, `maxSize` already falls back to 4096
(cost.go:14; observed `_maxSize = 4096` in the generated header). Rejecting/
bounding unbounded fields makes `_maxSize` the true analytic bound and shrinks
the inline encode buffer accordingly.

---

## 10. Config / opt-in decision

**Recommendation: a new opt-in footprint option layered on `corelib: c-cpp`,
not the default.** Proposed key under `targets.cpp`: `containers: fixed`
(default `dynamic`), gated to require `corelib: c-cpp` (error if set with
`corelib: cpp`). Optionally an umbrella `cpp.embedded: true` that implies
`containers: fixed` + the `-Os`/`gc-sections` Makefile flags.

Rationale:
- **API/source compatibility.** `containers: fixed` changes emitted member types
  (`std::string`→`FixedString<N>`, `std::vector`→`InlineVector`/`std::array`).
  Existing `c-cpp` users assign `obj.somestring = std::string{...}`; making it
  default silently breaks their source. Opt-in keeps `c-cpp` stable.
- **Unbounded-field policy.** Fixed mode must reject or fall back on unbounded
  fields (§9) — a decision the user must consciously opt into, not have imposed.
- **Wire compatibility is preserved either way**, so this is purely a build-time
  representation choice, appropriate for an opt-in flag.

The Makefile `-fno-exceptions -fno-rtti -ffunction-sections -fdata-sections
--gc-sections` (§6, project.go) can ship **immediately and unconditionally for
`c-cpp`** (generated + corelib code is `noexcept` and uses no RTTI) — it needs
no schema changes and is safe today; do that first regardless of the containers
work.

---

## 11. Risks / tradeoffs / open questions

- **Corelib coupling.** Strings-fixed depends on a corelib-c-cpp release (§8).
  Mitigation: the direct-`sofab_istream_read_string_noterm` bridge; or ship
  blob/native/`InlineVector` fixes first (no corelib dep) and strings last.
- **Static `_MsgSeq` reuse under nested variable-length sequences.** The corelib
  supports only one active variable-length array decoder at a time
  (`arrayDecoder_`, corelib `sofab.hpp` L780-791). Deeply nested string/blob
  sequences remain unsupported — unchanged by this plan; matrices of *native*
  arrays are fine (`InlineVector<std::array<...>>`).
- **Stack pressure.** Fixed capacity moves bytes from heap to the object; a
  message with large `maxlen`/`count` gets a big `sizeof`. Acceptable/expected
  for embedded (deterministic memory), but document it; users must size
  `maxlen/count` realistically.
- **Length/capacity iteration bug surface** (§6 `serializeArray`) — the main
  place a mechanical port could go wrong; cover with the round-trip conformance
  and an empty-vs-partial-vs-full golden per array kind.
- **Open questions.**
  - Should `encode()`'s `std::vector` return be *removed* in embedded mode or
    kept as convenience? (Recommend keep + add `encodeTo`; revisit if `<vector>`
    removal from the header is a hard requirement.)
  - `FixedString` interface surface: expose enough of `std::string`
    (`c_str/size/operator==/assignment from string_view`) to keep the JSON
    harness (`from_json` does `.assign(_s,_l)`, project.go:220) compiling without
    special-casing.
  - Container ownership boundary: confirm the corelib maintainers prefer a
    concept-generalized `read()` (cleaner) over per-type overloads.

---

## 12. Phased rollout (smallest, safest first)

1. **Build flags (no codegen change).** Add `-Os -ffunction-sections
   -fdata-sections -fno-exceptions -fno-rtti` + `--gc-sections` to
   `cppClibMakefile` (project.go:61-90). Ship for all `c-cpp`. Immediate `.text`
   win, zero API/wire risk.
2. **`encodeTo(dst,cap)`** alongside `encode()` (backend.go:162-166). Removes the
   always-on encode allocation; additive, no wire change.
3. **Blob → `FixedBytes<maxlen>`** (helpers.go `cppType`/`cppArrayElem`;
   backend.go blob encode/decode). **No corelib change.** Behind `containers:
   fixed`.
4. **Native-array already fixed** — no work; verify `<vector>` no longer forced
   by native arrays.
5. **Struct/union/matrix sequence arrays → `InlineVector<T,N>`** (helpers.go
   `cppArrayContainer`/`cppMsgSeqPrelude`; backend.go `deserializeSeqInto`).
   **No corelib change.** Drops `_MsgSeq` heap `emplace_back` + `reserve`.
6. **Unbounded-field policy** (§9) — reject with `allow_dynamic` opt-out
   (drive off `cost.go` `ok=false`).
7. **Strings → `FixedString<N>`** (scalar + sequence). **Requires the
   corelib-c-cpp decode overload (§8)** or the interim bridge. Last because it
   carries the external dependency and the most compare-site churn.
8. **Header-include gating** — drop `#include <string>`/`<vector>` when a
   fully-bounded schema leaves no dynamic fallback (backend.go:79-80). Final
   `.text`/compile-time win, gated on steps 3-7 + policy.

Deliver 1-2 independently (safe now); 3-5 as the no-corelib-dependency core of
`containers: fixed`; 6-8 to complete the profile.
