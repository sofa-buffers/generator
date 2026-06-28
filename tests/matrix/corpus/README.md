# Definition corpus (M7)

Corner-case SofaBuffers definitions, exercised hermetically by `tests/matrix`
(`go test ./tests/matrix/`):

- **`defs/`** — valid definitions. Each one is validated, lowered to an IR, and
  **generated across all 8 language backends** (with a parse check on the Go
  output). Covers: scalar boundaries, `fp` `decimals`, strings/blobs (incl.
  without `maxlen`), every array kind (numeric, array-of-string, array-of-blob),
  enums (shorthand + object form, negative values), bitfields (pos 0 and 63),
  nested structs, unions with `default_id`, large/non-contiguous field ids,
  metadata (`deprecated`/`unit`/`description`), and `$ref` reuse.
- **`invalid/`** — definitions that **must be rejected** by the hard gate
  (duplicate ids, out-of-range defaults, enum/union default mismatch, bitfield
  pos collision, blob/string default over `maxlen`, oversize/negative u64,
  array default-count mismatch, unknown keys, bad names, `decimals` > 15,
  `items.maxlen` on a numeric array, array-of-struct, recursive `$ref`, a
  cross-file `$ref` to a missing definition, …).
- **`shared/`** — definitions referenced from `defs/` via **cross-file `$ref`**
  (e.g. `common.yaml`); not validated standalone.

### `$ref` coverage

- `defs/multi_ref.yaml` — one `$defs` type referenced four times → a single
  shared generated type.
- `defs/cross_file.yaml` — `$ref` into `../shared/common.yaml`, including a
  definition (`Bounds`) whose own same-file `$ref` to `Vec3` is pulled in
  transitively; `Vec3` ends up shared across uses.
- `invalid/recursion.yaml` — a self-referential `$ref`; rejected, because a
  recursive value member has no finite size for the generated types.
