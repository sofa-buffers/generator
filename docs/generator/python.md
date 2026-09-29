# Python target — `targets.python`

Emits `message.py`: a `@dataclass` per message and per named struct/union, plus
the enums and bitfield constants they use. The generated code calls into
`sofa-buffers-corelib` (import package `sofab`).

## Options

This target has none of its own. The generic options — `emit`, `license`,
`max_message_size`, the `max_dyn_*` decode limits, and `format` (whether
`sofabgen` runs `ruff format` over what it emitted, see
[Formatting](#formatting)) — are documented in the
[generic config](README.md) and apply here unchanged.

## The reassembly buffer

`sofab.Decoder` joins a construct split across two fed chunks in a buffer the
caller supplies, sized once and never grown, and it holds no size of its own —
the size decides which well-formed messages a receiver can stream, so it is the
receiver's number. The generated module states it, derived from the schema and
the `max_dyn_*` limits:

```python
MAX_FIELD_SPAN = 4194309          # the largest single value this schema carries
REASSEMBLY = MAX_FIELD_SPAN + 65536
```

`MAX_FIELD_SPAN` is one *construct*: a `string` or `blob` payload, or one native
array's whole element run, with every varint charged its widest form and each
`max_dyn_*` limit standing in for a missing schema bound. A nested message is not
one construct — its fields are read one at a time — and neither is a wrapper
array of strings, blobs or structs, whose elements arrive one per sequence child.
Raising a `max_dyn_*` limit raises this number with it.

A field the receiver *skips* — an unknown id, or one whose wire tag contradicts
the schema — never enters the buffer at all, whatever its size, so the number
covers what a receiver reads and not what a sender might send.

`REASSEMBLY` adds room for one fed chunk, because the corelib holds the chunk it
was just handed alongside what it carried. 64 KiB is what `decoder()` assumes; a
caller streaming larger pieces passes its own:

```python
d = Telemetry.decoder(reassembly=MAX_FIELD_SPAN + (1 << 20))
```

The one-shot `Telemetry.decode(data)` needs neither: a message fed in a single
call never touches the buffer, so it is built with `MAX_FIELD_SPAN` — the most a
*truncated* message can leave behind.

## When a streamed message fills in

Part of a message is decoded through a *destination table*: the corelib writes
those fields straight into storage the module owns, without a Python callback per
field, and one pass moves them onto the dataclass. That pass runs when a decode
**completes**.

For `decode(data)` nothing is observable — it returns a finished message or
raises. For the streaming reader it means `.message` fills in from two directions:

```python
d = Telemetry.decoder()
d.feed(first)          # INCOMPLETE — fields the visitor handles are already there
d.feed(rest)           # COMPLETE   — the table's fields land now, all at once
d.message              # the whole message, either way
```

A message that never completes therefore shows only the part the visitor handled.
Nothing is lost: a decode short of COMPLETE has no finished message to report, and
both refusals still surface as `SofaDecodeError` / `SofaIncompleteError`.

Which fields go which way follows from the schema, not from a setting. Every
scalar rides the table — the narrow integers, `enum` and `bitfield` included,
whose declared width the table states and the decoder checks — as do `string`,
`blob`, native arrays with a declared `count` of at most 32, whole nested
structs, and unions whose options are all of those kinds, including struct and
union options made of them: the table records which option arrived last, and
only that option is read back. What stays on the visitor is an array the schema
leaves unbounded or declares longer than 32, an array of strings, blobs, structs,
unions or arrays, a union with an option of those kinds, a union with a struct
option whose string, blob or array member declares a non-empty default, and any
struct or union that contains one of these. A class with fewer than three
table-carried fields uses none at all; a class the table covers completely needs
no visitor behaviour at all.

The array limit is a cost, not a rule: an array on the table is written element by
element into slots and then built into the list your dataclass holds, so past a
few dozen elements the second pass costs more than the callback it saved — and the
slots are reserved whether the array arrives or not.

## Formatting

`sofabgen` can run `ruff format` over what it generated, and does so only when
you ask: it spawns no external tool on its own, so the same version writes the
same bytes on every machine, whatever happens to be installed. That matters more
here than elsewhere — `ruff` is not part of the Python toolchain, so it may
simply not be there.

Asking is one switch — the CLI flag `--format`, or the `generic.run_formatter`
config key (the flag wins):

| value | what `sofabgen` does |
|---|---|
| `off` (the default) | Never runs `ruff format`. The files are the generator's own output: valid, compilable, not canonically formatted. |
| `auto` | Runs `ruff format` when it is available; when it is not, writes the files unformatted and says so once on stderr. |
| `require` | Runs `ruff format`, and fails the run when it is not available. |

With the pass on, every generated `.py` module goes through `ruff format` once,
run in the output directory — so a `pyproject.toml` or `ruff.toml` of your own
that covers that directory is honoured, and the modules come out the way your own
`ruff format` over that tree would leave them. `ruff format --check` over a tree
holding them then passes, so generated modules need no exclusion from a
formatting gate. `ruff`'s output changes between releases, so a tree formatted by
one version and checked by another can still report a difference: use the same
version for both.

It is a convenience. The generated code is correct and compiles either way; the
switch only decides whether `ruff format` has already been over it when it reaches you,
and running `ruff format` over the output directory yourself gets you the
same tree.

Under `auto` and under `require` alike, a formatter that RUNS and rejects a
generated file fails the generation with that file named — that is a generator
bug, and writing the file would only move it into your build.


## Unions

A `union` holds exactly one of its options. It is a `@dataclass` of its own with
two fields — the id of the option held and that option's value — so an option
is only reached through the members below, and two options cannot be set side by
side:

```yaml
shape:
  id: 3
  type: union
  default_id: 2
  oneof:
    num:  { id: 0, type: u16, default: 5 }
    name: { id: 1, type: string, maxlen: 16 }
    pt:   { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 }, y: { id: 1, type: i32 } } }
    tags: { id: 3, type: array, items: { type: string, count: 4, maxlen: 8 } }
```

```python
@dataclass
class MShape:
    NUM_ID: ClassVar[int] = 0
    NAME_ID: ClassVar[int] = 1
    PT_ID: ClassVar[int] = 2
    TAGS_ID: ClassVar[int] = 3

    which: int                 # read-only property

    num: int                   # property with a setter
    def has_num(self) -> bool: ...

    name: str
    def has_name(self) -> bool: ...

    pt: MShapePt
    def has_pt(self) -> bool: ...
    def mutable_pt(self) -> MShapePt: ...

    tags: list[str]
    def has_tags(self) -> bool: ...
    def mutable_tags(self) -> list[str]: ...

    def clear(self) -> None: ...
```

| operation | Python |
|---|---|
| which option is held | `x.which` → the option's id |
| option ids | `MShape.PT_ID` (`<OPTION>_ID` constants) |
| test | `x.has_pt()` |
| read | `x.pt` |
| select with a value | `x.num = 7` |
| select at the default and edit in place | `x.mutable_pt().y = 2` |
| back to the default | `x.clear()` |

```python
m = M()                      # m.shape holds pt at its default: x=7, y=0
m.shape.num = 7              # now num = 7; pt is no longer held
m.shape.mutable_pt().y = 2   # pt again, from its default: x=7, y=2
if m.shape.has_pt():
    use(m.shape.pt.x)
if m.shape.which == MShape.NUM_ID:
    use(m.shape.num)
m.shape.clear()              # pt at its default again
```

Two unions compare equal (`==`) when they hold the same option with equal values.

**Reading** an option that is not held returns that option's default — a new
struct or union at its default, a new empty list for an array, `b""` for a blob,
the option's declared default for a number — and changes nothing; it does not
select the option, so changing what it returned is lost.

**Assigning** an option selects that option with the value assigned; the option
held before is no longer held. **`mutable_<option>()`** (struct, union and array
options) selects the option at its own default if another one is held, and
returns it. If the option is already held it is returned as it is, untouched. A
number, string or blob option is replaced by assigning it.

**Ownership.** Assigning an option keeps the object it is given, as assigning a
field does: after `x.pt = p`, `x.pt` returns that same `p` while `pt` is held.
Selecting a struct, union or array option that is not held — through
`mutable_<option>()`, by decoding into the union, or by `clear()` for the
`default_id` option — always starts from a new object at the option's default;
an object obtained earlier is never reset behind your back, it is merely no
longer held.

**Names.** The members are the option name: the property `<option>` (a Python
keyword takes a trailing underscore, as a field does), `has_<option>()`,
`mutable_<option>()` and the constant `<OPTION>_ID` (the name upper-cased). An
option whose name would land on one of the union's own members (`which`,
`clear`, `serialize`, `encode`, `decode`, `decoder`, `to_jsonable`,
`from_jsonable`, `MAX_SIZE`) or on a builtin the class body uses (`property`,
`classmethod`) gets a trailing underscore: an option `which` is the property
`which_`, with `has_which()` and `WHICH_ID`. A class has one
namespace, so two options that would produce the same member (`a` and `A_ID`
both give `A_ID`; `x` and `has_x` both give `has_x`) fail generation, naming
both.

**`$defs` unions** used with different `default_id`s are one class per
`default_id`, named after `<Name>_default_<option>`: `UnionShapeDefaultPt` and
`UnionShapeDefaultNum`.

**Defaults.** A new union holds the `default_id` option at that option's own
default; an omitted `default_id` means the option with the lowest id. Each
element of an array of unions starts the same way — including an element a
decoded array skips.

**Encode.** Only the held option is written. If it is the `default_id` option
at its default, the union is at its default and is left out. Any other held
option is written **even at its own default** — `0`, an empty string, an empty
blob or array, or an empty frame for a struct option, a union option, or an
array whose elements are strings, blobs, structs, unions or arrays — because
the receiver's new union holds `default_id`, and leaving it out would read
back as that.

**Decode.** The option received last wins. A different option replaces the
held one and starts from its own default; the held option received again
continues where it was (a struct or union option merges, anything else is
replaced). A field whose wire type does not match its option, and an unknown
id, change nothing. A union member is decoded through the destination table (see
[When a streamed message fills in](#when-a-streamed-message-fills-in)) when the
union and every struct between it and the message hold only fields the table
carries — for the union, every option and everything inside a struct or union
option — so a streamed one shows its option when the message completes. A union
the table cannot carry, a union that is an array element, and a union type
decoded on its own (`MyUnion.decode(...)`) are decoded field by field and show
their option as soon as it arrives. Both give the same result.

**JSON.** `to_jsonable()` returns a dict with exactly one member, the held
option — `{"pt": {"x": 7, "y": 2}}`, also when that is the `default_id` option at
its default — and `from_jsonable()` selects the option it finds. A union member
left out of a message's JSON reads as the union's default.
