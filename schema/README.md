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
| `array` | fixed-length; `items: { type, count, maxlen? }`; element `type` ∈ numeric primitives, **`string`**, or **`blob`** (`items.maxlen` is optional and only valid for string/blob elements) |
| `enum` | inline map or `{ $ref }`; values are **signed 32-bit** and may be negative (signed zig-zag varint on the wire — see below); `default` must match a value |
| `bitfield` | inline `bits` map or `{ $ref }`; each flag has `pos` 0–63 + optional `default` |
| `struct` | nested; `fields:` inline or `{ $ref }`; recursive |
| `union` | `oneof:` inline or `{ $ref }`; optional `default_id` |

Common optional metadata on every field: `description`, `unit`, `deprecated`
(floats also allow `decimals`). Numeric value-range validation is left to the
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
`sequence` keyword in the definition format. See the
[wire-format documentation](https://github.com/sofa-buffers/documentation/blob/main/README.md).

The wire encodes the type in the low 3 bits of each field's varint header:

| Bits | Wire type |
|---|---|
| `0b000` | unsigned integer |
| `0b001` | signed integer (zig-zag) |
| `0b010` | fixed-length value (fp32 / fp64 / UTF-8 string / blob) |
| `0b011` | array of unsigned |
| `0b100` | array of signed |
| `0b101` | array of fixed-length values |
| `0b110` | **sequence start** (opens a new, isolated id scope) |
| `0b111` | **sequence end** |

So the authoring types lower onto the wire like this:

- **`struct` and any nested structure** → emitted as a **sequence**
  (`sequence_begin … sequence_end`). Each sequence opens a fresh id scope, so a
  nested struct's field ids never collide with the parent's.
- **`array` of a numeric type** → a real **array** wire type (`0b011/100/101`),
  one length prefix for all elements.
- **`array` of `string`** or **`array` of `blob`** → **not an array** —
  arrays of dynamic-length elements are forbidden as an array wire type, so they
  are encoded as a **sequence of string/blob fields**.
- **`enum`** → a **signed (zig-zag) varint** (wire type `0b001`). Enum values are
  **signed 32-bit** (`-2147483648 … 2147483647`) and may be negative; signed
  zig-zag keeps both small positives and small negatives compact, so negatives
  carry no extra cost. The generated enum's backing integer is the smallest
  **signed** width (`i8`/`i16`/`i32`) that covers its value range; every backend
  derives it identically so an enum interoperates across languages.

This is why a definition only ever needs `array` (fixed, numeric/string/blob)
plus `struct`/`union`: the variable-length and dynamic-element cases are all
expressed as sequences by the corelib at encode time. The **generator** must
therefore route `struct`/`union`/`array-of-string`/`array-of-blob` through the
corelib's `sequence_begin/end` API, and require the `sequence` capability for them
(see the generator plan).

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
| `array` `default` | `length(default)` == `items.count` (min/maxItems via `$data`) |

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
      if (!/^-?(0|[1-9][0-9]*)$/.test(x)) return false;
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
- [ ] enforce **enum values are signed 32-bit** (`-2147483648 … 2147483647`), values and `default` alike;
- [ ] resolve `$ref` before validating, but keep `$ref` for generation;
- [ ] fail closed: located error, non-zero exit, no output.
