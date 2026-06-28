# SofaBuffers Message Definition Schema

This folder holds the JSON Schema that validates SofaBuffers **message definition
files** (the YAML/JSON a user writes to describe their messages).

| File | Purpose |
|---|---|
| [`sofabuffers-schema-v1.json`](sofabuffers-schema-v1.json) | Schema **v1** — JSON Schema **draft-07**. Authoritative description of the definition format. |

> **Read this whole file before reimplementing the validator.** The schema does
> **not** stand alone: correct validation depends on three things the bare JSON
> Schema cannot express — `$data` cross-field rules, two **custom keywords**
> (`uniqueIds`, `defaultMatchesEnum`), and a **dereference-then-validate** step.
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
| `u8 u16 u32 u64` | unsigned ints; `min`/`max`/`default` range-checked per width |
| `i8 i16 i32 i64` | signed ints; `min`/`max`/`default` range-checked per width |
| `fp32` `fp64` | floats; `min`/`max`/`default` (real numbers); optional `decimals` (0–15) |
| `boolean` | optional `default` |
| `string` | **required `maxlen`**, optional `minlen`, optional `default` |
| `blob` | **required `maxlen`**, optional `minlen`; `default` is base64 |
| `array` | fixed-length; `items: { type, count }`; element `type` ∈ numeric primitives, **`string`**, or **`blob`** |
| `enum` | inline map or `{ $ref }`; values may be negative; `default` must match a value |
| `bitfield` | inline `bits` map or `{ $ref }`; each flag has `pos` 0–63 + optional `default` |
| `struct` | nested; `fields:` inline or `{ $ref }`; recursive |
| `union` | `oneof:` inline or `{ $ref }`; optional `default_id` |

Common optional metadata on every field: `description`, `unit`, `deprecated`
(numeric fields also allow `decimals` on floats).

Every field **requires `id`** (a uint in `0 .. 2147483647`) and `type`.

---

## How this maps to the wire format ("where did `sequence` go?")

There is **no `sequence` type in the definition format**, and that is correct.
`sequence` is a **wire type**, not an authoring type. See the
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

- **`struct`, `union`, and any nested structure** → emitted as a **sequence**
  (`sequence_begin … sequence_end`). Each sequence opens a fresh id scope, so a
  nested struct's field ids never collide with the parent's.
- **`array` of a numeric type** → a real **array** wire type (`0b011/100/101`),
  one length prefix for all elements.
- **`array` of `string`** or **`array` of `blob`** → **not an array** —
  arrays of dynamic-length elements are forbidden as an array wire type, so they
  are encoded as a **sequence of string/blob fields**.

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
| every numeric type | `min` ≤ `max` (`min.maximum = {$data:"1/max"}`) |
| `string`, `blob` | `minlen` ≤ `maxlen` |
| `string` `default` | `minlen` ≤ `length(default)` ≤ `maxlen` |
| `array` `default` | `length(default)` == `items.count` (min/maxItems via `$data`) |

A Go/other reimplementation that can't run `$data` **must enforce these as
explicit semantic checks** after structural validation.

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

> **Scope caveat (must fix in a new impl):** in v1 the `uniqueIds` keyword is
> present **only on `messages.*.payload`**, *not* on `$defs/struct` or
> `$defs/union`. So duplicate ids **inside a nested struct/union are not caught**.
> Per the format, ids must be unique within **every** parent scope (each sequence
> is its own id scope), so a correct validator must apply the uniqueness check to
> struct and union members too — either by adding `uniqueIds` to those subschemas
> or as a semantic pass over every id scope.

### 4. Custom keyword: `defaultMatchesEnum`

Applied to an `enum`-typed field; asserts the field's `default` is one of the
enum's declared values. Reference implementation:

```js
ajv.addKeyword({
  keyword: "defaultMatchesEnum", type: "object", schemaType: "boolean", errors: true,
  validate(schema, data) {
    if (!schema || !data.default) return true;            // <-- see caveat
    const values = Object.values(data.enum).map(e => (typeof e === "object" ? e.value : e));
    return values.includes(data.default);
  },
});
```

> **Caveat (must fix in a new impl):** the guard `!data.default` also short-circuits
> on a **falsy** default — notably `default: 0`. An enum whose `default` is `0`
> when `0` is **not** a declared value would pass incorrectly. Use a presence test
> (`data.default === undefined` / `!("default" in data)`) instead of truthiness.
> Note this keyword reads `data.enum`, so it must run **after** `$ref` resolution
> (a `{ $ref }` enum is only a map of values once dereferenced).

### 5. Hard-gate semantics

Validation is an all-or-nothing gate: on any violation, the tool emits a clear,
located error, exits non-zero, and produces **no output**. Invalid definitions are
never code-generated. Run with `allErrors: true` so the report lists every problem
at once.

---

## Notes for a reimplementation (summary checklist)

A validator is only conformant if it does **all** of:

- [ ] enforce the structural schema (types, ranges per width, closedness, required `type` + `id`);
- [ ] enforce the `$data` rules of §2 (min≤max, minlen≤maxlen, default-length/array-count);
- [ ] enforce `id` **uniqueness in every id scope** — payload **and** nested struct/union (fixing the §3 scope gap);
- [ ] enforce enum-default membership with a **presence** (not truthiness) check (fixing the §4 caveat);
- [ ] resolve `$ref` before validating, but keep `$ref` for generation;
- [ ] fail closed: located error, non-zero exit, no output.

## Changelog

**v1 (current), recent fixes**

- **`id` is now required** on every field (base `field` definition: `required: ["type", "id"]`).
  Previously a field could omit `id` and still validate.
- **Float `min` accepts real numbers.** The `fp32`/`fp64` `min ≤ max` rule
  mistakenly constrained `min` to `integer`, rejecting e.g. `min: 1.5`; it is now
  `number`.
- **`array` of `blob` is now authorable.** Added `blob` to `array.items.type`
  (with a base64 `default` branch, matching the scalar `blob` default). On the
  wire it is a sequence of blobs, like `array` of `string`.

**Still open (intentional / design decisions):**

- No `sequence` authoring type — by design (it is a wire type; see above).
- `union.default_id` has no "matches a declared option" check (no analog to
  `defaultMatchesEnum`).
