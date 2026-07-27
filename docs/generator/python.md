# Python target — `targets.python`

Target-specific options, accepted under `targets.python`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

The Python target takes no options of its own — everything is set in the
[generic config](README.md).

## On-demand imports and helpers

The generated `message.py` imports only the corelib names it actually uses, and
emits its private helpers (`_trim_tail`, `_trim_tail_float`, `_pad_to`) only for
the schemas that reference them — an unconditional import list would leave unused
names in every module.

Each condition is a **schema walk that must mirror the emitter's own recursion**.
The §7.3 wire-type guard, the over-count and over-index rejects, and the maxlen
rejects are each emitted at two levels: on the field, and on every
wrapper-sequence *element* at every nesting depth. A gate that inspects only the
field (plus one level of native array element) therefore under-approximates for
`array<string>`, `array<blob>` and any nested row — and the module then names a
symbol it never imported.

That failure mode is invisible to a syntax check: the name is resolved when the
line executes, so `import message` succeeds and only a real decode of the
affected shape raises `NameError`. It shipped once for `FixlenSubtype`
(generator#246). `tests/matrix/corpus/defs/nested_arrays.yaml` now carries the
shapes that expose it, and the Python conformance job **decodes** it populated
rather than merely importing it.

When adding a gate, reuse the emitter's recursion shape literally and say so in
its doc comment, so the two drift together or not at all.

## Receiver-side decode limits

The `max_dyn_*` caps are [generic options](README.md); what is specific to this
target is how they land in the generated code — as `MAX_DYN_ARRAY_COUNT`,
`MAX_DYN_STRING_LEN` and `MAX_DYN_BLOB_LEN` module constants, passed to the
corelib as `Decoder(max_array_count=…, max_string_len=…, max_blob_len=…)`. A
violation raises `SofaLimitError` before anything is allocated.

## Benchmark row

Row `python` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **subtract** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.
