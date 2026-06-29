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

> **Status: implemented — all 8 language backends complete and CI-green.** The
> generator (`sbufgen`) emits typed code for **C, Go, Python, TypeScript, C++,
> Rust, C#, and Java**; each is built against its real corelib in CI, JSON
> round-trips every field kind, and is byte-exact against the shared wire
> vectors. The full design lives in [`docs/PLAN.md`](docs/PLAN.md) and the living
> architecture in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## Quick start

```sh
# Build the single static binary (or grab one from a release).
go build -o sbufgen ./cmd/sbufgen

# Generate typed sources for one language from a definition.
./sbufgen --lang go --in examples/example.yaml --out out/go

# Generate for every language.
for lang in c cpp go python typescript rust csharp java; do
  ./sbufgen --lang "$lang" --in examples/example.yaml --out "out/$lang"
done

# Scaffold a full buildable project + encode/decode harness (per PLAN §7):
#   sbufgen --config myconfig.yaml --lang rust --in examples --out out
```

Examples:
- [`examples/example.yaml`](examples/example.yaml) — a showcase exercising every
  field kind.
- [`examples/realworld/`](examples/realworld/) — a realistic connected-vehicle
  telemetry schema split across **multiple files** with cross-file `$ref`.

Generated sources for all 8 languages live on the
[`example-output`](https://github.com/sofa-buffers/generator/tree/example-output/output)
branch. Each backend's one-command conformance harness is `tests/<lang>/run.sh`.

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
