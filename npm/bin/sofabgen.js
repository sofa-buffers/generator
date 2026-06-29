#!/usr/bin/env node
// Thin launcher: ensure the prebuilt sofabgen binary is present (downloading it
// lazily if a `--ignore-scripts` install skipped postinstall), then exec it with
// the caller's args and stdio, and propagate its exit code.

"use strict";

const { spawnSync } = require("child_process");
const { binaryPath, ensureBinary } = require("../scripts/install.js");

ensureBinary()
  .then(() => {
    const res = spawnSync(binaryPath(), process.argv.slice(2), {
      stdio: "inherit",
    });
    if (res.error) {
      console.error(`sofabgen: ${res.error.message}`);
      process.exit(1);
    }
    process.exit(res.status === null ? 1 : res.status);
  })
  .catch((err) => {
    console.error(err.message);
    process.exit(1);
  });
