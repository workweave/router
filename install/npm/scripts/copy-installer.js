#!/usr/bin/env node
// Run by `npm pack` / `npm publish` (prepack hook). Copies the canonical
// install scripts from ../install/*.sh into the npm package root so the
// published tarball is self-contained. Keeps a single source of truth for
// the shell installer.

const { copyFileSync, chmodSync } = require("node:fs");
const path = require("node:path");

const root = path.resolve(__dirname, "..");
const installDir = path.resolve(root, "..");
const repoRoot = path.resolve(installDir, "..");

const files = ["install.sh", "uninstall.sh", "cc-statusline.sh"];
for (const f of files) {
  const src = path.join(installDir, f);
  const dst = path.join(root, f);
  copyFileSync(src, dst);
  chmodSync(dst, 0o755);
  console.log(`copied ${f}`);
}

// LICENSE lives at the repo root and applies to the whole project. npm
// surfaces it on the package page when bundled alongside package.json.
copyFileSync(path.join(repoRoot, "LICENSE"), path.join(root, "LICENSE"));
console.log("copied LICENSE");
