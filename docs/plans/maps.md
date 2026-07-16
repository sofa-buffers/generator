# Proposal: `map` field type

> Status: **proposal / design** — not yet implemented. Feature-design doc under
> `docs/plans/` (see ARCHITECTURE §13). This proposes adding an associative
> **`map`** type to the message-definition language and the config/JSON schemas,
> and verifies every corelib + config option can support it.
>
> **Verification status.** Source-confirmed against fresh shallow clones of the
> `documentation` repo (MESSAGE_SPEC) and all 10 `corelib-*` repos
> (`/root/corelibs`). The former open questions on the three fixed-storage
> profiles (C, C++ `c-cpp`, Rust `no_std`) are now resolved from source — see §9.

---

## 1. Headline finding

**A map needs no wire-format change and no change to any corelib's byte codec.**

The wire has exactly eight wire types (ARCHITECTURE §9.1); there is no `map` tag.
**MESSAGE_SPEC §5.4** is explicit:

> *"there is no distinct map type; a map is `array of struct{ key, value }` (a
> wrapper sequence of two-field structs) ... a pattern, not a distinct type."*

with entry field ids `key = 0`, `value = 1`, **no length field on the wire**
(entries end-delimited at their index id), and a fixed-capacity map declared with
`count` becoming *N entry-struct slots* whose trailing all-default run is elided
(§5.1). The proposed `map` authoring type is therefore **pure sugar for the
existing array-of-struct pattern**: the wire bytes are byte-identical to
hand-writing `type: array, items: {type: struct, fields: {key, value}}` today. It
reuses `sequence_begin`/`sequence_end` and the existing key/value wire types, and
needs **no new encode/decode wire path** — it rides the array-of-struct path
already exercised by `example.yaml`, `realworld`, the corpus
(`tests/matrix/corpus/.../array_of_struct.yaml`), and every conformance harness.

Consequences, matching the §1/§8 "additive" rule:

- The **core pipeline** (parse → validate → IR) gains a new `Kind`, a lowering
  step, and validation — no wire logic.
- Every **corelib** already encodes/decodes wrapper-sequences-of-structs (that
  is how arrays of `struct` work today), so **all 10 corelibs and every config
  option already have the wire capability**. The only per-target work is
  **generated code**: choosing the idiomatic surface container and iterating it
  in canonical order.
- **Cross-language interop** is automatic — the bytes are an array-of-pairs the
  vectors already exercise in structure.

The rest of this doc specifies the authoring surface, the lowering, the two
genuinely-hard cross-cutting rules (canonical order, duplicate keys), and the
per-corelib/per-config verification matrix.

---

## 2. Authoring surface (message-definition schema)

Add `"map"` to the field `type` enum. A `map` field:

```yaml
labels:
  type: map
  id: 7
  key:   { type: string, maxlen: 32 }     # key element definition (leaf types only)
  value: { type: u32 }                      # value element definition (any field type)
  count: 128                                # optional capacity (max entries)
  description: "per-node labels"
```

- **`key`** and **`value`** reuse the existing `arrayItems` element shape (so a
  `value` may itself be a `struct`/`union`/`array`/`enum`/`bitfield`/`map`, each
  carrying its own `fields`/`oneof`/`items`/`enum`/`bits`/`maxlen`). This keeps
  one element grammar instead of inventing a second.
- **Key types are restricted** to hashable/comparable leaves:
  `u8…u64, i8…i64, boolean, enum, string`. **Excluded as keys:** `fp32`/`fp64`
  (NaN / equality hazards), `blob`, `struct`, `union`, `array`, `map` — none map
  cleanly onto a target-language map key, and floats break byte-exact equality.
  A new `keyType` validation rule enforces this.
- **`count`** is the capacity (max entries), analogous to array `count`:
  optional at the schema level, **required by fixed-storage targets** (C always;
  C++ `c-cpp` and Rust `no_std` unless `allow_dynamic`). It also drives the
  receiver-side decode cap (§6).
- **`maxlen`** on a string/blob key or value behaves exactly as in arrays.
- Recursive maps (a map that transitively contains itself by value) are rejected
  by the existing recursive-ref gate; nesting counts toward `MaxNestingDepth`.

### JSON-Schema (`sofabuffers-schema-v1.json`) changes

The concrete, mergeable fragments are in **`docs/plans/maps-schema-draft.json`**;
summary:

1. Add `"map"` to **both** `type` enums (`#/$defs/field` and `#/$defs/arrayItems`),
   so a map is usable as a field, a struct field, **and** an array/map element.
2. New `#/$defs/mapKey` — a closed element schema whose `type` is restricted to
   the hashable/comparable leaves `{u8..u64, i8..i64, boolean, enum, string}`,
   allowing `maxlen` (string only) and `enum` (inline or `{$ref}`).
3. A `field`-level `allOf` branch: `if type == map then required [key, value]`,
   closed to `{key, value, count, description, deprecated}`; `key` → `mapKey`,
   `value` → `arrayItems` (full type surface, incl. nested map/struct/array).
4. Reverse-implication guards: `key`/`value` imply `type == map`; a top-level
   `count` implies `type == map` (array capacity lives under `items.count`, so a
   top-level `count` is map-only — no collision).
5. The same `map` branch added inside `arrayItems` (key/value there too) so
   **array-of-map** and **map-valued map** validate.
6. New custom validation keyword **`mapKeyType`** — the located-error twin of the
   `mapKey` `enum` restriction, mirroring `defaultMatchesEnum` et al.
   (`schema/README.md` §Validation, ARCHITECTURE §5). The `enum` alone rejects a
   bad key structurally; the keyword names the offending field/line.
7. `default` on a map is **out of scope** for v1 (a map literal default can
   follow, like array `default` did).

---

## 3. Lowering (Model stage) — no new wire path

The map reuses the array-of-composite hoisting that already exists
(`internal/model/model.go` `elemRef`/`refForComposite`, ARCHITECTURE §6):

- The builder hoists a synthetic entry `NamedType` (category `struct`, `Inline`,
  key `<parentKey>_<field>_entry`) with two fields: `key` (id 0) and `value`
  (id 1). This is a real shared struct type, so the JSON harness and any
  struct-emitting code reuse the existing path unchanged.
- The map field carries **`Kind = Map`** plus `KeyElem`/`ValueElem` (the same
  `ArrayElem`/`TypeRef` machinery arrays use for `Elem`/`ElemRef`), and
  `Count`/`HasCount`/`ElemMax*` for the key/value bounds.

Two representation choices, and why the IR keeps `Map` rather than fully
desugaring to `array`:

- **On the wire / encode-decode plumbing**, a map *is* the wrapper sequence of
  the entry struct — identical calls, identical bytes.
- **In the IR surface**, keeping a distinct `Map` kind lets each backend emit its
  **idiomatic container** (`map`/`dict`/`std::map`/`BTreeMap`/`Dictionary`/…)
  instead of a `Vec<Entry>`. Fully desugaring to `array<Entry>` in the model
  would be less generator work but would throw away the whole point of a map
  (dedup + lookup + idiomatic type). So: **map wire = array-of-pairs; map surface
  = native map**.

### IR additions (`internal/ir`)

- `Kind`: add `Map` to the closed enum (§6). `AlignRank`/layout: a map is a
  heap/composite member → alignment rank 8 (like other composites), except on the
  fixed-storage profiles where it is inline fixed storage.
- `Field`: `KeyElem *ArrayElem`, `ValueElem *ArrayElem` (+ their `*Ref` for
  composite values), reusing the array element carriers. `--dump-ir` golden and
  the frozen-IR snapshot get a map case.

---

## 4. Cross-cutting rule #1 — canonical encode order (affects every backend)

Byte-exact shared vectors (ARCHITECTURE §12.1) demand deterministic, *identical
across languages* encode output. But native maps iterate non-deterministically:
Go `map` is deliberately randomized, Rust `HashMap`/C# `Dictionary`/Zig
`AutoHashMap` are unordered, only insertion-ordered ones (Python `dict`, JS
`Map`, C++ `std::map`, Rust `BTreeMap`) are stable — and "insertion order" is not
a cross-language concept anyway.

**Normative rule:** every backend **sorts entries by a canonical key order on
encode**:

- integer/boolean/enum keys → by numeric (enum: signed) value;
- string keys → lexicographic by raw UTF-8 bytes.

Decode imposes no order (a map is unordered), but sort-on-encode makes the output
byte-stable and language-independent, so the shared vectors are meaningful. This
is a **generated-code** rule (the corelibs stay dumb codecs, §11), exactly like
sparse omission. `std::map`/`BTreeMap` get it for free; the hash-based containers
sort a key view before the encode loop.

## 5. Cross-cutting rule #2 — duplicate keys on decode

The wire is just a sequence, so a malformed/hostile encoder can send duplicate
keys. Two admissible policies (MESSAGE_SPEC is authoritative — confirm there):

- **Recommended default: last-wins.** Inserting into the target map overwrites;
  no per-decode state, so it costs nothing on footprint targets. A re-encode is
  canonical (dedup + sorted), so round-tripping *malformed* input is
  intentionally lossy — acceptable and consistent with sparse re-canonicalization.
- **Stricter alternative: reject as INVALID (§9.3).** Needs a generated "seen
  keys" guard — cheap on heap targets, but unwanted state on C / `no_std`. Adopt
  only if MESSAGE_SPEC mandates it.

## 5b. Empty / default (sparse) behaviour

A map lowers to a wrapper sequence → **always framed** (ARCHITECTURE §11): an
empty map encodes as an empty wrapper sequence (present, not omitted), never a
dropped field — same as an array of struct. Each entry is a struct (always
framed); within an entry, `key`/`value` follow the normal per-field `!= default`
omission and gap-fill on decode.

---

## 6. Decode resource bounds (§9.5)

An unbounded map (no `count`) is a heap-amplification surface just like an
unbounded array. The wrapper sequence carries **no count header** (it grows only
with delivered entry bytes), so there is no allocation-amplification vector — but:

- Heap targets: govern an unbounded map's entry count with the existing
  `max_dyn_array_count` analog. Cleanest is a **new `max_dyn_map_entries`** config
  key (generic + per-target, unset = unlimited), parallel to the array/string/blob
  caps; reusing `max_dyn_array_count` is the lower-churn alternative.
- Fixed-capacity collectors (C++ `c-cpp`, Rust `no_std`, C) **must** carry the
  §9.5 capacity guard: an entry whose canonical index reaches capacity is dropped
  (payload skipped), never looped — this is the same #126-class infinite-loop DoS
  fix already applied to fixed string/blob-array collectors.

---

## 7. Verification — every corelib + config option

Requirements the lowering imposes: (a) SEQUENCE capability + the key/value wire
types (no new wire type); (b) the wrapper-sequence-of-struct decode path (every
corelib already has it for arrays-of-struct); (c) an idiomatic surface container
(the only part that varies by storage profile / config).

### 7.1 Heap / managed profiles — fully idiomatic maps, generator-only work

| Target | Corelib | Decode model (§9.3) | Surface | Notes |
|---|---|---|---|---|
| Go | corelib-go | push child-visitor | `map[K]V` | `BeginSequence` returns an entry collector; on `sequence_end` insert. Sort keys on encode (Go maps randomize). |
| Python | corelib-py | pull-parser | `dict` | Collect entries → `dict`. Sort on encode. |
| Java | corelib-java | flat-visitor stack | `Map<K,V>` (HashMap) | u64 key via `toUnsignedString`/`long`. Sort on encode. |
| C# | corelib-cs | flat-visitor stack | `Dictionary<K,V>` | Guard eager alloc from wire count (§9.5). Sort on encode. |
| TS | corelib-ts | monomorphic cursor | `Map<K,V>` | **`int64` config gotcha — see 7.3.** |
| C++ `cpp` | corelib-cpp | child-visitor | `std::map<K,V>` | `std::map` is sorted → canonical encode order for free. |
| Rust `rs` (std) | corelib-rs | flat-visitor stack | `BTreeMap<K,V>` | `BTreeMap` sorted → canonical order free (vs `HashMap`). |
| Zig (maxspeed) | corelib-zig | flat-visitor stack | `std.AutoHashMap`/`StringHashMap` | Decode already takes an allocator; sort a key view on encode. |

All eight: **✅ no corelib change**, wire capability already present. The only new
generated logic is container fill + canonical-order encode.

### 7.2 Fixed-storage / footprint profiles — supported, with caveats

These are the profiles the question really targets. All can carry a map on the
wire unchanged; the open question is the *surface container*.

| Target | Config | Surface options | Verdict |
|---|---|---|---|
| **C** | corelib-c-cpp, descriptor table | Fixed array of `{ key; value }` slots + a used-count member; a generated linear-scan lookup helper. | **✅ wire-capable, ⚠️ not O(1).** Requires `count`. String keys/values are **sized** like the existing sized-blob mechanism (#128/#130) so short keys keep their length. `object.h` exposes `SOFAB_OBJECT_FIELD_SEQUENCE` (nested sub-object) — **confirmed** — so the entry-slot array is modelled exactly as struct arrays already are. No native map type exists in C: it is an association array. |
| **C++ `c-cpp`** | fixed containers (`sofab.hpp`) | (a) add `sofab::FixedMap<K,V,N>` to corelib-c-cpp; **or** (b) lower to the existing `InlineVector<Entry,N>` (vector-of-pairs surface). | **✅ wire-capable.** **Confirmed from source:** `sofab.hpp` has `FixedString`/`FixedBytes`/`InlineVector` and **no `FixedMap`**. Option (b) needs **zero corelib change**; option (a) is a small *storage-only* (non-wire) corelib addition for an idiomatic map. `allow_dynamic` → `std::map` fallback for an unbounded map. Requires `count` otherwise. |
| **Rust `rs-no-std`** | heapless (generator-emitted) | (a) `heapless::FnvIndexMap<K,V,N>`; **or** (b) `heapless::Vec<Entry,N>` (vec-of-pairs). | **✅ wire-capable.** **Confirmed:** the corelib is *storage-agnostic* (no `heapless` dependency of its own — the generated crate pulls it), so the container is a **project-template** choice, not a corelib capability. The corelib already ships the **`sequence`** feature (`Cargo.toml` `[features]`). If option (a): `heapless::FnvIndexMap` requires power-of-two `N` and key `Hash + Eq` (fine for ints / `heapless::String`); option (b) needs nothing new. `allow_dynamic` → `alloc` `BTreeMap`/`Vec` fallback. The generated crate's `default-features = false` set must include `sequence` (+ value-driven `fp64`/`value64`/`array`) with the `require!` guard. |

**The one and only decision point where a corelib *could* change:** whether to
add a fixed-capacity map container (`sofab::FixedMap` in corelib-c-cpp;
`FnvIndexMap` vs `Vec` in the generated no-std crate) for an idiomatic surface on
the two footprint profiles. Choosing the vector-of-pairs surface there keeps the
feature **100% additive on the generator side across all 10 corelibs** (no
corelib edits at all). Recommended: ship v1 with the vec-of-pairs surface
everywhere fixed (zero corelib work), then add `FixedMap`/`FnvIndexMap` as an
ergonomic follow-up.

### 7.3 Config-option interactions to handle explicitly

- **TS `int64: long` / `number` with a 64-bit *key*.** In `long` mode a u64/i64
  scalar is a corelib `Long` **object**; a `Map<Long, V>` would key by object
  identity, so two equal-valued `Long`s become distinct entries — broken dedup
  and lookup. Fix: 64-bit map **keys** always use a primitive key (`bigint`, or
  `number` in `number` mode) regardless of `int64` mode; only *array/scalar
  values* use the `Long` fast path. Wire-identical. Values may still be `Long`.
- **rs-no-std / c-cpp capability set.** A map contributes `sequence` (always) and,
  from its value type, `fp64` / `value64` (64-bit) / `array` to the required
  feature/`SOFAB_DISABLE_*` set. The generator's used-feature computation must
  include the map's key/value types, or the `require!` / `#error` guard fires.
  The C++ `c-cpp` wrapper already hard-requires SEQUENCE, so maps are in-budget.
- **Fixed-storage `count` requirement.** `checkBounded` must treat an unbounded
  map on C / `c-cpp` / `no_std` exactly like an unbounded array: a located
  generation error, unless `allow_dynamic` (c-cpp/no_std) opts it into the heap
  fallback. C has no escape hatch → `count` mandatory, plus `maxlen` on any
  string/blob key/value.
- **Decode caps.** New `max_dyn_map_entries` (or reuse `max_dyn_array_count`) as a
  `generic`/per-target key (§6); inert on the statically-bounded profiles.

### 7.4 Summary

| Question | Answer |
|---|---|
| New wire type? | **No.** Map = wrapper sequence of `{key,value}` entries. |
| Any corelib byte-codec change? | **No — for all 10 corelibs, every config option.** |
| Corelib change to get an *idiomatic fixed-capacity map*? | **Optional**, only on C++ `c-cpp` (`sofab::FixedMap`) and Rust `no_std` (`FnvIndexMap` vs `Vec`). Vec/array-of-pairs surface = zero corelib change everywhere. C has no native map regardless. |
| Config options that need explicit handling? | TS `int64: long/number` (64-bit keys), rs-no-std/c-cpp capability gating, fixed-storage `count`/`allow_dynamic`, decode caps. |

---

## 8. Definition-of-done (per CLAUDE.md)

1. **Generator:** `map` in both schema `type` enums + `mapKeyType`/bounds
   validation; `Kind.Map` + `KeyElem`/`ValueElem` in the IR + `--dump-ir` golden;
   Model hoists the `_entry` struct; each backend emits its surface container +
   canonical-order encode + wrapper-sequence encode/decode; `checkBounded`,
   capability, and decode-cap handling per §6/§7.3; `max_dyn_map_entries` (or
   reuse) in the config schema.
2. **Tests:** unit tests per backend; corpus defs (bounded + unbounded + nested
   value + each key type); a `maps` case in the shared vectors (byte-exact,
   canonical order); round-trip in every `tests/conformance/<lang>/run.sh` incl.
   a malformed **duplicate-key** and **over-capacity** vector; feature-subset
   coverage for rs-no-std / c-cpp.
3. **Docs:** update ARCHITECTURE §4 (type table), §6 (IR), §9.1/§9.5, §10 (per-lang
   surface container), `schema/README.md`, `docs/generator/<lang>.md` map notes,
   config-schema docs, and this plan promoted from proposal to implemented.

## 9. Source-confirmation results (`/root/corelibs`, shallow clones)

1. **MESSAGE_SPEC §5.4 — RESOLVED.** A map *is* `array of struct{ key, value }`,
   "a pattern, not a distinct type." Entry ids are **`key = 0`, `value = 1`**; no
   length field on the wire (entries end-delimited by index id); a `count`-bounded
   map is *N* entry-struct slots with trailing all-default elision. The spec fixes
   the **structure** but is **silent on entry order and duplicate-key policy** —
   so both remain generator policy: this proposal keeps **sort-by-key on encode**
   (for byte-exact vectors, §4) and **last-wins on decode** (§5), which are
   wire-legal under the spec (any entry order is valid; the spec never asserts
   key uniqueness).
2. **corelib-c-cpp `sofab.hpp` — RESOLVED.** Confirmed: `FixedString`,
   `FixedBytes`, `InlineVector` exist; **no `FixedMap`**. `object.h` provides
   `SOFAB_OBJECT_FIELD_SEQUENCE` (nested sub-object) and
   `SOFAB_OBJECT_FIELD_BLOB_SIZED`, so an entry-slot array with sized string/blob
   keys/values is expressible today via the struct-array path. → C++ `c-cpp`
   surface = `InlineVector<Entry,N>` (option b) with zero corelib change, or add
   `FixedMap` later.
3. **corelib-rs-no-std — RESOLVED.** The corelib is **storage-agnostic** (its
   `Cargo.toml` has no `heapless` dependency; `heapless` types are emitted by the
   generator into the *generated crate*). The `sequence` feature is present in
   `[features]` (default set). So `FnvIndexMap` vs `Vec` is purely a generated
   project-template decision; the corelib needs no change. `FnvIndexMap`'s
   power-of-two `N` / `Hash + Eq` constraints apply only if option (a) is chosen.
4. **Nested-map depth / value = map — DESIGN-CONFIRMED.** A map value may be a
   map/struct/array; each nesting adds one sequence depth, bounded by
   `MAX_DEPTH = 255` (MESSAGE_SPEC §5.3) and the generator's `MaxNestingDepth`.
   The JSON harness handles it because the entry is a normal hoisted struct.

---

## 10. IR + Model desugaring sketch

**Representation decision: a map is a `KindMap`-tagged array-of-`{key,value}`-struct.**
The Model synthesizes an inline entry struct `{ key(id 0); value(id 1) }`, hoists it
through the *existing* inline-composite path (`refForComposite`), and tags the
field `KindMap` while reusing the array carriers `Elem`/`ElemRef`/`HasCount`/
`Count`. Consequences:

- **No new `ir.Field` members.** Key/value types live in the hoisted entry
  struct's two fields (`ElemRef.Target.Fields[0]`/`[1]`); capacity reuses
  `Count`. This is the whole reason the change stays tiny.
- Every core `Kind` switch gets `KindMap` **next to** `KindArray` — the traversal
  is identical (element = the entry struct).
- A backend that hasn't special-cased maps yet still emits **wire-correct**
  code by treating `KindMap` like an array-of-struct; a map-aware backend reads
  the entry's two fields to pick `map<K,V>` as the surface type.

Rejected alternative: dedicated `Field.KeyElem`/`ValueElem` carriers + a fully
first-class kind. More IR surface, and it duplicates `Children`/`Bounds`/layout
logic that the array-of-struct shape already provides for free.

### 10.1 IR changes (`internal/ir`)

```go
// ir.go — one enum value + label; NO new Field fields.
const ( … KindUnion; KindMap )                 // append after KindUnion
kindNames[KindMap] = "map"

// Convenience accessors (Fields are id-sorted → [0]=key(id0), [1]=value(id1)).
func (f *Field) MapKey() *Field   { return f.ElemRef.Target.Fields[0] }
func (f *Field) MapValue() *Field { return f.ElemRef.Target.Fields[1] }
```

```go
// layout.go — AlignRank: a map is a composite/heap member → rank 8.
case KindString, KindBlob, KindArray, KindStruct, KindUnion, KindMap:
    return 8
```

```go
// limits.go — decode-bounds walk: identical to an array (Elem is the entry
// struct, so the existing KindStruct arm recurses into key/value). One token:
case KindArray, KindMap:
    walkElem(f.Elem, f.ElemRef, f.ElemItems, f.HasCount, f.Count, f.ElemMaxHas, f.ElemMax)
```

`Children()` already appends `ElemRef.Target`, so the entry struct is walked with
no change. `dump.go` gains a `KindMap` arm that prints `Elem`/`ElemRef`/`Count`
exactly like the array arm (update the `--dump-ir` + frozen-IR goldens).

### 10.2 Model desugaring (`internal/model/model.go`)

```go
// buildField: add one case.
case "map":
    b.buildMap(fld, f, name, parentKey)

// A map lowers to KindMap + an inline {key,value} entry struct, hoisted through
// the SAME path inline struct elements already use.
func (b *builder) buildMap(fld *ir.Field, f map[string]any, name, parentKey string) {
    fld.Kind = ir.KindMap
    fld.Elem = ir.KindStruct                 // element = the entry struct
    if c, ok := asInt(f["count"]); ok {
        fld.HasCount, fld.Count = true, c     // map capacity
    }
    fld.ElemRef = b.mapEntryRef(f, name, parentKey)
}

// mapEntryRef synthesizes {key(id 0), value(id 1)} and hoists it as an inline
// struct. Because it goes through buildFields→buildField, the key/value element
// defs are lowered by the ordinary per-type paths — string maxlen, enum ref,
// struct/array/union values, and NESTED maps (value: {type: map}) all "just work".
func (b *builder) mapEntryRef(def map[string]any, name, parentKey string) *ir.TypeRef {
    entry := map[string]any{
        "key":   withID(asMap(def["key"]), 0),
        "value": withID(asMap(def["value"]), 1),
    }
    return b.refForComposite(entry, ir.CatStruct, name+"_entry", parentKey)
}

func withID(el map[string]any, id int) map[string]any {
    out := map[string]any{"id": id}                 // element defs carry no id
    for k, v := range el { if k != "id" { out[k] = v } }
    return out
}
func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
```

Map as an **array element** / deeper nesting reuses the same helper — two
one-liners mirroring how `struct` elements are already hoisted:

```go
// elemRef:        case "map": return b.mapEntryRef(items, name+"_elem", parentKey)
// buildArrayElem: case "map": e.ElemRef = b.mapEntryRef(items, name+"_elem", parentKey)
```

### 10.3 Worked lowering

```yaml
labels: { type: map, id: 7, key: {type: string, maxlen: 32}, value: {type: u32}, count: 128 }
```
is equivalent to (and produces the identical IR + wire bytes as) hand-writing:
```yaml
labels:
  type: array
  id: 7
  items: { type: struct, count: 128,
           fields: { key: {type: string, id: 0, maxlen: 32}, value: {type: u32, id: 1} } }
```
IR:
```
Field{ Name:"labels", ID:7, Kind:KindMap, Elem:KindStruct,
       HasCount:true, Count:128, ElemRef:{Key:"<msg>_labels_entry"} }
Named["<msg>_labels_entry"] = NamedType{ Category:Struct, Inline:true, Fields:[
    Field{Name:"key",   ID:0, Kind:KindString, HasMaxlen:true, Maxlen:32},
    Field{Name:"value", ID:1, Kind:KindU32} ] }
```

### 10.4 Stages that do NOT change / adjacent work

- **Analysis** — the entry struct resolves like any inline struct; the
  nesting-depth check counts the extra struct+sequence levels; a self-referential
  map (`value: {$ref}` back to itself) is caught by the existing recursive-ref
  gate. No code change.
- **Parser / validation (stage 1)** — separate from this desugaring: the
  validator sees the authored `map` and must gain the §2 rules
  (`type:map`⇒`key,value`, `mapKeyType`, optional `count`) mirroring
  `maps-schema-draft.json`. Desugaring is Model-only, so `--dump-ir`, docs, and
  round-trip all see the authored `map`.
- **checkBounded** (fixed-storage) — treat a `KindMap` without `count` like an
  unbounded array (located generation error unless `allow_dynamic`); its
  string/blob key/value bounds fall out of the entry-struct recursion.
- **Backends** — the actual footprint: a `case KindMap` that (a) routes the wire
  through the existing array-of-struct sequence calls via `ElemRef`, and (b)
  emits the idiomatic container from `MapKey()`/`MapValue()`, plus canonical-order
  sort on encode (§4). Backends may suppress the public `<msg>_<field>_entry`
  type when they inline `K`/`V` into the map surface.
