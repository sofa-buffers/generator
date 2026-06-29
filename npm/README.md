# @sofa-buffers/generator (npm distribution — EXPERIMENTAL)

> ⚠️ **Experimental scaffold, on a branch.** Not published yet. This packages the
> `sofabgen` code generator (a Go binary) for npm so it can be used as a
> project-local dev dependency — no global install, no Go toolchain.

> **Package name vs. command:** the package is **`@sofa-buffers/generator`** (it
> matches the GitHub repo), but the CLI command it installs is **`sofabgen`** —
> the same package-name ≠ command split as `@angular/cli`→`ng` or `typescript`→
> `tsc`. Install the package; run `sofabgen`.

## Usage (the goal)

```sh
npm install --save-dev @sofa-buffers/generator
# then, from the project (the command is `sofabgen`):
npx sofabgen --lang cpp --in messages/example.yaml --out generated/
```

Or wire it into `package.json` scripts so codegen is reproducible per project:

```json
{
  "scripts": {
    "gen": "sofabgen --lang ts --in messages/ --out src/generated/"
  },
  "devDependencies": {
    "@sofa-buffers/generator": "0.2.0"
  }
}
```

## How it works — per-platform optional dependencies

`sofabgen` is a single static Go binary. The release workflow builds one per
platform/arch (`sofabgen-<os>-<arch>` + a `.sha256`) and attaches them to each
GitHub release. The npm distribution uses the **optional-dependency** pattern
(as esbuild / swc / Biome / turbo do):

- **`@sofa-buffers/generator`** (this package) is tiny: a `bin/sofabgen.js`
  launcher and an `optionalDependencies` entry for every platform package. It
  carries **no binary** and runs **no install script**.
- **`@sofa-buffers/generator-<platform>-<arch>`** — one package per target
  (`linux-x64`, `linux-ia32`, `linux-arm64`, `linux-arm`, `darwin-x64`,
  `darwin-arm64`, `win32-x64`, `win32-ia32`, `win32-arm64`). Each declares
  matching `"os"`/`"cpu"` and ships exactly one binary at `bin/sofabgen[.exe]`.

On install, npm reads the `os`/`cpu` of each optional dependency and installs
**only the one** that matches the host (silently skipping the rest). At runtime
`bin/sofabgen.js` resolves that package
(`require.resolve("@sofa-buffers/generator-<platform>-<arch>/package.json")`) and
execs its binary, forwarding args/stdio/exit code. No download, no `postinstall`,
no runtime dependencies.

Why this over a `postinstall` download: it works **offline / air-gapped / in CI /
with `--ignore-scripts`**, the binary is **integrity-hashed in the lockfile** and
**reproducible** with `npm ci`, and it is **cached by corporate npm proxies**
(Artifactory/Verdaccio) instead of re-hitting GitHub.

> **`--omit=optional` caveat:** installing with `--no-optional` / `--omit=optional`
> skips the binary; the launcher then prints a clear error. (This is the one
> tradeoff of the optional-deps model.)

## Building & publishing the packages

The platform packages are generated from the release binaries — never committed
(`npm/packages/` is git-ignored):

```sh
cd npm
# download each binary from the v<version> release, verify its sha256, and write
# the 9 platform packages into npm/packages/  (or: --from <dir> to copy locally)
node scripts/build-platform-packages.js
# publish platform packages FIRST, then the main package (so its optional deps resolve)
for d in packages/*/; do npm publish "$d" --access public; done
npm publish . --access public
```

This must run with the package `version` already bumped to match a published
GitHub release tag `v<version>` (the binaries are downloaded from it). The
natural home is a step in `.github/workflows/release.yml`, gated on the `v*` tag,
using an npm token with publish rights to the `@sofa-buffers` scope.

## Open questions / decisions before publishing

- **Version ↔ release coupling** — `version` in `package.json` (and in every
  `optionalDependencies` entry) must equal the GitHub release tag the binaries
  come from. A release step should bump all of them together.
- **Publish automation** — wire `scripts/build-platform-packages.js` + the
  publish loop into the release workflow with an `NPM_TOKEN` secret; publish
  platform packages before the main package.
- **Smoke test** — after publishing, `npm i -D @sofa-buffers/generator` on each
  OS in CI and run `sofabgen --version` to catch a missing/mis-tagged platform
  package.
