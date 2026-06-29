// Resolve, download, and cache the prebuilt `sofabgen` binary for the current
// platform/arch. Used both as the `postinstall` step and lazily by the launcher
// (bin/sofabgen.js) so it still works when installed with `--ignore-scripts`.
//
// The binaries (and a `.sha256` for each) are published per release at:
//   https://github.com/sofa-buffers/generator/releases/download/v<version>/sofabgen-<goos>-<goarch>[.exe]
// so this package carries NO binary itself — it fetches exactly the one binary
// the host needs. Uses only Node built-ins (no dependencies).

"use strict";

const fs = require("fs");
const path = require("path");
const https = require("https");
const crypto = require("crypto");

const REPO = "sofa-buffers/generator";
const VERSION = require("../package.json").version;

// Node platform/arch -> Go release-asset os/arch.
const GOOS = { linux: "linux", darwin: "darwin", win32: "windows" };
const GOARCH = { x64: "amd64", arm64: "arm64", ia32: "386", arm: "arm" };

// Combinations the release actually ships (Go publishes no 32-bit/arm darwin).
function target() {
  const goos = GOOS[process.platform];
  const goarch = GOARCH[process.arch];
  if (!goos || !goarch) {
    throw new Error(
      `sofabgen: unsupported platform ${process.platform}/${process.arch}`
    );
  }
  if (goos === "darwin" && goarch !== "amd64" && goarch !== "arm64") {
    throw new Error(`sofabgen: no macOS build is published for ${goarch}`);
  }
  const ext = goos === "windows" ? ".exe" : "";
  return { goos, goarch, ext, asset: `sofabgen-${goos}-${goarch}${ext}` };
}

function binaryPath() {
  return path.join(__dirname, "..", "bin", `sofabgen${target().ext}`);
}

// GET a URL into a Buffer, following GitHub's redirect to the asset CDN.
function fetch(url) {
  return new Promise((resolve, reject) => {
    https
      .get(url, { headers: { "User-Agent": "sofabgen-npm-installer" } }, (res) => {
        if (
          res.statusCode >= 300 &&
          res.statusCode < 400 &&
          res.headers.location
        ) {
          res.resume();
          return resolve(fetch(res.headers.location));
        }
        if (res.statusCode !== 200) {
          res.resume();
          return reject(new Error(`sofabgen: HTTP ${res.statusCode} for ${url}`));
        }
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => resolve(Buffer.concat(chunks)));
      })
      .on("error", reject);
  });
}

// Download (once) and verify the binary; returns its path. Idempotent.
async function ensureBinary() {
  const dest = binaryPath();
  if (fs.existsSync(dest)) return dest;

  const { asset } = target();
  const base = `https://github.com/${REPO}/releases/download/v${VERSION}`;
  const [bin, shaText] = await Promise.all([
    fetch(`${base}/${asset}`),
    fetch(`${base}/${asset}.sha256`),
  ]);

  const want = shaText.toString("utf8").trim().split(/\s+/)[0];
  const got = crypto.createHash("sha256").update(bin).digest("hex");
  if (want && got !== want) {
    throw new Error(
      `sofabgen: checksum mismatch for ${asset} (expected ${want}, got ${got})`
    );
  }

  fs.mkdirSync(path.dirname(dest), { recursive: true });
  fs.writeFileSync(dest, bin, { mode: 0o755 });
  return dest;
}

module.exports = { target, binaryPath, ensureBinary };

// Run directly (postinstall): fetch the binary, but never fail the whole install
// on a transient network error — the launcher retries lazily on first use.
if (require.main === module) {
  ensureBinary()
    .then((p) => console.log(`sofabgen: ready at ${p}`))
    .catch((err) => {
      console.warn(
        `sofabgen: could not pre-download the binary (${err.message}); ` +
          `it will be fetched on first run.`
      );
    });
}
