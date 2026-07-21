# @sofa-buffers/generator (npm distribution)

> This packages the `sofabgen` code generator (a Go binary) for npm so it can be
> used as a project-local dev dependency — no global install, no Go toolchain.
> Publishing is automated in the release workflow via **npm trusted publishing
> (OIDC, no token)** and smoke-tested on Linux/macOS/Windows
> (`.github/workflows/npm.yml`). Each package's first-ever version is bootstrapped
> once by hand (OIDC cannot create a package); every release after that is
> automatic.

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
    "@sofa-buffers/generator": "0.19.4"
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
(`npm/packages/` is git-ignored). This is **automated** by the `npm-publish` job
in `.github/workflows/release.yml`: on a `v*` tag it builds the packages from the
just-released binaries (`--from ../dist --version <tag>`) and publishes them via
[npm trusted publishing (OIDC)](https://docs.npmjs.com/trusted-publishers/) — no
token — with automatic [provenance](https://docs.npmjs.com/generating-provenance-statements),
platform packages first so the main package's optional deps resolve.

To build/publish by hand (e.g. the one-time bootstrap, or a backfill):

```sh
cd npm
# download each binary from the v<version> release, verify its sha256, and write
# the 9 platform packages into npm/packages/  (or: --from <dir> to copy locally).
# --version is REQUIRED: the committed version is a placeholder (see below).
node scripts/build-platform-packages.js --version v<version>
# publish platform packages FIRST, then the main package (so its optional deps resolve)
for d in packages/*/; do npm publish "$d" --access public; done
npm publish . --access public
```

`--version <x>` supplies the real version and rewrites the root `package.json`
(version + every `optionalDependencies` pin) in lockstep, so the whole set always
matches the tag the binaries come from. Without it the script refuses to run.

## Version ↔ release coupling

**The release tag `v<version>` is the single source of truth for the version.** The
committed `version` here is a placeholder (`0.0.0-dev`) and is never published as-is:
the `npm-publish` job injects the tag at publish time (`build-platform-packages.js
--version <tag>`), rewriting the version and every `optionalDependencies` pin, so the
published package version always equals the tag the binaries came from. A guard in
the workflow asserts this equality across the main package and all platform packages
before publishing.

So there is **nothing to bump here** for a release — just push the `v<version>` tag.
The `npm` smoke workflow doesn't read this placeholder either; it resolves the latest
published release and exercises the production path against that.

## Publishing prerequisites

- **`NPM_TOKEN`** — a repository secret with publish rights to the `@sofa-buffers`
  scope. Without it the release job builds the packages and skips publishing (a
  CI warning, not a failure), so npm is opt-in per repository.
- **Provenance** — the job requests `id-token: write` so npm attaches a signed
  build-provenance statement; no extra setup needed on the origin repo.
