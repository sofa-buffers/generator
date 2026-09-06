# SofaBuffers Message Definition Schema

This folder holds the JSON Schema that validates SofaBuffers **message definition
files** (the YAML/JSON a user writes to describe their messages).

| File | Purpose |
|---|---|
| [`sofabuffers-schema-v1.json`](sofabuffers-schema-v1.json) | Schema **v1** — JSON Schema **draft-07**. Authoritative description of the definition format. |

> **Read this whole file before reimplementing the validator.** The schema does
> **not** stand alone: correct validation depends on three things the bare JSON
> Schema cannot express — `$data` cross-field rules, six **custom keywords**
> (`uniqueIds`, `uniquePositions`, `defaultMatchesEnum`, `defaultIdMatchesUnion`,
> `blobDefaultLength`, `int64Range`), and a **dereference-then-validate** step.
> A stock draft-07 validator will silently accept definitions the reference
> implementation rejects. These extra checks are specified in
> [§ Validation contract](#validation-contract) and **must be ported**.

---

## Definition file structure

A definition document has this top level (it must contain `$defs`, `messages`, or both):

```yaml
version: 1            # const 1
$defs:                # optional: reusable, $ref-able definitions
  struct:   { <Name>: { <field>: {...} } }
  union:    { <Name>: { <option>: {...} } }
  enum:     { <Name>: { <KEY>: <int> | { value: <int>, description: "" } } }
  bitfield: { <Name>: { <FLAG>: { pos: 0..63, default?: bool } } }
messages:             # each key = a message name
  <MessageName>:
    summary: "..."    # optional
    payload:          # required; field-id uniqueness enforced here (see below)
      <fieldName>:
        id: 0         # REQUIRED, unique within the parent, 0 .. 2^31-1
        type: <type>
        # ...type-specific constraints + metadata...
```

All names match `^[A-Za-z][A-Za-z0-9_]*$`. Objects are **closed**
(`additionalProperties: false`) almost everywhere, so typos and stray keys are
rejected rather than ignored.

### Field types

| `type` | Notes / constraints |
|---|---|
| `u8 u16 u32 u64` | unsigned ints; optional `default` (range-checked per width). For `u64`, a `default` beyond 2^53 must be a **JSON string** (exact) — see §8 |
| `i8 i16 i32 i64` | signed ints; optional `default` (range-checked per width). For `i64`, a `default` beyond ±2^53 must be a **JSON string** — see §8 |
| `fp32` `fp64` | floats; optional `default` (real number); optional `decimals` (0–15) |
| `boolean` | optional `default` |
| `string` | optional `maxlen`, optional `default` |
| `blob` | optional `maxlen`; `default` is base64 |
| `array` | `items: { type, count?, ... }`; element `type` ∈ numeric primitives, `string`, `blob`, **`enum` / `boolean` / `bitfield`**, or the composites **`struct` / `union` / `array`** (nested, recursive). `count` is the capacity and is **optional** (required only by no-heap targets, like `maxlen`); composite/enum/bitfield elements carry their own `fields` / `oneof` / `items` / `enum` / `bits`. `items.maxlen` only for string/blob elements |
| `enum` | inline map or `{ $ref }`; values are **signed 32-bit** and may be negative (signed zig-zag varint on the wire — see below); `default` must match a value |
| `bitfield` | inline `bits` map or `{ $ref }`; each flag has `pos` 0–63 + optional `default` |
| `struct` | nested; `fields:` inline or `{ $ref }`; recursive |
| `union` | `oneof:` inline or `{ $ref }`; optional `default_id` |

Common optional metadata on every field: `description`, `deprecated`. **`unit`
is allowed only on the numeric types** (`u8…u64`, `i8…i64`, `fp32`, `fp64`);
floats also allow `decimals`. Numeric value-range validation is left to the
application, as in protobuf / FlatBuffers / Cap'n Proto.

`string`/`blob` carry an optional **`maxlen`**. It is optional at the schema level,
but **targets that cannot allocate dynamically (e.g. C `char s[N]`, `no_std` Rust)
require it** to size static storage — so such a backend rejects a `maxlen`-less
`string`/`blob` as a generator-side, per-language check (see PLAN §5.7). The same
applies element-wise to a `string`/`blob` **array** via `items.maxlen`: when it is
present a fixed-storage target can emit a **2-D buffer** (e.g. C
`char data[count][maxlen]`).

Every field **requires `id`** (a uint in `0 .. 2147483647`) and `type`.

---

## How definition types map to the wire format

`sequence` is a **wire type**, not an authoring type — there is intentionally no
`sequence` keyword in the definition format.

The complete mapping from every definition type to its wire structure — scalars,
the two array forms, arrays of `struct`/`union`/`array` and of
`enum`/`boolean`/`bitfield`, structs, unions, maps, recursive types, and the
empty/default rules — is specified **once** in the
[Message & Marshalling Specification](https://github.com/sofa-buffers/documentation/blob/main/MESSAGE_SPEC.md),
with the byte/bit layout in
[CORELIB_PLAN](https://github.com/sofa-buffers/documentation/blob/main/CORELIB_PLAN.md)
(both in the documentation repo). This README does **not** duplicate them.

Two generator-side specifics those documents do not cover:

- **enum backing type:** the generated enum's backing integer is the smallest
  **signed** width (`i8`/`i16`/`i32`) that covers its value range; every backend
  derives it identically so an enum interoperates across languages.
- **sequence routing / capability:** `struct`, `union`, and arrays of dynamic or
  composite elements (`string`/`blob`/`struct`/`union`/`array`) are emitted as
  sequences, so the generator must route them through the corelib's
  `sequence_begin/end` API and require the `sequence` capability for them (see the
  generator plan). Those same five element kinds are the ones whose array
  `default` is refused (§8.4) — a wrapper sequence has no default form in any
  target. The two rules must name the same set; if you change one, change both.

---

## Validation contract

The reference implementation (the TypeScript POC) validates with **Ajv**,
configured `{ allErrors: true, strict: true, $data: true }`, after **resolving all
`$ref`s**. Plain JSON Schema validation is **not sufficient**. A conforming
validator (e.g. a Go reimplementation) must reproduce **all** of the following.

### 1. Dereference, then validate — but emit with `$ref` intact

The POC dereferences every `$ref` (via `@apidevtools/json-schema-ref-parser`)
**before** validation, so the schema validates the fully-resolved tree. It then
**returns the original, non-dereferenced document** to the code generator, so a
shared `$defs/...` type stays a single shared generated type instead of being
duplicated. Reproduce both halves:

- validate the **resolved** document (a dangling `$ref` thus fails fast), and
- generate from the **unresolved** document (preserve the shared-type graph).

### 2. `$data` cross-field rules (Ajv `$data` extension)

The schema uses Ajv's `$data` to compare one field against another at validation
time. **`$data` is not part of standard draft-07** — a stock validator ignores
these (or fails to compile them), silently dropping the checks. They are:

| Where | Rule |
|---|---|
| `string` `default` | `length(default)` ≤ `maxlen` (when `maxlen` is present) |
| `array` `default` | `length(default)` ≤ `items.count` (maxItems via `$data`; `count` is the optional capacity) |

A Go/other reimplementation that can't run `$data` **must enforce these as
explicit semantic checks** after structural validation.

> `blob` `default` length is **not** a `$data` rule: its default is base64, so the
> base64 *string* length ≠ the decoded *byte* length the bounds apply to. It is
> enforced by the `blobDefaultLength` custom keyword (§5) instead.

### 3. Custom keyword: `uniqueIds`

Applied to a `payload` object; asserts that the `id` of every direct child field
is unique. Reference implementation:

```js
ajv.addKeyword({
  keyword: "uniqueIds", type: "object", schemaType: "boolean", errors: false,
  validate(schema, data) {
    if (!schema) return true;
    const ids = Object.values(data).map(f => f.id);
    return new Set(ids).size === ids.length;
  },
});
```

> **Scope (every id scope):** `uniqueIds` is applied to `messages.*.payload`
> **and** to `#/$defs/struct` and `#/$defs/union`, because ids must be unique
> within **every** parent scope (each sequence is its own id scope). A
> reimplementation must run the uniqueness check over all three, not just the
> top-level payload.

### 4. Custom keyword: `defaultMatchesEnum`

Applied to an `enum`-typed field; asserts the field's `default` is one of the
enum's declared values. Reference implementation:

```js
ajv.addKeyword({
  keyword: "defaultMatchesEnum", type: "object", schemaType: "boolean", errors: true,
  validate(schema, data) {
    if (!schema || data.default === undefined) return true;   // presence test, not truthiness
    const values = Object.values(data.enum).map(e => (typeof e === "object" ? e.value : e));
    return values.includes(data.default);
  },
});
```

> **Use a presence test** (`data.default === undefined` / `!("default" in data)`),
> so a **falsy** default — notably `default: 0`, a common valid enum value — is
> still checked rather than skipped. This keyword reads `data.enum`, so it must run
> **after** `$ref` resolution (a `{ $ref }` enum is only a map of values once
> dereferenced).

### 5. Custom keyword: `blobDefaultLength`

Applied to a `blob`-typed field; asserts that the **decoded byte length** of the
base64 `default` does not exceed `maxlen`. (Plain string-length checks would
measure the base64 text, which is ~4/3 longer than the bytes it encodes, so this
cannot be expressed with `$data`/`maxLength`.) Reference implementation:

```js
ajv.addKeyword({
  keyword: "blobDefaultLength", type: "object", schemaType: "boolean", errors: true,
  validate(schema, data) {
    if (!schema || data.default === undefined || data.maxlen === undefined) return true;
    const bytes = Buffer.from(String(data.default), "base64").length;
    return bytes <= data.maxlen;
  },
});
```

> `Buffer.from(.., "base64")` tolerates the whitespace the `default` `pattern`
> allows. A non-JS reimplementation must base64-decode the default (ignoring
> whitespace) and compare the **byte** count to `maxlen`. Skips when `maxlen` is
> absent (it is optional), and uses a presence test (`=== undefined`) on `default`.

### 6. Custom keyword: `uniquePositions`

Applied to a `bitfield` definition (`#/$defs/bitfield`); asserts that every flag's
`pos` is unique, so two flags cannot occupy the same bit. Same shape as
`uniqueIds`, but over `pos`:

```js
ajv.addKeyword({
  keyword: "uniquePositions", type: "object", schemaType: "boolean", errors: false,
  validate(schema, data) {
    if (!schema) return true;
    const pos = Object.values(data).map(f => f.pos);
    return new Set(pos).size === pos.length;
  },
});
```

> Attached to `#/$defs/bitfield`, so it covers **both** an inline `bits` map and a
> `$defs` bitfield reached via `{ $ref }` (after dereferencing).

### 7. Custom keyword: `defaultIdMatchesUnion`

Applied to a `union`-typed field; asserts that `default_id` (if present) matches
the `id` of one of the union's declared options. The union analog of
`defaultMatchesEnum`:

```js
ajv.addKeyword({
  keyword: "defaultIdMatchesUnion", type: "object", schemaType: "boolean", errors: true,
  validate(schema, data) {
    if (!schema || data.default_id === undefined) return true;   // presence test
    const ids = Object.values(data.oneof).map(o => o.id);
    return ids.includes(data.default_id);
  },
});
```

> Reads `data.oneof`, so — like `defaultMatchesEnum` — it must run **after** `$ref`
> resolution (a `{ $ref }` union is only a map of options once dereferenced). Uses
> a presence test so `default_id: 0` (a valid option id) is not skipped.

### 8. Custom keyword: `int64Range`

Applied to an `i64` / `u64` field; enforces the **exact** 64-bit range of its
`default`. JSON/JS numbers are IEEE-754 doubles, so integers past 2^53 lose
precision — therefore the schema accepts the `default` as **`integer | string`**:
a plain number for everyday values (≤ 2^53), or a **JSON string** for exact values
up to the full 64-bit bound. The keyword carries the kind (`"i64"` / `"u64"`) and
checks the value with BigInt:

```js
ajv.addKeyword({
  keyword: "int64Range", type: "object", schemaType: "string", errors: true,
  validate(kind, data) {
    const x = data.default;
    if (x === undefined) return true;
    let big;
    if (typeof x === "string") {
      // A u64 takes the UNSIGNED pattern, the one the schema's own `default`
      // carries: "-0" parses to zero, so the sign test below would wave it
      // through and the literal "-0" would reach the emitted source.
      const re = kind === "u64" ? /^(0|[1-9][0-9]*)$/ : /^-?(0|[1-9][0-9]*)$/;
      if (!re.test(x)) return false;
      big = BigInt(x);
    } else if (typeof x === "number") {
      if (!Number.isSafeInteger(x)) return false;   // imprecise -> require a string
      big = BigInt(x);
    } else return false;
    return kind === "u64"
      ? big >= 0n && big <= 18446744073709551615n
      : big >= -9223372036854775808n && big <= 9223372036854775807n;
  },
});
```

> **Numbers stay simple; strings make it exact.** A small `u64`/`i64` `default`
> can still be written as a plain integer (`default: 42`). A value beyond the
> double-safe range **must** be a string (`default: "18446744073709551615"`) — a
> plain number there is rejected, because it would already have lost precision
> before validation. (The `pattern` on `default` constrains only the string form;
> the keyword does the range check for both.) **YAML note:** an unquoted big
> integer is parsed as a lossy number, so always **quote** 64-bit values past 2^53.
> A reimplementation must parse the string with a big-integer type and range-check
> against the exact 64-bit bounds.

Two rules the keyword above cannot state, because JSON has no way to express them,
and a reimplementation reading a **YAML** document must add both:

- **A number spelled with a decimal point or an exponent is rejected**, even when
  its value is an exact integer. In JSON `1000000.0` and `1000000` are the same
  number and `Number.isSafeInteger` cannot tell them apart, but YAML hands the
  generator a float, which it renders through the shortest decimal form — and that
  flips to exponent notation at 1e6, so `default: 100000.0` emits `100000` while
  `default: 1000000.0` emits `1e+06`. Measured one build per target, `1e+06` does
  not compile in c, cpp, rust, kotlin, csharp or typescript, throws
  `NumberFormatException` at class-init in java, and binds a float to an
  `int`-declared field in python; only go, zig and dart render it correctly. The
  spelling is therefore refused outright, and the error names the integer to write.
- **A negative u64 is refused by SPELLING, not by value.** `"-0"` parses to zero,
  so a sign test on the parsed value passes it, and the literal `-0` then reaches
  the emitted source (`-0` into a Rust `u64` is E0600). The `pattern` on the u64
  `default` above is already the unsigned one, `^(0|[1-9][0-9]*)$`, so a
  reimplementation that applies the documented pattern gets this for free; it is
  the keyword's own string branch that must not fall back to the signed pattern.

#### 8.1 Array-of-`bitfield` element masks

An **array** is the only place a `bitfield` default is written as a **number**: the
field-level form is a set of per-flag booleans (`bits: { LOW: { default: true } }`)
that the generator folds into a mask itself. So `items.type: bitfield` +
`default: [...]` is the one spot where an author spells a mask out, and it is
checked like a `u64`: each element is a **non-negative integer**, or a **quoted
decimal string** for a value the double-safe range cannot carry. A quoted
non-decimal spelling is rejected — write `0x10` unquoted and YAML converts it
before the validator ever sees it — and so is a number with a decimal point or an
exponent, because a bit pattern has no fractional spelling and the generator would
render `1000000.0` into the emitted source as `1e+06`.

A bit set at a position no flag declares is **accepted**: nothing masks a bitfield
down to its declared positions, the wire carries the whole unsigned value, and an
undeclared bit is how a peer built from a newer schema carries a flag this one does
not declare yet.

What a stock JSON Schema validator can check here is the **spelling and the sign**:
the shipped branch carries `"type": ["integer", "string"]`, `"minimum": 0` (the
integer form) and `"pattern": "^(0|[1-9][0-9]*)$"` (the string form). Two bounds
are **generator-side only**:

- the **exact 64-bit top**, for the same reason as §8 — `18446744073709551615` is
  not representable as an IEEE-754 double, so a `maximum` written at that magnitude
  rounds up to 2^64 and admits a value the generator rejects; and the `pattern`
  constrains only the string form anyway;
- the **backing width**. Six of the eleven targets back a bitfield with the
  smallest unsigned type that holds its highest declared `pos` (c, cpp, rust, go,
  zig, csharp; the other five carry it at full width), so a two-flag bitfield is one
  byte wide there and `default: [1000]` does not fit the member they emit. One
  definition has to generate for all eleven, so the schema takes the narrowest
  target's bound. It depends on the sibling `bits` map, which is not something a
  keyword-free branch can reach.

#### 8.2 Array-of-`u64` / `i64` element defaults

An `items.type` of `u64` or `i64` puts the **same** value in an array that §8 puts
in a field, so it is spelled the same way and checked by the same rule: each
element is an **integer**, or a **quoted decimal string** where the value needs
more than the double-safe range — above 2^63-1 the quoted form is the only
spelling a `u64` element has that every reader of the definition can carry, since
JSON has no unsigned 64-bit number. (A YAML reader that hands the validator an
unsigned integer of its own — as `gopkg.in/yaml.v3` does — may accept the unquoted
literal too, and the generator does; a reimplementation is not obliged to.)
Everything §8 says applies per element, the two additions above included: no
fractional or exponent spelling, and for a `u64` no sign, so `"-0"` is refused
like any other negative — as is an unquoted `-1`, which is reported for its sign
rather than for its range.

What a stock JSON Schema validator can check here is again the **spelling and the
sign**. The shipped branches carry `"type": ["integer", "string"]` plus, for
`u64`, `"minimum": 0` and `"pattern": "^(0|[1-9][0-9]*)$"`, and for `i64`,
`"minimum": -9223372036854775808` and `"pattern": "^-?(0|[1-9][0-9]*)$"`. Two
bounds stay **generator-side only**:

- the **exact 64-bit top**, for the reason §8 and §8.1 both give — neither
  `18446744073709551615` nor `9223372036854775807` is representable as an IEEE-754
  double, so a `maximum` written at either magnitude rounds *up* (to 2^64 and 2^63)
  and admits a value the generator rejects, while reading as if the range were
  covered. The `i64` floor `-9223372036854775808` **is** exact, so it stays;
- the **decimal-point / exponent refusal**, which JSON cannot express at all.

> Before this rule existed the two halves disagreed in both directions: the Go
> validator accepted **any** string for a `u64` element (`"nonsense"` reached all
> eleven backends verbatim), while the shipped branch accepted **no** string at
> all — so it rejected `tests/matrix/corpus/defs/arrays.yaml`, a definition in the
> generator's own corpus, for spelling `18446744073709551615` the only way it can
> be spelled.

#### 8.3 The decimal-point / exponent refusal covers **every** integer default

The first of §8's two YAML-only rules is not specific to a 64-bit type. It applies
wherever the definition declares an **integer** and the author writes a number with
a decimal point or an exponent:

| where | example |
|---|---|
| a `u8`…`i32` field `default` | `{ type: u32, default: 1000000.0 }` |
| a `u8`…`i32` array element | `items: { type: u32 }, default: [1000000.0]` |
| an `enum` field `default` | `{ type: enum, enum: {...}, default: 1000000.0 }` |
| an `enum` array element | `items: { type: enum, ... }, default: [1000000.0]` |
| a `u64`/`i64` field `default` or array element | §8, §8.2 |
| an array-of-`bitfield` element mask | §8.1 |

The reason is the same in all six rows and has nothing to do with width: the
generator carries the decoded value into the emitted source, and a float renders
through the shortest decimal form, which flips to exponent notation at 1e6. So
`default: 100000.0` emits `100000` and compiles everywhere, while
`default: 1000000.0` emits `1e+06` — measured on this tree into a 32-bit member as
`.a = 1e+06` (c), `a: 1e+06` (rust, `E0308`), `new int[]{1e+06}` (java, *incompatible
types*), and the same shape in the other eight. A rule that accepted a float only
below a threshold nothing in the schema names would be worse than one that refuses
the spelling, so the spelling is refused and the error names the integer to write.

This is invisible to a JSON reader — `1000000.0` and `1000000` are the same JSON
number, and the shipped `"type": "integer"` branches accept both — which is exactly
why it is written down here. A **YAML** reimplementation has to add it by hand, at
every site above; there is no keyword that can carry it.

Not covered, deliberately: `fp32`/`fp64` defaults (a float is the point), and
schema *knobs* rather than defaults — `count`, `maxlen`, `decimals`, `pos`,
`default_id`, enum member values — which are coerced to an integer and never echoed
into the emitted source, so their spelling cannot reach a compiler.

#### 8.4 An array element that is lowered to a **wrapper sequence** takes no `default` at all

The three sub-sections above all answer *how* an array element default is
spelled. This one says where there is no such thing to spell.

The set is **not** "composite" in the narrow sense. It is exactly the five
element kinds the sequence-routing rule near the top of this document already
names — *"arrays of dynamic or composite elements (`string`/`blob`/`struct`/
`union`/`array`) are emitted as sequences"* — and exactly the complement of the
native element kinds (`u8`…`i64`, `fp32`, `fp64`, `boolean`, `enum`, `bitfield`),
which are the kinds a backend materializes into a real initializer. When
`items.type` is one of those five, the array field takes **no `default` key**.
Writing one is an error, whatever it contains, an empty sequence included.

| written | verdict |
|---|---|
| `items: { type: string, maxlen: 8 }, default: ["a", "b"]` | `an array of string takes no default` |
| `items: { type: blob, maxlen: 4 }, default: ["AAE="]` | `an array of blob takes no default` |
| `items: { type: struct, fields: {...} }, default: [{ x: 7 }]` | `an array of struct takes no default` |
| `items: { type: union, oneof: {...} }, default: [{ p: 7 }]` | `an array of union takes no default` |
| `items: { type: array, items: {...} }, default: [[1, 2]]` | `an array of array takes no default` |

This is a **refusal, not a feature removal**. No target has ever emitted such a
default. Measured on this tree, one generation per target, for all twelve:

- `items: { type: array, items: { type: u64 } }, default: [["nonsense", 1000000.0]]`
  — all eleven code backends (c, cpp, rust, go, java, kotlin, csharp, typescript,
  python, zig, dart) generate the member with **no initializer at all**; grepping
  each output tree for `nonsense` and for `1e+06` finds nothing, while `docs`
  prints `[["nonsense", 1e+06]]` into `message.html`. `array<struct>` and
  `array<union>` behave identically.
- `items: { type: string, count: 2, maxlen: 16 }, default: ["zzmarkerzz"]` and the
  `blob` twin — same result, and this is the half that looks like it should work,
  because a string element default *is* a plain scalar literal. It is not
  emitted either: cpp constructs `std::vector<std::string> strs = {}` (and the
  same in `reset()`), go declares `Strs []string` and resets it to `m.Strs[:0]`,
  rust `strs: Vec::new()`, java `new ArrayList<>()`, ts `strs: string[] = []`,
  zig `&.{}`, dart `<String>[]`, and the C backend emits no defaults struct for
  the field at all. The marker string appears in exactly one output tree: `docs`.
  The control in the same message, `items: { type: u32 }, default: [7654321, 1]`,
  reaches **all twelve**.

The omission test tells the same story: a backend compares a native array against
its declared default (`!slices.Equal(m.Ctrl, []uint32{7654321, 1})` in go) but
compares a wrapper-element array against **empty** (`len(m.Strs) == 0`). So the
declared value is not merely un-materialized at construction — nothing in the
generated code ever refers to it.

So the value was already being discarded; the only thing the rule changes is that
the author is told, instead of the generated *documentation* claiming a default
the generated *code* never produces.

Implementing the shape instead would mean a wrapper-sequence initializer form in
eleven renderers, plus — for the struct and union members of the set — a spelling
rule for an element value that the definition language does not have. A
field-level `struct` or `union` takes no `default` either (the closed key set for
those two field types does not list one, which is why the defect exists only
inside `items`).

Unlike §8.1–§8.3, this rule **is** expressible in stock JSON Schema, so it is
enforced in both halves rather than in the generator alone: the `array` branch of
the shipped schema carries an
`if items.type ∈ {string, blob, struct, union, array}` → `then not required
default`. The `string` and `blob` array-`default` branches that stood beside it
before generator#497 are gone: they blessed a shape no code backend emits, so the
two halves of the contract disagreed with each other.

An array *of* such an element is otherwise entirely normal — the corpus generates
`array<string>`, `array<blob>`, `array<struct>`, `array<union>` and nested rows at
depth 2 and 3 for every backend. Only the `default` key is refused.

### 9. Hard-gate semantics

Validation is an all-or-nothing gate: on any violation, the tool emits a clear,
located error, exits non-zero, and produces **no output**. Invalid definitions are
never code-generated. Run with `allErrors: true` so the report lists every problem
at once.

---

## Notes for a reimplementation (summary checklist)

A validator is only conformant if it does **all** of:

- [ ] enforce the structural schema (types, ranges per width, closedness, required `type` + `id`);
- [ ] enforce the `$data` rules of §2 (string default ≤ maxlen, array default-count);
- [ ] enforce `id` **uniqueness in every id scope** — payload **and** nested struct/union (the schema attaches `uniqueIds` to all three);
- [ ] enforce enum-default membership with a **presence** (not truthiness) check (§4);
- [ ] enforce `blob` **default byte-length** by base64-decoding and comparing to `maxlen` (§5);
- [ ] enforce `bitfield` **`pos` uniqueness** across a bitfield's flags (§6);
- [ ] enforce `union` **`default_id` membership** against the declared option ids (§7);
- [ ] enforce **exact 64-bit range** for `i64`/`u64` `default`s, accepting an integer or string and range-checking with a big-integer type (§8);
- [ ] enforce **array-of-`bitfield` element defaults** as non-negative decimal masks — an integer or a quoted decimal string, within the exact 64-bit range and within the bitfield's own backing width (§8.1);
- [ ] enforce **array-of-`u64`/`i64`** element defaults by the same rule as the field default of that type — an integer or a quoted decimal string, exact-64-bit-range-checked, no fractional or exponent spelling, and no sign for a `u64` (§8, §8.2);
- [ ] enforce **enum values are signed 32-bit** (`-2147483648 … 2147483647`), values and `default` alike;
- [ ] refuse a **decimal-point or exponent spelling** for *every* integer default — narrow scalars, enums, their array elements, `u64`/`i64` and bitfield masks alike — and name the integer to write (§8.3);
- [ ] refuse a **`default` on an array whose `items.type` is `string`, `blob`, `struct`, `union` or `array`** — all five are lowered to a wrapper sequence, and no target has a default form for one; these are the same five kinds the sequence-routing rule names, and the complement of the native element kinds (§8.4);
- [ ] resolve `$ref` before validating, but keep `$ref` for generation;
- [ ] fail closed: located error, non-zero exit, no output.
