# Tagged unions, family-wide — binding design for generator#608

Status: **binding** for the #608 workflow. Every language milestone implements
this document; it does not relitigate it. A milestone that finds a decision here
wrong says so precisely, fixes this file in the same commit, and explains why.

Spec: `sofa-buffers/documentation` main @ `b6586b5` — MESSAGE_SPEC §2, §3, §4.2,
§5.1, §6, §7.3, §7.4, §7.4.1 and CORELIB_PLAN §6.0.1. Where this document and the
spec disagree, the spec wins and this document is fixed.

Generator base: `main` @ `aa3609f` (includes the #609 Go nested-defaults fix and
corelib-go `NewMessageSeqInit`, corelib-go @ `7dc2f68`).

Revised after design review round 1: select-if-not-held switch rule (§0); §7.3
subtype/kind, `$defs`-split, struct-`D` element, 64-bit and forced-kind driver
cases (§2); folded split-name collision check and validator call sites (§1);
TypeScript typed slots, GC ownership/aliasing, `reset()`/`clear()` naming and
the hand-written sources per milestone (§5); C prefix default image and
select-at-default (§5.2).

Revised after design review round 2: the 64-bit JSON input dialect is an explicit
driver flag `--int64-json number|string` with a per-language table, and
`--int64-safe` names its exact values (§2, §2.4); the driver schema gains unions
whose `D` is a union, a compact array and a wrapper array (`r`/`r2`/`r3`), and a
union element two array levels down (`g`), with E35–E44, D36–D37 and the D0
defaults (§2.1–§2.3); a repeated wrapper-array option inside a union (D34/D35);
`SOFAB_OBJECT_DESCR_UNION` takes the image as a pointer (or `NULL`) and the C
image rule is restated as one "NULL iff" condition (§5.1, §5.2).

---

## 0. The rule in one page (identical in every target)

**Value.** A union holds exactly one option. A fresh union holds `default_id` at
that option's own default. Selecting another option discards the held one; the
new option starts from its own default.

**Encode** (per union value, `D` = the union type's default option):

| held option | what is written inside the union frame |
|---|---|
| `D` | `D` exactly like an ordinary field of its kind: omitted iff equal to its default; a `struct`/`union`/wrapper-array `D` closes with `end` |
| any other option `O`, scalar (`u*`/`i*`/`fp*`/`boolean`/`enum`/`bitfield`) | the value, **no ≠-default guard** |
| `O` `string` / `blob` | the value, no guard — the empty value is a zero-length payload |
| `O` compact array (native numeric/bool/enum/bitfield/fp element) | count + elements, no guard — empty is count 0 (an fp array keeps its `fixlen_word`) |
| `O` wrapper array (string/blob/struct/union/array element) | `begin_lazy(id)` … elements … **`end_keep`** |
| `O` `struct` / `union` | `begin_lazy(id)`, the option's own `serialize` (normal per-field omission inside), **`end_keep`** |

`isDefault(union)` ≡ `held == D && D equals its own default` (the existing
predicate of `D`'s kind). The union **field** keeps its existing framing
(`begin_lazy … end`, or the backend's `isDefault` guard); a union **element** keeps
the element rules of §5.1 (interior gap iff `isDefault`, last element `end_keep`).
A union frame is therefore never empty on a conformant encoder's output.

**Decode** (MESSAGE_SPEC §7.4.1). A child whose id names option `o` and whose
header **passed the §7.3 gate for `o`'s declared kind** — wire type, fixlen
subtype (`string` vs `blob`, `fp32` vs `fp64`) **and** array kind (unsigned /
signed / fixlen compact array vs wrapper sequence):

1. if `o != held`: `held := o`, and a `struct`/`union` option is put at its own
   default (construct / reset in place) before its payload. Every other kind is
   replaced whole by its payload anyway (a scalar is assigned, a string/blob is
   assembled, an array is replaced per §7.4), so it needs no reset.
2. the payload is applied exactly as for an ordinary field of that kind: a
   `struct`/`union` option continues its scope (§7.4 merge), everything else is
   replaced.

A child skipped under §7.3 and a child with an unknown id **do nothing** (no
switch, no discard). Several children in one frame, a re-opened frame and an empty
frame are all legal and never `INVALID`.

**The switch is "select if not held" (binding in every backend).** It is exactly
step 1: it acts only when `o != held`, and it **never resets, clears, re-emplaces
or re-binds an option that is already held**. It is therefore idempotent, and it
must stay correct in a hook that fires **more than once per occurrence**:

* per chunk — corelib-cpp delivers a split string/blob/array once per chunk and
  continues into the destination (`progress() > 0` on every delivery after the
  first);
* on resume — corelib-cpp replays the ids of the open sequence levels after a
  `feed` boundary (the re-entered-sequence flag, `sofab.hpp` ~5939–5955), so a
  union's sequence-begin arm runs again for the same occurrence; corelib-py
  replays `on_*_begin`;
* per delivery — Rust's `string`/`blob` callbacks take `(total, offset, chunk)`.

An unconditional `emplace` / `set` / `= Variant(…)` / `reset()` in such a hook
wipes the chunks already delivered — the resumable-temporary trap. The §7.4
replacement of a held **leaf or array** option by a *new* occurrence is not part
of the switch: it stays the existing per-occurrence logic of that kind (the
payload store, or the array header's first-delivery clear), which the backend
already makes resume-safe.

**Switch placement (every visitor backend).** The switch runs at the hook that is
reached **only past the §7.3 gate** for that option; where that hook can fire
several times per occurrence, the select-if-not-held rule above makes that
harmless:

| option kind | hook that switches |
|---|---|
| scalar | the typed value callback, together with the store (after the width check) |
| string / blob, assembled then assigned (go, java, kotlin, csharp, rust, zig, typescript) | the completion store — never `fixlenBegin`, never a per-chunk branch |
| string / blob, bound in place (c via corelib, cpp, dart) | the header hook that binds the destination, after the subtype gate (per chunk on cpp: select-if-not-held) |
| any option, python | the typed value hook of that kind, or `on_sequence_begin` (below) |
| compact array | the array header hook, after the kind/subtype gate, where the destination is opened/cleared today |
| wrapper array, `struct`, `union` | the sequence-begin arm (re-entered on resume on cpp: select-if-not-held) |

Where a corelib routes by id alone (TS `fixlenBegin` / `arrayBulk`, Go
`FixlenBegin` / `ArrayBegin`, Java/Kotlin/C# `afill`, cpp `is.fixType()`), the
generated subtype/kind gate that exists today runs **first** and the switch sits
behind it. The driver cases D20–D27 (§2.3) put a subtype or kind mismatch at a
union option id, so a switch placed before the gate fails.

Python additionally follows issue §5: the switch belongs in the typed value hooks
and `on_sequence_begin` only — never in `on_field` / `on_schema_bound` /
`on_array_begin` / `on_*_begin`, which a resumed read can replay.

Verify per backend (the driver cases E5/E8 decoded back, and D19, catch it): a **zero-length**
string/blob must still switch. If a corelib does not call the payload callback for
`total == 0`, that backend switches at the header for `total == 0` only.

**JSON (harnesses and every generated JSON surface).** A union is an object with
exactly **one** member, the held option: `{"<option name>": <value>}`, printed
even when the held option is `D` at its default. A parent that omits the union
member means the union's default. `{}` and multi-member objects are never sent by
any driver; a harness may reject them.

---

## 1. Core (IR, model, analysis, validator)

### 1.1 `default_id` reaches every site — one IR type per (union, default_id)

**Decision:** `default_id` is carried by the **union type**, not by the site.
`ir.NamedType.DefaultID` (exists, never populated today) becomes **always
non-nil for a union after analysis**. A `$defs` union referenced by sites with
different effective `default_id`s is split into one `NamedType` per distinct
`default_id`. Every backend reads `Ref.Target.DefaultID` / `ElemRef.Target.DefaultID`
and nothing else; the type's constructor/`Default`/`init`/`setDefaults`, its
`serialize` and its `isDefault` all bake the one default in. This is what makes a
type-level gap fill (Rust `T: Default`, Go zero value / `setDefaults`, C++ default
constructor, C default image, the factories of the GC corelibs) correct.

Changes:

* `internal/ir/ir.go` — `TypeRef` gains `DefaultID *int64 // union site's raw
  default_id as written; nil when absent. Consumed by analysis only.` The comment
  on `NamedType.DefaultID` becomes: *"union: the option id a fresh value holds;
  always set after analysis (the site's `default_id`, else the lowest option id)."*
* `internal/model/model.go`
  * `buildField` `case "union"`: keep `fld.Default = id` (the docs target and the
    dump read it), **and** set `fld.Ref.DefaultID = &id`.
  * `elemRef(etyp, items, …)` `case "union"`: set the returned ref's `DefaultID`
    from `items["default_id"]`. This fixes the element drop (issue §1) for
    `Field.ElemRef` **and** every nested `ArrayElem.ElemRef` (both go through
    `elemRef`).
* `internal/analysis/analysis.go` — replace the placeholder `checkUnionDefaults()`
  with `bindUnionDefaults()`, called after `resolveRefs` and before `checkDepth`:
  1. Walk every `*ir.TypeRef` whose target is a union: `f.Ref`, `f.ElemRef` and
     every `e.ElemRef` along `f.ElemItems`, for every field of every message and
     of every named type (iterate a snapshot of `NamedOrder`).
  2. Effective id of a site = `site.DefaultID` if set, else the **lowest option
     id** of the target (see 1.2).
  3. Group sites by target key. One distinct id (or no site at all) → set
     `Target.DefaultID`, done.
  4. Several distinct ids → for each id `d`, ascending: clone the `NamedType`
     (shallow: `Fields` slice shared — the IR is immutable after analysis) with
     `Name = orig.Name + "_default_" + opt(d).Name`,
     `Key  = orig.Key  + "_default_" + opt(d).Name`, `DefaultID = &d`; register it
     in `Named` and in `NamedOrder` **at the original's position** (variants in
     ascending id order); repoint each site's `Key`/`Target`; delete the original.
     A variant that collides with an existing type is an analysis error at the
     site: `union %q is used with default_id %d and %d; the generated type %q for
     one of them collides with the existing type %q — rename one`. The collision
     test is **folded**, not raw: two names collide when they are equal after
     lowercasing and dropping every character that is not a letter or digit.
     Every backend derives its type identifiers from the key by case changes and
     separator removal (Go/Java/Kotlin/C#/Dart/TS PascalCase, C/Rust/Zig
     sanitising), so a folded match is the conservative superset of every
     backend's own clash — e.g. variant `union/Shape_default_pt` against the
     inline option type `union/ShapeDefault_pt` of a `$defs` union `ShapeDefault`
     (both Go `UnionShapeDefaultPt`). The check runs only for split variants, so
     no schema that generates today starts failing.
  Only `$defs` unions can split: an inline union has exactly one site (a union
  field inside a shared struct is still one `Field`).
  Per-backend identifiers that the Core check cannot see — the option-id
  constants (Go package-level `<Type><Opt>ID`, C `<PREFIX>_<OPTION>_ID`, and the
  class-scoped constants of the other targets) — are collision-checked by that
  backend in its milestone against every identifier it emits in the same scope,
  with a located error and a backend unit test (C: the existing `checkMacroNames`;
  Go: a new package-scope check, since a package-level constant can clash with a
  type name).
* `internal/ir/union.go` (new) — the two helpers every backend uses, so no backend
  re-derives them:
  ```go
  // DefaultOption is the option a fresh union holds (the field whose ID is
  // *n.DefaultID). Post-analysis it is non-nil for every union.
  func (n *NamedType) DefaultOption() *Field
  // IsDefaultOption reports whether f is n's default option.
  func (n *NamedType) IsDefaultOption(f *Field) bool
  ```
* `internal/ir/dump.go` — print `"default_id": N` on a union named type (the
  `--dump-ir` golden changes accordingly).

### 1.2 Validator rules (new)

In `internal/parser/validate.go`, one helper `checkUnionOptions(oneof any, loc
string)` called from exactly **two** places: `checkUnionField` (which already
serves both a union field and, through the `union` branch of `checkArrayItems`,
a union element — calling it from `checkArrayItems` as well would report every
element error twice) and the `union` branch of `validateDefs`.

Validation runs on the `$ref`-resolved document (`parser.Document.Resolve`
copies the target into every site), so a violation inside a `$defs` union is
reported at `#/$defs/union/<Name>/…` **and** at each site that references it —
exactly how every existing `$defs`-level check (duplicate option ids, …) already
reports. That is kept: one report **per location**, never two at the same
location. A test pins it (§1.4).

The rules:

1. `oneof` must declare **at least one option** — `a union holds exactly one
   option; "oneof" must declare at least one` (an empty `oneof` has no option to
   hold).
2. An option of type `string`, `blob` or `array` must not declare a **non-empty**
   `default` (MESSAGE_SPEC §4.2 / §6): `default: ""`, an empty blob default and
   `default: []` stay legal. Message: `a union option of type %s must not declare
   a non-empty default (MESSAGE_SPEC §4.2): a held option other than default_id is
   always written, so a non-empty default would be sent in full`.

**`default_id` omitted → the option with the lowest id.** MESSAGE_SPEC assumes a
`default_id` exists and says nothing about an omitted one; the generator fixes the
lowest option id (deterministic, no schema change). This is a **spec gap** to be
raised in `sofa-buffers/documentation` by the user (we do not write spec PRs).

Documentation in the same Core commit:
* `schema/README.md` — the union row says what an omitted `default_id` means; a
  new custom-keyword-style section *"Union options"* states rules 1 and 2 with the
  rationale; the checklist gains both items.
* `schema/sofabuffers-schema-v1.json` — `$defs/union` gains `"minProperties": 1`;
  its `patternProperties` item becomes a new `$defs/unionOption` =
  `allOf[{ $ref field }, if type=string then default maxLength 0, if type=blob
  then default is "" or maxItems 0 (match how blob defaults are spelled), if
  type=array then default maxItems 0]`; the `default_id` descriptions say
  "omitted: the option with the lowest id".
* `docs/ARCHITECTURE.md` §4 union row, §5 validation contract, §6 (the "never
  populated; do not rely on it" sentence goes; `NamedType.DefaultID` is the
  contract; `Field.Default` on a union field is the raw schema value, kept for the
  docs target only); §9.3 gains *"Decode verdict: a union holds one option
  (§7.4.1)"* beside the §7.4 verdict (the §0 decode rule and switch-placement
  table); §11 gains a *"Tagged unions"* decision (the §0 encode rule, the
  per-(union, default_id) type, the GC storage decision, and a per-target table
  that each language milestone fills in), and the *Lazy sequence framing* closer
  table gains the row "union option other than `default_id` → `end_keep`"; §12
  names `check_union.py`.

### 1.3 MAX_SIZE timing

`internal/ir/wiresize.go` (`:147` field, `:221` element) **keeps the sum** until
the Finish milestone. Every backend writes at most one option only once its own
milestone lands; until the last one has, the sum is the only safe bound. In
Finish: union = header + **max** over options + 1 terminator, and the comment
claiming "a safe over-estimate, since only one is ever written" is rewritten to
say why max is exact. Note that a held non-`D` option at its default is never
larger than that option's maximum, so max stays safe under the forced write.

### 1.4 Core tests

* `internal/parser/validate_test.go` — rule 2 for string/blob/array × field /
  element / `$defs` union, plus the empty-default controls (`""`, `[]`) that stay
  valid; rule 1 (empty `oneof`) at all three sites; and
  `TestUnionOptionErrorOncePerLocation`: an element union's violation is reported
  exactly once, and a `$defs` union's exactly once at `#/$defs/…` plus once per
  referencing site (no location twice).
* `internal/model/model_test.go` — a union element and a union two array levels
  down carry their `default_id` on the `TypeRef`.
* `internal/analysis/analysis_test.go` — `TestUnionDefaultIDBound` (explicit,
  omitted → lowest id, element, nested element), `TestUnionSplitByDefaultID`
  (names `Shape_default_num` / `Shape_default_pt`, `NamedOrder` position, both
  sites repointed, an omitted and an explicit-equal site do **not** split),
  `TestUnionSplitCollision` (raw key clash) and `TestUnionSplitFoldedCollision`
  (`Shape_default_pt` against the inline option type of a `$defs` union
  `ShapeDefault` with option `pt`).
* `internal/ir/union_test.go` — the two helpers.
* IR golden (`internal/ir/golden_test.go`) regenerated for `default_id`.
* Corpus (§3) valid and invalid files.
* `go test ./...` green; every backend still generates the corpus (backends still
  emit product types until their own milestone — the split only renames types).

---

## 2. Shared conformance driver `tests/conformance/lib/check_union.py`

One driver, one concern, its own schema printed by itself (`--emit-schema`),
exactly the shape of `check_repeated_id.py` / `check_defaults.py`. It forges wire
images from MESSAGE_SPEC §4.3–§4.9 with its own builders (copy the builders of
`check_repeated_id.py`: `varint`/`header`/`signed`/`string`/`uarray`/`seq`, and
add, all from CORELIB_PLAN §4.4–§4.8):

* `unsigned(fid, n)` = `header(fid, 0) + varint(n)`;
* `blob(fid, b)` — fixlen subtype `0b011` = 3;
* `fp32(fid, x)` / `fp64(fid, x)` — fixlen, `fixlen_word` `0x20` / `0x41`, payload
  `struct.pack("<f"/"<d")`;
* `sarray(fid, values)` — wire type 4, zig-zag elements;
* `farray(fid, values, sub)` — wire type 5, count, **one** `fixlen_word` (`0x20`
  fp32 / `0x41` fp64) **even when the count is 0**, then the packed elements.

Usage:
```
check_union.py --emit-schema                   # the WHOLE document: version, $defs, messages
check_union.py --self-test                     # builders vs. hand-written hex, no harness
check_union.py <label> [--cwd DIR] [--sizes 1,2,3,5,0] [--no-stream]
               [--int64-json number|string] [--int64-safe]
               [--known-gap CASE=REASON]... [--message NAME] -- <harness argv...>
```
Verbs used: `encode <msg>` (JSON on stdin → wire on stdout), `decode <msg>` (wire
→ JSON), `streamdecode <msg> <chunk>` (as in `check_repeated_id.py`). Loud, never
quiet: every case must run, a harness failure is a failure, the case count is
printed, and the summary names what was covered.

* `--int64-json number|string` picks how a 64-bit scalar (`q.big`, `q.sig`) is
  **spelled in the encode input**. `number` (the default) writes it as a bare JSON
  integer — `json.dumps` of a Python `int`, exactly the dialect of
  `maxsize_fill.json` (`"f_u64":18446744073709551615`, `"f_i64":-9223372036854775808`),
  which every harness already encodes byte-exactly in all 11 `run.sh`. `string`
  writes the decimal string (`"18446744073709551615"`). The flag exists because the
  two dialects are **not** interchangeable: C and C++ read a 64-bit value through
  `sofab_json_u64`/`sofab_json_i64` (corelib `test/shared/sofab_test_json.c`,
  `if (v->type != SOFAB_JSON_NUMBER) return 0`) and Zig through
  `jsonU64`/`jsonI64` (`generators/zig/project.go`, `.string` → `else => return
  0`), so a quoted value silently reads as 0 there; the TypeScript harness parses
  its input with plain `JSON.parse` (`generators/typescript/project.go`), so a bare
  integer above 2^53 is rounded before `fromJSON` sees it, while its `fromJSON`
  takes `string | number` through `BigInt()`/`Number()` in every int64 mode. A
  harness is therefore driven in the one dialect it reads exactly (§2.4), and a
  wrong-dialect run fails loudly (E23 would still pass by accident on a quoted 0,
  E25–E28 would not) instead of being "fixed" inside the harness. The summary
  prints the dialect used. Nothing else in the driver's input is 64-bit.
* `--int64-safe` is for a harness whose 64-bit scalar is a JS `number` (TS
  `int64: number`, documented as lossy above 2^53). It replaces exactly three
  values, input and expectation alike: **E26** `sig` → `9007199254740991`
  (wire `seq(4, signed(1, 2**53-1))`), **E27** `sig` → `-9007199254740991`
  (wire `seq(4, signed(1, -(2**53-1)))`), **E28** `big` (u64) →
  `9007199254740991` (wire `seq(4, unsigned(0, 2**53-1))`). E23–E25 are exact
  in a double and stay (E25 = `2^32`, low word zero). The summary prints that the
  substitution happened.
* `--known-gap CASE=REASON` runs the case anyway and prints its verdict and the
  reason under a `KNOWN GAP` heading instead of failing on it; a known gap that
  **passes** is reported as `KNOWN GAP NOW PASSES — drop the flag`. It exists for
  exactly one case (D27 on cpp, if §8 item 2 applies) and is never used silently.

64-bit values travel in the dialect `--int64-json` names on the way in (bare
integers by default; strings for TypeScript only, §2.4) and are compared **by
value** across the string/number spelling on the way out, whichever spelling the
harness prints (the rule of `check_array_lengths.py`, whose quoted input is
TypeScript-only for the reason above).

### 2.1 The schema (`--emit-schema`, message `uni`)

`--emit-schema` prints the **complete** document — `version`, the `$defs` block and
`messages` — so the `run.sh` block (§2.4) writes it with one redirect and no
header of its own.

```yaml
version: 1
$defs:
  union:
    Pick:                                  # ONE $defs union, three sites, two default_ids -> split (§1.1)
      n: { id: 0, type: u16, default: 6 }
      t: { id: 1, type: struct, fields: { k: { id: 0, type: u8, default: 2 } } }
      s: { id: 2, type: string, maxlen: 4 }
messages:
  uni:
    payload:
      u:                                   # default_id on a STRUCT option that is NOT the first
        id: 0
        type: union
        default_id: 2
        oneof:
          num:   { id: 0, type: u16, default: 5 }
          s:     { id: 1, type: string, maxlen: 8 }
          pt:    { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 }, y: { id: 1, type: i32 } } }
          arr:   { id: 3, type: array, items: { type: u16, count: 4 } }
          strs:  { id: 4, type: array, items: { type: string, count: 3, maxlen: 4 } }
          bl:    { id: 5, type: blob, maxlen: 4 }
          inner: { id: 6, type: union, default_id: 1, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: i8, default: -2 } } }
          box:   { id: 7, type: struct, fields: { z: { id: 0, type: u8, default: 3 } } }
          e:     { id: 8, type: enum, enum: { A: 0, B: 1, C: 2 }, default: 2 }
          fl:    { id: 9, type: bitfield, bits: { r: { pos: 0 }, w: { pos: 1, default: true } } }
          f:     { id: 10, type: fp32, default: 1.5 }
          fa:    { id: 11, type: array, items: { type: fp32, count: 2 } }
          bo:    { id: 12, type: boolean, default: true }
      v:                                   # array of unions, default_id on the NON-first option
        id: 1
        type: array
        items:
          type: union
          count: 4
          default_id: 1
          oneof:
            i: { id: 0, type: i32 }
            s: { id: 1, type: string, maxlen: 8 }
      w: { id: 2, type: u8 }               # a sibling right after u and v: a frame desync shows up here
      v2:                                  # array of unions whose D is a STRUCT with a NON-ZERO default
        id: 3
        type: array
        items:
          type: union
          count: 3
          default_id: 1
          oneof:
            a: { id: 0, type: u8 }
            p: { id: 1, type: struct, fields: { q: { id: 0, type: u8, default: 9 } } }
      q:                                   # 64-bit options: a u64 non-D and an i64 D
        id: 4
        type: union
        default_id: 1
        oneof:
          big: { id: 0, type: u64 }
          sig: { id: 1, type: i64 }
      pf: { id: 5, type: union, default_id: 1, oneof: { $ref: "#/$defs/union/Pick" } }             # -> Pick_default_t
      pe: { id: 6, type: array, items: { type: union, count: 3, default_id: 0, oneof: { $ref: "#/$defs/union/Pick" } } }  # -> Pick_default_n
      po: { id: 7, type: union, oneof: { $ref: "#/$defs/union/Pick" } }                            # omitted = lowest id 0 -> Pick_default_n
      r:                                   # D is itself a UNION whose own D is NOT its first option
        id: 8
        type: union
        default_id: 0
        oneof:
          nu: { id: 0, type: union, default_id: 1, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: u8, default: 4 } } }
          ar: { id: 1, type: array, items: { type: u8, count: 2 } }
      r2:                                  # D is a COMPACT ARRAY
        id: 9
        type: union
        default_id: 0
        oneof:
          ca: { id: 0, type: array, items: { type: u8, count: 2 } }
          x:  { id: 1, type: u8 }
      g:                                   # union ELEMENT two array levels down, non-first D at a non-zero default
        id: 10
        type: array
        items: { type: array, count: 2, items: { type: union, count: 2, default_id: 1, oneof: { lo: { id: 0, type: u8 }, hi: { id: 1, type: u32, default: 4 } } } }
      r3:                                  # D is a WRAPPER ARRAY
        id: 11
        type: union
        default_id: 0
        oneof:
          ws: { id: 0, type: array, items: { type: string, count: 2, maxlen: 4 } }
          x:  { id: 1, type: u8 }
```
Everything is bounded, so C, C++ `c-cpp` and Rust `no_std` build it. `pt.x`
defaults to 7, `box.z` to 3, `p.q` to 9 and `Pick`'s `n` to 6 / `t.k` to 2, so
"starts from its default" is distinguishable from "starts from zero" and from
"kept stale" at a field, inside a union, and at an **element gap**. `Pick` is the
runtime proof of the per-(union, `default_id`) split (§1.1): the same `$defs`
union is a field whose `D` is the struct `t`, an array element whose `D` is `n`,
and a field with `default_id` omitted (lowest id → `n`, sharing the element's
type). `e`/`fl`/`f`/`bo` are the kinds whose ≠-default guard is special-cased in
some backend; `fa` is the fp array that must keep its `fixlen_word` when empty.

`D` covers every kind that the §0 predicate `isDefault(union) = held == D &&
D.isDefault` treats differently: a struct (`u`, `v2`, `pf`), a leaf (`v`, `q`,
`pe`/`po`), a **union** (`r`: `nu`, whose own `D` is its non-first option `b` = 4),
a **compact array** (`r2`) and a **wrapper array** (`r3`). `r` is the one that
executes the recursion: `r` holding `nu` which holds its non-`D` option `a` = 0
is **not** default (E36) — a backend that tests only the held option's value, or
C without the `_UNION_FORCED` line in `_field_is_default` (§5.1 item 1), omits
`r` and decodes `nu = {"b":4}`: silent data loss that no other field shows (E10
does not, because `inner` is a non-`D` option of `u` and is forced anyway). `g`
runs the gap fill of an **inner** array of unions (Rust `reserve_elem`, Go
`NewMessageSeqInit`, the GC factories, the C holder) with a non-first `D` at a
non-zero default, through the nested `ArrayElem.ElemRef` that §1.1 fixes — the
corpus `grid` only generates it.

Default value of the message (what `decode(b"")` must print — case D0):
`u = {"pt": {"x": 7, "y": 0}}`, `v = []` (or `null`), `w = 0`, `v2 = []`,
`q = {"sig": 0}`, `pf = {"t": {"k": 2}}`, `pe = []`, `po = {"n": 6}`,
`r = {"nu": {"b": 4}}`, `r2 = {"ca": []}`, `g = []`, `r3 = {"ws": []}`.

### 2.2 Encode cases — JSON in, exact wire out, then decode(wire) == JSON

`seq(id, …)` = header(id, 6) … `07`. All expectations are built with the builders.
After each encode, `decode(wire)` **and** `streamdecode(wire)` at every `--sizes`
split must equal the input with the defaults filled in — so every encode case is
also a streamed decode case.

| case | JSON `encode` input | expected wire | pins |
|---|---|---|---|
| E1 fresh | `{}` | `b""` | §2 zero bytes |
| E2 default option explicit | `{"u":{"pt":{"x":7,"y":0}}}` | `b""` | `D` at its default is omitted |
| E3 default option set | `{"u":{"pt":{"x":1}}}` | `seq(0, seq(2, signed(0,1)))` | `D` framed normally |
| E4 scalar at own default | `{"u":{"num":5}}` | `seq(0, unsigned(0,5))` | forced write, scalar |
| E5 string empty | `{"u":{"s":""}}` | `seq(0, string(1,""))` | forced write, empty string |
| E6 compact array empty | `{"u":{"arr":[]}}` | `seq(0, uarray(3,[]))` | explicit empty count 0 |
| E7 wrapper array empty | `{"u":{"strs":[]}}` | `seq(0, seq(4))` | `end_keep` on wrapper option |
| E8 blob empty | `{"u":{"bl":[]}}` | `seq(0, blob(5,b""))` | forced write, empty blob |
| E9 union option at its default | `{"u":{"inner":{"b":-2}}}` | `seq(0, seq(6))` | `end_keep`, union of union |
| E10 union option, inner non-default option at its default | `{"u":{"inner":{"a":0}}}` | `seq(0, seq(6, unsigned(0,0)))` | forced write one level down |
| E11 struct option at its default | `{"u":{"box":{"z":3}}}` | `seq(0, seq(7))` | `end_keep` on a non-`D` struct option: present, empty |
| E12 array of unions, gaps | `{"v":[{"i":4},{"s":""},{"i":0},{"s":"z"}]}` | `seq(1, seq(0,signed(0,4)), seq(2,signed(0,0)), seq(3,string(1,"z")))` | element 1 = element default (gap); element 2 = non-`D` at own default, framed with its child |
| E13 last element all-default | `{"v":[{"i":1},{"s":""}]}` | `seq(1, seq(0,signed(0,1)), seq(1))` | last element `end_keep` |
| E15 enum at own default | `{"u":{"e":2}}` | `seq(0, signed(8,2))` | forced write through the enum guard |
| E16 bitfield at own default | `{"u":{"fl":2}}` | `seq(0, unsigned(9,2))` | forced write through the bitfield guard |
| E17 fp32 at own default | `{"u":{"f":1.5}}` | `seq(0, fp32(10,1.5))` | forced write through the float (bit-pattern) guard |
| E18 boolean at own default | `{"u":{"bo":true}}` | `seq(0, unsigned(12,1))` | forced write through the boolean guard |
| E19 fp32 array empty | `{"u":{"fa":[]}}` | `seq(0, farray(11,[],fp32))` = `… 5d 00 20 …` | count 0 **keeps** its `fixlen_word` (CORELIB_PLAN §4.8) |
| E20 struct `D` element last, all-default | `{"v2":[{"a":1},{"p":{"q":9}}]}` | `seq(3, seq(0,unsigned(0,1)), seq(1))` | `D` struct at default closes with `end` (elided) inside, the last element with `end_keep` |
| E21 struct `D` element interior gap | `{"v2":[{"p":{"q":9}},{"a":2}]}` | `seq(3, seq(1,unsigned(0,2)))` | gap = `D` at its **non-zero** default |
| E22 struct `D` element set | `{"v2":[{"p":{"q":4}}]}` | `seq(3, seq(0, seq(1,unsigned(0,4))))` | `D` framed normally inside an element |
| E23 u64 non-`D` at 0 | `{"q":{"big":0}}` | `seq(4, unsigned(0,0))` | forced write of a 64-bit option |
| E24 i64 `D` at default | `{"q":{"sig":0}}` | `b""` | 64-bit `D` omitted |
| E25 i64 `D`, low word zero | `{"q":{"sig":4294967296}}` | `seq(4, signed(1,2**32))` | the TS `long` `(low, high)` omission test must look at `high` |
| E26 i64 `D` wide | `{"q":{"sig":1152921504606846977}}` | `seq(4, signed(1,2**60+1))` | exact above 2^53 |
| E27 i64 `D` wide negative | `{"q":{"sig":-1152921504606846977}}` | `seq(4, signed(1,-(2**60+1)))` | |
| E28 u64 non-`D` max | `{"q":{"big":18446744073709551615}}` | `seq(4, unsigned(0,2**64-1))` | |
| E29 `$defs` field, its `D` at default | `{"pf":{"t":{"k":2}}}` | `b""` | the field's type is `Pick_default_t` |
| E30 `$defs` field holding the other site's `D` | `{"pf":{"n":6}}` | `seq(5, unsigned(0,6))` | `n` is **not** `D` here: forced |
| E31 `$defs` omitted-`default_id` site, its `D` | `{"po":{"n":6}}` | `b""` | omitted = lowest id |
| E32 `$defs` omitted site holding `t` at default | `{"po":{"t":{"k":2}}}` | `seq(7, seq(1))` | `end_keep` |
| E33 `$defs` element holding the field's `D` | `{"pe":[{"t":{"k":2}}]}` | `seq(6, seq(0, seq(1)))` | option `end_keep`, last element `end_keep` |
| E34 `$defs` element gap = the element site's `D` | `{"pe":[{"n":6},{"s":"x"}]}` | `seq(6, seq(1, string(2,"x")))` | gap fill is `n` = 6, not `t` |
| E35 union `D` holding its own `D` at default | `{"r":{"nu":{"b":4}}}` | `b""` | recursive `isDefault` true |
| E36 union `D` holding its non-`D` at 0 | `{"r":{"nu":{"a":0}}}` | `seq(8, seq(0, unsigned(0,0)))` | recursive `isDefault` **false**: `r` is written although `nu`'s held value is 0 (C: `_UNION_FORCED` in `_field_is_default`) |
| E37 union `D`, its `D` set | `{"r":{"nu":{"b":5}}}` | `seq(8, seq(0, unsigned(1,5)))` | `D` framed normally |
| E38 non-`D` compact array empty beside a union `D` | `{"r":{"ar":[]}}` | `seq(8, uarray(1,[]))` | forced write, count 0 |
| E39 compact-array `D` empty | `{"r2":{"ca":[]}}` | `b""` | array `D` at its (empty) default is omitted |
| E40 compact-array `D` set | `{"r2":{"ca":[1]}}` | `seq(9, uarray(0,[1]))` | |
| E41 scalar non-`D` beside an array `D` | `{"r2":{"x":0}}` | `seq(9, unsigned(1,0))` | forced write |
| E42 wrapper-array `D` empty | `{"r3":{"ws":[]}}` | `b""` | wrapper `D` at its default is omitted |
| E43 wrapper-array `D` set | `{"r3":{"ws":["a"]}}` | `seq(11, seq(0, string(0,"a")))` | wrapper `D` framed, closed with `end` (present: it has a child) |
| E44 union element two levels down | `{"g":[[{"hi":4},{"lo":1}]]}` | `seq(10, seq(0, seq(1, unsigned(0,1))))` | inner element 0 = `D` `hi` at 4 → gap; inner element 1 non-`D` → framed; the inner array is the outer's last element |

(E14 is a decode: `decode(seq(0, seq(7)))` → `u={"box":{"z":3}}` — the empty frame
selects `box` at its own (non-zero) default, not zero.)

The 64-bit inputs of E23–E28 are shown as bare integers, the `--int64-json number`
spelling; under `string` the same values are quoted. Under `--int64-safe` E26 →
`9007199254740991`, E27 → `-9007199254740991`, E28 → `9007199254740991` (u64),
with the wires given in §2 usage.

### 2.3 Decode cases — forged wire in, JSON out, one-shot AND streamed

Each is decoded with `decode`, then with `streamdecode` at every `--sizes` split,
and compared against the same expectation. Cases marked **re** additionally
re-encode the decoded JSON and compare against the canonical wire given.

| case | wire | expected | pins |
|---|---|---|---|
| D0 empty message | `b""` | the §2.1 default value, every field | per-site `D` (`pf` vs `po`); union/array `D`s (`r`, `r2`, `r3`) |
| D1 multi-child, last wins (**re** → `seq(0, string(1,"x"))`) | `seq(0, unsigned(0,9), string(1,"x"))` | `u={"s":"x"}` | §4.2 several children, §7.4.1 |
| D2 three children, switch to struct starts at default | `seq(0, unsigned(0,9), string(1,"x"), seq(2, signed(1,3)))` | `u={"pt":{"x":7,"y":3}}` | new option from its default |
| D3 re-opened frame, other option | `seq(0, unsigned(0,9)) seq(0, string(1,"x"))` | `u={"s":"x"}` | switch across frames |
| D4 re-opened frame, same struct option merges | `seq(0, seq(2, signed(0,1))) seq(0, seq(2, signed(1,2)))` | `u={"pt":{"x":1,"y":2}}` | §7.4 merge |
| D5 same struct option twice in one frame | `seq(0, seq(2, signed(0,1)), seq(2, signed(1,2)))` | `u={"pt":{"x":1,"y":2}}` | merge within a frame |
| D6 away and back starts from default | `seq(0, seq(2, signed(0,1))) seq(0, unsigned(0,5)) seq(0, seq(2, signed(1,2)))` | `u={"pt":{"x":7,"y":2}}` | discarded state does not survive |
| D7 §7.3-mistyped other option does not switch | `seq(0, string(1,"ab"), string(0,"zz"))` | `u={"s":"ab"}` | string at a `u16` option id is skipped |
| D8 §7.3-mistyped held option keeps value | `seq(0, string(1,"ab"), unsigned(1,4))` | `u={"s":"ab"}` | |
| D9 sequence at a string option id | `seq(0, unsigned(0,9), seq(1, signed(0,1)))` | `u={"num":9}` | skip of a whole subtree, no switch |
| D10 unknown id does not switch | `seq(0, unsigned(0,9), unsigned(99,1))` | `u={"num":9}` | |
| D11 empty union frame (**re** → `b""`) | `seq(0)` | `u` = default | accepted, treated as omitted |
| D12 empty re-opened frame keeps held | `seq(0, unsigned(0,9)) seq(0)` | `u={"num":9}` | no occurrence, no discard |
| D13 repeated string option replaced | `seq(0, string(1,"ab"), string(1,"c"))` | `u={"s":"c"}` | |
| D14 repeated array option replaced | `seq(0, uarray(3,[1,2,3]), uarray(3,[4]))` | `u={"arr":[4]}` | §7.4 array replace |
| D15 element gap is `default_id` at default (**re** → same bytes) | `seq(1, seq(0, signed(0,4)), seq(2, string(1,"q")))` | `v=[{"i":4},{"s":""},{"s":"q"}]` | element default = `D` (non-first option) |
| D16 element re-opened with other option | `seq(1, seq(0, signed(0,4)), seq(0, string(1,"z")))` | `v=[{"s":"z"}]` | §7.4 element continue + §7.4.1 switch |
| D17 empty element frame | `seq(1, seq(0), seq(1, signed(0,3)))` | `v=[{"s":""},{"i":3}]` | |
| D18 nested union switch | `seq(0, seq(6, unsigned(0,1)), seq(6, signed(1,-5)))` | `u={"inner":{"b":-5}}` | union of union |
| D19 chunked string after a switch | `seq(0, unsigned(0,9), string(1,"abcdefgh")) + unsigned(2,3)` | `u={"s":"abcdefgh"}`, `w=3` | a string option split across feeds |
| D20 blob at a string option, other held | `seq(0, unsigned(0,9), blob(1,b"\x01"))` | `u={"num":9}` | **subtype** gate (string vs blob) before the switch |
| D21 blob at the held string option | `seq(0, string(1,"ab"), blob(1,b"\x01"))` | `u={"s":"ab"}` | the mistyped payload is not bound into the held destination |
| D22 string at a blob option | `seq(0, unsigned(0,9), string(5,"q"))` | `u={"num":9}` | subtype gate (blob vs string) |
| D23 signed array at a `u16` array option | `seq(0, unsigned(0,9), sarray(3,[1]))` | `u={"num":9}` | **array-kind** gate (TS `arrayBulk`, Go `ArrayBegin`, `afill`) |
| D24 fp32 fixlen array at a `u16` array option | `seq(0, unsigned(0,9), farray(3,[1.0],fp32))` | `u={"num":9}` | array-kind gate (compact integer vs fixlen) |
| D25 compact array at a wrapper option | `seq(0, unsigned(0,9), uarray(4,[1]))` | `u={"num":9}` | array vs sequence at a wrapper option |
| D26 fp64 scalar at an fp32 option | `seq(0, unsigned(0,9), fp64(10,2.0))` | `u={"num":9}` | fp subtype gate (TS/Go `fixlenBegin`) |
| D27 fp64 array at an fp32 array option | `seq(0, unsigned(0,9), farray(11,[2.0],fp64))` | `u={"num":9}` | fixlen-array subtype gate (cpp `is.fixType()`; `--known-gap` only if §8 item 2 applies) |
| D28 union option away and back | `seq(0, seq(6, unsigned(0,1))) seq(0, unsigned(0,5)) seq(0, seq(6))` | `u={"inner":{"b":-2}}` | a re-selected union option restarts at **its** default (the kept-slot reset on GC targets) |
| D29 compact array after a switch, streamed | `seq(0, unsigned(0,9), uarray(3,[1,2,3]))` | `u={"arr":[1,2,3]}` | per-chunk array delivery: select-if-not-held must not wipe earlier chunks |
| D30 blob after a switch, streamed | `seq(0, unsigned(0,9), blob(5,b"\x01\x02\x03\x04"))` | `u={"bl":…01020304}` | per-chunk blob delivery (cpp binds in place) |
| D31 wrapper option after a switch, streamed | `seq(0, unsigned(0,9), seq(4, string(0,"ab"), string(1,"cd")))` | `u={"strs":["ab","cd"]}` | a sequence arm **re-entered on resume** (cpp) must not reset the option |
| D32 `$defs` omitted site, struct option empty frame | `seq(7, seq(1))` | `po={"t":{"k":2}}` | the empty frame selects `t` at its default on the `Pick_default_n` type |
| D33 `$defs` element gaps | `seq(6, seq(0), seq(1, seq(1)))` | `pe=[{"n":6},{"t":{"k":2}}]` | an empty element frame = **that site's** `D` (`n`, not `pf`'s `t`) |
| D34 wrapper option repeated in one frame (**re** → `seq(0, seq(4, string(0,"x")))`) | `seq(0, seq(4, string(0,"ab"), string(1,"cd")), seq(4, string(0,"x")))` | `u={"strs":["x"]}` | §7.4 wrapper **replace** (not merge) survives the union sequence-begin arm, which routes through `mutable<Opt>()` / `<opt>_mut()` / `Mut<Opt>()` and must not bypass the existing wrapper reset |
| D35 wrapper option repeated across re-opened frames | `seq(0, seq(4, string(0,"ab"), string(1,"cd"))) seq(0, seq(4, string(0,"x")))` | `u={"strs":["x"]}` | same, the held option continuing across frames |
| D36 union `D` in an empty frame | `seq(8, seq(0))` | `r={"nu":{"b":4}}` | an empty `nu` frame is `nu` at **its** default (its `D` `b` = 4), not its first option |
| D37 inner-array gap of union elements | `seq(10, seq(0, seq(1, unsigned(0,1))))` | `g=[[{"hi":4},{"lo":1}]]` | the inner-row gap fill = the element type's `D` (`hi` = 4), two array levels down |

D29–D31 and D34/D35 are ordinary cases (every case is streamed); they are listed
because the split at `--sizes 1` lands **inside** the payload after a switch or
inside a repeated wrapper, which is where a non-idempotent switch loses data (§0
"select if not held") and where a resume-replayed sequence arm could clear the
wrapper a second time or not at all.

Comparison: a **union level is strict** — the decoded union must be an object with
exactly the expected single key (a product-type harness that prints every arm
fails here, which is the point). Below the union, the tolerant `same()` of
`check_repeated_id.py` applies (member order, `null` for an empty list, integers
as strings). A blob compares through `as_container()` (base64 or list), and an
empty blob accepts `""`, `[]` and `null`. A float compares by bit pattern of the
declared width.

`--self-test` asserts, without a harness, that the builders produce the documented
hex for E4, E7, E12, E19, E25, E33, E36, E44, D6, D24 and D34 (hand-written hex in
the file), so the driver itself is tested in the Core milestone.

### 2.4 How a language opts in

In the language's milestone, `tests/conformance/<lang>/run.sh` gains a block
modelled on its `check_repeated_id.py` block (same generator config, same harness
build, Python on **both** engines through the existing `for ENGINE in $ENGINES`
loop). The driver prints the whole document, so there is no `printf` header:
```sh
echo "==> §4.2/§7.4.1 tagged unions: one option held, last option wins (generator#608)"
python3 "$ROOT/tests/conformance/lib/check_union.py" --emit-schema > "$WORK/union.yaml"
<generate + build exactly as the repeated-id block does>
python3 "$ROOT/tests/conformance/lib/check_union.py" "<Label>" -- <harness argv>
```
C, C++ run it for **every** corelib/config the script already loops over (cpp:
`corelib: cpp` and `corelib: c-cpp`, static/dynamic where the script builds both);
Rust for `std` and `no_std`; TypeScript for `bigint`, `long` and `number` (the
last with `--int64-safe`), each mode generated separately — the `q` union makes
the three runs execute different code (the Long `(low, high)` test, a Long vs
bigint vs number option slot and setter).

64-bit input dialect per language (§2 `--int64-json`), each backed by the
harness's own 64-bit reader and, for `number`, by the byte-exact
`maxsize_fill.json` check the same `run.sh` already passes:

| language | `--int64-json` | why |
|---|---|---|
| c, cpp (both corelibs) | `number` (default) | `sofab_json_u64`/`_i64` read `SOFAB_JSON_NUMBER` only; a string reads 0 |
| zig | `number` (default) | `jsonU64`/`jsonI64` read `.integer`/`.number_string`; `.string` reads 0 |
| rust (std, no_std) | `number` (default) | serde `u64`/`i64` from a JSON integer |
| go, java, kotlin, csharp, dart | `number` (default) | the dialect of `maxsize_fill.json`, encoded byte-exactly by every one of them |
| python (both engines) | `number` (default) | `json.loads` yields an exact `int` |
| typescript (`bigint`, `long`, `number`) | **`string`**, all three modes | `JSON.parse` rounds a bare integer above 2^53; `fromJSON` reads `string \| number` through `BigInt()` / `Number()`; `number` mode adds `--int64-safe` |

A milestone that finds its harness reads the other dialect exactly as well does
not switch; the table is what the `run.sh` passes. The output comparison is by
value in both dialects.

### 2.5 The other shared drivers

* `check_repeated_id.py` — the schema gains `uni: { id: 6, type: union,
  default_id: 0, oneof: { n: { id: 0, type: u32 }, p: { id: 1, type: struct,
  fields: { x: { id: 0, type: i32 }, y: { id: 1, type: i32 } } } } }` (Core
  milestone: the field is harmless to product-type backends). Two cases run only
  with a new `--union` flag: `union_same_option_merges` (`seq(6, seq(1,
  signed(0,11))) seq(6, seq(1, signed(1,22)))` → `{"p":{"x":11,"y":22}}`) and
  `union_other_option_replaces` (`seq(6, seq(1, signed(0,11))) seq(6,
  unsigned(0,5))` → `{"n":5}`), compared strictly at the union level. Each language
  milestone adds `--union` to its invocation; Finish makes the cases unconditional
  and deletes the flag.
* `check_nondefault.py` — **no code change.** The round-trip fixture keeps
  `"someunion":{"option1":4242}` (the `default_id` option), so baseline and fixture
  both carry exactly `option1`. Each language milestone deletes its
  `--except '$.someunion.option2,…'` list (dead once the baseline prints one
  option) and the "a union renders every arm" comment. Finish rewrites the
  docstring paragraph about `--except` and unions.
* `json_equal.py` — **no code change** (structural comparison already handles a
  one-member object). Finish rewrites the docstring's "both print every arm"
  example.
* `check_declared_width_kinds.py` — unchanged: `find_leaf` searches by key, and
  each union case holds exactly the option it asserts. Each milestone confirms it
  stays green.
* `maxsize_fill.yaml` / `.json` / `.hex` — unions join in Finish (the fill value
  selects, per union, the option with the largest wire size), and the comment
  about the sum goes.

---

## 3. Corpus (`tests/matrix/corpus`)

`defs/unions.yaml` keeps `Un` and gains (all bounded, so C generates it):
```yaml
$defs:
  union:
    Shape:
      num:   { id: 0, type: u16, default: 5 }
      name:  { id: 1, type: string, maxlen: 16 }
      pt:    { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 }, y: { id: 1, type: i32 } } }
      vals:  { id: 3, type: array, items: { type: u16, count: 4 } }
      names: { id: 4, type: array, items: { type: string, count: 2, maxlen: 8 } }
      raw:   { id: 5, type: blob, maxlen: 8 }
      e:     { id: 6, type: enum, enum: { A: 0, B: 1, C: 2 }, default: 1 }
      flags: { id: 7, type: bitfield, bits: { r: { pos: 0 }, w: { pos: 1, default: true } } }
      f:     { id: 8, type: fp32, default: 1.5 }
messages:
  UnRef:
    payload:
      first:  { id: 0, type: union, default_id: 2, oneof: { $ref: "#/$defs/union/Shape" } }  # -> Shape_default_pt
      second: { id: 1, type: union, default_id: 0, oneof: { $ref: "#/$defs/union/Shape" } }  # -> Shape_default_num
      third:  { id: 2, type: union, oneof: { $ref: "#/$defs/union/Shape" } }                 # omitted = lowest id 0 -> Shape_default_num
      nested:                                                                               # union of union
        id: 3
        type: union
        default_id: 1
        oneof:
          flag:  { id: 0, type: boolean }
          inner: { id: 1, type: union, default_id: 1, oneof: { a: { id: 0, type: i8 }, b: { id: 1, type: fp64, default: 2.5 } } }
      list:                                                                                 # array of unions, default_id on a struct option
        id: 4
        type: array
        items:
          type: union
          count: 4
          default_id: 2
          oneof:
            i: { id: 0, type: i32 }
            s: { id: 1, type: string, maxlen: 8 }
            p: { id: 2, type: struct, fields: { q: { id: 0, type: u8, default: 9 } } }
      grid:                                                                                 # union element two array levels down
        id: 5
        type: array
        items: { type: array, count: 2, items: { type: union, count: 2, default_id: 1, oneof: { lo: { id: 0, type: u8 }, hi: { id: 1, type: u32, default: 4 } } } }
      sparse:  { id: 6, type: union, default_id: 7, oneof: { a: { id: 3, type: u8 }, b: { id: 7, type: string, maxlen: 4 } } }  # non-contiguous option ids
      empties: { id: 7, type: union, default_id: 1, oneof: { s: { id: 0, type: string, maxlen: 4, default: "" }, a: { id: 1, type: array, items: { type: u8, count: 2 }, default: [] } } }  # explicit empty defaults stay legal
```
`invalid/` gains: `union_option_string_default.yaml` (field union),
`union_option_blob_default.yaml` (element union), `union_option_array_default.yaml`
(`$defs` union, `default: [0, 0]` — all-zero is not empty),
`union_empty_oneof.yaml`, `union_split_name_collision.yaml` (`$defs` unions `Shape`
and `Shape_default_pt`, `Shape` used with `default_id` 0 and 2),
`union_split_folded_collision.yaml` (`$defs` unions `Shape` and `ShapeDefault`,
the latter with a struct option `pt`, `Shape` used with `default_id` 0 and 2). The
corpus README's union bullet is updated.

The corpus is **generate-only** (every backend generates it, Go output is
parse-checked; nothing is compiled or run), so `UnRef` proves that the split types
generate everywhere, not that they behave. The runtime proof of the split — a
`$defs` union shared by a field, an array element and an omitted-`default_id` site
with two different `D`s — is the driver's `Pick` (§2.1, D0/D32/D33/E29–E34), run on
every configuration of every language. Likewise the runtime counterparts of
`nested` (union `D`), `empties` (array `D`) and `grid` (union element two array
levels down) are the driver's `r`, `r2`/`r3` and `g` (E35–E44, D36, D37).

---

## 4. Bench

**No bench schema or payload change is needed.** `vehicle_telemetry.yaml` already
has a union field (`aux_sensor`, `$ref` `SensorSample`, `default_id: 0`) and an
array of unions (`energy_sources`, `default_id: 0`), and the payload already sets
exactly one option per union (`{"analog": 3.875}`, `[{"electric": 42.5},
{"combustion": 31.25}]`), so it is the same JSON under the old and new form. It
exercises a held `D` at a non-default value, a held non-`D` scalar option in an
array (`combustion`), and the element path. It has no struct option, so the
struct-option path is proven by conformance, not by the bench; `unbounded_ingest`
has no union (its rows are the control).

Rows each milestone measures (`tests/bench/run.sh --rows …`, corelib override env
where needed, e.g. `SOFAB_C_CORELIB=/root/corelibs/wt-c-cpp-union`), then
**restores** `results.txt` and `results-raw.txt`:

| milestone | rows |
|---|---|
| c | `c` (+ corelib `tools/footprint.sh`) |
| cpp | `cpp-c-cpp`, `cpp-c-cpp-dyn`, `cpp-cpp`, `cpp-cpp-static` (`cpp-cpp-unbounded` as control) |
| rust | `rust-rs-no-std`, `rust-rs-no-std-dyn`, `rust-rs`, `rust-rs-static` (`rust-rs-unbounded` control) |
| zig | `zig` (`zig-unbounded` control) |
| go | `go` (`go-unbounded` control) |
| java / kotlin / csharp / dart | `java` / `kotlin` / `csharp` / `dart` |
| typescript | `ts-bigint`, `ts-long`, `ts-number` |
| python | `python`, `python-native` |

Budget (the user's constraint: size and speed move only minimally): a maxspeed row
may move by at most **±1 % Ir/op** (encode and decode separately), a footprint row
by at most **+64 B `.text`** per target and **+0 B `.data`** for the generated
code; anything beyond is reported with its cause and either fixed or argued in
the milestone's result. The expected direction is ≤ 0 on encode (one option
compared and written instead of three) and ≈ 0 on decode (one tag store per
option occurrence). Finish runs the **full** bench and commits `results.txt` from
that full run only.

**What the bench cannot see, measured separately.** Both bench unions use
`default_id: 0` with all-zero option defaults, so the C default image (§5.2) never
appears in the `c` / `cpp-c-cpp` rows. The C milestone therefore also measures the
generated object (`size` on the `-Os` `arm-none-eabi` object, `.rodata`/`.data`)
of the driver schema (§2.1: `u` has a struct `D` with `default_id: 2`, `q` a leaf
`D` with `default_id: 1`, `Pick_default_t` a struct `D`; `r`/`r2`/`r3` are the
`NULL`-image controls — `default_id: 0` with a union, compact-array and wrapper `D`)
and of the corpus `UnRef`,
before and after, and states the image bytes per union in its result. The same
holds for the struct-option path on every target: its cost is argued from the
emitted code and proven by conformance, not measured by a row.

---

## 5. Per language

Common to every backend below:

* **Types.** The union type's default is its `Target.DefaultID` (§1.1). Option
  storage is described per language. Accessor names are derived from option names
  with the backend's existing identifier rules; a name that collides with a fixed
  member (the union API names below, or the backend's existing reserved members,
  e.g. Go's `String`) is mangled with the backend's existing suffix rule, and two
  options that derive the **same** accessor (`foo` / `set_foo` → `SetFoo`) are a
  located generation error naming both options.
* **Encode / isDefault** exactly §0. The union's `serialize` is one `switch` over
  the held option with one arm per option; the `D` arm keeps today's guarded write,
  every other arm the unguarded write and, for sequence-framed options, `end_keep`.
* **Decode** exactly §0, switch placement per the table there, always behind the
  §7.3 gate that exists today (Go `FixlenBegin`/`ArrayBegin`, Java/Kotlin/C#
  `afill`, Rust/Zig `askip`, TS kind/subtype tests, Python
  `declineOnMismatch`/`tagMismatch`, C++ `is.wire()`/`is.fixType()`). A path into
  a `struct`/`union`/array option (location-stack visitors) goes through the
  option's **mutable accessor**, which selects the option (at its default) when it
  is not held — so a store is correct even if a begin arm were skipped.
* **Option id constants** and the **API** follow the table:

| target | tag read | option id constant | read | write (select) | test | struct/union/array option in place | back to default |
|---|---|---|---|---|---|---|---|
| c | `u.which` (`sofab_object_descr_id_t`) | `#define <PREFIX>_<OPTION>_ID n` | `u.u.<opt>` | assign `which` + value; a sequence option at its default: `sofab_object_init(&<option descr>, &u.u.<opt>)` (5.2) | `which == …_ID` | `u.u.<opt>` | `<msg>_init` |
| cpp (both) | `which()` → `Which` | `enum class Which : sofab::id { <opt> = n, … }` | `<opt>()` | `set_<opt>(v)` | `has_<opt>()` | `mutable_<opt>()` | `reset()` |
| rust (both) | `which() -> Id` | `pub const <OPT>_ID: Id` | `match` / `<opt>()` → `Option<&T>` | assign the variant | `matches!` | `<opt>_mut()` | `= Default::default()` |
| zig | `which() sofab.Id` | `pub const <opt>_id: sofab.Id` | `switch` on the tagged union | assign `.{ .<opt> = v }` | `u == .<opt>` | `<opt>Mut()` | `= .init` |
| go | `Which() sofab.ID` | `const <Type><Opt>ID sofab.ID = n` | `<Opt>()` | `Set<Opt>(v)` | `Has<Opt>()` | `Mut<Opt>()` | `Clear()` |
| java | `which()` | `public static final int <OPT>_ID` | `get<Opt>()` | `set<Opt>(v)` | `has<Opt>()` | `mutable<Opt>()` | `reset()` |
| kotlin | `val which: Int` | `companion object { const val <OPT>_ID }` | property `<opt>` | property `<opt> = v` | `has<Opt>()` | `mutable<Opt>()` | `reset()` |
| csharp | `int Which` | `public const int <Opt>Id` | property `<Opt>` | property setter | `bool Has<Opt>` | `Mutable<Opt>()` | `Clear()` |
| dart | `int get which` | `static const int <opt>Id` | getter `<opt>` | setter `<opt> = v` | `has<Opt>` | `mutable<Opt>()` | `reset()` |
| typescript | `get which(): number` | `static readonly <OPT>_ID` | getter `<opt>` | setter | `has<Opt>()` | `mutable<Opt>()` | `clear()` |
| python | property `which` | class attr `<OPT>_ID: ClassVar[int]` | property `<opt>` | property setter | `has_<opt>()` | `mutable_<opt>()` | `clear()` |

Rust and Zig read by pattern matching on the native type (Rust additionally
`<opt>()` → `Option<&T>`), so there is no default-returning getter there. In every
accessor-based target, a **read of an option that is not held returns that
option's default** — a fresh
default instance for a `struct`/`union` option, a fresh empty container for an
array option — and never stores it; writing into such a detached default is lost
by design, which is what the mutable accessor is for. A write selects: the option
becomes held, the previous one is discarded.

**GC-target storage decision** (deviates from issue §6's `prim`/`ref` slots for
Java/Kotlin/C#/Dart/TypeScript): `which` + **one typed private slot per option**.
Primitives stay unboxed and typed (no cast, no bit conversion on any access), and a
reference-typed option (struct/union/array/string/blob destination) is created
**on first selection and kept** when another option is selected; re-selecting it
resets it to its default **in place**. So a reused destination (`reset()` +
decode, the Dart/Java/Kotlin reuse path) re-selects without allocating, and a
fresh object allocates only the options that are actually held — never all of
them eagerly as today. The cost against `prim`/`ref` is one 4–8-byte slot per
extra option; the win is zero casts and zero per-decode allocations.

* **TypeScript uses typed slots too** (one per option, every slot initialised in
  the constructor — scalars to their default, reference slots to `null` — so the
  hidden class is fixed at construction). A single `_v` slot would not be "free
  under dynamic typing": V8 tracks a field representation per property, and a
  property that holds a number at one time and a boolean, string or object at
  another is generalised to `Tagged`, after which **every store of a non-Smi
  double allocates a fresh HeapNumber** — a per-set and per-decode allocation on a
  maxspeed target (the bench's `aux_sensor` holds `3.875` in a union whose other
  options are `boolean` and `u32`), and `serialize`/`isDefault` loads turn
  polymorphic. With one slot per option each property keeps one representation
  (`Double` for an fp option, `Smi` for a small integer, `HeapObject` for bigint/
  Long/string/object). TypeScript has no destination-reuse path, so on a **real**
  switch to a reference option a fresh default instance is created and the slot of
  the option left behind is set to `null` (releases it; an object slot's
  representation does not change); a scalar slot left behind keeps its stale
  value, which `which` makes unreachable. The `ts-*` rows (§4) confirm it; if a
  row misses the ±1 % budget, the milestone reports the measured numbers of both
  layouts before choosing.
* **Python keeps the issue's single `_value` slot.** CPython stores every
  attribute as an object reference, so there is no representation to keep
  monomorphic and a typed slot per option would only add dataclass fields. On a
  real switch a fresh default instance is created.

**Ownership and aliasing (stated in every GC target's Unions chapter).** A setter
stores the **reference** it is given (no copy), exactly as assigning a plain
field of that type does today; a getter of the held reference option returns the
slot itself. On Java/Kotlin/C#/Dart a kept reference slot is **reset in place**
when its option is selected again (by the user through the setter/mutable
accessor, or by decode), so a struct, list or destination obtained earlier from
`get<Opt>()`/`mutable<Opt>()` — or passed to `set<Opt>(v)` — is reset and then
overwritten at that point. This is the same aliasing the message-level `reset()`
of Java/Kotlin/Dart already has for nested messages (it resets them in place,
`generators/java/backend.go` `emitResetField`), and it is what keeps same-option
and reuse decodes allocation-free; a caller that needs an independent value copies
it. Go stores options **by value** (5.6): `Set<Opt>(v)` copies, `<Opt>()` returns a
copy, and only a pointer from `Mut<Opt>()` aliases the slot (and sees the reset on
re-selection). TypeScript and Python allocate fresh on a real switch, so a
reference obtained earlier is never reset behind the caller's back (it is merely
no longer held).

**`reset()` vs `clear()`.** Java, Kotlin and Dart already emit a message-level
`reset()` on every generated class — the destination-reuse API — and a union type
is such a class, so its "back to default" **is** that `reset()`; adding a
`clear()` beside it would give one class two names for the same operation. Go,
C#, TypeScript and Python have no message-level reset, so the union gets the
issue's `clear()` in the target's casing (`Clear()` in Go/C#).

**Hand-written sources that use a union option as a product-type field.** Each
milestone rewrites its own to the new API (they fail to compile or run otherwise):

| milestone | files |
|---|---|
| c | `tests/conformance/c/example_roundtrip.c:28,69`; `c/bool_check.c:135-136` and the `choice` union in `c/run.sh` (`boolchk`) |
| cpp | `tests/conformance/cpp/sample.hpp:107-108`; `cpp/bool_check.cpp:143-144,199-200` and the `choice` union in `cpp/run.sh` |
| rust | `tests/conformance/rust/streaming_check.rs:127` |
| zig | `tests/conformance/zig/ownership_check.zig:81-83` |
| go | the Go program embedded in `tests/conformance/go/run.sh` (~1046-1048) |
| java | `tests/conformance/java/OwnershipCheck.java:102-106` |
| kotlin | `tests/conformance/kotlin/OwnershipCheck.kt:96-99` |
| csharp | `tests/conformance/csharp/OwnershipCheck.cs:102-105`; the `only_union` / `union_mixed` schemas in `csharp/run.sh` (~826-845) and whatever drives them |
| dart | `tests/conformance/dart/ownership_check.dart:74-88` |
| python | `tests/conformance/python/ownership_check.py:103-146` |

Each milestone re-greps its language directory for the union's option names
(`someunion`, `someunionarray`, `choice`, `as_…`) before it starts, because this
table is a snapshot of `aa3609f`.

### 5.1 corelib-c-cpp (C object API) — finish draft #182

Work only in `/root/corelibs/wt-c-cpp-union` (branch `feat/tagged-union`,
`657d6ca`); new commits on top, no force-push.

1. **Forced write (§2 option b).** In `src/object.c` add
   ```c
   #define _DEFAULT_TAG(info) ((info)->default_values \
       ? *(const sofab_object_descr_id_t *)(info)->default_values : 0u)
   #define _UNION_FORCED(info, obj) (_IS_UNION(info) && _TAG(obj) != _DEFAULT_TAG(info))
   ```
   * `sofab_object_encode`: the skip becomes
     `if (!_SOFAB_ELEMENT_HELD(i) && !_UNION_FORCED(info, src) && _field_is_default(info, field, src))`
     — the held option (the only one that survives `_NOT_HELD`) is written even at
     its default: a scalar as its value, a string/sized blob as the empty value, a
     sized array as count 0, a sequence option (struct/union/wrapper holder) as a
     present (eager) frame.
   * `_field_is_default`, SEQUENCE branch: before the loop,
     `if (_UNION_FORCED(ninfo, nsrc)) return 0;` — a union holding a non-default
     option is never default.
   * `sofab_object_init`: the tag comes from `_DEFAULT_TAG(info)` — a **NULL image
     now means tag 0**, so a union whose image would carry nothing but zeros needs
     no image and costs no `.rodata` (the exact condition is in 5.2).
   * **`SOFAB_OBJECT_DESCR_UNION` takes the image as a pointer.** Today it expands
     `(const void *)&(default_struct)`, so `NULL` cannot be passed. Change the
     parameter to `default_image` — a pointer, or `NULL` — expanded as
     `(const void *)(default_image)`, exactly as `SOFAB_OBJECT_DESCR_WITH_DEFAULTS`
     already takes its `default_struct` (the generator emits `&<image>` there,
     `generators/c/backend.go`). One macro, no `_NODEFAULTS` twin: the image is a
     prefix type, not `obj` (5.2), so a pointer is the natural form anyway, and #182
     is an unmerged draft, so no caller breaks. Update the in-tree callers
     (`test/c/test_object.c`) to pass `&image`; `union_null_image_holds_id_0` builds
     its descriptor with `NULL` through the macro, so a regression to the
     address-of form fails to compile in the corelib's own suite.
   * Verify the empty explicit forms with tests: a zero-length string/blob, a
     count-0 sized array (and a count-0 fp array keeping its `fixlen_word`), an
     empty wrapper frame.
2. **The single-default-image limitation — resolved without a new image.** Choose
   **(b) in its spec form**: MESSAGE_SPEC §4.2 now forbids a non-empty default on a
   string/blob/array option (enforced by the validator, §1.2). Under §2(b) the image
   never needs another option's default: init seeds only `default_id`; the
   ≠-default test only ever compares a held `default_id` option (a held non-`D`
   option is forced); on decode a switch re-inits a sequence option through its own
   descriptor and a leaf option is overwritten whole by its payload. Replace the
   `@warning PROTOTYPE LIMITATION` in `object.h` with that invariant, stated as the
   reason (a)/(c) are not needed. C's public API has no setter, so selecting a
   leaf option with a non-zero declared default is the caller writing the value,
   and selecting a sequence option at its default is `sofab_object_init` on its
   descriptor — both documented in `docs/generator/c.md` (5.2). The image contract
   becomes "the tag and the default option only" (5.2, prefix image); `object.h`
   states it and `union_prefix_image` tests it.
3. **Build switch.** CMake option `SOFAB_DISABLE_UNION_SUPPORT` + README row
   (with the §6.2.2 profile-variation statement: without it a union descriptor
   behaves as a plain struct, so the switch must be configured identically for the
   library and every includer); the generated header refuses it (5.2).
4. **Tests** (`test/c/test_object.c`): update
   `union_encodes_only_the_held_option` (the degenerate case is now **written**);
   add `union_held_option_at_own_default_is_written` (scalar, string, sized blob,
   sized array, struct option → empty frame, wrapper holder → empty frame, nested
   union), `union_null_image_holds_id_0`, `union_nested_union_not_default_when_forced`,
   and a decode of the forced forms back to the same tag. `ctest` green; `gcc
   -std=c99 -Wall -Wextra -Werror -pedantic` clean in full, `SOFAB_DISABLE_UNION_SUPPORT`,
   no-sequence and minimal configs.
5. **Shared vectors: none.** The corelib codec is union-agnostic (a union frame is
   an ordinary sequence); the message-level rule is covered by the Unity tests and
   by `check_union.py` on all thirteen configurations. (Deviation from issue §9.)
6. **Footprint** with `tools/footprint.sh` (Release, `libsofabuffers.a`) against
   `657d6ca`'s numbers; expected ≤ +32 B on top of #182's +108…+218 B full-config
   cost, ±0 minimal. Update the PR #182 body (still draft) with the final numbers
   and remove "prototype" from it once the C milestone is green.
7. The **C++ side** of c-cpp (`sofab.hpp`, `seq.hpp`) needs no change.

### 5.2 c (generator)

* **Type** (C99: named members, no anonymous structs/unions):
  ```c
  typedef struct {
      sofab_object_descr_id_t which;            /* held option: MESSAGE_…_<OPT>_ID */
      union {
          uint16_t num;
          char name[17];
          message_union_Shape_pt_t pt;          /* struct/union/wrapper option: its named type */
          struct { uint8_t len; uint8_t data[4]; } raw;      /* sized blob option */
          struct { uint16_t len; uint16_t items[4]; } vals;  /* sized array option */
      } u;
  } message_union_Shape_t;
  ```
  The tag is first (`SOFAB_OBJECT_ASSERT_LEN_FIRST`); the widest-first rule applies
  inside nothing (the union overlays). A sized option's companion width follows the
  existing rule (at least the element's alignment).
* **Descriptor**: `SOFAB_OBJECT_DESCR_UNION(fields, n, nested, k, <&image or NULL>,
  T, which)` (the pointer form of 5.1 item 1) with option fields addressed as
  `u.<opt>` (sized: `u.<opt>.data`/`u.<opt>.items` with length `u.<opt>.len`).
  **The image pointer is `NULL` iff `default_id == 0` and either `D` is a sequence
  option (struct / union / wrapper array — seeded through its own descriptor, never
  from the parent image) or `D` is a leaf option (scalar, string, blob, compact
  array) whose default is all-zero / empty.** In every other case the prefix image
  below is emitted — including `default_id != 0` with a sequence `D` whose nested
  defaults are non-zero (the tag-only image), and `default_id == 0` with a leaf `D`
  at a non-zero default (tag 0 + `D`). A tag-only image with tag 0 is never
  emitted: it is byte-for-byte what `NULL` means.
* **The image is a prefix, never a full `T`.** For a union the corelib reads the
  image at exactly two places: the tag (`_DEFAULT_TAG`) and `D`'s own
  `(offset, size)` plus `D`'s companion length (init, and the ≠-default test of a
  held `D`); a held non-`D` option is forced and never compared, and a sequence
  option is seeded through its own descriptor, never from the parent's image
  (object.c init/`_field_is_default`, the three `default_values` uses). So a full
  `sizeof(T)` image — which scales with the **largest** option (e.g. a 64-byte
  string option) although only the tag is needed — is not emitted. Instead:
  * `D` is a struct/union/wrapper option → the image type is
    `struct { sofab_object_descr_id_t which; }` (2 or 4 B, the profile's id width);
  * `D` is a leaf option → `struct { sofab_object_descr_id_t which; union {
    <D's member type> <d>; <A> _align; } u; }`, where `<A>` is the scalar with the
    alignment of `T`'s option union (the backend's existing alignment computation)
    so that `offsetof(image, u) == offsetof(T, u)`; the `.c` file asserts that
    with the negative-array-size idiom the corelib macros already use. Cost: tag
    + `D`'s member, rounded to the union's alignment — never the largest option.

  Item 1 of 5.1 documents this in `object.h` as the contract ("a union's image
  covers the tag and the default option only") and adds a Unity test that runs a
  union whose image object is exactly the prefix size under ASan
  (`union_prefix_image`). The measured bytes (§4) are stated in the C result.
* **Selecting a sequence option at its declared default.** `which = …_ID` alone
  leaves the previous option's overlay bytes in `u.<opt>`. For a
  struct/union/wrapper option the header therefore declares the option type's
  descriptor `extern const sofab_object_descr_t <descr>;` (it already has external
  linkage; the declaration costs nothing) and `docs/generator/c.md` documents the
  two-statement select:
  `u.which = <PREFIX>_<OPT>_ID; sofab_object_init(&<descr>, &u.u.<opt>);`. A leaf
  option is selected by writing `which` and the value (a string/blob/array option
  has an empty declared default by the §1.2 rule, so "the value" is its content
  and length). No per-option code or macro is emitted.
* **Macros**: one `#define <PREFIX>_<OPTION>_ID <id>` block per union named type,
  `<PREFIX>` built exactly like the bitfield prefix (`g.prefix` + sanitized
  `"named/" + key`), deduped per key, covered by `checkMacroNames`.
* **Capability guard** in the header: `#if defined(SOFAB_DISABLE_UNION_SUPPORT)
  #error "… uses unions …"` when the message reaches a union.
* **JSON harness** (`generators/c/project.go`, and
  `tests/conformance/c/example_roundtrip.c`): print/parse only the held option.
* **Tests** (`generators/c/backend_test.go`): the type shape above, descriptor macro
  and the image rule, one case per branch (`NULL` for id 0 with a sequence `D`
  whose nested defaults are non-zero, and for id 0 with an all-zero leaf `D`;
  tag-only image for id ≠ 0 with a sequence `D`; tag + `D` prefix image with the
  offset assertion for a leaf `D`, at id 0 with a non-zero default and at id ≠ 0), macro
  names + collision check, split variants get two descriptors, the `extern`
  option descriptors, guard emitted.
* **Conformance**: `SOFAB_C_CORELIB=/root/corelibs/wt-c-cpp-union
  tests/conformance/c/run.sh` with the `check_union.py` block and `--union` on
  `check_repeated_id.py`. CI `lang-c` stays red until #182 merges — expected.
* **Cost**: generated `.data` ±0 on the bench (no union there needs an image),
  `.bss` −4 B (`SensorSample` 12 → 8 B), `.text` ±0 (C emits no per-field code).
  Image `.rodata` for `default_id != 0` measured on the driver schema and `UnRef`
  (§4) — expected 2–4 B per union with a struct `D` and tag + `D` for a leaf `D`.
  Ir/op: `_NOT_HELD`/`_IS_UNION` bit tests on every object walk; if the `c` row
  moves by more than +0.3 % hoist `_IS_UNION(info)` out of the three loops.

### 5.3 cpp — `corelib: cpp` and `corelib: c-cpp`

* **`corelib: cpp`**: the union type stays a `sofab::Message` subclass; storage is
  `std::variant<Alt…>` with alternatives in option-id order and **index-based**
  access only (`std::in_place_index`, `std::get_if<I>`), so two options of the
  same C++ type are fine. The default constructor emplaces the `D` alternative at
  its default. `which()` maps index → id through a `static constexpr` table.
* **`corelib: c-cpp`** (freestanding, no `<variant>`): `Which which_;` + an
  anonymous C++ `union` of the option members (`FixedString`, `FixedBytes`,
  `InlineVector`, struct/union option types — all trivially destructible, so the
  union's destructor stays trivial). A user-provided default constructor
  placement-news `D`; copy constructor and copy assignment switch on `which_` and
  placement-new the held member (inline in the header, so unused ones cost no
  `.text`); a switch placement-news the new option at its default (`<new>` is
  freestanding).
* **Encode**: `serialize` switches on the held option; `D` arm = today's guarded
  write (`writeLazy` for a struct/union `D`); other arms unguarded: `os.write(id,
  v)` for scalars/strings/blobs/arrays (verify an empty container writes count 0),
  `os.write(id, msg)` — the **keep** form — for a struct/union option, and the
  wrapper-array option closed with the keep form as well.
* **Decode** (`deserialize` arms): where the corelib's read returns a bound flag
  (corelib-cpp scalar `read(v)` → `bool`), switch inside the `if`; everywhere
  else gate on `is.wire()` (and `is.fixType()` for fixlen) **before** switching and
  binding in place. The switch is **select if not held** (§0): `if (which() !=
  Which::<opt>) { emplace/placement-new at default }`, never an unconditional
  `emplace`/`set_<opt>()`. corelib-cpp calls the same arm once **per chunk** of a
  split string/blob/array (`progress() > 0` after the first) and **again on
  resume** for every open sequence level (the re-entered-sequence flag), so an
  unconditional emplace would wipe the chunks already bound — D29–D31 at
  `--sizes 1` catch it. The existing first-delivery clear of a repeated leaf/array
  (§7.4) stays where it is, keyed on `progress() == 0`. Record in the milestone
  result whether `is.fixType()` is valid for an `ArrayFixlen` at callback time; if
  not, D27 (an fp64 array at an fp32 array option) is the known #232 asymmetry:
  documented, run with `--known-gap D27=…`, not fixed here (§8 item 2).
* Arrays of unions: `MessageSeq` / `FixedMessageSeq` default-construct elements →
  the element type's `D` (per-type default via the split).
* **JSON harness** (`generators/cpp/project.go`): only the held option.
* **Tests** (`generators/cpp/backend_test.go`): variant/tagged-union shapes for
  both corelibs, `D` vs non-`D` arms (`writeLazy` vs `write`), gate-then-switch
  order, `enum class Which`, `reset()`.
* **Conformance**: `tests/conformance/cpp/run.sh /root/corelibs/corelib-cpp
  /root/corelibs/wt-c-cpp-union` (every config the script builds).
* **Cost**: `cpp-c-cpp*` `.text` ≤ +64 B (switch arms replace three guarded
  writes; copy/placement code only if used); `cpp-cpp*` Ir/op ±1 %.

### 5.4 rust — std and no_std

* **Type**: a native enum, variants PascalCase of the option names, with
  `#[serde(rename = "<option>")]` per variant (serde's externally tagged form *is*
  the §0 JSON form; no generated JSON code):
  ```rust
  #[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
  pub enum UnionShape { #[serde(rename = "num")] Num(u16), #[serde(rename = "name")] Name(String), #[serde(rename = "pt")] Pt(UnionShapePt) }
  impl Default for UnionShape { fn default() -> Self { UnionShape::Pt(UnionShapePt::default()) } }
  impl UnionShape { pub const NUM_ID: Id = 0; …; pub fn which(&self) -> Id; pub fn pt_mut(&mut self) -> &mut UnionShapePt; … }
  ```
  `no_std` uses the same enum over its heapless storage types. `Default` is the
  `D` variant — so `seq::reserve_elem`'s `T: Default` gap fill is right per type.
* **Encode**: `match self` with the §0 arms.
* **Decode** (`generators/rust/visitor.go`): a scalar store assigns the variant
  (`self.m.a = UnionShape::Num(v as u16)`) — the switch and the store are one
  statement. A string/blob option is assigned **only at the completion store**,
  after the existing payload accumulator has the whole value (`string`/`blob`
  take `(total, offset, chunk)` and fire per chunk; the per-chunk part never
  touches the variant). The sequence-begin arm of a struct/union/wrapper option
  calls `<opt>_mut()`, and a compact array option calls `<opt>_mut()` in
  `array_begin` after the `askip` gate; `<opt>_mut()` is **select if not held**
  (constructs the variant at its default only when another one is held, §0), so a
  repeated or resumed call keeps what was decoded. Every path below a union option
  goes through `<opt>_mut()`.
* **Tests**: enum shape + serde renames, `Default` = `D`, arms, `_mut` paths,
  split variants.
* **Conformance**: both corelibs. **Cost**: no_std `.text` ≤ +64 B, std Ir/op ±1 %.

### 5.5 zig

* **Type**: `union(enum)` with the option fields, plus
  `pub const init: UnionShape = .{ .pt = .{} };` (the `D`) and `<opt>_id`
  constants, `which()`, `<opt>Mut()`. A union field is declared `= .init`.
* **Encode**: `switch (self.*)`, §0 arms. **Decode**: stores assign the tagged
  union; paths into an option use `<opt>Mut()` (never access an inactive field —
  that is safety-checked illegal behaviour); `sofab.arrays.reserveElem(T, …, fill)`
  is passed `T.init`.
* **JSON harness**: only the held option. **Conformance**: with a local
  `--cache-dir` (see the zig CI gotcha). **Cost**: Ir/op ±1 %.

### 5.6 go

* **Storage**: unexported `which sofab.ID` + one unexported typed field per
  option; **struct/union options by value** (zero allocation per switch and per
  decode; memory = the sum of the struct options, exactly today's). By pointer
  would add one allocation per switch/decode for nothing the bench can see — the
  bench has no struct option, so the bench only confirms that the scalar-option
  path does not regress. Issue §9 asked for this choice to be **measured**; it is
  decided by argument instead (listed as a deviation in §8 item 3): by value is
  never slower (no allocation, no indirection, same memory as today's product
  type), so a measurement could only confirm it. If the Go milestone finds a case
  where by value costs (e.g. a very large struct option copied by `<Opt>()`), it
  measures both on the driver schema and reports.
* **Zero value is the default** (Go convention): the tag is stored **relative to
  `D`** — `which` holds `id ^ <DefaultID>`, so the zero value holds `D`, and for
  `default_id: 0` the XOR folds away. `setDefaults()` (the #609 mechanism) is
  emitted only when `D` has a non-zero default; `New<Msg>` calls it and an array
  of unions uses `sofab.NewMessageSeqInit(…, (*T).setDefaults)` exactly like a
  struct array. **No corelib-go change** (`NewMessageSeqInit`, corelib-go#147, is
  the factory the issue asked for).
* **API** per the table; `<Opt>()` of a struct option returns a copy (the held
  value or a fresh default), `Mut<Opt>()` returns a pointer into the slot.
  `reservedGoMethod` gains `Which`, `Clear`, and the union type's `Has*`/`Set*`/
  `Mut*` names are checked for collisions. The package-level option-id constants
  `<Type><Opt>ID` are checked against every other package-level identifier the
  backend emits (types, constants), a located error with a unit test (§1.1).
* **JSON**: generated `MarshalJSON` (value receiver) / `UnmarshalJSON` (pointer
  receiver) on each union type — per-schema option arms, the only JSON code Go
  emits, because the fields are unexported.
* **Decode**: the union type is its own child visitor: `Unsigned`/`Signed`/…
  arms switch and store; `String`/`Bytes` switch at completion; `ArrayBegin`
  switches after the kind gate; `BeginSequence` switches — select if not held
  (§0): only when another option is held is the struct option reset to `T{}` +
  `setDefaults` — and returns `&m.<opt>`.
* **Tests**: shapes, XOR tag, `setDefaults` only when needed, JSON methods,
  collision error. **Conformance**: `SOFAB_GO_CORELIB=/root/corelibs/corelib-go`.
  **Cost**: `go` row Ir/op ±1 %, allocs/op unchanged. gofmt with the 1.27
  toolchain only.

### 5.7 java

* **Storage**: `private int which = <D>;` + one private typed slot per option
  (`long` for integer kinds as today, `String`, `byte[]`, arrays/lists, struct/union
  types), reference slots created on first selection and kept; the constructor
  creates only `D`. `reset()` = select `D` at its default in place.
* **API** per the table. **Encode**: `switch (which)`. **Decode**
  (`generators/java/visitor.go`): stores call the setter; `sequenceBegin`
  `fkNormal`/`fkSeqObj` arms call `mutable<Opt>()`; paths through an option use
  `mutable<Opt>()`; compact array options switch at the array header behind
  `afill`; `Seq.reserveElem(list, id, T::new, bound)` gets the union's
  constructor (per type).
* **JSON harness** (`project.go`, Gson): a per-union `TypeAdapter` in the
  **harness** project (the library stays JSON-free) writing/reading one member.
* **Tests / conformance / cost** as the common pattern; `java` row ±1 %.

### 5.8 kotlin

As Java, with properties: `var <opt>: T` (getter returns the default when not
held; setter selects), `val which: Int`, `fun has<Opt>()`, `fun mutable<Opt>()`,
`companion object { const val <OPT>_ID = n }`, exact-width Kotlin types
(`UByte` …) in the typed slots. `ktReservedMembers` gains `which` and the union
API names for union types. Hand-written JSON harness renders one member.
`Seq.reserveElem(out, id, cap, rcap) { T() }`. `kotlin` row ±1 %.

### 5.9 csharp

As Java, with C# properties (`<Opt> { get; set; }`, `int Which`, `bool
Has<Opt>`, `Mutable<Opt>()`, `Clear()`, `public const int <Opt>Id`). No
`StructLayout` overlay (typed slots are enough; see the GC decision). JSON: a
per-union `JsonConverter<T>` emitted in the **harness** project and registered in
its `JsonSerializerOptions` (STJ would otherwise serialize every property).
`Seq.ReserveElem<T>(list, id, () => new T(), cap, rcap)`. `csharp` row ±1 %.

### 5.10 dart

* **Storage**: `int _which = <D>;` + one private slot per option; an `Inline…`
  destination (string/blob/array option) is created **on first selection** (at its
  bound when ≤ `eagerDestBytes`, else at the header, the existing rule) and kept;
  re-selection sets `length = 0` in place. A struct option instance likewise, reset
  in place by `reset()`. An fp32 option keeps the existing raw-bits companion slot
  pattern (§6.5 sNaN). The constructor creates only `D`'s storage.
* **Decode** (`_<Type>Visitor`): `onUnsigned`/… arms switch + store; `onString`/
  `onBlob`/`on*Array` switch and return the (now held) destination; 
  `onSequenceStart` returns the child visitor of `mutable<Opt>()`. §7.3 is
  structural (dispatch by wire kind), so no extra gate. `MessageSeq(out, cap, () =>
  T(), …)` per type.
* **Encode**: `switch (_which)`; forced arms write `storage`/`length` even when
  `length == 0`. JSON (`project.go`): one member. `dart` row ±1 %.

### 5.11 typescript — int64 `bigint` / `long` / `number`

* **Storage**: `private _which: number = <D>;` + **one typed private slot per
  option** (§5 GC decision: a shared slot would generalise V8's field
  representation to `Tagged` and box every non-Smi double store), each typed per
  the int64 mode exactly as a field of that kind is today and initialised in the
  constructor (scalar → its default, reference → `null`, `D` → its default
  instance). Getters/setters per option, `mutable<Opt>()`, `has<Opt>()`,
  `clear()`, `static readonly <OPT>_ID`. A real switch to a reference option
  creates a fresh default instance and nulls the reference slot left behind.
  `toJSON()` returns `{ "<opt>": … }` for the held option only; `fromJSON` reads
  the one member.
* **Decode** (`visitor.go`): the switch goes **after** the generated kind/subtype
  gate in `arrayBegin`/`fixlenBegin`/`sequenceBegin` arms (`arrayBulk` and
  `fixlenBegin` route by id alone — issue §5); a string/blob switches at the
  payload completion store; paths into an option use `mutable<Opt>()`.
  `FramedSeq<T>(out, () => new T(), …)` per type.
* **Encode**: `switch (this._which)`; a u64/i64 `D` option under `long` keeps the
  `(low, high)` omission test (the driver's `q`, E24/E25, pins it). **Cost**: all
  three `ts-*` rows ±1 %, decode and encode Ir/op; the per-option slots are the
  layout measured — if a row misses the budget, the milestone also measures a
  per-representation layout (number / bigint-or-Long / object slot) on the same
  rows and reports both before choosing. Rebuild `corelib-ts` `dist/` before the
  suite. Conformance runs the driver in all three modes (`number` with
  `--int64-safe`).

### 5.12 python — native and pure engines

* **Storage**: the union stays a `@dataclass` (so `__eq__`/`__repr__` are
  dataclass-generated, not emitted helpers) with fields `_which: int = <D>` and
  `_value: object` (default factory = `D`'s default), under the same dataclass
  options the backend already uses, plus per-option properties, `has_<opt>()`, `mutable_<opt>()`,
  `clear()`, and `<OPT>_ID: ClassVar[int]`. JSON helpers render one member.
* **Binding** (`generators/python/binding.go`): a union is **not bindable** —
  `bindableSubtree` returns false for `ir.KindUnion`, and `bindScope` skips a
  union field (`continue`), so the enclosing table is open and the visitor
  receives the union's `on_sequence_begin` (issue §5: a fixed slot per id cannot
  express "last of several ids wins").
* **Decode**: switch only in the typed value hooks and `on_sequence_begin` (§0).
  `reserve_elem(out, id, T, …)` per type.
* **Conformance**: rebuild the native `.so`, run the driver on **both** engines.
  **Cost**: `python` ±1 %; `python-native` decode may rise because `aux_sensor`
  leaves the Binding — budget +2 % decode, reported with the measured number.

### 5.13 docs target

`generators/docs`: a union type is rendered as "one of" with its **default
option** named from `NamedType.DefaultID` (so an omitted `default_id` shows its
lowest-id option, marked "implicit"), and the rule "a held option other than the
default is always written; the last option received wins" stated once in the
union section. Split variants render as their own types. Test in
`generators/docs/backend_test.go`; no bench row.

### 5.14 Per-language docs and review checklist

Every language milestone also updates:
* `docs/generator/<lang>.md` — a **"Unions"** chapter: the generated type, the API
  table row for that language with a short example (select, read, in-place edit,
  back to default), JSON form, and what an array of unions defaults to. User
  documentation only: no issue numbers, no history.
* `docs/ARCHITECTURE.md` — that language's §10 row, and its row in the §11
  "Tagged unions" table (added in Core, one row per landed language).

Review checklist for the per-language review pass (all must hold):
1. One option held; fresh value = `D` at its default; `D` from `Target.DefaultID`.
2. Encode arms exactly §0 (forced write for non-`D`, `end_keep` for sequence-framed
   non-`D`, `D` guarded + `end`); `isDefault` and `serialize` agree.
3. Decode switch placement per §0, behind the §7.3 gate (wire type, subtype,
   array kind); the switch is **select if not held** — idempotent, never resets a
   held option, safe in a per-chunk or resume-replayed hook; struct/union options
   reset to their default on a real switch, merge when held.
4. Array-of-union gap fill = `D` at its default (per type).
5. JSON: exactly one member.
6. No generic union helper emitted (CLAUDE.md static-helper rule): only option
   arms, id routing and types are per schema.
7. Backend unit tests, `check_union.py`, `check_repeated_id.py --union`, and the
   whole existing suite green locally for every config of that language.
8. Bench rows measured, within budget or explained; results files restored.
9. Docs updated (user chapter + ARCHITECTURE rows); the Unions chapter states the
   setter's ownership and the in-place-reset aliasing of that target (§5).
10. Every hand-written source of that language that used a union option as a
   product-type field (§5 table, re-grepped) is on the new API.
11. Split variants and option-id constants cannot clash in that backend's emitted
   names (§1.1: Core folded check + the backend's own constant check).

---

## 6. Milestones (strictly sequential, one commit per step)

| # | milestone | contents | commit |
|---|---|---|---|
| 0 | Design | this file | `docs(plan): tagged-union design for #608` |
| 1 | Core | §1, §2 (driver + `--self-test`, `check_repeated_id` schema field + `--union` flag), §3 corpus, ARCHITECTURE §4/§5/§6/§11/§12, schema README + JSON schema | `feat(core): …` |
| 2 | c | §5.1 corelib-c-cpp commits on `feat/tagged-union`, then §5.2 | corelib: `feat(object): …`; generator: `feat(c): …` |
| 3 | cpp | §5.3 (both corelibs) | `feat(cpp): …` |
| 4 | rust | §5.4 (std + no_std) | `feat(rust): …` |
| 5 | zig | §5.5 | `feat(zig): …` |
| 6 | go | §5.6 | `feat(go): …` |
| 7 | java | §5.7 | `feat(java): …` |
| 8 | kotlin | §5.8 | `feat(kotlin): …` |
| 9 | csharp | §5.9 | `feat(csharp): …` |
| 10 | dart | §5.10 | `feat(dart): …` |
| 11 | typescript | §5.11 (three int64 modes) | `feat(typescript): …` |
| 12 | python | §5.12 (both engines) | `feat(python): …` |
| 13 | docs | §5.13 | `feat(docs): …` |
| 14 | Finish | MAX_SIZE = max (§1.3) + `wiresize_test.go` / `tests/matrix/maxsize_test.go`; unions in `maxsize_fill`; `check_repeated_id` union cases unconditional; driver docstrings (§2.5); ARCHITECTURE §9.6 and §11 finalised; **full** bench run and `results.txt`; final review of every language against §5.14; push `feat/tagged-union-608`; draft PR referencing corelib-c-cpp#182 with the breaking-change note | `feat(core): MAX_SIZE charges the largest union option …`, `bench: …` |

No dependency forces a different order. The only cross-repo dependency is C on
corelib-c-cpp#182, and it is handled inside milestone 2 (corelib first, then the
backend). `cpp` with `corelib: c-cpp` uses only the C++ side of that corelib,
which needs no change, so it does not wait for #182 to merge. corelib-go needs no
change (`NewMessageSeqInit` is on main). Every other corelib needs no change.
Until Finish, `MAX_SIZE` stays the sum, which over-estimates but is always safe.

CI expectation: `lang-c` stays red until corelib-c-cpp#182 merges (it pins corelib
main); every other `lang-*` job should be green on the branch. Local conformance
against the branch is the proof for C.

---

## 7. Breaking-change note (release notes, minor version bump)

> **Breaking: unions are tagged unions.** A schema `union` used to generate a
> record holding every option side by side; it now generates a type that holds
> **exactly one option** (MESSAGE_SPEC §4.2, §7.4.1).
>
> * **API.** Code that reads or writes union options as plain fields must move to
>   the new accessors: C `u.which` + `u.u.<option>` and `<PREFIX>_<OPTION>_ID`;
>   C++ `which()`, `<option>()`, `set_<option>()`, `has_<option>()`,
>   `mutable_<option>()`; Rust and Zig a native `enum` / `union(enum)`; Go
>   `Which()`, `<Option>()`, `Set<Option>()`, `Has<Option>()`, `Mut<Option>()`,
>   `Clear()`; Java/Kotlin/C#/Dart/TypeScript/Python getters and setters (or
>   properties), `has…`, `mutable…` and `which`. See the "Unions" chapter of each
>   target's documentation.
> * **Semantics.** Setting an option discards the one held before. Decoding keeps
>   only the last option received; a repeated struct option merges, a different
>   option replaces.
> * **Wire.** Encoders write at most one option per union, and now also write a
>   held option other than `default_id` when it equals its own default (it used to
>   be omitted and read back as `default_id`). Existing decoders read the new
>   bytes unchanged.
> * **Schema.** A union option of type `string`, `blob` or `array` may no longer
>   declare a non-empty `default`; an empty `oneof` is rejected; an omitted
>   `default_id` means the option with the lowest id. A `$defs` union referenced
>   with different `default_id`s now generates one type per `default_id`, named
>   `<Name>_default_<option>`.
> * **JSON.** A union renders as `{"<option>": value}` with only the held option.
> * **Sizes.** `MAX_SIZE` constants shrink: a union is charged its largest option
>   instead of the sum of all of them.

---

## 8. Open points for the user (not blockers for the workflow)

1. **Spec gap:** MESSAGE_SPEC does not say what an omitted `default_id` means; the
   generator fixes "lowest option id" (§1.2). Worth a documentation issue.
2. **cpp fixlen-array subtype at a union option** (§5.3): if `is.fixType()` is not
   known at an `ArrayFixlen` callback, an fp32/fp64-array mismatch at a union
   option id cannot be gated; this is the existing #232 asymmetry, not a new one.
   The driver case is D27; it would run under `--known-gap D27=…` on cpp only.
3. **Deviations from issue #608**, all argued above: GC targets **and TypeScript**
   use one typed slot per option instead of `prim`/`ref` / a single `value` (§5,
   §5.11 — V8 field representations); Go stores struct/union options **by value**,
   decided by argument, not by the measurement issue §9 asked for (§5.6); the
   "back to default" operation is the existing message-level `reset()` on
   Java/Kotlin/Dart and `clear()`/`Clear()` elsewhere, not a uniform `clear()`
   (§5); no corelib-go change (§5.6); no corelib-c-cpp shared vector (§5.1
   item 5); no bench schema change (§4); the default-image fix is the spec-backed
   validator rule, not (a)/(c) (§5.1 item 2), and the C image is a prefix of the
   tag and `D` only (§5.2).
