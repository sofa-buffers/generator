# Rust target — `targets.rust`

Emits one struct per message and named type. Two corelibs are available and the
choice decides the whole profile — throughput or footprint.

## Options

| key | type | default | effect |
|---|---|---|---|
| `corelib` | `rs` \| `rs-no-std` | `rs` | Which Rust corelib the generated crate targets. |
| `no_std` | boolean | `true` for `rs-no-std` | Emit a genuinely `#![no_std]`, heap-free crate. `corelib: rs-no-std` only. |
| `allow_dynamic` | boolean | depends on `corelib` | Storage for schema-bounded fields: `String`/`Vec` or fixed-capacity `heapless` ones. |

The generic options apply here too — including `format`, which decides whether
`sofabgen` runs `rustfmt` over what it emitted (see [Formatting](#formatting));
see the [generic config](README.md).

## `corelib`

| value | runtime | profile |
|---|---|---|
| `rs` | `corelib-rs`, std | throughput |
| `rs-no-std` | `corelib-rs-no-std`, `#![no_std]`, feature-gated per wire type | footprint |

The two produce **identical wire bytes**.

**`rs-no-std` requires a fully bounded schema.** Every `string`, `blob` and
`array` must carry a `maxlen` or `count`; an unbounded field fails generation
rather than falling back to the heap.

## `no_std`

Only meaningful with `corelib: rs-no-std`, where it is on by default. It makes
the generated crate genuinely `#![no_std]`:

- fixed-capacity `heapless::String<N>` / `heapless::Vec<T, N>` fields, sized
  from the schema bound,
- a decode stack bounded at compile time,
- `serde` behind a cargo feature rather than an unconditional dependency.

Set it to `false` to emit an ordinary `std` crate that still links the no-std
corelib — useful when the same schema has to be consumed by a host-side tool
built from the same generated code.

`corelib: rs-no-std` has no receiver-side limit to stand in for a schema bound,
so it requires every field to be bounded by the schema whatever `no_std` says:
a `maxlen` on each string and blob, a `count` on each array, and `items.maxlen`
on string and blob arrays. A schema that leaves one open is rejected at
generation time with an error naming the field; use `corelib: rs` for genuinely
unbounded fields.

## `allow_dynamic`

Decides the **storage of schema-bounded fields only**. The wire is identical
either way — this is never a format or API decision.

| value | a `maxlen: 32` string becomes |
|---|---|
| `true` | `String`, holding what the message actually carries |
| `false` | `heapless::String<32>`, inline and heap-free |

**The default depends on `corelib`** — `false` for `rs-no-std` (a firmware
target has no heap to spare), `true` for `rs` (a server target would rather
allocate what a message carries than its declared worst case).

Two things worth knowing before switching it off under `corelib: rs`:

- **It adds a `heapless` dependency** to the generated crate, and turns on the
  corelib's `heapless` feature, which lets the corelib grow and fill those
  fixed-capacity arrays while decoding.
- **Unbounded fields are unaffected.** They stay in `String` / `Vec`, so the
  switch applies per field wherever a bound exists and static storage can be
  turned on without changing the schema.
- **A fully bounded schema decodes without the heap.** The decoder's own
  scope stack is a fixed array sized from the schema's nesting depth on every
  `std` build, so with no unbounded field left a `try_decode` allocates
  nothing. (A `Decoder` fed in chunks still reassembles a string or blob split
  across two feeds in a heap buffer.)

This is the Rust analogue of the C++ [`allow_dynamic`](cpp.md#allow_dynamic),
and behaves the same way.

## Encoding

```rust
pub fn encode(&self) -> Result<Vec<u8>, sofab::Error>
pub fn serialize<_F: sofab::Flush>(&self, os: &mut sofab::OStream<'_, _F>) -> Result<(), sofab::Error>
```

With `corelib: rs-no-std` `encode()` returns `Result<heapless::Vec<u8, N>, sofab::Error>`,
`N` being the message's `MAX_SIZE`. Every write the corelib reports is passed on
with `?`, so an encode that fails never returns bytes. Ignoring the result of
`serialize` is a `#[must_use]` warning.

`encode()` writes into one buffer of exactly `MAX_SIZE` bytes when the schema
bounds the message. A value that holds more than its declared `maxlen` or
`count` allows does not fit and the call returns `Err(sofab::Error::BufferFull)`
instead of a short message. This can only happen with `String`/`Vec` storage
(`allow_dynamic: true`, the `rs` default); a `heapless` container refuses the
over-long value when it is built, so an encode never sees one. A schema with an
unbounded field drains through a scratch buffer into a `Vec` and has no size to
overrun; its `Err` can only be a status the corelib reports for an argument it
refuses, such as an id past the wire limit.

## Field names

A field's struct member is the field's schema name. A field named like a Rust
keyword is written as a raw identifier — the field `type` is `r#type` — so it
keeps its name; serde strips the `r#`, so the JSON key is unchanged. The four
keywords Rust refuses as raw identifiers, `self`, `Self`, `crate` and `super`,
get a trailing underscore instead (`self_`) and a `serde(rename)` that keeps the
JSON key. A method never collides with a field in Rust, so `encode` or
`serialize` stay as they are. A struct with a field that is not snake_case —
`Self_`, or a schema name such as `legacyId` — carries
`#[allow(non_snake_case)]`, so a `-D warnings` build accepts the name the schema
chose. The list lives in `generators/rust/reserved.go`.

## Type names

Every type is named after its place in the schema, each name in PascalCase and
joined with `_`:

| schema | Rust |
|---|---|
| message `vehicle_telemetry` | `VehicleTelemetry` |
| `$defs` struct, union, enum or bitfield `point` | `Point` |
| inline struct or union of field `a` in message `m` | `M_A` |
| inline element of an array field `a` in message `m` | `M_A` |
| `$defs` union `shape` used with `default_id` of option `pt` | `Shape__DefaultPt` |
| the incremental decoder of message `m` | `M__Decoder` |

An enum or bitfield is a module of that name holding one constant per value or
flag, upper-cased: `Color::RED`, `Flags::ON`.

A type whose name would be one the generated module uses itself — `DecodeError`,
a name from the Rust prelude such as `Vec`, `Option`, `String` or `Default`, or
the keyword `Self` — gets a trailing underscore: a message `vec` is `Vec_`, and
its decoder is still `Vec__Decoder`. The corelib and serde are only ever named
by path (`sofab::OStream`, `serde::Serialize`), so a message called `o_stream`
or `serialize` keeps its name. A type spelled with `_` or `__` carries
`#[allow(non_camel_case_types)]`.

## Unions

A `union` holds exactly one of its options. It is a Rust `enum` with one
variant per option, so holding two at once cannot be written:

```yaml
shape:
  id: 3
  type: union
  default_id: 2
  oneof:
    num:  { id: 0, type: u16, default: 5 }
    name: { id: 1, type: string, maxlen: 16 }
    pt:   { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 }, y: { id: 1, type: i32 } } }
```

```rust
pub enum M_Shape {
    Num(u16),
    Name(String),
    Pt(M_Shape_Pt),
}

impl Default for M_Shape { /* M_Shape::Pt(M_Shape_Pt::default()) */ }

impl M_Shape {
    pub const NUM_ID: sofab::Id = 0;
    pub const NAME_ID: sofab::Id = 1;
    pub const PT_ID: sofab::Id = 2;
    pub fn which(&self) -> sofab::Id;

    pub fn num(&self) -> Option<&u16>;
    pub fn num_mut(&mut self) -> &mut u16;
    pub fn pt(&self) -> Option<&M_Shape_Pt>;
    pub fn pt_mut(&mut self) -> &mut M_Shape_Pt;
    // ... likewise for name
}
```

| operation | Rust |
|---|---|
| which option is held | `x.which()` → the option's id; or `match x { M_Shape::Num(v) => …, … }` |
| option ids | `M_Shape::PT_ID` |
| test | `matches!(x, M_Shape::Pt(_))` |
| read | `match`, or `x.pt()` → `Some(&value)` when `pt` is held, else `None` |
| select with a value | `x = M_Shape::Num(7)` |
| select at the default and edit in place | `x.pt_mut().y = 2` |
| back to the default | `x = M_Shape::default()` |

```rust
let mut s = M_Shape::default();  // holds pt at its default: { x: 7, y: 0 }
s = M_Shape::Num(7);             // now num = 7; pt is gone
s.pt_mut().y = 2;               // pt again, from its default: { x: 7, y: 2 }
if let Some(p) = s.pt() { use_it(p.x); }
match &s {
    M_Shape::Num(n) => use_it(*n),
    M_Shape::Name(n) => use_it(n),
    M_Shape::Pt(p) => use_it(p),
}
s = M_Shape::default();          // pt at its default again
```

**`<option>_mut()`** selects the option at its own default if another one is
held, and returns a mutable reference to it. If the option is already held it
is returned as it is, untouched.

**Ownership.** The enum owns the option it holds: assigning a variant moves the
value in, and selecting another option drops the one held before. The
references `<option>()` and `<option>_mut()` return borrow the union, as any
field reference does.

**Storage.** An option's own type follows `no_std` and `allow_dynamic` exactly
as a struct member of that type does (`String` or `heapless::String<N>`, and so
on). The enum is held inline and is as large as its largest option; nothing is
boxed, so a union costs no allocation on any profile.

**Names.** The variants are the option names in PascalCase; the accessors are
the option names. An option named like a Rust keyword is a raw identifier
(`r#type()`, `type_mut()`); an option named `self` is the variant `Self_`. An
accessor whose name is on this list gets a trailing underscore: `which`,
`serialize`, `deserialize`, `default`, `clone`, `clone_from`, `eq`, `ne`, `fmt`,
`to_owned`, `clone_into`, `borrow`, `borrow_mut`, `into`, `try_into`,
`type_id` — the union's own members and the methods it has from its derives
and the standard library's blanket impls, which a same-named accessor would
shadow — and `new`, `as_mut`, `deref_mut`, which clippy refuses as look-alikes
of a constructor or the standard traits' methods. Option `which` is `which_()`
and `which_mut()`. A getter also gets the underscore when its name has the shape
of another member: an option ending in `_mut` (`x_mut_()` beside `x_mut()`, the
accessor of option `x`), or an all-upper-case one ending in `_ID` (`A_ID_()`
beside the constant `A_ID` of option `a`). Where `<option>_mut` itself is on the
list, the mutable accessor is `<option>__mut` instead: option `as` is `r#as()`
and `as__mut()`, option `borrow` is `borrow_()` and `borrow__mut()`. So no two
options ever produce the same variant, accessor or id constant.

**Code size (`no_std`).** On `corelib: rs-no-std` every `<option>_mut()` of a
union with more than one option is `#[inline(never)]`: the decoder reaches an
option through it wherever it stores below that option, and one out-of-line
copy of the select is smaller than one per store. `std` leaves inlining to the
compiler.

**`$defs` unions** used with different `default_id`s are one enum per
`default_id`, named `<Name>__Default<Option>`: `Shape__DefaultPt` and
`Shape__DefaultNum`.

**Defaults.** `Default` holds the `default_id` option at that option's own
default; an omitted `default_id` means the option with the lowest id. Each
element of an array of unions starts the same way — including an element a
decoded array skips.

**Encode.** Only the held option is written. If it is the `default_id` option
at its default, the union is at its default and is left out. Any other held
option is written **even at its own default** — `0`, an empty string, an empty
blob or array, or an empty frame for a struct option, a union option, or an
array whose elements are strings, blobs, structs, unions or arrays — because
the receiver's fresh union holds `default_id`, and leaving it out would read
back as that.

**Decode.** The option received last wins. A different option replaces the
held one and starts from its own default; the held option received again
continues where it was (a struct or union option merges, anything else is
replaced). A field whose wire type does not match its option, and an unknown
id, change nothing.

**JSON (serde).** A union serializes in serde's externally tagged form: an
object with exactly one member, the held option — `{"pt": {"x": 7, "y": 2}}`,
also when that is the `default_id` option at its default. A union member left
out of a message's JSON reads as the union's default.

## Including the generated module

The generated code builds clean under `RUSTFLAGS="-D warnings"` and
`cargo clippy -- -D warnings`. It carries no crate- or module-wide lint
`allow`, so declare the module the way you would any library surface:

```rust
pub mod message;
use message::*;
```

Declared private (`mod message;`), every part of the API your crate does not
call is dead code to rustc and warns. With `no_std` on, the generated crate is a
lib that re-exports the module, so a dependent crate has nothing to declare.

## Formatting

`sofabgen` can run `rustfmt` over what it generated, and does so only when you
ask: it spawns no external tool on its own, so the same version writes the same
bytes on every machine, whatever happens to be installed.

Asking is one switch — the CLI flag `--format`, or the `generic.run_formatter`
config key (the flag wins):

| value | what `sofabgen` does |
|---|---|
| `off` (the default) | Never runs `rustfmt`. The files are the generator's own output: valid, compilable, not canonically formatted. |
| `auto` | Runs `rustfmt` when it is available; when it is not, writes the files unformatted and says so once on stderr. |
| `require` | Runs `rustfmt`, and fails the run when it is not available. |

With the pass on, every generated `.rs` file goes through `rustfmt` once, at the
edition the generated `Cargo.toml` declares, run in the output directory — so a
`rustfmt.toml` of your own that covers that directory is honoured, and the files
come out the way your own `cargo fmt` over that tree would leave them.
`cargo fmt --check` over a tree holding them then passes, so generated files need
no exclusion from a formatting gate and never come back reformatted.

It is a convenience. The generated code is correct and compiles either way; the
switch only decides whether `rustfmt` has already been over it when you receive it,
and running `cargo fmt` over the output directory yourself gets you the same tree.

Under `auto` and under `require` alike, a `rustfmt` that RUNS and rejects a
generated file fails the generation with the file named — that is a generator
bug, and writing the file would only move it into your build.

