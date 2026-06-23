#!/usr/bin/env node
// Run by `npm pack` / `npm publish` (prepack hook). Copies the canonical
// install scripts from ../install/*.sh into the npm package root so the
// published tarball is self-contained. Keeps a single source of truth for
// the shell installer.

const { copyFileSync, cpSync, chmodSync, mkdirSync, readdirSync, lstatSync, realpathSync } = require("node:fs");
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
  console.log(`Copied ${f}.`);
}

// Mirror install/commands/ into the package root. install.sh resolves the
// commands dir relative to its own location, so colocating it alongside the
// script makes the bundle self-contained for `npx @workweave/router`.
const commandsSrc = path.join(installDir, "commands");
const commandsDst = path.join(root, "commands");
const commandsSrcReal = realpathSync(commandsSrc);
mkdirSync(commandsDst, { recursive: true });
for (const f of readdirSync(commandsSrc)) {
  if (!f.endsWith(".md")) continue;
  const src = path.join(commandsSrc, f);
  const stat = lstatSync(src);
  if (stat.isSymbolicLink()) {
    throw new Error(`Refusing to package symlinked command file: ${src}`);
  }
  const srcReal = realpathSync(src);
  if (!srcReal.startsWith(commandsSrcReal + path.sep)) {
    throw new Error(`Refusing to package command outside commands dir: ${src}`);
  }
  copyFileSync(srcReal, path.join(commandsDst, f));
  console.log(`Copied commands/${f}.`);
}

// Bundle the pi extension so the single @workweave/router package is BOTH the
// installer and the pi-router extension: pi loads it via the "pi.extensions"
// field in package.json, and install.sh adds `npm:@workweave/router` to pi's
// settings. Source of truth lives at install/pi-router/src.
const piSrc = path.join(installDir, "pi-router", "src");
const piDst = path.join(root, "pi-router", "src");
mkdirSync(path.dirname(piDst), { recursive: true });
cpSync(piSrc, piDst, { recursive: true });
// package.json marks the sources as ESM (type:module); README is docs.
for (const f of ["package.json", "README.md"]) {
  copyFileSync(path.join(installDir, "pi-router", f), path.join(root, "pi-router", f));
}
console.log("Copied pi-router/ (extension).");

// Bundle the opencode Codex-subscription plugin the same way. install.sh
// (--codex/--opencode) drops opencode-weave/src/index.ts into the user's
// opencode plugins dir and registers it via opencode.json's "plugin" array.
// Source of truth lives at install/opencode-weave/src.
const ocSrc = path.join(installDir, "opencode-weave", "src");
const ocDst = path.join(root, "opencode-weave", "src");
mkdirSync(path.dirname(ocDst), { recursive: true });
cpSync(ocSrc, ocDst, { recursive: true });
for (const f of ["package.json", "README.md"]) {
  copyFileSync(path.join(installDir, "opencode-weave", f), path.join(root, "opencode-weave", f));
}
console.log("Copied opencode-weave/ (plugin).");

// LICENSE lives at the repo root and applies to the whole project. npm
// surfaces it on the package page when bundled alongside package.json.
copyFileSync(path.join(repoRoot, "LICENSE"), path.join(root, "LICENSE"));
console.log("Copied LICENSE.");
