# Packaging

One subdirectory per package-manager channel. Every channel is a thin consumer of
the **same release assets** — the static, CGO-free `sofabgen` binaries that
`release.yml` cross-compiles and attaches to a `v*` GitHub release — so there is
one artifact set and one checksum to trust, whichever way a user installs.

| Directory | Channel | Install |
|---|---|---|
| [`npm/`](npm/) | npmjs.com, `@sofa-buffers/generator` | `npm i -D @sofa-buffers/generator` |
| [`pypi/`](pypi/) | PyPI, `sofabgen` | `uv tool install sofabgen` / `pip install sofabgen` |

Both work the same way in spirit — ship the prebuilt binary, resolve the host's
one automatically, never download anything at install time — but the mechanics
differ because the registries do:

- **npm** cannot put platform-specific payloads in one package, so the channel is
  a launcher package plus nine per-platform `optionalDependencies` (the
  esbuild/swc model) and a shim that execs the right binary.
- **PyPI** resolves platforms natively through wheel tags: one project name,
  thirteen wheels, and the binary is unpacked straight onto `PATH`. No shim, no
  Python code.

Not in here, on purpose: `install.sh`, `.github/actions/setup-sofabgen/` and
`go install`. They consume the release assets directly rather than republishing
them through a registry, and they live at the repository root.

Each channel's maintainer runbook — trusted-publishing setup, bootstrap, failure
modes — is the `PUBLISHING.md` in its directory.
