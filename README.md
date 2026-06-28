<p align="center"><img src="assets/sofabuffers_logo.png" alt="SofaBuffers" height="140"></p>

# SofaBuffers

<b>Structured Objects For Anyone</b><br>
<i>... so optimized, feels amazing.</i>

[Would you like to know more?](https://github.com/sofa-buffers)

---

## SofaBuffers Code Generator

This repository holds the **SofaBuffers code generator** — the tool that turns a
declarative **YAML/JSON object definition** (validated against
[`schema/sofabuffers-schema-v1.json`](schema/sofabuffers-schema-v1.json)) into
**idiomatic, typed source code** for every supported language. The generated code
is a thin, zero-overhead wrapper that calls into the highly-optimized **corelib**
runtime for its language, so the hard part — a fast, portable, footprint-tuned
wire codec with a uniform streaming API — is owned by the corelibs, not the
generated code.

> **Status: design / planning.** The from-scratch generator described here is
> being specified before implementation. The full design lives in
> [`docs/PLAN.md`](docs/PLAN.md). Verified proof-of-concept artifacts (hand-written
> code mirroring the generated shape, plus the capability guards) exist in the
> `generator-old` repository's `test/` tree. This repo supersedes that TypeScript
> POC. The living architecture document, [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md),
> is created at milestone **M0**.

### What it does

1. Reads an **object/message definition** in YAML (JSON also accepted).
2. **Validates it against the JSON Schema first** — a hard, non-optional gate.
   Invalid input produces a clear, located error, a non-zero exit, and **no
   output**. Invalid definitions are never code-generated.
3. Resolves `$ref` into a shared-type graph and lowers the definition into a
   language-neutral **Intermediate Representation (IR)**.
4. Emits **one typed `serialize`/`deserialize` type per object** for the selected
   target language, tuned to that corelib's profile (minimal-footprint vs.
   maximum-throughput).
5. Ships as a **single, statically-linked, cross-platform binary** (`sbufgen`).

### Supported targets

The generator emits code against one corelib per language:

| Target | Corelib | Profile |
|---|---|---|
| C (`object.h`) | `corelib-c-cpp` | Embedded, minimal footprint, no heap |
| C++ (embedded) | `corelib-c-cpp` (`sofab.hpp`) | Embedded |
| C++ (max speed) | `corelib-cpp` | Header-only C++20, zero-copy |
| Rust (`no_std`) | `corelib-rs-no-std` | Embedded, no `alloc` by default |
| Go | `corelib-go` | High throughput |
| Python | `corelib-py` | High throughput |
| Java | `corelib-java` | High throughput |
| C# / .NET | `corelib-cs` | High throughput |
| TypeScript | `corelib-ts` | High throughput |

Because every corelib speaks the **same wire format**, code generated for one
language interoperates with code generated for any other for free.

### CLI (planned)

The CLI is deliberately tiny — everything configurable lives in a config file:

```sh
sbufgen --config <file> --lang <c|cpp-embedded|cpp|rust|go|python|java|csharp|ts> \
        [--in <dir>] [--out <dir>]
```

| Argument | Required | Purpose |
|---|---|---|
| `--config <file>` | yes | YAML/JSON config carrying all other options |
| `--lang <target>` | yes | Which backend to generate |
| `--in <dir>` | no | Override the config's input definition folder |
| `--out <dir>` | no | Override the config's output folder |

## Repository layout

```
.
├── schema/
│   └── sofabuffers-schema-v1.json   # the message-definition JSON Schema (draft-07)
├── docs/
│   ├── PLAN.md                      # full implementation plan & design
│   └── ARCHITECTURE.md              # living architecture doc (created at M0)
├── assets/                          # logo & icon
├── .devcontainer/                   # Go toolchain dev container
└── LICENSE                          # MIT
```

The definition schema (`schema/sofabuffers-schema-v1.json`) is authoritative for
the input format: field types (`u8`–`u64`, `i8`–`i64`, `fp32`/`fp64`, `boolean`,
`string`, `blob`, `array`, `enum`, `bitfield`, `struct`, `union`), unique field
ids, `$ref`-able `$defs`, and the custom keywords `uniqueIds` and
`defaultMatchesEnum`.

## Development

A `.devcontainer` (Ubuntu 24.04 + Go) is provided. To use it locally, copy the
secrets template first (the real `.env` is gitignored):

```sh
cp .devcontainer/.env.example .devcontainer/.env
```

Then open the folder in a devcontainer-aware editor, or build the image directly
via the scripts under `.devcontainer/`. The recommended generator implementation
language is **Go** (single static binary, frictionless `GOOS`/`GOARCH`
cross-compilation); see [`docs/PLAN.md` §2.1](docs/PLAN.md) for the rationale.

## Documentation

- **[`docs/PLAN.md`](docs/PLAN.md)** — the complete design: input format, corelib
  runtime contract, generated-code shape, optimization strategy, configuration,
  generator architecture (Composite / Visitor / Builder / Strategy), the
  testing & conformance matrix, and the phased roadmap (M0–M8).
- **[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)** — the maintained, up-to-date
  description of how the generator works (kept current per the per-milestone rule).

## License

[MIT](LICENSE) © 2026 SofaBuffers
