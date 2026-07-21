#!/usr/bin/env node
// Launcher for @sofa-buffers/generator. The real binary ships in a per-platform
// optional-dependency package (@sofa-buffers/generator-<platform>-<arch>); npm
// installs only the one matching the host (via the platform package's os/cpu
// fields). This shim resolves that package and execs its binary — no download,
// no postinstall.

"use strict";

const { spawnSync } = require("child_process");
const path = require("path");

const platform = process.platform; // 'linux' | 'darwin' | 'win32' | ...
const arch = process.arch; // 'x64' | 'arm64' | 'ia32' | 'arm' | ...
const pkg = `@sofa-buffers/generator-${platform}-${arch}`;
const binName = platform === "win32" ? "sofabgen.exe" : "sofabgen";

function resolveBinary() {
  // Resolve the platform package's manifest (always resolvable when installed),
  // then build the binary path inside it. Avoids relying on require.resolve of
  // an extensionless executable.
  const manifest = require.resolve(`${pkg}/package.json`);
  return path.join(path.dirname(manifest), "bin", binName);
}

let binPath;
try {
  binPath = resolveBinary();
} catch (e) {
  console.error(
    `@sofa-buffers/generator: no prebuilt sofabgen binary for ${platform}/${arch}.\n` +
      `\n` +
      `Expected the optional dependency '${pkg}' to be installed.\n` +
      `Supported platforms: linux (x64/ia32/arm64/arm), darwin (x64/arm64), win32 (x64/ia32/arm64).\n` +
      `\n` +
      `If you installed with --no-optional / --omit=optional / --ignore-scripts, or your\n` +
      `lockfile omits this platform's package, reinstall allowing optional dependencies.`
  );
  process.exit(1);
}

const res = spawnSync(binPath, process.argv.slice(2), { stdio: "inherit" });
if (res.error) {
  console.error(
    `@sofa-buffers/generator: failed to run ${binPath}: ${res.error.message}`
  );
  process.exit(1);
}
process.exit(res.status === null ? 1 : res.status);
