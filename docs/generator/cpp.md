# C++ target — `targets.cpp`

Language-specific options for the C++ backend. For shared options (`emit`,
`file_layout`, `buffer`, `omit_defaults`, …) see the [`generic`](README.md)
config.

## Honored options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `corelib` | `cpp` \| `c-cpp` | `cpp` | Which C++ corelib the generated code targets (see below). |
| `namespace` | string | `sofabuffers` | C++ namespace wrapping the generated types. Also settable in `generic`. |

### `corelib`

Both corelibs expose the same `sofab::` interface and produce **byte-identical
wire output**; they differ only in the decode of variable-length fields.

- **`cpp`** (default) — the pure-C++20, header-only [`corelib-cpp`]. `read()`
  resizes string/blob targets for you. Build with
  `make SOFAB_CPP_DIR=/path/to/corelib-cpp SOFAB_C_DIR=/path/to/corelib-c-cpp`.
- **`c-cpp`** — the C++ wrapper over the C library in [`corelib-c-cpp`]. The
  wrapper binds a decode target by address and fills it after the field
  callback, so the generated decode pre-sizes strings/blobs and reads
  blobs/sequences via the wrapper's native overloads. The generated `Makefile`
  compiles and links the corelib's C sources, so only
  `make SOFAB_C_DIR=/path/to/corelib-c-cpp` is needed.

```yaml
targets:
  cpp:
    namespace: myproj
    corelib: c-cpp     # default: cpp
```

[`corelib-cpp`]: https://github.com/sofa-buffers/corelib-cpp
[`corelib-c-cpp`]: https://github.com/sofa-buffers/corelib-c-cpp

## Reserved options

Accepted by the schema validator but not yet honored by the generator:

`cpp_standard` · `header_only` · `max_size_strategy` · `zero_copy_strings`
