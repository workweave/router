// Run with: node --test install/npm/login.test.js
const { test } = require("node:test");
const assert = require("node:assert");
const { parseHeaderLines, normalizeProvider, extractAccountId } = require("./login.js");

test("parseHeaderLines parses newline-delimited Header: value lines", () => {
  const raw = "X-Weave-Router-Key: rk_abc123\nX-Weave-User-Email: dev@example.com\nX-App: claude-code";
  const headers = parseHeaderLines(raw);
  assert.equal(headers["X-Weave-Router-Key"], "rk_abc123");
  assert.equal(headers["X-Weave-User-Email"], "dev@example.com");
  assert.equal(headers["X-App"], "claude-code");
});

test("parseHeaderLines tolerates empty and malformed input", () => {
  assert.deepEqual(parseHeaderLines(""), {});
  assert.deepEqual(parseHeaderLines(undefined), {});
  assert.deepEqual(parseHeaderLines("no-colon-here"), {});
});

test("normalizeProvider maps aliases to canonical constants", () => {
  assert.equal(normalizeProvider("claude"), "anthropic");
  assert.equal(normalizeProvider("anthropic"), "anthropic");
  assert.equal(normalizeProvider("chatgpt"), "openai");
  assert.equal(normalizeProvider("codex"), "openai");
  assert.equal(normalizeProvider("openai"), "openai");
  assert.equal(normalizeProvider("gemini"), "");
});

test("extractAccountId reads chatgpt_account_id from a JWT id_token claim", () => {
  const claims = { chatgpt_account_id: "acct-77" };
  const payload = Buffer.from(JSON.stringify(claims)).toString("base64url");
  const jwt = `header.${payload}.sig`;
  assert.equal(extractAccountId({ id_token: jwt }), "acct-77");
});

test("extractAccountId falls back to the organizations claim", () => {
  const claims = { organizations: [{ id: "org-5" }] };
  const payload = Buffer.from(JSON.stringify(claims)).toString("base64url");
  const jwt = `header.${payload}.sig`;
  assert.equal(extractAccountId({ access_token: jwt }), "org-5");
});
