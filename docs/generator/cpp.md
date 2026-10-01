# C++ target — `targets.cpp`

Emits one class per message and named type. Two corelibs are available and the
choice decides the whole profile — throughput or footprint.

## Options

| key | type | default | effect |
|---|---|---|---|
| `corelib` | `cpp` \| `c-cpp` | `cpp` | Which C++ corelib the generated code targets. |
| `allow_dynamic` | boolean | depends on `corelib` | Storage for schema-bounded fields: dynamic containers or fixed-capacity inline ones. |
| `namespace` | string | `message` | The namespace wrapping the generated types. |

The generic options apply here too; see the [generic config](README.md).

## `corelib`

| value | runtime | profile |
|---|---|---|
| `cpp` | `corelib-cpp`, header-only C++20 | throughput; dynamic containers |
| `c-cpp` | the C++ wrapper over `corelib-c-cpp` | footprint; heap-free, fixed capacity |

The two produce **identical wire bytes**. What differs is what the generated
code costs at runtime and what the schema must declare.

**`c-cpp` requires a fully bounded schema.** Every `string`, `blob` and `array`
must carry a `maxlen` or `count`; an unbounded field fails generation rather
than falling back to the heap. That is the point of the profile — the storage a
message can occupy is known at compile time.

**`cpp` accepts either.** A bounded field can still be given fixed storage (see
`allow_dynamic`); an unbounded one stays in a `std::string` / `std::vector`.

## `allow_dynamic`

Decides the **storage of schema-bounded fields only**. The wire is identical
either way — this is never a format or API decision.

| value | a `maxlen: 32` string becomes |
|---|---|
| `true` | `std::string`, holding what the message actually carries |
| `false` | `sofab::FixedString<32>`, inline and heap-free |

Arrays and blobs follow the same split: `std::vector<T>` / `sofab::InlineVector<T, N>`
and `std::string` / `sofab::FixedBytes<N>`.

**A `boolean` array's element `T` is `std::uint8_t`, not `bool`**, in both storage
modes. The member itself is the decode destination, and `std::vector<bool>` — the
bit-packed specialisation, with no `data()` — cannot be one. It is still a truth
value (`0` is false, anything else true) and still appears as `true`/`false` in
JSON; only the C++ element type differs.

- **Decode.** Every element is stored as `0` or `1`: any non-zero wire value,
  however wide, decodes to `1`.
- **Encode.** Every element is written as it is stored. Keep elements at `0` or
  `1` so the message carries the canonical `true`.

**The default depends on `corelib`** — `false` for `c-cpp` (an embedded target
has no heap to spare), `true` for `cpp` (a server target would rather allocate
what a message carries than its declared worst case).

Two things worth knowing before switching it on under `corelib: cpp`:

- **Unbounded fields are unaffected.** They have no bound to size storage from,
  so they stay dynamic. The switch applies per field, wherever a bound exists —
  which means static storage can be turned on without changing the schema.
- **A declared bound is honoured whatever its size.** There is no threshold above
  which a large `maxlen` silently falls back to the heap, so `maxlen: 1048576`
  really does put a megabyte inline. Setting `allow_dynamic: false` makes every
  declared bound a decision about `sizeof`.

## `namespace`

Wraps every generated type; the default is `message`. `generic.namespace` sets
it for every target that has one, and this key overrides that for C++ alone.

## Names

Every name is derived from the schema, and every schema the validator accepts
generates without a clash.

**Types.** A message or `$defs` type is its name in PascalCase: message
`vehicle_telemetry` is `VehicleTelemetry`, the `$defs` struct `point` is
`Point`. A type declared inline takes the path to it, each step in PascalCase,
joined with `_`: the struct of field `a` in message `m` is `M_A`, the element
struct of an array field `pts` is `M_Pts`, and so is an enum or bitfield
declared inline.

**Enum constants** are PascalCase members of their `enum class`
(`Color::Red`). **Bitfield flags** are enumerators at namespace level, named
`<Type>_<Flag>`: flag `ready` of bitfield `StatusFlags` is `StatusFlags_Ready`.

**Files.** Each message is one header, named after the message in lower case:
`VehicleTelemetry` is `vehicletelemetry.hpp`, `vehicle_telemetry` is
`vehicle_telemetry.hpp`. A name Windows reserves for a device (`con`, `prn`,
`aux`, `nul`, `com0`–`com9`, `lpt0`–`lpt9`, in any case) gets a trailing
underscore, so message `con` is `con_.hpp`.

**Field members.** A field's member is the field's schema name. C++ has no way
to escape a reserved name, so a field whose name the generated class cannot
take as a member gets a trailing underscore — the field `class` is the member
`class_`, and `encode` is `encode_`. Those names are:

- the C++ keywords;
- the members every generated class declares itself: `serialize`,
  `deserialize`, `reset`, and on a message also `encode`, `encodeTo`, `decode`,
  `try_decode`;
- the members corelib-cpp looks for to recognise an array collector: `cap`,
  `dynCap`, `elemDestCap`, `elemWire`, `elemFix`, `prepare`, and `MAX_SIZE`;
- every macro of the headers the generated code includes (`NULL`, `EOF`,
  `INT8_MAX`, `errno`, `stdin`, ...; in GNU mode also `linux` and `unix`), and
  every name starting with `SOFAB_` or `SOFABGEN_`. The macro set covers
  glibc and newlib (each with libstdc++), including the names only one of them
  defines (`SYS_read` on glibc, `EFTYPE` on newlib); on another C library a
  macro of its own that neither defines is not escaped;
- the name of any type or bitfield flag the schema generates, so that a field
  named like its own message, or like a type its class uses, is `Point_`.

Only the member changes: the wire is keyed by the field id, and the JSON key
stays the schema name. An enum constant spelled like a macro takes the same
underscore.

**Reserved type names.** A type whose name is one a generated class already
sees — `Which`, `Message`, `OStreamMessage`, `IStreamMessage`, `Context` --
or a macro, or a component of the configured `namespace`, gets a trailing
underscore: message `message` is `Message_`. The same holds for a bitfield flag
spelled like a macro.

**Parameters and locals** of the generated member functions all start with an
underscore (`_os`, `_is`, `_id`, `_data`, `_len`, `_out`), so no field can
collide with one.

## Unions

A `union` holds exactly one of its options. It is a class of its own (a
`sofab::Message`, so it nests and encodes like a struct) whose options are
reached through accessors, never as members:

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

```cpp
struct M_Shape : sofab::Message {
    enum class Which : sofab::id { num = 0, name = 1, pt = 2 };
    Which which() const noexcept;

    bool has_num() const noexcept;
    std::uint16_t num() const noexcept;
    void set_num(std::uint16_t _v) noexcept;
    std::uint16_t &mutable_num() noexcept;

    bool has_pt() const noexcept;
    const M_Shape_Pt &pt() const noexcept;
    void set_pt(const M_Shape_Pt &_v);
    M_Shape_Pt &mutable_pt() noexcept;
    // ... likewise for name

    void reset() noexcept;
};
```

| operation | C++ |
|---|---|
| which option is held | `x.which()` → `M_Shape::Which` |
| option ids | `M_Shape::Which::pt`; the enumerator's value is the option id |
| test | `x.has_pt()` |
| read | `x.num()` returns the value; `x.pt()` / `x.name()` return a `const` reference |
| select with a value | `x.set_num(7)`, `x.set_pt(p)` |
| select at the default and edit in place | `x.mutable_pt().y = 2` |
| back to the default | `x.reset()` |

```cpp
M_Shape s;                      // holds pt at its default: {x = 7, y = 0}
s.set_num(7);                  // now num = 7; pt is gone
s.mutable_pt().y = 2;          // pt again, from its default: {x = 7, y = 2}
if (s.has_pt()) { use(s.pt().x); }
switch (s.which()) {
case M_Shape::Which::num:  use(s.num()); break;
case M_Shape::Which::name: use(s.name()); break;
case M_Shape::Which::pt:   use(s.pt()); break;
}
s.reset();                     // pt at its default again
```

**Reading an option that is not held** returns that option's default — its
declared `default` for a scalar, an empty string / blob / array, a
default-constructed struct or union — and changes nothing. Only `set_` and
`mutable_` select.

**`mutable_<option>()`** selects the option at its default if it is not held,
and returns a reference to it. If it is already held it is returned as it is,
untouched.

**Ownership.** `set_<option>(v)` copies `v` into the union. The references that
`<option>()` and `mutable_<option>()` return point into the union's own storage:
selecting another option ends the previous option's lifetime, so a reference
taken to it earlier must not be used afterwards.

**Storage.** With `corelib: cpp` the options live in a `std::variant`, one
alternative per option in id order. With `corelib: c-cpp` — freestanding, no
`<variant>` — they share a C++ `union` beside a `Which` tag; a switch places the
new option with placement `new`. The options' own types follow `allow_dynamic`
as any member does. The API is the same on both.

**Accessor names** are the option names. `has_`, `set_` and `mutable_` are
prefixed to the option's schema name as it is: option `reset` is `has_reset()`,
`set_reset()`, `mutable_reset()`. The getter and the `Which` enumerator follow
the member rules of [Names](#names), and take a trailing underscore on top when
the option is named like one of the union's own members (`which`, `Which`) or
starts with `set_`, `has_` or `mutable_`: option `reset` is read with
`reset_()`, and options `foo` and `set_foo` side by side have the getters
`foo()` and `set_foo_()` and the setters `set_foo()` and `set_set_foo()`.

**`$defs` unions** used with different `default_id`s are one class per
`default_id`, named `<Name>_default_<Option>`: `Shape_default_Pt` and
`Shape_default_Num`.

**Defaults.** A new union holds its `default_id` option at that option's own
default; an omitted `default_id` means the option with the lowest id. Each
element of an array of unions starts the same way — including an element a
decoded array skips.

**Encode.** Only the held option is written. If it is the `default_id` option
at its default, the union is at its default and is left out. Any other held
option is written **even at its own default** — `0`, an empty string, an empty
blob or array, or an empty frame for a struct, union or array-of-string option
— because the receiver's fresh union holds `default_id`, and leaving it out
would read back as that.

**Decode.** The option received last wins. A different option replaces the
held one and starts from its own default; the held option received again
continues where it was (a struct or union option merges, anything else is
replaced). A field whose wire type does not match its option, and an unknown
id, change nothing.

**JSON (project harness).** A union is an object with exactly one member, the
held option: `{"pt": {"x": 7, "y": 2}}`, also when that is the `default_id`
option at its default.

**`corelib: c-cpp` corelib cost.** The C++ unions do not use the C object
API. `corelib-c-cpp`'s object API carries its own union walk in every build
that keeps sequence support, so a firmware that links `object.c` pays for it
whether or not the schema has a union; there is no switch to leave it out.

With `corelib: c-cpp`, a header whose message uses a union refuses to compile
against a corelib that predates unions (one without `SOFAB_OBJECT_DESCR_UNION`),
with an `#error` naming the cause.
