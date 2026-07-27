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

## On-demand corelib imports

The generated `message.py` imports from `sofab` only the names its own body can
name: `SofaDecodeError` (over-count / over-index / over-maxlen rejects),
`FixlenSubtype` (the §7.3 guards where the wire type alone is ambiguous) and
`math` (the float trim helper) are emitted per schema, so a module never carries
a dead name. Each gate (`schemaHas*` in `backend.go`) is therefore a *mirror of
an emitter* and must be kept in lockstep with it — including where that emitter
fires. Python has none of the compile-time help a typed language gives here: a
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
only — `schemaHasCountedNativeArray` and `schemaHasCountedFloatArray`, mirroring
the fixed-count native-array over-count reject, `_pad_to` and the trim helpers,
all of which are emitted for array *fields* alone — correctly do not.

## Benchmark row

Row `python` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **subtract** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.
