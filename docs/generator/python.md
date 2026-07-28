# Python target — `targets.python`

Target-specific options, accepted under `targets.python`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

The Python target takes no options of its own — everything is set in the
[generic config](README.md).

## Receiver-side decode limits

The `max_dyn_*` caps are [generic options](README.md); what is specific to this
target is how they land in the generated code — as `MAX_DYN_ARRAY_COUNT`,
`MAX_DYN_STRING_LEN` and `MAX_DYN_BLOB_LEN` module constants, passed to the
corelib as `Decoder(max_array_count=…, max_string_len=…, max_blob_len=…)`. A
violation raises `SofaLimitError` before anything is allocated.

## Arrays — `count` is a capacity

Every array field maps to a Python `list`, and the list's length is the array's
length. A schema `count: N` is a **capacity**, not a length: it never reaches the
wire, it bounds the array (an element count or element id past `N` raises
`SofaDecodeError`), and it lets fixed-storage targets pre-size — but it never adds
elements.

What that means from Python:

| field | dataclass default |
| --- | --- |
| `count: 3` native (`u32`) | `field(default_factory=list)` |
| `count: 3` wrapper (`string`) | `field(default_factory=list)` |
| `count: 5` native with `default: [1, 2]` | `field(default_factory=lambda: [1, 2])` |
| count-less (either kind) | `field(default_factory=list)` |

- `Msg()` leaves a `count: N` array **empty** unless the schema declares a
  `default`, and a declared default shorter than `N` is materialized exactly as
  written (never tail-padded to `N`).
- Encode writes **every** element the list holds. `[1, 2, 0, 0]` and `[1, 2]` are
  different values with different bytes.
- Decode yields exactly the elements the wire carried: `len()` after a round trip
  equals `len()` before it, for both the compact scalar form and the wrapper form.
- A field is omitted only when it **equals its default** — for an array with no
  declared default, only when it is empty. An all-zero `[0, 0, 0, 0]` is a
  four-element value and stays on the wire.

Both array kinds now agree with each other and with the fixed-storage camp, whose
`count: N` slot is likewise a capacity holding `0..N` elements.

## Element sparsity is positional

Inside a wrapper-sequence array (string/blob/struct/union/nested-array elements)
the **interior is sparse** and the **last element is always written** — one rule
for both element kinds (MESSAGE_SPEC §2). An interior element equal to its element
default is omitted and leaves an id **gap**, which `unmarshalArray` restores from
that same default; the element at the highest index is the only one whose
*presence* carries the length (§5.1), so it is written whatever its value.

Two generator pieces implement it, and both read the position out of the **value**
at run time — the schema cannot answer it:

- `lastElemExpr` — the `or _i0 == len(...) - 1` disjunct on the `string`/`blob`
  leaf omit test, and on the write of a **native row** (which has no frame of its
  own, so the rule lands on the write itself).
- `emitSeqEnd` — the closer of a sequence-form element: `write_sequence_end_keep`
  at the last index, `write_sequence_end` (which drops the lazily-held frame) in
  the interior. A sequence-typed **field** — a struct/union field, an array
  wrapper — always takes the dropping closer, decided statically.

So `["a", ""]` → `06020a610a0207`, `["", ""]` → `060a0207`, `["", "x", ""]` →
`060a0a78120207`, and an interior all-default struct element vanishes
(`[{1},{},{3}]` → `06060001071600030707`) while a trailing one survives as an
empty frame (`[{},{}]` → `060e0707`). `[]` still vanishes entirely, so the empty
array keeps its zero-byte encoding — `["a", ""]`, `["a"]` and `[]` are three
distinct values with three distinct encodings.

Because an interior gap is now reachable, **every** element kind is placed at
`target[id]` on decode after gap-filling, never appended. The leaf paths always
did this; the matrix-row and wrapper-row collectors did not, and an appending
collector shifts every later row down by one as soon as a row is elided.

## On-demand corelib imports

The generated `message.py` imports from `sofab` only the names its own body can
name: `SofaDecodeError` (over-count / over-index / over-maxlen rejects) and
`FixlenSubtype` (the §7.3 guards where the wire type alone is ambiguous) are
emitted per schema, so a module never carries a dead name. Each gate
(`schemaHas*` in `backend.go`) is therefore a *mirror of an emitter* and must be
kept in lockstep with it — including where that emitter fires. Python has none of the compile-time help a typed language gives here: a
missing name is a `NameError` raised from `_unmarshal`, i.e. at decode time, on
an otherwise importable module.

The trap this walked into once (generator#246): a §7.3 guard is emitted at **two**
levels — for the field, and for every wrapper-sequence **element** down the array
element chain (`pyElemWireGuard`). A gate that inspects field kinds plus one level
of *native* array element misses `array<string>`, `array<blob>` and nested rows
such as `array<array<fp32>>`, all of which name `FixlenSubtype` from an element
guard while no field is fixlen. When adding a gate, ask which emitter it mirrors
and whether that emitter can fire for an element or a nested row; if it can, the
walk must descend (`fieldHasFixlenGuard`, `schemaHasCountedWrapperArray` and
`schemaHasMaxlenStringBlob` do). Gates whose emitter is structurally top-level
only — `schemaHasCountedNativeArray`, mirroring the fixed-count native-array
over-count reject, which is emitted for array *fields* alone — correctly do not.

## Benchmark row

Row `python` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **subtract** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.
