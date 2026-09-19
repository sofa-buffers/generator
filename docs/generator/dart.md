# Dart target — `targets.dart`

Emits the generated classes for every message and named type, against
`corelib-dart`.

## Options

This target has none of its own. The generic options — `emit`, `license`,
`max_message_size`, the `max_dyn_*` decode limits, and `format` (whether
`sofabgen` runs `dart format` over what it emitted, see
[Formatting](#formatting)) — are documented in the
[generic config](README.md) and apply here unchanged.

## Field types

Scalars are plain Dart values. Every field whose payload the codec writes in
place — a string, a blob, a numeric array — is one of corelib-dart's **inline
destinations**: a typed list of fixed capacity (`storage`) plus the number of
elements in use (`length`). Decoding writes straight into that storage, so a
decode makes no copy and, into a reused object, allocates nothing.

| schema | member type |
|---|---|
| `u8`..`u64` `i8`..`i64` `enum` `bitfield` | `int` |
| `fp32` `fp64` | `double` (an fp32 NaN's raw bits in `<name>Fp32Bits`) |
| `boolean` | `bool` |
| `string` | `final sofab.InlineString` — its UTF-8 bytes |
| `blob` | `final sofab.InlineBytes` |
| array of `u*` `i*` `enum` `bitfield` | `final sofab.InlineInt64Array` |
| array of `boolean` | `final sofab.InlineInt64Array` — `0` is `false`, anything else `true` |
| array of `fp32` / `fp64` | `final sofab.InlineFloat32Array` / `InlineFloat64Array` |
| array of `string` / `blob` | `List<sofab.InlineString>` / `List<sofab.InlineBytes>` |
| array of numeric arrays (matrix) | `List<sofab.InlineInt64Array>` (or the float kinds) |
| struct, union | the generated class |
| array of struct / union / other arrays | a `List` of the element type |

Reading and writing a destination:

```dart
m.name.assignString('Ada');          // set a string
print(m.name.toString());            // read it
m.samples.assign([1, 2, 3]);         // set a numeric array
for (var i = 0; i < m.samples.length; i++) {
  use(m.samples[i]);                 // or m.samples.toList()
}
m.tags = [sofab.InlineString.of('a'), sofab.InlineString.of('b')];
```

Things worth knowing:

- **A destination field is `final`.** Change its value with `assign`,
  `assignString` or by setting `length`; the object itself stays, and so does
  its storage.
- **`length` is the value, `storage` is capacity.** Elements past `length` are
  left over from an earlier value and are not part of this one. A schema
  `count: N` / `maxlen: N` is a capacity too: a fresh array is empty, and a
  shorter value is legal and not padded.
- **Storage is sized once.** A destination whose schema bound fits in 1 KiB is
  allocated at that bound when the object is built. A larger or unbounded one
  starts empty and gets storage of exactly the announced size when a decode
  first needs it — after the size has been checked against the schema bound or
  the `max_dyn_*` limit. A reused object (`tryDecode`, `decoder`) keeps every
  byte of storage it has; `reset()` only sets the lengths back.
- **Strings are bytes.** A string field holds UTF-8; the codec validates it on
  decode, and the encoder validates it on the way out, so `assign` of bytes
  that are not UTF-8 makes `encode()` throw `SofabException`
  (`invalidArgument`). `toString()` builds the Dart `String` on demand.
- **A decoded bool array holds the wire's integers.** Any non-zero element is
  `true`; encoding writes `1` for it, and a value is compared with its default
  as booleans.
- **fp32 arrays keep raw bits.** An `InlineFloat32Array` stores the 32-bit
  patterns, so a signaling or payload NaN survives a round trip as long as it
  is read through `storage`, not widened into a `double`.
- **A destination is complete when the decode reports `complete`.** After
  `incomplete` or a refusal its contents are unspecified.

## Formatting

`sofabgen` can run `dart format` over what it generated, and does so only when
you ask: it spawns no external tool on its own, so the same version writes the
same bytes on every machine, whatever happens to be installed.

Asking is one switch — the CLI flag `--format`, or the `generic.format` config
key (the flag wins):

| value | what `sofabgen` does |
|---|---|
| `off` (the default) | Never runs `dart format`. The files are the generator's own output: valid, compilable, not canonically formatted. |
| `auto` | Runs `dart format` when it is available; when it is not, writes the files unformatted and says so once on stderr. |
| `require` | Runs `dart format`, and fails the run when it is not available. |

With the pass on, every generated `.dart` file goes through `dart format` once,
at the language version the generated `pubspec.yaml` declares. That version is
not a detail: `dart format` chooses its style by it — the short style below 3.7,
the tall style from 3.7 on — so the files come out in the style your own
`dart format` inside the generated package produces, and
`dart format --output=none --set-exit-if-changed` over a tree holding them
passes. With `emit: sources` there is no generated pubspec; the same language
version is used, and if the package you drop the file into declares a different
one, your formatter will restyle it.

It is a convenience. The generated code is correct and compiles either way; the
switch only decides whether `dart format` has already been over it when it reaches you,
and running `dart format` over the output directory yourself gets you the
same tree.

Under `auto` and under `require` alike, a formatter that RUNS and rejects a
generated file fails the generation with that file named — that is a generator
bug, and writing the file would only move it into your build.

