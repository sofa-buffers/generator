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
pub enum MShape {
    Num(u16),
    Name(String),
    Pt(MShapePt),
}

impl Default for MShape { /* MShape::Pt(MShapePt::default()) */ }

impl MShape {
    pub const NUM_ID: Id = 0;
    pub const NAME_ID: Id = 1;
    pub const PT_ID: Id = 2;
    pub fn which(&self) -> Id;

    pub fn num(&self) -> Option<&u16>;
    pub fn num_mut(&mut self) -> &mut u16;
    pub fn pt(&self) -> Option<&MShapePt>;
    pub fn pt_mut(&mut self) -> &mut MShapePt;
    // ... likewise for name
}
```

| operation | Rust |
|---|---|
| which option is held | `x.which()` → the option's id; or `match x { MShape::Num(v) => …, … }` |
| option ids | `MShape::PT_ID` |
| test | `matches!(x, MShape::Pt(_))` |
| read | `match`, or `x.pt()` → `Some(&value)` when `pt` is held, else `None` |
| select with a value | `x = MShape::Num(7)` |
| select at the default and edit in place | `x.pt_mut().y = 2` |
| back to the default | `x = MShape::default()` |

```rust
let mut s = MShape::default();  // holds pt at its default: { x: 7, y: 0 }
s = MShape::Num(7);             // now num = 7; pt is gone
s.pt_mut().y = 2;               // pt again, from its default: { x: 7, y: 2 }
if let Some(p) = s.pt() { use_it(p.x); }
match &s {
    MShape::Num(n) => use_it(*n),
    MShape::Name(n) => use_it(n),
    MShape::Pt(p) => use_it(p),
}
s = MShape::default();          // pt at its default again
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
shadow. Option `which` is `which_()` and `which_mut()`; option `borrow` is
`borrow_()` and `borrow_mut_()`. Two options that would produce the same
variant, accessor or id constant — `a_b` and `aB`, or `x` and `x_mut` — fail
generation, naming both.

**Code size (`no_std`).** On `corelib: rs-no-std` every `<option>_mut()` of a
union with more than one option is `#[inline(never)]`: the decoder reaches an
option through it wherever it stores below that option, and one out-of-line
copy of the select is smaller than one per store. `std` leaves inlining to the
compiler.

**`$defs` unions** used with different `default_id`s are one enum per
`default_id`, named after `<Name>_default_<option>`: `UnionShapeDefaultPt` and
`UnionShapeDefaultNum`.

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

