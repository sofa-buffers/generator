<p align="center"><img src="https://raw.githubusercontent.com/sofa-buffers/generator/main/assets/sofabuffers_logo.png" alt="SofaBuffers" height="140"></p>

# SofaBuffers

<b>Structured Objects For Anyone</b><br>
<i>... so optimized, feels amazing.</i>

[Would you like to know more?](https://github.com/sofa-buffers)

---

## SofaBuffers Code Generator — `sofabgen`

**`sofabgen`** — the SofaBuffers code generator. It compiles a message
definition (YAML/JSON) into typed encode/decode wrappers for your target
language. Installing this package puts the `sofabgen` command on your `PATH` —
no Go toolchain, no compiler, nothing to build.

## Install

As a throwaway tool run, without installing anything permanently:

```sh
uvx sofabgen --version
```

As a standalone CLI:

```sh
uv tool install sofabgen     # or: pipx install sofabgen
```

Or pinned into a project's development environment:

```sh
pip install sofabgen
```

## Usage

Generate code from a message definition:

```sh
sofabgen --lang python --in messages/example.yaml --out src/generated/
```

Key flags:

- `--lang <target>` — one of `c`, `cpp`, `csharp`, `dart`, `docs`, `go`, `java`,
  `kotlin`, `python`, `rust`, `typescript`, `zig`.
- `--in <path>` — a message-definition file, or a directory of them.
- `--out <dir>` — where the generated code is written.
- `--config <file>` — the YAML/JSON config carrying all options.
- `--version` prints the version; `--help` lists every flag.

## Runtime dependency: the corelib

`sofabgen` is a **build-time** tool — it emits typed code but never touches wire
bytes itself. The generated code calls into a small per-language **runtime
library** ("corelib") that owns the wire format. So alongside the generator, your
project needs the corelib for your target language at runtime.

For the Python target that is **[`corelib-py`](https://github.com/sofa-buffers/corelib-py)**;
generated modules import from it:

```python
from sofab import Encoder, Decoder
```

It is not published on PyPI yet — install it from the repository until it is. For
any other target language, install that language's corelib in its own ecosystem
(npm, Cargo, Go modules, NuGet, Maven, …).

## How the binary is delivered

`sofabgen` is a single static Go binary. This project publishes one **wheel per
platform**, each containing nothing but that binary; pip picks the one matching
your host and unpacks it straight onto your `PATH`. There is no Python code in
the package, no import package, and no build step — `pip install sofabgen`
downloads exactly one binary and is done.

Supported hosts: Linux (x86-64, x86-32, ARM64, ARMv7 — glibc and musl alike),
macOS (Intel and Apple Silicon), Windows (x86-64, x86-32, ARM64). The binaries
are the very ones attached to the matching
[GitHub release](https://github.com/sofa-buffers/generator/releases), published
from that release by a
[Trusted Publisher](https://docs.pypi.org/trusted-publishers/) with
[attestations](https://docs.pypi.org/attestations/) — so the file you install is
verifiably the one CI built from the tagged source.

> Only wheels are published, deliberately: with a source distribution present,
> pip on an unsupported platform would try to build from source and fail
> obscurely instead of telling you there is no matching distribution.

## Links

- **Source & documentation:** https://github.com/sofa-buffers/generator
- **Runtime library (Python):** https://github.com/sofa-buffers/corelib-py

MIT licensed.
