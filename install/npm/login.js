// `npx @workweave/router login` — enroll a Claude or ChatGPT subscription
// account into the router's per-user pool so the router rotates through your
// plans as each hits its usage cap.
//
// The OAuth flow runs entirely on your machine (browser PKCE for ChatGPT, the
// manual code#state paste for Claude, mirroring Claude Code's own flow). Only
// the resulting token bundle is pushed to the router's /v1/subscriptions
// endpoint, authenticated with the router key from your existing install.
//
// Plain Node (>=18), zero dependencies — crypto, http, and fetch are built in.

const crypto = require("node:crypto");
const http = require("node:http");
const readline = require("node:readline");
const { spawn } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

// ---- OAuth constants (public app identifiers, same as the opencode plugin) --
const CHATGPT_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann";
const CHATGPT_ISSUER = process.env.WEAVE_CODEX_OAUTH_ISSUER || "https://auth.openai.com";
const OAUTH_PORT = 1455;

const ANTHROPIC_CLIENT_ID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e";
const ANTHROPIC_AUTHORIZE_BASE = process.env.WEAVE_ANTHROPIC_OAUTH_AUTHORIZE || "https://claude.ai";
const ANTHROPIC_TOKEN_URL = process.env.WEAVE_ANTHROPIC_OAUTH_TOKEN || "https://console.anthropic.com/v1/oauth/token";
const ANTHROPIC_REDIRECT_URI = "https://console.anthropic.com/oauth/code/callback";
const ANTHROPIC_SCOPE = "org:create_api_key user:profile user:inference";

async function main(argv) {
  const flags = parseFlags(argv);
  if (flags.help) {
    printUsage();
    return;
  }

  const config = discoverConfig(flags);
  if (!config.routerUrl || !config.routerKey) {
    console.error(
      "Weave Router: couldn't find your router URL and key. Install the router first\n" +
        "(npx @workweave/router) or pass --base-url and set WEAVE_ROUTER_KEY.",
    );
    process.exit(1);
  }

  let provider = flags.provider;
  if (!provider) provider = await promptProvider();
  provider = normalizeProvider(provider);
  if (!provider) {
    console.error("Weave Router: --provider must be 'claude' or 'chatgpt'.");
    process.exit(1);
  }

  const email = flags.email || config.userEmail || (await prompt("Your email (used to scope this account to you): "));
  if (!email || !email.includes("@")) {
    console.error("Weave Router: a valid --email is required.");
    process.exit(1);
  }

  let bundle;
  try {
    bundle = provider === "openai" ? await loginChatGPT() : await loginClaude();
  } catch (err) {
    console.error("Weave Router: login failed —", err.message);
    process.exit(1);
  }

  const body = {
    provider,
    user_email: email,
    access_token: bundle.accessToken,
    refresh_token: bundle.refreshToken,
    expires_at: bundle.expiresAt ? new Date(bundle.expiresAt).toISOString() : undefined,
    chatgpt_account_id: bundle.accountId,
    account_label: flags.label || bundle.label || undefined,
  };

  const url = config.routerUrl.replace(/\/+$/, "") + "/v1/subscriptions";
  const res = await fetch(url, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Weave-Router-Key": config.routerKey,
    },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    console.error(`Weave Router: enrollment failed (HTTP ${res.status}). ${text}`);
    process.exit(1);
  }
  const created = await res.json().catch(() => ({}));
  const label = created.account_label ? ` (${created.account_label})` : "";
  console.log(
    `\n✓ Enrolled ${provider === "openai" ? "ChatGPT" : "Claude"} account${label}.\n` +
      "  The router will rotate to it once your other plans hit their caps.\n" +
      "  Run this again to add another account.",
  );
}

// ---- ChatGPT browser PKCE flow ---------------------------------------------

async function loginChatGPT() {
  const pkce = await generatePKCE();
  const state = base64Url(crypto.randomBytes(24));
  const redirectUri = `http://localhost:${OAUTH_PORT}/auth/callback`;

  const codePromise = awaitCallback(state);
  const authorizeUrl = buildChatGPTAuthorizeUrl(redirectUri, pkce, state);
  console.log("\nOpening your browser to sign in to ChatGPT…");
  console.log(`If it doesn't open, visit:\n${authorizeUrl}\n`);
  openBrowser(authorizeUrl);

  const code = await codePromise;
  const tokens = await exchangeChatGPTCode(code, redirectUri, pkce);
  return {
    accessToken: tokens.access_token,
    refreshToken: tokens.refresh_token,
    expiresAt: Date.now() + (tokens.expires_in || 3600) * 1000,
    accountId: extractAccountId(tokens),
  };
}

function buildChatGPTAuthorizeUrl(redirectUri, pkce, state) {
  const params = new URLSearchParams({
    response_type: "code",
    client_id: CHATGPT_CLIENT_ID,
    redirect_uri: redirectUri,
    scope: "openid profile email offline_access",
    code_challenge: pkce.challenge,
    code_challenge_method: "S256",
    id_token_add_organizations: "true",
    codex_cli_simplified_flow: "true",
    state,
    originator: "codex_cli_ts",
  });
  return `${CHATGPT_ISSUER}/oauth/authorize?${params.toString()}`;
}

async function exchangeChatGPTCode(code, redirectUri, pkce) {
  const res = await fetch(`${CHATGPT_ISSUER}/oauth/token`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "authorization_code",
      code,
      redirect_uri: redirectUri,
      client_id: CHATGPT_CLIENT_ID,
      code_verifier: pkce.verifier,
    }).toString(),
  });
  if (!res.ok) throw new Error(`ChatGPT token exchange failed: ${res.status}`);
  return res.json();
}

// A single-shot loopback server that resolves with the authorization code.
function awaitCallback(expectedState) {
  return new Promise((resolve, reject) => {
    const server = http.createServer((req, res) => {
      const url = new URL(req.url || "/", `http://localhost:${OAUTH_PORT}`);
      if (url.pathname !== "/auth/callback") {
        res.writeHead(404);
        res.end("Not found");
        return;
      }
      const code = url.searchParams.get("code");
      const state = url.searchParams.get("state");
      const error = url.searchParams.get("error_description") || url.searchParams.get("error");
      const finish = (status, msg, ok) => {
        res.writeHead(status, { "Content-Type": "text/html; charset=utf-8" });
        res.end(`<html><body style="font-family:system-ui;padding:3rem;text-align:center">${msg}</body></html>`);
        server.close();
        if (ok) resolve(code);
        else reject(new Error(msg));
      };
      if (error) return finish(200, `Authorization failed: ${error}`, false);
      if (!code) return finish(400, "Missing authorization code", false);
      if (state !== expectedState) return finish(400, "Invalid state — possible CSRF", false);
      finish(200, "✓ Signed in. You can close this tab and return to your terminal.", true);
    });
    server.on("error", reject);
    server.listen(OAUTH_PORT);
  });
}

// ---- Claude manual code#state flow -----------------------------------------

async function loginClaude() {
  const pkce = await generatePKCE();
  const authorizeUrl = buildClaudeAuthorizeUrl(pkce);
  console.log("\nOpening your browser to sign in to Claude…");
  console.log(`If it doesn't open, visit:\n${authorizeUrl}\n`);
  openBrowser(authorizeUrl);

  const pasted = await prompt('Paste the code shown after authorizing (format "code#state"): ');
  const tokens = await exchangeClaudeCode(pasted.trim(), pkce.verifier);
  return {
    accessToken: tokens.access_token,
    refreshToken: tokens.refresh_token,
    expiresAt: Date.now() + (tokens.expires_in || 3600) * 1000,
  };
}

function buildClaudeAuthorizeUrl(pkce) {
  const url = new URL(`${ANTHROPIC_AUTHORIZE_BASE}/oauth/authorize`);
  url.searchParams.set("code", "true");
  url.searchParams.set("client_id", ANTHROPIC_CLIENT_ID);
  url.searchParams.set("response_type", "code");
  url.searchParams.set("redirect_uri", ANTHROPIC_REDIRECT_URI);
  url.searchParams.set("scope", ANTHROPIC_SCOPE);
  url.searchParams.set("code_challenge", pkce.challenge);
  url.searchParams.set("code_challenge_method", "S256");
  url.searchParams.set("state", pkce.verifier);
  return url.toString();
}

async function exchangeClaudeCode(pasted, verifier) {
  if (!pasted.includes("#")) {
    throw new Error('expected the pasted value in "code#state" format');
  }
  const [authCode, state] = pasted.split("#");
  const res = await fetch(ANTHROPIC_TOKEN_URL, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      grant_type: "authorization_code",
      code: authCode,
      state,
      client_id: ANTHROPIC_CLIENT_ID,
      redirect_uri: ANTHROPIC_REDIRECT_URI,
      code_verifier: verifier,
    }),
  });
  if (!res.ok) throw new Error(`Claude token exchange failed: ${res.status}`);
  return res.json();
}

// ---- PKCE + JWT helpers -----------------------------------------------------

function base64Url(buf) {
  return buf.toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

async function generatePKCE() {
  const verifier = base64Url(crypto.randomBytes(32));
  const challenge = base64Url(crypto.createHash("sha256").update(verifier).digest());
  return { verifier, challenge };
}

function parseJwtClaims(token) {
  const parts = token.split(".");
  if (parts.length !== 3) return undefined;
  try {
    return JSON.parse(Buffer.from(parts[1], "base64url").toString());
  } catch {
    return undefined;
  }
}

function extractAccountId(tokens) {
  for (const token of [tokens.id_token, tokens.access_token]) {
    if (!token) continue;
    const claims = parseJwtClaims(token);
    if (!claims) continue;
    const id =
      claims.chatgpt_account_id ||
      claims["https://api.openai.com/auth"]?.chatgpt_account_id ||
      claims.organizations?.[0]?.id;
    if (id) return id;
  }
  return undefined;
}

// ---- Config discovery -------------------------------------------------------
// Router URL + key + email, in priority order: explicit flags/env, then the
// installed Claude Code / Codex / opencode configs.

function discoverConfig(flags) {
  const out = {
    routerUrl: flags.baseUrl || process.env.WEAVE_ROUTER_URL || "",
    routerKey: process.env.WEAVE_ROUTER_KEY || "",
    userEmail: process.env.WEAVE_USER_EMAIL || "",
  };
  const sources = [readClaudeConfig, readCodexConfig, readOpencodeConfig];
  for (const read of sources) {
    if (out.routerUrl && out.routerKey) break;
    const found = read();
    if (!found) continue;
    out.routerUrl = out.routerUrl || found.routerUrl || "";
    out.routerKey = out.routerKey || found.routerKey || "";
    out.userEmail = out.userEmail || found.userEmail || "";
  }
  return out;
}

function readClaudeConfig() {
  const settings = readJson(path.join(os.homedir(), ".claude", "settings.json"));
  const local = readJson(path.join(os.homedir(), ".claude", "settings.local.json"));
  const env = { ...(settings?.env || {}), ...(local?.env || {}) };
  const headers = parseHeaderLines(env.ANTHROPIC_CUSTOM_HEADERS);
  return {
    routerUrl: env.ANTHROPIC_BASE_URL,
    routerKey: headers["X-Weave-Router-Key"],
    userEmail: headers["X-Weave-User-Email"],
  };
}

function readCodexConfig() {
  const text = readText(path.join(os.homedir(), ".codex", "config.toml"));
  if (!text) return null;
  const baseUrl = matchToml(text, /base_url\s*=\s*"([^"]+)"/);
  const routerKey = matchToml(text, /"X-Weave-Router-Key"\s*=\s*"([^"]+)"/);
  const email = matchToml(text, /"X-Weave-User-Email"\s*=\s*"([^"]+)"/);
  return {
    // Codex base_url includes the /v1 suffix; strip it back to the router root.
    routerUrl: baseUrl ? baseUrl.replace(/\/v1\/?$/, "") : "",
    routerKey,
    userEmail: email,
  };
}

function readOpencodeConfig() {
  const candidates = [
    path.join(os.homedir(), ".config", "opencode", "opencode.json"),
    path.join(process.cwd(), "opencode.json"),
  ];
  for (const file of candidates) {
    const json = readJson(file);
    const weave = json?.provider?.weave?.options;
    if (!weave) continue;
    const headers = weave.headers || {};
    return {
      routerUrl: weave.baseURL ? weave.baseURL.replace(/\/v1\/?$/, "") : "",
      routerKey: headers["X-Weave-Router-Key"],
      userEmail: headers["X-Weave-User-Email"],
    };
  }
  return null;
}

// ANTHROPIC_CUSTOM_HEADERS is newline-delimited "Header: value" lines.
function parseHeaderLines(value) {
  const out = {};
  if (!value) return out;
  for (const line of value.split(/\r?\n/)) {
    const idx = line.indexOf(":");
    if (idx <= 0) continue;
    out[line.slice(0, idx).trim()] = line.slice(idx + 1).trim();
  }
  return out;
}

function matchToml(text, re) {
  const m = text.match(re);
  return m ? m[1] : "";
}

function readJson(file) {
  const text = readText(file);
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

function readText(file) {
  try {
    return fs.readFileSync(file, "utf8");
  } catch {
    return "";
  }
}

// ---- small CLI helpers ------------------------------------------------------

function parseFlags(argv) {
  const flags = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--help" || a === "-h") flags.help = true;
    else if (a === "--provider") flags.provider = argv[++i];
    else if (a === "--email") flags.email = argv[++i];
    else if (a === "--label") flags.label = argv[++i];
    else if (a === "--base-url") flags.baseUrl = argv[++i];
    else if (a === "--local") flags.baseUrl = "http://localhost:8080";
  }
  return flags;
}

function normalizeProvider(p) {
  const v = String(p || "").toLowerCase().trim();
  if (v === "claude" || v === "anthropic") return "anthropic";
  if (v === "chatgpt" || v === "codex" || v === "openai") return "openai";
  return "";
}

async function promptProvider() {
  const answer = await prompt("Which subscription? [claude/chatgpt]: ");
  return answer;
}

function prompt(question) {
  const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
  return new Promise((resolve) => {
    rl.question(question, (answer) => {
      rl.close();
      resolve(answer.trim());
    });
  });
}

function openBrowser(url) {
  const cmd = process.platform === "darwin" ? "open" : process.platform === "win32" ? "cmd" : "xdg-open";
  const args = process.platform === "win32" ? ["/c", "start", "", url] : [url];
  try {
    spawn(cmd, args, { stdio: "ignore", detached: true }).unref();
  } catch {
    // Non-fatal: the URL is already printed for manual navigation.
  }
}

function printUsage() {
  console.log(
    "Usage: npx @workweave/router login [options]\n\n" +
      "Enroll a Claude or ChatGPT subscription account into the router pool.\n\n" +
      "Options:\n" +
      "  --provider <claude|chatgpt>  Which subscription to enroll (prompts if omitted)\n" +
      "  --email <email>              Account owner email (scopes the pool to you)\n" +
      "  --label <text>               Friendly label for this account\n" +
      "  --base-url <url>             Router URL (else discovered from your install)\n" +
      "  --local                      Shorthand for --base-url http://localhost:8080\n",
  );
}

module.exports = { main, discoverConfig, parseHeaderLines, normalizeProvider, extractAccountId };
