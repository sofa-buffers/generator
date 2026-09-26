# Tagged unions, family-wide — binding design for generator#608

Status: **binding** for the #608 workflow. Every language milestone implements
this document; it does not relitigate it. A milestone that finds a decision here
wrong says so precisely, fixes this file in the same commit, and explains why.

Spec: `sofa-buffers/documentation` main @ `b6586b5` — MESSAGE_SPEC §2, §3, §4.2,
§5.1, §6, §7.3, §7.4, §7.4.1 and CORELIB_PLAN §6.0.1. Where this document and the
spec disagree, the spec wins and this document is fixed.

Generator base: `main` @ `aa3609f` (includes the #609 Go nested-defaults fix and
corelib-go `NewMessageSeqInit`, corelib-go @ `7dc2f68`).

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
header **passed the §7.3 gate for `o`'s declared kind**:

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

**Switch placement (every visitor backend).** The switch runs at the one hook
that fires **exactly once per occurrence** and is reached **only past the §7.3
gate** for that option:

| option kind | hook that switches |
|---|---|
| scalar | the typed value callback, together with the store (after the width check) |
| string / blob, assembled then assigned (go, java, kotlin, csharp, rust, zig, typescript) | the completion store — never `fixlenBegin` |
| string / blob, bound in place (c via corelib, cpp, dart) | the header hook that binds the destination, after the subtype gate |
| any option, python | the typed value hook of that kind, or `on_sequence_begin` (below) |
| compact array | the array header hook, after the kind/subtype gate, where the destination is opened/cleared today |
| wrapper array, `struct`, `union` | the sequence-begin arm |

Python additionally follows issue §5: the switch belongs in the typed value hooks
and `on_sequence_begin` only — never in `on_field` / `on_schema_bound` /
`on_array_begin` / `on_*_begin`, which a resumed read can replay.

Verify per backend (the driver cases E5/E8/D19 catch it): a **zero-length**
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
     A variant key that already exists is an analysis error at the site:
     `union %q is used with default_id %d and %d; the generated type %q for one of
     them collides with an existing type — rename one`.
  Only `$defs` unions can split: an inline union has exactly one site (a union
  field inside a shared struct is still one `Field`).
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
string)` called from all three union definition sites — `checkUnionField`, the
`union` branch of `checkArrayItems`, and the `union` branch of `validateDefs`:

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
  valid; rule 1 (empty `oneof`) at all three sites.
* `internal/model/model_test.go` — a union element and a union two array levels
  down carry their `default_id` on the `TypeRef`.
* `internal/analysis/analysis_test.go` — `TestUnionDefaultIDBound` (explicit,
  omitted → lowest id, element, nested element), `TestUnionSplitByDefaultID`
  (names `Shape_default_num` / `Shape_default_pt`, `NamedOrder` position, both
  sites repointed, an omitted and an explicit-equal site do **not** split),
  `TestUnionSplitCollision`.
* `internal/ir/union_test.go` — the two helpers.
* IR golden (`internal/ir/golden_test.go`) regenerated for `default_id`.
* Corpus (§3) valid and invalid files.
* `go test ./...` green; every backend still generates the corpus (backends still
  emit product types until their own milestone — the split only renames types).

---

## 2. Shared conformance driver `tests/conformance/lib/check_union.py`

One driver, one concern, its own schema printed by itself (`--emit-schema`),
exactly the shape of `check_repeated_id.py` / `check_defaults.py`. It forges wire
images from MESSAGE_SPEC §4.3–§4.9 with its own `varint`/`header`/`signed`/
`unsigned`/`string`/`blob`/`uarray`/`seq` builders (copy the builders of
`check_repeated_id.py`; add `unsigned(fid, n)` = `header(fid, 0) + varint(n)` and
`blob(fid, b)` with fixlen subtype `0b011` = 3, CORELIB_PLAN §4.6).

Usage:
```
check_union.py --emit-schema
check_union.py --self-test                     # builders vs. hand-written hex, no harness
check_union.py <label> [--cwd DIR] [--sizes 1,2,3,5,0] [--no-stream]
               [--message NAME] -- <harness argv...>
```
Verbs used: `encode <msg>` (JSON on stdin → wire on stdout), `decode <msg>` (wire
→ JSON), `streamdecode <msg> <chunk>` (as in `check_repeated_id.py`). Loud, never
quiet: every case must run, a harness failure is a failure, the case count is
printed, and the summary names what was covered.

### 2.1 The schema (`--emit-schema`, message `uni`)

```yaml
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
      w: { id: 2, type: u8 }               # a sibling after the unions: a frame desync shows up here
```
Everything is bounded, so C, C++ `c-cpp` and Rust `no_std` build it; no 64-bit
values, so the TS int64 modes do not matter. `pt.x` defaults to 7 so "starts from
its default" is distinguishable from "starts from zero" and from "kept stale".

Default value of the message (what `decode(b"")` must print):
`u = {"pt": {"x": 7, "y": 0}}`, `v = []` (or `null`), `w = 0`.

### 2.2 Encode cases — JSON in, exact wire out, then decode(wire) == JSON

`seq(id, …)` = header(id, 6) … `07`. All expectations are built with the builders.

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

Also E14: `decode(seq(0, seq(7)))` → `u={"box":{"z":3}}` — the empty frame selects
`box` at its own (non-zero) default, not zero.

After each encode, `decode(wire)` must equal the input with the defaults filled in.

### 2.3 Decode cases — forged wire in, JSON out, one-shot AND streamed

Each is decoded with `decode`, then with `streamdecode` at every `--sizes` split,
and compared against the same expectation. Cases marked **re** additionally
re-encode the decoded JSON and compare against the canonical wire given.

| case | wire | expected | pins |
|---|---|---|---|
| D1 multi-child, last wins (**re** → `seq(0, string(1,"x"))`) | `seq(0, unsigned(0,9), string(1,"x"))` | `u={"s":"x"}` | §4.2 several children, §7.4.1 |
| D2 three children, switch to struct starts at default | `seq(0, unsigned(0,9), string(1,"x"), seq(2, signed(1,3)))` | `u={"pt":{"x":7,"y":3}}` | new option from its default |
| D3 re-opened frame, other option | `seq(0, unsigned(0,9)) seq(0, string(1,"x"))` | `u={"s":"x"}` | switch across frames |
| D4 re-opened frame, same struct option merges | `seq(0, seq(2, signed(0,1))) seq(0, seq(2, signed(1,2)))` | `u={"pt":{"x":1,"y":2}}` | §7.4 merge |
| D5 same struct option twice in one frame | `seq(0, seq(2, signed(0,1)), seq(2, signed(1,2)))` | `u={"pt":{"x":1,"y":2}}` | merge within a frame |
| D6 away and back starts from default | `seq(0, seq(2, signed(0,1))) seq(0, unsigned(0,5)) seq(0, seq(2, signed(1,2)))` | `u={"pt":{"x":7,"y":2}}` | discarded state does not survive |
| D7 §7.3-mistyped other option does not switch | `seq(0, string(1,"ab"), string(0,"zz"))` | `u={"s":"ab"}` | string at a `u16` option id is skipped |
| D8 §7.3-mistyped held option keeps value | `seq(0, string(1,"ab"), unsigned(1,4))` | `u={"s":"ab"}` | |
| D9 sequence at a string option id | `seq(0, unsigned(0,9), seq(1, signed(0,1)))` | `u={"num":9}` | skip of a whole subtree, no switch |
| D10 unknown id does not switch | `seq(0, unsigned(0,9), unsigned(9,1))` | `u={"num":9}` | |
| D11 empty union frame (**re** → `b""`) | `seq(0)` | `u` = default | accepted, treated as omitted |
| D12 empty re-opened frame keeps held | `seq(0, unsigned(0,9)) seq(0)` | `u={"num":9}` | no occurrence, no discard |
| D13 repeated string option replaced | `seq(0, string(1,"ab"), string(1,"c"))` | `u={"s":"c"}` | |
| D14 repeated array option replaced | `seq(0, uarray(3,[1,2,3]), uarray(3,[4]))` | `u={"arr":[4]}` | §7.4 array replace |
| D15 element gap is `default_id` at default (**re** → same bytes) | `seq(1, seq(0, signed(0,4)), seq(2, string(1,"q")))` | `v=[{"i":4},{"s":""},{"s":"q"}]` | element default = `D` (non-first option) |
| D16 element re-opened with other option | `seq(1, seq(0, signed(0,4)), seq(0, string(1,"z")))` | `v=[{"s":"z"}]` | §7.4 element continue + §7.4.1 switch |
| D17 empty element frame | `seq(1, seq(0), seq(1, signed(0,3)))` | `v=[{"s":""},{"i":3}]` | |
| D18 nested union switch | `seq(0, seq(6, unsigned(0,1)), seq(6, signed(1,-5)))` | `u={"inner":{"b":-5}}` | union of union |
| D19 chunked string after a switch | `seq(0, unsigned(0,9), string(1,"abcdefgh")) + unsigned(2,3)` | `u={"s":"abcdefgh"}`, `w=3` | a string option split across feeds |

Comparison: a **union level is strict** — the decoded union must be an object with
exactly the expected single key (a product-type harness that prints every arm
fails here, which is the point). Below the union, the tolerant `same()` of
`check_repeated_id.py` applies (member order, `null` for an empty list, integers
as strings). A blob compares through `as_container()` (base64 or list), and an
empty blob accepts `""`, `[]` and `null`.

`--self-test` asserts, without a harness, that the builders produce the documented
hex for E4, E7, E12 and D6 (hand-written hex in the file), so the driver itself is
tested in the Core milestone.

### 2.4 How a language opts in

In the language's milestone, `tests/conformance/<lang>/run.sh` gains a block
modelled on its `check_repeated_id.py` block (same generator config, same harness
build, Python on **both** engines through the existing `for ENGINE in $ENGINES`
loop):
```sh
echo "==> §4.2/§7.4.1 tagged unions: one option held, last option wins (generator#608)"
printf 'version: 1\nmessages:\n' > "$WORK/union.yaml"
python3 "$ROOT/tests/conformance/lib/check_union.py" --emit-schema >> "$WORK/union.yaml"
<generate + build exactly as the repeated-id block does>
python3 "$ROOT/tests/conformance/lib/check_union.py" "<Label>" -- <harness argv>
```
C, C++ run it for **every** corelib/config the script already loops over (cpp:
`corelib: cpp` and `corelib: c-cpp`, static/dynamic where the script builds both);
Rust for `std` and `no_std`; TypeScript for `bigint`, `long` and `number`.

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
and `Shape_default_pt`, `Shape` used with `default_id` 0 and 2). The corpus
README's union bullet is updated.

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
| c | `u.which` (`sofab_object_descr_id_t`) | `#define <PREFIX>_<OPTION>_ID n` | `u.u.<opt>` | assign `which` + value | `which == …_ID` | `u.u.<opt>` | `<msg>_init` |
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
Java/Kotlin/C#/Dart): `which` + **one typed private slot per option**. Primitives
stay unboxed and typed (no cast, no bit conversion on any access), and a
reference-typed option (struct/union/array/string/blob destination) is created
**on first selection and kept** when another option is selected; re-selecting it
resets it to its default **in place**. So a reused destination (`reset()` +
decode, the Dart/Java/Kotlin reuse path) re-selects without allocating, and a
fresh object allocates only the options that are actually held — never all of
them eagerly as today. The cost against `prim`/`ref` is one 4–8-byte slot per
extra option; the win is zero casts and zero per-decode allocations. TypeScript
and Python keep the issue's single `value` slot: dynamic typing gains nothing
from typed slots, and their decode builds fresh objects.

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
     now means tag 0**, so a union whose `default_id` is 0 and whose options all
     default to zero needs no image and costs no `.rodata` (the generator emits the
     plain `SOFAB_OBJECT_DESCR_UNION` with a NULL image then; see 5.2).
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
   leaf option with a non-zero declared default is the caller writing the value —
   documented in `docs/generator/c.md`.
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
* **Descriptor**: `SOFAB_OBJECT_DESCR_UNION(fields, n, nested, k, <image>, T, which)`
  with option fields addressed as `u.<opt>` (sized: `u.<opt>.data`/`u.<opt>.items`
  with length `u.<opt>.len`). The image is emitted only when `default_id != 0` or
  the default option has a non-zero default; it sets `.which = default_id` and
  that option's default. Otherwise the image pointer is NULL (5.1 item 1).
* **Macros**: one `#define <PREFIX>_<OPTION>_ID <id>` block per union named type,
  `<PREFIX>` built exactly like the bitfield prefix (`g.prefix` + sanitized
  `"named/" + key`), deduped per key, covered by `checkMacroNames`.
* **Capability guard** in the header: `#if defined(SOFAB_DISABLE_UNION_SUPPORT)
  #error "… uses unions …"` when the message reaches a union.
* **JSON harness** (`generators/c/project.go`, and
  `tests/conformance/c/example_roundtrip.c`): print/parse only the held option.
* **Tests** (`generators/c/backend_test.go`): the type shape above, descriptor macro
  and image rules (NULL when all-zero with id 0), macro names + collision check,
  split variants get two descriptors, guard emitted.
* **Conformance**: `SOFAB_C_CORELIB=/root/corelibs/wt-c-cpp-union
  tests/conformance/c/run.sh` with the `check_union.py` block and `--union` on
  `check_repeated_id.py`. CI `lang-c` stays red until #182 merges — expected.
* **Cost**: generated `.data` ±0 on the bench (no union there needs an image),
  `.bss` −4 B (`SensorSample` 12 → 8 B), `.text` ±0 (C emits no per-field code).
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
  binding in place. Record in the milestone result whether `is.fixType()` is valid
  for an `ArrayFixlen` at callback time; if not, an fp32/fp64-array mismatch at a
  union option id is the known #232 asymmetry and is documented, not fixed here.
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
* **Decode** (`generators/rust/visitor.go`): a scalar or string store assigns the
  variant (`self.m.a = UnionShape::Num(v as u16)`) — the switch and the store are
  one statement; the sequence-begin arm of a struct/union/wrapper option calls
  `<opt>_mut()` (selects at default when not held); every path below a union
  option goes through `<opt>_mut()`. A compact array option switches in
  `array_begin` after the `askip` gate.
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
  path does not regress.
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
  `Mut*` names are checked for collisions.
* **JSON**: generated `MarshalJSON` (value receiver) / `UnmarshalJSON` (pointer
  receiver) on each union type — per-schema option arms, the only JSON code Go
  emits, because the fields are unexported.
* **Decode**: the union type is its own child visitor: `Unsigned`/`Signed`/…
  arms switch and store; `String`/`Bytes` switch at completion; `ArrayBegin`
  switches after the kind gate; `BeginSequence` switches (resetting a struct option
  to `T{}` + `setDefaults`) and returns `&m.<opt>`.
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

* **Storage**: `private _which: number = <D>; private _v: <T0> | <T1> | …` (one
  value slot); getters/setters per option (typed per the int64 mode exactly as a
  field of that kind is today), `mutable<Opt>()`, `has<Opt>()`, `clear()`,
  `static readonly <OPT>_ID`. `toJSON()` returns `{ "<opt>": … }` for the held
  option only; `fromJSON` reads the one member.
* **Decode** (`visitor.go`): the switch goes **after** the generated kind/subtype
  gate in `arrayBegin`/`fixlenBegin`/`sequenceBegin` arms (`arrayBulk` and
  `fixlenBegin` route by id alone — issue §5); a string/blob switches at the
  payload completion store; paths into an option use `mutable<Opt>()`.
  `FramedSeq<T>(out, () => new T(), …)` per type.
* **Encode**: `switch (this._which)`; a u64/i64 `D` option under `long` keeps the
  `(low, high)` omission test. **Cost**: all three `ts-*` rows ±1 %. Rebuild
  `corelib-ts` `dist/` before the suite.

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
3. Decode switch placement per §0, behind the §7.3 gate, once per occurrence;
   struct/union options reset to their default on switch, merge when held.
4. Array-of-union gap fill = `D` at its default (per type).
5. JSON: exactly one member.
6. No generic union helper emitted (CLAUDE.md static-helper rule): only option
   arms, id routing and types are per schema.
7. Backend unit tests, `check_union.py`, `check_repeated_id.py --union`, and the
   whole existing suite green locally for every config of that language.
8. Bench rows measured, within budget or explained; results files restored.
9. Docs updated (user chapter + ARCHITECTURE rows).

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
3. **Deviations from issue #608**, all argued above: GC targets use one typed slot
   per option instead of `prim`/`ref` (§5); no corelib-go change (§5.6); no
   corelib-c-cpp shared vector (§5.1 item 5); no bench schema change (§4); the
   default-image fix is the spec-backed validator rule, not (a)/(c) (§5.1 item 2).
