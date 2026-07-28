# Go target — `targets.go`

Target-specific options, accepted under `targets.go`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `package` | string | `message` | The `package <name>` clause of the generated `.go` files. |
| `module_path` | string | `example.com/generated` | The module path written to the generated `go.mod` (project mode). |
| `go_version` | string | `1.21` | The `go <version>` directive written to the generated `go.mod` (project mode). |

```yaml
targets:
  go:
    package: messages
    module_path: github.com/me/myproj
    go_version: "1.22"
```

## Receiver-side decode limits

The `max_dyn_*` caps are [generic options](README.md); what is specific to this
target is how they land in the generated code — as `MaxDyn*` package constants,
passed into every `Decode<Msg>` via `sofab.WithMaxArrayCount`,
`WithMaxStringLen` and `WithMaxBlobLen`. A violation returns
`sofab.ErrLimitExceeded`.

The corelib enforces them globally per decode rather than per field, so each cap
is raised to the largest schema bound of its kind — schema-bounded fields stay
governed by their own bound. A key whose kind has no unbounded field emits
nothing.

## Arrays — `count` is a capacity

Every array field maps to a Go slice, and the slice's length is the array's
length. A schema `count: N` is a **capacity**, not a length: it never reaches the
wire, it bounds the array (an element count or element id past `N` fails the
decode as invalid), and it lets fixed-storage targets pre-size — but it never
adds elements.

The consequences you can observe from Go:

- `New<Msg>()` leaves a `count: N` array **empty** unless the schema declares a
  `default`, and a declared default shorter than `N` is materialized exactly as
  written (never tail-padded to `N`).
- Encode writes **every** element the slice holds. `[]uint32{1, 2, 0, 0}` and
  `[]uint32{1, 2}` are different values with different bytes.
- Decode yields exactly the elements the wire carried: `len()` after a round trip
  equals `len()` before it, for both the compact scalar form and the wrapper form.
- A field is omitted only when it **equals its default** — for an array with no
  declared default, only when it is empty. An all-zero `[]uint32{0, 0, 0, 0}` is a
  four-element value and stays on the wire.

Inside a wrapper-sequence array (string/blob/struct/union/nested-array elements)
the **interior is sparse**: an element equal to the element default is dropped and
leaves an id gap, which decode restores from that same default. The **last**
element is always written — as its value, or as an empty frame for a
struct/union/nested element — because its presence is what carries the length.
So `[]string{"a", ""}`, `[]string{"a"}` and `[]string{}` are three distinct values
that encode and decode distinctly.

## Struct field order (widest-first)

Generated struct fields are declared **widest-first** (8→4→2→1-byte alignment;
strings, slices and nested types rank as 8), not in schema order — Go lays
structs out in declaration order, so this cuts padding between fields. Fields
of equal alignment keep their schema order. This affects **declaration order
only** — encode walks the schema/field-id order, so the wire bytes are
byte-identical to every other target. Construct values with keyed struct
literals (`Point{X: 1, Y: 2}`), not positionally.

## Benchmark row

Row `go` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **toggle (symbol `main.run_<workload>`)** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.
