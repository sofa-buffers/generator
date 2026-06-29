# @sofa-buffers/sofabgen (npm distribution — EXPERIMENTAL)

> ⚠️ **Experimental scaffold, on a branch.** Not published yet. This packages the
> `sofabgen` code generator (a Go binary) for npm so it can be used as a
> project-local dev dependency — no global install, no Go toolchain.

## Usage (the goal)

```sh
npm install --save-dev @sofa-buffers/sofabgen
# then, from the project:
npx sofabgen --lang cpp --in messages/example.yaml --out generated/
```

Or wire it into `package.json` scripts so codegen is reproducible per project:

```json
{
  "scripts": {
    "gen": "sofabgen --lang ts --in messages/ --out src/generated/"
  },
  "devDependencies": {
    "@sofa-buffers/sofabgen": "0.2.0"
  }
}
```

## How it works

`sofabgen` is a single static Go binary. The CI release workflow already builds
one per platform/arch (`sofabgen-<os>-<arch>` + a `.sha256`) and attaches them to
each GitHub release. This npm package ships **no binary of its own** — instead:

1. On `postinstall`, `scripts/install.js` maps the host
   (`process.platform`/`process.arch`) to the matching release asset, downloads
   it from `…/releases/download/v<version>/sofabgen-<os>-<arch>`, verifies its
   SHA-256, and writes it to `bin/`.
2. `bin/sofabgen.js` (the package's `bin`) `exec`s that binary, forwarding args,
   stdio, and exit code.
3. If the install ran with `--ignore-scripts`, the launcher downloads the binary
   lazily on first run, so it still works.

No runtime dependencies; only Node built-ins (`https`, `crypto`, `fs`).

## Open questions / decisions before publishing

- **Package name & scope** — `@sofa-buffers/sofabgen` is a placeholder; confirm
  the npm org/scope and availability.
- **Version ↔ release coupling** — the package `version` must correspond to a
  published GitHub release tag `v<version>` (the download URL derives from it).
  Publishing the npm package therefore has to be tied to the release workflow.
- **Alternative packaging** — instead of a `postinstall` download, the more
  hermetic pattern (used by esbuild/swc) is **per-platform optional-dependency
  packages** (`@sofa-buffers/sofabgen-linux-x64`, …) selected via `os`/`cpu`, so
  there's no install-time network or script. More packages to publish, but no
  postinstall and works offline from a cache. This scaffold uses the simpler
  download approach first; switching is a follow-up if desired.
- **Private/air-gapped installs** — a download-on-install package needs network
  access to GitHub releases; the optional-deps approach avoids that.
