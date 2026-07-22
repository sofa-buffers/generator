#!/usr/bin/env node
// Build the per-platform optional-dependency packages for @sofa-buffers/generator
// from the release binaries, into npm/packages/. Run at publish time (e.g. in the
// release workflow), then `npm publish` each generated package + the main one.
//
// Usage:
//   node scripts/build-platform-packages.js              # download binaries from the v<version> release
//   node scripts/build-platform-packages.js --from DIR   # copy binaries from a local dir (named like the release assets)
//   node scripts/build-platform-packages.js --only linux-x64   # build a single target (testing)
//
// Each generated package @sofa-buffers/generator-<node-platform>-<node-arch>:
//   - declares "os"/"cpu" so npm installs only the matching one;
//   - ships exactly one binary at bin/sofabgen[.exe];
//   - has NO "bin" field (the binary is exec'd by the main package's launcher);
//   - carries a short README.md pointing at the main package (for its npmjs page).

"use strict";

const fs = require("fs");
const os = require("os");
const path = require("path");
const https = require("https");
const crypto = require("crypto");

const ROOT = path.join(__dirname, "..");
const ROOT_PKG = path.join(ROOT, "package.json");
// Version the packages carry. The committed package.json version is a
// placeholder (0.0.0-dev) — the release tag is the single source of truth.
// --version <x> (or $SOFABGEN_NPM_VERSION) supplies the real version and
// rewrites the root package.json so its version + every optionalDependencies
// pin stay in lockstep with the tag the release workflow publishes from. Without
// an override, main() refuses to build (the placeholder is not releasable).
let VERSION = require(ROOT_PKG).version;
const REPO = "sofa-buffers/generator";
const OUT = path.join(ROOT, "packages");

// node platform / node arch  ->  release asset (goos-goarch) + ext
const TARGETS = [
  { platform: "linux", arch: "x64", goos: "linux", goarch: "amd64" },
  { platform: "linux", arch: "ia32", goos: "linux", goarch: "386" },
  { platform: "linux", arch: "arm64", goos: "linux", goarch: "arm64" },
  { platform: "linux", arch: "arm", goos: "linux", goarch: "arm" },
  { platform: "darwin", arch: "x64", goos: "darwin", goarch: "amd64" },
  { platform: "darwin", arch: "arm64", goos: "darwin", goarch: "arm64" },
  { platform: "win32", arch: "x64", goos: "windows", goarch: "amd64", ext: ".exe" },
  { platform: "win32", arch: "ia32", goos: "windows", goarch: "386", ext: ".exe" },
  { platform: "win32", arch: "arm64", goos: "windows", goarch: "arm64", ext: ".exe" },
];

function parseArgs(argv) {
  const a = { from: null, only: null, version: null };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === "--from") a.from = argv[++i];
    else if (argv[i] === "--only") a.only = argv[++i];
    else if (argv[i] === "--version") a.version = argv[++i];
  }
  return a;
}

// Pin the root package.json (main package) to `version`: its own version and
// every optionalDependencies entry, regenerated from TARGETS so the set can
// never drift from what we actually build.
function rewriteRootPackage(version) {
  const pkg = JSON.parse(fs.readFileSync(ROOT_PKG, "utf8"));
  pkg.version = version;
  const deps = {};
  for (const t of TARGETS) {
    deps[`@sofa-buffers/generator-${t.platform}-${t.arch}`] = version;
  }
  pkg.optionalDependencies = deps;
  fs.writeFileSync(ROOT_PKG, JSON.stringify(pkg, null, 2) + "\n");
}

function fetch(url) {
  return new Promise((resolve, reject) => {
    https
      .get(url, { headers: { "User-Agent": "sofabgen-pkg-builder" } }, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          res.resume();
          return resolve(fetch(res.headers.location));
        }
        if (res.statusCode !== 200) {
          res.resume();
          return reject(new Error(`HTTP ${res.statusCode} for ${url}`));
        }
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => resolve(Buffer.concat(chunks)));
      })
      .on("error", reject);
  });
}

// Return the binary bytes for a target, from --from dir or the GitHub release.
async function getBinary(t, fromDir) {
  const asset = `sofabgen-${t.goos}-${t.goarch}${t.ext || ""}`;
  if (fromDir) {
    return fs.readFileSync(path.join(fromDir, asset));
  }
  const base = `https://github.com/${REPO}/releases/download/v${VERSION}`;
  const [bin, sha] = await Promise.all([
    fetch(`${base}/${asset}`),
    fetch(`${base}/${asset}.sha256`),
  ]);
  const want = sha.toString("utf8").trim().split(/\s+/)[0];
  const got = crypto.createHash("sha256").update(bin).digest("hex");
  if (want && got !== want) {
    throw new Error(`checksum mismatch for ${asset} (want ${want}, got ${got})`);
  }
  return bin;
}

function platformPackageJson(t) {
  return {
    name: `@sofa-buffers/generator-${t.platform}-${t.arch}`,
    version: VERSION,
    description: `sofabgen prebuilt binary for ${t.platform}/${t.arch} (the @sofa-buffers/generator code generator).`,
    license: "MIT",
    homepage: "https://github.com/sofa-buffers/generator",
    repository: {
      type: "git",
      url: "git+https://github.com/sofa-buffers/generator.git",
      directory: "npm",
    },
    // Scoped packages default to `restricted`, which requires a paid npm plan
    // and fails any publish (incl. the hand-bootstrapped first version) with
    // E402. Pin public access into the manifest so every publish path is public.
    publishConfig: { access: "public" },
    // npm installs this package ONLY on a matching host.
    os: [t.platform],
    cpu: [t.arch],
    // Keep the binary on disk (don't zip) under Yarn PnP.
    preferUnplugged: true,
    files: [`bin/sofabgen${t.ext || ""}`, "README.md"],
  };
}

// A short README shown on the package's npmjs.com page. These per-platform
// packages otherwise render with no README (only the main package carries one);
// this points readers at the main package they should actually install.
function platformReadme(t) {
  const name = `@sofa-buffers/generator-${t.platform}-${t.arch}`;
  return `# ${name}

Prebuilt \`sofabgen\` binary for **${t.platform}/${t.arch}** — a platform-specific
optional dependency of
[**@sofa-buffers/generator**](https://www.npmjs.com/package/@sofa-buffers/generator),
the SofaBuffers code generator.

## Do not install this package directly

Install the main package instead:

\`\`\`sh
npm install --save-dev @sofa-buffers/generator
\`\`\`

npm reads each optional dependency's \`os\`/\`cpu\` and installs **only the one**
matching your host (silently skipping the rest); the launcher in
\`@sofa-buffers/generator\` then execs the binary this package ships. No download,
no install script — the binary is lockfile-hashed and reproducible with
\`npm ci\`.

- **Main package (install this):** https://www.npmjs.com/package/@sofa-buffers/generator
- **Source & documentation:** https://github.com/sofa-buffers/generator

Released in lockstep with \`@sofa-buffers/generator\`; the version is injected from
the release tag at publish time. MIT licensed.
`;
}

async function buildOne(t, fromDir) {
  const dir = path.join(OUT, `generator-${t.platform}-${t.arch}`);
  const binDir = path.join(dir, "bin");
  fs.mkdirSync(binDir, { recursive: true });
  fs.writeFileSync(
    path.join(dir, "package.json"),
    JSON.stringify(platformPackageJson(t), null, 2) + "\n"
  );
  fs.writeFileSync(path.join(dir, "README.md"), platformReadme(t));
  const bin = await getBinary(t, fromDir);
  const dest = path.join(binDir, `sofabgen${t.ext || ""}`);
  fs.writeFileSync(dest, bin, { mode: t.ext ? 0o644 : 0o755 });
  return { name: `@sofa-buffers/generator-${t.platform}-${t.arch}`, dir, bytes: bin.length };
}

async function main() {
  const args = parseArgs(process.argv.slice(2));

  // Version override (release workflow passes the tag). Rewrites the root package
  // so version + optionalDependencies match what we build and download.
  const override = args.version || process.env.SOFABGEN_NPM_VERSION;
  if (override) {
    VERSION = String(override).replace(/^v/, "");
    rewriteRootPackage(VERSION);
    console.log(`pinned @sofa-buffers/generator + platform deps to ${VERSION}`);
  }

  // The committed package.json version is a placeholder (0.0.0-dev): the release
  // tag is the single source of truth. Refuse to build/download against the
  // placeholder — a real build must pass --version <tag> (or $SOFABGEN_NPM_VERSION).
  if (/^0\.0\.0-dev$/.test(VERSION)) {
    throw new Error(
      "no release version given: pass --version <x> (or set $SOFABGEN_NPM_VERSION); " +
        "the committed package.json version is a placeholder, not a release version"
    );
  }

  let targets = TARGETS;
  if (args.only) {
    targets = TARGETS.filter((t) => `${t.platform}-${t.arch}` === args.only);
    if (!targets.length) {
      console.error(`unknown --only ${args.only}`);
      process.exit(1);
    }
  }
  fs.mkdirSync(OUT, { recursive: true });
  const built = [];
  for (const t of targets) {
    process.stdout.write(`building generator-${t.platform}-${t.arch} … `);
    const r = await buildOne(t, args.from);
    built.push(r);
    console.log(`ok (${(r.bytes / 1e6).toFixed(1)} MB)`);
  }
  console.log(`\n${built.length} platform package(s) under ${path.relative(process.cwd(), OUT)}/`);
  console.log("publish order (platform packages first, then the main package):");
  for (const b of built) console.log(`  npm publish ${path.relative(process.cwd(), b.dir)} --access public`);
  console.log(`  npm publish ${path.relative(process.cwd(), ROOT)} --access public`);
}

main().catch((err) => {
  console.error(`build-platform-packages: ${err.message}`);
  process.exit(1);
});
