# `map` type — worklog & handoff

> Working notes for the `map` feature so it can be resumed later. Companion to
> the design doc **`docs/plans/maps.md`** (rationale, verification matrix,
> schema draft, IR/Model sketch) and **`docs/plans/maps-schema-draft.json`**
> (mergeable JSON-Schema fragments).
>
> Branch: **`feat/map-type-ir-model`** (NOT merged, NOT released). Nothing pushed.
> Full `go test ./...` + `go vet` green at every commit.

---

## 1. TL;DR — the one thing to remember

**A map is not a new wire type.** `MESSAGE_SPEC §5.4`: a map *is*
`array of struct{ key(id 0), value(id 1) }` — "a pattern, not a distinct type."
So it rides the existing **array-of-struct wrapper-sequence** path. **No corelib
changes, ever.** All work is generator-side: a new authoring `Kind`, a Model
desugaring, and per-backend surface-container emission. This was source-confirmed
against MESSAGE_SPEC and all 10 cloned corelibs (`/root/corelibs`).

Proof: `--dump-ir` shows a `map` lowering byte-for-byte to a hand-written
`array of struct{key,value}` (only the `kind` tag and the `_entry` vs `_elem`
synthetic name differ). Six compiled backends emit **byte-identical** output for
the demo (ASCII keys) → cross-language interop confirmed.

---

## 2. What's an author writing?

```yaml
labels:
  type: map
  id: 7
  key:   { type: string, maxlen: 32 }   # leaf keys only: u8..u64,i8..i64,boolean,string,enum
  value: { type: u32 }                   # any field type (scalar/string/blob/struct/union/array/map)
  count: 128                             # optional capacity; required by fixed-storage targets
```

Key types are restricted (no `fp32/fp64`, `blob`, or composite keys). Value is
unrestricted (nested maps work). See `docs/plans/maps.md §2`.

---

## 3. Status

| Backend | Corelib(s) | Surface | State |
|---|---|---|---|
| Go | corelib-go | `map[K]V` | ✅ done, round-trip proven |
| Rust | `rs` (std) | `BTreeMap<K,V>` | ✅ done, round-trip proven |
| Rust | `rs-no-std` | — | ⏸ **rejected** (needs heapless map) |
| C++ | `cpp` | `std::map<K,V>` | ✅ done, round-trip proven |
| C++ | `c-cpp` | — | ⏸ **rejected** (needs `sofab::FixedMap`) |
| C# | corelib-cs | `Dictionary<K,V>` | ✅ done, round-trip proven |
| Java | corelib-java | `HashMap` (boxed) | ✅ done, round-trip proven |
| Python | corelib-py | `dict` | ✅ done, round-trip proven |
| TypeScript | corelib-ts | `Map<K,V>` | 🟡 codegen only (no offline TS build to round-trip) |
| docs | — | `map<K,V>` HTML | ✅ done |
| Zig | corelib-zig | (AutoHashMap) | ⏸ **rejected** (structural blocker, §6) |
| C | corelib-c-cpp | (assoc-array) | ⏸ **rejected** (no dynamic containers) |

### Commits (on the branch, oldest → newest)
```
025fd25 docs(maps): propose map type + schema draft + IR/Model desugaring
7aec12b feat(map): lower map fields to array-of-struct in IR + Model
74156bc feat(map,go): emit native map[K]V
5f6a63a feat(map,rust): emit BTreeMap (std)
79fd598 feat(map,cpp): emit std::map
98847db feat(map,csharp): emit Dictionary<K,V>
52b8834 feat(map,java): emit Map<K,V>
4a8edd9 feat(map,python): emit dict
e2e9841 feat(map,typescript): emit Map<K,V>
d724766 feat(map): docs renders map<K,V>; zig defers maps
a1f0f07 feat(map,c): defer maps with a located error
093f6f1 docs(maps): record per-backend implementation status
```

---

## 4. What's implemented (the core — done, don't redo)

All in commit `7aec12b`. **No new `ir.Field` fields** — a map reuses the array
carriers.

- **`internal/ir/ir.go`** — `KindMap` enum value + label; `MapKey()`/`MapValue()`
  accessors (= `ElemRef.Target.Fields[0]`/`[1]`, id-sorted so key=0, value=1).
- **`internal/ir/layout.go`** — `KindMap` → alignment rank 8.
- **`internal/ir/limits.go`** — `KindMap` bundled with `KindArray` in the
  decode-bounds walk (element is the entry struct, so it recurses into key/value).
- **`internal/ir/dump.go`** — `--dump-ir` renders `elem`/`count`/`elem_ref`.
- **`internal/model/model.go`** — `buildMap` sets `Kind=KindMap, Elem=KindStruct,
  Count`, and `ElemRef = mapEntryRef(...)`. `mapEntryRef` synthesizes the
  `{key(id0), value(id1)}` entry struct and hoists it via the **existing**
  `refForComposite` inline-composite path — so string maxlen, enum refs,
  struct/array/union values, and nested maps all lower for free. Map is also a
  valid array element (`elemRef` case).
- **`internal/parser/validate.go`** — stage-1 map rules: `mapKeyTypes`
  (u8..i64, boolean, string, enum), `checkMapField`/`checkMapKV`/`checkMapKey`,
  value via `checkArrayItems` (full element grammar), map as an array element.

**Not done at the core level:** the JSON-Schema *file*
(`schema/sofabuffers-schema-v1.json`) still has no `map` — only the hand-written
Go validator does. The mergeable fragments are ready in
`docs/plans/maps-schema-draft.json`; apply them so the published schema matches.

---

## 5. The per-backend recipe (how each done backend works)

Every backend does the same three things; only the container/idioms differ.

1. **Surface type** (`*Type`/`*Annot` helper): `KindMap` → the native map, with
   `K`/`V` from `MapKey()`/`MapValue()` (recurse for nested maps).
2. **Marshal**: the map lowers to a wrapper sequence of entry values. **Sort the
   keys** (containers are unordered / insertion-ordered → non-canonical), then per
   entry write `sequenceBegin(index)`, construct the hoisted entry type, set
   key+value, call the entry's own `marshal`, `sequenceEnd`. (`std::map`/`BTreeMap`
   are already sorted → no explicit sort.)
3. **Decode**: build the map from the wrapper sequence's entries; **last write
   wins** on a duplicate key. Also treat a map as *dynamic* in the max-size/cost
   model (else the encode buffer under-counts) and follow `ElemRef` in any
   named-type-reachability walk.

### Decode by model family

- **Push child-visitor (Go)**: a generic `_mapSeq` collector in the prelude
  gathers entries; builds the map on the wrapper's `EndSequence`.
- **Child-visitor synchronous (C++ `cpp`)**: a `_MapSeq<Map,Entry>`
  `IStreamMessage` decodes each entry into a temp and inserts.
- **Pull-parser (Python)**: loop `d.next()` over the wrapper, decode each entry
  dataclass, `dict[key] = value`. Mirrors the struct-array reader.
- **Monomorphic pull cursor (TS)**: loop `c.readHeader()`, `Entry.decodeFrom(c)`,
  `map.set(...)`.
- **Flat-visitor location-stack (Rust `rs`, C#, Java)** — the reusable pattern:
  add a **`fkMap` frame** with a per-map **scratch entry** field on the visitor
  (`sc_<loc>`). On the map field's `sequenceBegin`: clear the map, descend to the
  map loc. On a per-entry `sequenceBegin`: reset the scratch, descend to the entry
  loc. On the **entry's `sequenceEnd`**: insert `scratch.key → scratch.value` into
  the map. This **composes for nested maps** (an inner map inserts into the outer
  scratch's value). The map's insert target is the parent path + field; the entry
  fields decode into `self.<scratch>` via a normal object frame.

### Reference implementations to copy from
- Flat-visitor scratch pattern: `generators/rust/visitor.go` (`fkMap`, `scratch`,
  `sequenceEnd` insert), mirrored in `generators/csharp/visitor.go` and
  `generators/java/visitor.go`.
- Collector-in-prelude: `generators/golang/backend.go` (`_mapSeq`),
  `generators/cpp/helpers.go` (`cppMapSeqPrelude`).

---

## 6. Problems / blockers (why the deferrals)

### Zig — the real structural blocker (highest-value follow-up)
`generators/zig/backend.go`: `pub fn marshal(self, os: *sofab.OStream)` takes
**only the OStream — no allocator**. Canonical map encode must **sort the keys**,
which needs a scratch allocation. Only `encode(self, alloc)` has an allocator.
Options:
- **(a)** Thread an allocator through every `marshal` signature + call site
  (encode → marshal, nested struct `.marshal`, struct-array element `.marshal`).
  Zig **errors on unused params**, so map-free marshals need `_ = alloc;`. Doable
  but broad (drifts the zig `scalars` golden; re-run zig conformance). Then:
  field type = `std.AutoHashMapUnmanaged(K,V)` (int/enum/bool keys) or
  `std.StringHashMapUnmanaged(V)` (string keys), default `= .{}` (empty, no
  deinit needed — the decode contract is "pass an arena, free all at once").
  Decode = scratch-entry + `self.m.field.put(self.alloc, k, v) catch {}` on the
  entry's `sequenceEnd` (add an insert switch before the pop in `emitSequence`).
  Encode = collect keys into an `ArrayList(K)` from `alloc`, `std.mem.sort`,
  iterate. `zig` 0.16.0 is at `/root/.claude/jobs/.../zig-x86_64-linux-0.16.0/zig`;
  corelib-zig requires 0.16.0.
- **(b)** A sorted map container so iteration is naturally ordered (no std one).
Current state: **rejected** in `Generate` with a located error + `firstMapField`
helper + `TestZigMapRejected`.

### C — no dynamic containers (footprint)
Descriptor-table backend, no heap. A map would be a **fixed-capacity assoc-array**
of `{key, value}` slots + a `used_len` member, driven by the static descriptor,
with **sized keys** (like the `#128` sized-blob mechanism: companion length
member + a sized descriptor). Requires a schema `count`. Rejected in `Generate`
with `firstMapField` + `TestCMapRejected`.

### Rust `rs-no-std` / C++ `c-cpp` — fixed-capacity profiles
Need a heap-free map container: `heapless::FnvIndexMap<K,V,N>` (power-of-two N,
key `Hash+Eq`) for no_std, and a new `sofab::FixedMap<K,V,N>` in corelib-c-cpp
(none exists — confirmed; `sofab.hpp` has FixedString/FixedBytes/InlineVector
only). Cheapest first step: lower to a **vector-of-pairs** surface
(`heapless::Vec<Entry,N>` / `InlineVector<Entry,N>`) — zero corelib change, not
an idiomatic map. Both currently **rejected** with located errors.

### TS — no offline runtime verification
Codegen is done and structurally verified (mirrors the pattern proven
byte-identical in 6 backends), but corelib-ts ships as TS source with
`.js`-extension imports over `.ts` files; without a build (`dist`/`tsc`/`node_modules`
absent, no network) node can't resolve it. Round-trip is left to CI.

### Cross-cutting: non-ASCII string-key ordering (correctness gap)
Canonical order must be **UTF-8 byte order** for byte-exact cross-language
vectors. Today each backend uses its language's natural sort:
- Go `sort.Strings`, Rust `BTreeMap` (both = UTF-8 bytes ✅),
- C++ `std::map` (byte order ✅),
- C# `List<string>.Sort()` (culture-sensitive ⚠), Java `Collections.sort`
  / Python `sorted` / JS `<` (UTF-16 / code-point ⚠).

For **ASCII keys they all agree** (the six-way byte-identical result stands). For
non-ASCII keys the managed backends diverge. Fix: emit an explicit ordinal /
UTF-8-byte comparator in C#/Java/Python/TS (and confirm Go/Rust/C++). Until then,
non-ASCII map keys are not guaranteed interoperable.

---

## 7. TODO (to call the feature "done" per CLAUDE.md)

- [ ] **Zig** map support (blocker §6 — thread the allocator).
- [ ] **C** map support (fixed assoc-array).
- [ ] **Rust `rs-no-std`** + **C++ `c-cpp`** map support (fixed containers, or
      vec-of-pairs surface as a first pass).
- [ ] **TS runtime** round-trip (build corelib-ts in CI and add the conformance leg).
- [ ] **Non-ASCII canonical sort**: UTF-8-byte comparator in C#/Java/Python/TS.
- [ ] **Shared conformance vectors**: add a `maps` case to `assets/test_vectors.json`
      (in each corelib repo) — bounded + unbounded + nested + each key type + a
      malformed **duplicate-key** vector; wire each `tests/conformance/<lang>/run.sh`.
- [ ] **Goldens**: add a map corpus def under `tests/matrix/corpus/defs/`, refresh
      `--dump-ir` golden + the frozen IR golden + per-backend goldens.
- [ ] **JSON harness (project mode)**: decide the canonical-JSON map representation
      (object with string keys? array of `[k,v]` pairs?) and wire `toJSON`/`fromJSON`
      in every backend. Currently project mode generates but the map JSON path is a
      stub/default (not exercised by any test yet).
- [ ] **Apply the JSON-Schema file**: merge `docs/plans/maps-schema-draft.json`
      into `schema/sofabuffers-schema-v1.json` (+ `mapKeyType` custom keyword in the
      validator's located-error twin; the structural `enum` is already enforced).
- [ ] **ARCHITECTURE.md**: document the map type (§4 type table, §6 IR, §9.1/§9.5,
      §10 per-lang surface) — required before pushing to main (CLAUDE.md rule).
- [ ] **CI**: the `lang-<x>` jobs already build/round-trip; they'll exercise maps
      once the conformance vectors + corpus def land.

---

## 8. How to verify / resume (commands)

Demo schema used throughout (ASCII keys → deterministic, cross-lang identical):
`/tmp/.../scratchpad/maps_demo.yaml` — recreate:
```yaml
version: 1
messages:
  Inventory:
    payload:
      counts:   { type: map, id: 1, key: {type: string, maxlen: 32}, value: {type: u32}, count: 128 }
      byStatus: { type: map, id: 2, key: {type: enum, enum: {OK: 0, WARN: 1, ERR: 2}},
                  value: {type: struct, fields: {total: {type: u64, id: 0}, label: {type: string, id: 1, maxlen: 16}}} }
      matrix:   { type: map, id: 3, key: {type: u32}, value: {type: map, key: {type: u32}, value: {type: u8}} }
```
Expected encoded bytes (all 6 done backends): **90 bytes**, hex begins
`0e0602326170706c6573...07070707`.

- **Prove the lowering:** `go run ./cmd/sofabgen --dump-ir --in maps_demo.yaml`
- **Generate a backend:** `go run ./cmd/sofabgen --lang <go|rust|cpp|csharp|java|python|typescript|docs> --in maps_demo.yaml --out OUT`
- **Reject check:** `--lang zig` / `--lang c` → located error; `--lang rust --config {corelib: rs-no-std}` and `--lang cpp --config {corelib: c-cpp}` → located error.

Corelibs are cloned at `/root/corelibs/*` (see `[[corelib-checkouts]]`). Round-trip
recipes proven this session:
- **Go**: temp module, `replace github.com/sofa-buffers/corelib-go => /root/corelibs/corelib-go`.
- **Rust**: `sofab = { package = "sofa-buffers-corelib", path = "/root/corelibs/corelib-rs" }`; strip serde derives for a harness (serde unavailable offline).
- **C++**: `g++ -std=c++20 -I/root/corelibs/corelib-cpp/include`.
- **C#**: net9.0 project, `ProjectReference` to `/root/corelibs/corelib-cs/src/SofaBuffers/SofaBuffers.csproj`.
- **Java**: `javac` the corelib `src/main/java` sources + generated + a `Main`; corelib package `org.sofabuffers.sofab`.
- **Python**: `PYTHONPATH=/root/corelibs/corelib-py/src python3 rt.py`.

Struct/class equality for the round-trip check: generated types lack `operator==`
in some langs — assert **re-encode idempotence** (`decode(encode(x)).encode() ==
encode(x)`) instead, plus spot-check scalar-valued maps.

---

## 9. Gotchas hit this session

- **`fieldCost`/`maxSize` must treat a map as dynamic** (return "unbounded") in
  every backend that sizes an encode buffer (Rust/C++/C#/Java) — else the buffer
  under-counts and encode overflows/truncates. Go/Python are dynamic (no cost model).
- **Reachability walks must follow `ElemRef`, not `Ref`**, for maps — bit the C++
  `reachable()` (entry struct wasn't emitted → compile error). C#/Java emit all
  named types so they were unaffected; Java's `reachable()` (used elsewhere) got
  the fix too.
- **ASCII-only generator output**: the C++/Go/etc. prelude comments must not use
  `§`/`—` (the `TestGeneratedOutputIsASCII` guard) — write `S5.4`, `-`.
- **Golden drift**: adding a shared prelude (`_mapSeq`) drifts the `scalars.yaml`
  golden even though scalars has no map — regenerate just that backend's golden
  (`go run ./cmd/sofabgen --lang <x> --in tests/matrix/corpus/defs/scalars.yaml
  --out tests/matrix/testdata/golden/<x>`).
- **No `Co-Authored-By: Claude`** in commits ([[no-claude-attribution]]).
