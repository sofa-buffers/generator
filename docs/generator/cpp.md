# C++ target — `targets.cpp`

Emits one class per message and named type. Two corelibs are available and the
choice decides the whole profile — throughput or footprint.

## Options

| key | type | default | effect |
|---|---|---|---|
| `corelib` | `cpp` \| `c-cpp` | `cpp` | Which C++ corelib the generated code targets. |
| `allow_dynamic` | boolean | depends on `corelib` | Storage for schema-bounded fields: dynamic containers or fixed-capacity inline ones. |
| `namespace` | string | `message` | The namespace wrapping the generated types. |
| `emit` | `sources` \| `project` | `sources` | `project` additionally scaffolds a CMake build and the JSON conformance harness. |
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size. See the [generic config](README.md). |
| `max_dyn_array_count` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |
| `max_dyn_string_len` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |
| `max_dyn_blob_len` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |

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
