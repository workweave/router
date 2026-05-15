#!/usr/bin/env node
// Thin wrapper that runs install.sh with the user's arguments.
// Bundled install.sh ships with the npm package so `npx weave-router` works
// offline (modulo the router API ping the installer does).

const { spawnSync } = require("node:child_process");
const { existsSync } = require("node:fs");
const path = require("node:path");

const args = process.argv.slice(2);
const uninstallIdx = args.indexOf("--uninstall");
const isUninstall = uninstallIdx !== -1;
if (isUninstall) args.splice(uninstallIdx, 1);

const scriptName = isUninstall ? "uninstall.sh" : "install.sh";
const script = path.join(__dirname, scriptName);

if (!existsSync(script)) {
  console.error(
    `weave-router: ${scriptName} missing from package — please report at https://github.com/workweave/router/issues`,
  );
  process.exit(1);
}

const bash = pickBash();
if (!bash) {
  console.error(
    "weave-router: bash is required. On Windows install Git Bash or run inside WSL.",
  );
  process.exit(1);
}

const result = spawnSync(bash, [script, ...args], {
  stdio: "inherit",
  env: process.env,
});

if (result.error) {
  console.error("weave-router:", result.error.message);
  process.exit(1);
}
process.exit(result.status ?? 1);

function pickBash() {
  if (process.platform !== "win32") return "bash";
  const candidates = [
    process.env.SHELL,
    "C:\\Program Files\\Git\\bin\\bash.exe",
    "C:\\Program Files (x86)\\Git\\bin\\bash.exe",
  ].filter(Boolean);
  for (const c of candidates) {
    if (existsSync(c)) return c;
  }
  return null;
}
