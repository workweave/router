/**
 * Environment, routing-knob presets, identity, and the static per-process
 * header policy shared by every part of the extension.
 *
 * The whole design hinges on one pi constraint: `before_provider_request` can
 * mutate the request body but cannot set HTTP headers (headers bind when the
 * Anthropic client is created, before the hook runs). So routing-knob headers
 * are *static per provider config*, i.e. static per process. Subagents are
 * separate processes, which is exactly why "quality for the main loop, speed
 * for subagents" maps cleanly onto process identity (`WEAVE_PI_SUBAGENT`).
 */

import { execFileSync } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import type { ProviderModelConfig } from "@mariozechner/pi-coding-agent";

/** Provider name. NOT "anthropic" — overriding the built-in provider hijacks the Claude OAuth token. */
export const PROVIDER_NAME = "weave";

export type Role = "main" | "subagent";

/** Children spawned by the dispatch tool run with WEAVE_PI_SUBAGENT=1. */
export function isSubagent(): boolean {
	return process.env.WEAVE_PI_SUBAGENT === "1";
}

export function getRole(): Role {
	return isSubagent() ? "subagent" : "main";
}

function numEnv(name: string, fallback: number): number {
	const raw = process.env[name];
	if (raw === undefined || raw.trim() === "") return fallback;
	const n = Number(raw);
	return Number.isFinite(n) ? n : fallback;
}

// ---------- router endpoint ----------

/**
 * Router base URL — the ROOT, with no /v1 and no trailing slash. pi's
 * anthropic-messages provider uses @anthropic-ai/sdk, which appends /v1/messages
 * to this; a trailing /v1 would produce /v1/v1/messages and 404. Order:
 * WEAVE_ROUTER_URL env (children inherit it), then the installer-written
 * models.json baseUrl (so the extension always agrees with however the user
 * installed — hosted, --local, or custom), then the local default. Without the
 * models.json fallback, a hosted install with no env var would be silently
 * re-pointed at localhost. The /v1 strip on the models.json value keeps installs
 * written by an older installer (which appended /v1) working after an update.
 */
export function getRouterBaseUrl(): string {
	const env = process.env.WEAVE_ROUTER_URL?.trim();
	if (env) return env.replace(/\/v1\/?$/, "").replace(/\/+$/, "");
	const fromModels = readWeaveProvider().baseUrl?.trim();
	if (fromModels) return fromModels.replace(/\/v1\/?$/, "").replace(/\/+$/, "");
	return "http://localhost:8080";
}

// ---------- router key resolution ----------

/** Agent config dir, honoring PI_CODING_AGENT_DIR (set for project-scope installs). */
function getAgentDir(): string {
	const env = process.env.PI_CODING_AGENT_DIR?.trim();
	if (env) return env.startsWith("~/") ? path.join(os.homedir(), env.slice(2)) : env;
	return path.join(os.homedir(), ".pi", "agent");
}

export function getKeyFilePath(): string {
	return process.env.WEAVE_ROUTER_KEY_FILE?.trim() || path.join(getAgentDir(), ".weave_router_key");
}

interface WeaveProviderConfig {
	baseUrl?: string;
	apiKey?: string;
}

let cachedWeaveProvider: WeaveProviderConfig | undefined;

/** Read the installer-written `weave` provider block from models.json (cached). */
function readWeaveProvider(): WeaveProviderConfig {
	if (cachedWeaveProvider) return cachedWeaveProvider;
	let result: WeaveProviderConfig = {};
	try {
		const raw = fs.readFileSync(path.join(getAgentDir(), "models.json"), "utf-8");
		const parsed = JSON.parse(raw) as { providers?: { weave?: WeaveProviderConfig } };
		result = parsed.providers?.weave ?? {};
	} catch {
		/* no models.json — fall through to env/defaults */
	}
	cachedWeaveProvider = result;
	return result;
}

/** Router key from WEAVE_ROUTER_KEY, else the installer-written key file, else models.json. */
export function resolveRouterKey(): string | undefined {
	const envKey = process.env.WEAVE_ROUTER_KEY?.trim();
	if (envKey) return envKey;
	try {
		const contents = fs.readFileSync(getKeyFilePath(), "utf-8").trim();
		if (contents) return contents;
	} catch {
		/* no key file — fall through */
	}
	return readWeaveProvider().apiKey?.trim() || undefined;
}

// ---------- identity ----------

export interface Identity {
	email?: string;
	name?: string;
}

function gitConfig(key: string): string | undefined {
	try {
		const out = execFileSync("git", ["config", "--get", key], {
			encoding: "utf-8",
			stdio: ["ignore", "pipe", "ignore"],
		}).trim();
		return out || undefined;
	} catch {
		return undefined;
	}
}

let cachedIdentity: Identity | undefined;

/** Resolve user identity the same way the installer does: env overrides, then git config. */
export function resolveIdentity(): Identity {
	if (cachedIdentity) return cachedIdentity;
	const email = process.env.WEAVE_USER_EMAIL?.trim() || gitConfig("user.email");
	const name = process.env.WEAVE_USER_NAME?.trim() || gitConfig("user.name");
	cachedIdentity = { email: email || undefined, name: name || undefined };
	return cachedIdentity;
}

// ---------- routing-knob presets ----------

export interface Knobs {
	alpha: number;
	speedWeight: number;
	outputCostRatio: number;
	expectedOutputTokens: number;
}

// Validated against the scorer: alpha + speedWeight <= 1.
//   main:       0.80 + 0.05 = 0.85  (quality-biased; first turn pins a high tier)
//   subagent:   0.25 + 0.45 = 0.70  (speed + cheap fan-out, isolated session)
//   compaction: 0.05 + 0.55 = 0.60  (cheapest — summarization is throwaway)
const MAIN_LOOP_KNOBS: Knobs = { alpha: 0.8, speedWeight: 0.05, outputCostRatio: 0.5, expectedOutputTokens: 3000 };
const FAST_CHEAP_KNOBS: Knobs = { alpha: 0.25, speedWeight: 0.45, outputCostRatio: 2.0, expectedOutputTokens: 1500 };
const COMPACTION_KNOBS: Knobs = { alpha: 0.05, speedWeight: 0.55, outputCostRatio: 3.0, expectedOutputTokens: 1000 };

/** Knobs for this process, with per-knob env overrides applied on top of the role preset. */
export function knobsForRole(role: Role): Knobs {
	const base = role === "subagent" ? FAST_CHEAP_KNOBS : MAIN_LOOP_KNOBS;
	return {
		alpha: numEnv("WEAVE_ROUTING_ALPHA", base.alpha),
		speedWeight: numEnv("WEAVE_ROUTING_SPEED_WEIGHT", base.speedWeight),
		outputCostRatio: numEnv("WEAVE_ROUTING_OUTPUT_COST_RATIO", base.outputCostRatio),
		expectedOutputTokens: numEnv("WEAVE_ROUTING_EXPECTED_OUTPUT_TOKENS", base.expectedOutputTokens),
	};
}

export function compactionKnobs(): Knobs {
	return { ...COMPACTION_KNOBS };
}

// ---------- header builders ----------

const KNOB_HEADER = {
	alpha: "x-weave-routing-alpha",
	speedWeight: "x-weave-routing-speed-weight",
	outputCostRatio: "x-weave-routing-output-cost-ratio",
	expectedOutputTokens: "x-weave-routing-expected-output-tokens",
} as const;

export function knobHeaders(k: Knobs): Record<string, string> {
	return {
		[KNOB_HEADER.alpha]: String(k.alpha),
		[KNOB_HEADER.speedWeight]: String(k.speedWeight),
		[KNOB_HEADER.outputCostRatio]: String(k.outputCostRatio),
		[KNOB_HEADER.expectedOutputTokens]: String(k.expectedOutputTokens),
	};
}

/** Identity + auth headers. The router authenticates off X-Weave-Router-Key (authHeader stays false). */
export function identityHeaders(role: Role, key: string): Record<string, string> {
	const id = resolveIdentity();
	const headers: Record<string, string> = {
		"X-Weave-Router-Key": key,
		"X-App": role === "subagent" ? "pi-subagent" : "pi",
	};
	if (id.email) headers["X-Weave-User-Email"] = id.email;
	if (id.name) headers["X-Weave-User-Name"] = id.name;
	return headers;
}

/** The full static header set for this process's `weave` provider. */
export function providerHeaders(role: Role, key: string): Record<string, string> {
	return { ...identityHeaders(role, key), ...knobHeaders(knobsForRole(role)) };
}

// ---------- model list ----------

// Carried as a shared constant rather than relying on registerProvider preserving
// an omitted `models` list, so the extension is self-sufficient: a freshly
// dispatched child (and a `pi -e` smoke test with no models.json) can still
// resolve `weave/<model>`. The list mirrors the installer's headline models;
// the router re-routes every request regardless, so this is a UX/label surface.
export const WEAVE_MODELS: ProviderModelConfig[] = [
	model("claude-opus-4-8", "Claude Opus 4.8 (via Weave Router)", 64000),
	model("claude-opus-4-7", "Claude Opus 4.7 (via Weave Router)", 64000),
	model("claude-sonnet-4-6", "Claude Sonnet 4.6 (via Weave Router)", 64000),
	model("claude-haiku-4-5", "Claude Haiku 4.5 (via Weave Router)", 32000),
];

function model(id: string, name: string, maxTokens: number): ProviderModelConfig {
	return {
		id,
		name,
		reasoning: true,
		input: ["text", "image"],
		// Real cost is decided by the router per request and is unknown client-side.
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: 200000,
		maxTokens,
	};
}

// ---------- dispatch / misc tunables ----------

export const ROUTED_MODEL_HEADER = (process.env.WEAVE_ROUTED_MODEL_HEADER || "x-router-model").toLowerCase();
/** Marker a headless child prints to stderr so the parent dispatch can read its routed model. */
export const ROUTED_MODEL_STDERR_PREFIX = "weave-routed-model:";

export const SUBAGENT_MODEL = process.env.WEAVE_PI_SUBAGENT_MODEL?.trim() || "claude-sonnet-4-6";
export const DISPATCH_CONCURRENCY = Math.max(1, numEnv("WEAVE_PI_DISPATCH_CONCURRENCY", 4));
export const MAX_SUBAGENTS = 8;
export const SUBAGENT_TIMEOUT_MS = Math.max(1000, numEnv("WEAVE_PI_SUBAGENT_TIMEOUT_MS", 600000));
export const DEFAULT_READONLY_TOOLS = ["read", "grep", "find", "ls"];

// Tools that let a subagent mutate the filesystem or run arbitrary commands.
// dispatch strips these from model-requested per-task `tools` (and downgrades
// readOnly:false's "all tools") unless WEAVE_PI_ALLOW_SUBAGENT_TOOLS=1, so a
// prompt-injected main loop can't silently escalate a read-only fan-out into
// writes/exec. The catastrophic-command gate in safety.ts is a separate, narrower
// backstop; this is the capability gate.
export const DANGEROUS_SUBAGENT_TOOLS = new Set(["bash", "edit", "write", "patch", "multiedit", "apply_patch"]);
