# @sofa-buffers/generator

**`sofabgen`** — the SofaBuffers code generator. It compiles a message
definition (YAML/JSON) into typed encode/decode wrappers for your target
language. Install it as a project-local dev dependency — no global install and no
Go toolchain required.

> **Package name vs. command:** the package is **`@sofa-buffers/generator`**, but
> the CLI command it installs is **`sofabgen`** — the same split as
> `@angular/cli` → `ng` or `typescript` → `tsc`. Install the package; run
> `sofabgen`.

## Install

```sh
npm install --save-dev @sofa-buffers/generator
```

## Usage

Generate code from a message definition:

```sh
npx sofabgen --lang typescript --in messages/example.yaml --out src/generated/
```

Or wire it into `package.json` scripts so codegen is reproducible per project:

```json
{
  "scripts": {
    "gen": "sofabgen --lang typescript --in messages/ --out src/generated/"
  },
  "devDependencies": {
    "@sofa-buffers/generator": "^0.19.7"
  }
}
```

Key flags:

- `--lang <target>` — one of `c`, `cpp`, `csharp`, `docs`, `go`, `java`,
  `python`, `rust`, `typescript`, `zig`.
- `--in <path>` — a message-definition file, or a directory of them.
- `--out <dir>` — where the generated code is written.
- `--version` prints the version; `--help` lists every flag.

## Runtime dependency: `@sofa-buffers/corelib`

`sofabgen` is a **build-time** tool — it emits typed code but never touches wire
bytes itself. The generated code calls into a small per-language **runtime
library** ("corelib") that owns the wire format. So, alongside the generator as a
dev dependency, your project needs the corelib for your target language as a
**runtime dependency**.

For the TypeScript / JavaScript target that is **[`@sofa-buffers/corelib`](https://www.npmjs.com/package/@sofa-buffers/corelib)** —
the generated code imports from it:

```sh
npm install @sofa-buffers/corelib
```

```json
{
  "dependencies": {
    "@sofa-buffers/corelib": "^0.8.1"
  },
  "devDependencies": {
    "@sofa-buffers/generator": "^0.19.7"
  }
}
```

Generating in **project mode** emits a ready-to-build package that already lists
`@sofa-buffers/corelib` for you. For any other target language, install that
language's corelib in its own ecosystem (Cargo, Go modules, NuGet, Maven, …).

## How the binary is delivered

`sofabgen` is a single static Go binary. This package is a tiny launcher; the
binary ships in a per-platform **optional dependency**
(`@sofa-buffers/generator-<os>-<arch>`, the esbuild / swc model). On install, npm
reads each optional dependency's `os`/`cpu` and installs **only the one** that
matches your host; the launcher then execs it.

No download, no `postinstall`, no extra runtime dependency for the generator
itself — it works offline, in CI, and with `--ignore-scripts`; the binary is
integrity-hashed in your lockfile and reproducible with `npm ci`, and it is
cached by corporate npm proxies (Artifactory / Verdaccio) rather than re-hitting
GitHub.

> **`--omit=optional` caveat:** installing with `--no-optional` /
> `--omit=optional` skips the binary, and the launcher then prints a clear error.
> This is the one tradeoff of the optional-dependency model.

## Links

- **Source & documentation:** https://github.com/sofa-buffers/generator
- **Runtime library (TypeScript):** https://www.npmjs.com/package/@sofa-buffers/corelib

MIT licensed.
