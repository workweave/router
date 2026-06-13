/**
 * `dispatch` — parallel, context-isolated subagents.
 *
 * pi has no native subagents. The canonical pattern (examples/extensions/
 * subagent) is to spawn a fresh `pi --print --mode json --no-session` process
 * per task: true context isolation, reuses pi's own agent loop, and an
 * independent router session. We layer Weave routing on top by loading this
 * same extension in each child (`-e <self>`) with WEAVE_PI_SUBAGENT=1, so the
 * child registers the `weave` provider with the speed/cheap knobs and a
 * "subagent:" session id. Only the final assistant text comes back to the
 * parent — intermediate tool output stays in the child, keeping the main
 * context tiny.
 */

import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import * as fs from "node:fs";
import * as path from "node:path";
import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import type { Message } from "@mariozechner/pi-ai";
import { Text } from "@mariozechner/pi-tui";
import { Type } from "typebox";
import {
	DANGEROUS_SUBAGENT_TOOLS,
	DEFAULT_READONLY_TOOLS,
	DISPATCH_CONCURRENCY,
	getRouterBaseUrl,
	MAX_SUBAGENTS,
	PROVIDER_NAME,
	resolveIdentity,
	resolveRouterKey,
	ROUTED_MODEL_STDERR_PREFIX,
	SUBAGENT_MODEL,
	SUBAGENT_TIMEOUT_MS,
} from "./config.js";

const SIGKILL_GRACE_MS = 5000;

const TaskItem = Type.Object({
	prompt: Type.String({ description: "The full instruction for this subagent." }),
	tools: Type.Optional(
		Type.Array(Type.String(), {
			description: "Tool names this subagent may use (e.g. read, grep). Overrides readOnly for this task. Dangerous tools (bash, write, edit) are ignored unless WEAVE_PI_ALLOW_SUBAGENT_TOOLS=1.",
		}),
	),
	cwd: Type.Optional(Type.String({ description: "Working directory for this subagent. Defaults to the parent cwd." })),
});

const DispatchParams = Type.Object({
	tasks: Type.Array(TaskItem, {
		minItems: 1,
		maxItems: MAX_SUBAGENTS,
		description: "Tasks to run as parallel, context-isolated subagents.",
	}),
	readOnly: Type.Optional(
		Type.Boolean({
			default: true,
			description: "When true (default), subagents get read-only tools (read, grep, find, ls). Per-task `tools` overrides this. Without WEAVE_PI_ALLOW_SUBAGENT_TOOLS=1, readOnly:false still yields read-only tools (no silent bash/write).",
		}),
	),
});

interface ChildResult {
	index: number;
	finalText: string;
	routedModel?: string;
	exitCode: number;
	error?: string;
}

/** Resolve how to launch a nested pi (node/bun script, compiled binary, or `pi` on PATH). */
function getPiInvocation(args: string[]): { command: string; args: string[] } {
	const currentScript = process.argv[1];
	const isBunVirtualScript = currentScript?.startsWith("/$bunfs/root/");
	if (currentScript && !isBunVirtualScript && fs.existsSync(currentScript)) {
		return { command: process.execPath, args: [currentScript, ...args] };
	}
	const execName = path.basename(process.execPath).toLowerCase();
	const isGenericRuntime = /^(node|bun)(\.exe)?$/.test(execName);
	if (!isGenericRuntime) return { command: process.execPath, args };
	return { command: "pi", args };
}

async function mapWithConcurrencyLimit<TIn, TOut>(
	items: TIn[],
	concurrency: number,
	fn: (item: TIn, index: number) => Promise<TOut>,
): Promise<TOut[]> {
	if (items.length === 0) return [];
	const limit = Math.max(1, Math.min(concurrency, items.length));
	const results: TOut[] = new Array(items.length);
	let nextIndex = 0;
	const workers = new Array(limit).fill(null).map(async () => {
		while (true) {
			const current = nextIndex++;
			if (current >= items.length) return;
			results[current] = await fn(items[current], current);
		}
	});
	await Promise.all(workers);
	return results;
}

/** Full text of the child's last assistant message (all text blocks joined). */
function finalAssistantText(messages: Message[]): string {
	for (let i = messages.length - 1; i >= 0; i--) {
		const msg = messages[i];
		if (msg.role !== "assistant") continue;
		const texts: string[] = [];
		for (const part of msg.content ?? []) {
			if (part.type === "text") texts.push(part.text);
		}
		if (texts.length > 0) return texts.join("");
	}
	return "";
}

function runChild(
	selfPath: string,
	task: { prompt: string; tools?: string[]; cwd?: string },
	readOnly: boolean,
	defaultCwd: string,
	key: string,
	signal: AbortSignal | undefined,
	index: number,
): Promise<ChildResult> {
	const args = ["--print", "--mode", "json", "--no-session", "-e", selfPath, "--model", `${PROVIDER_NAME}/${SUBAGENT_MODEL}`];

	// Secure-by-default tool gating. Task input is model-influenced, so without an
	// explicit opt-in we never hand a child write/exec tools: model-requested
	// `tools` are stripped of dangerous entries, and readOnly:false's "all tools"
	// is downgraded to read-only. WEAVE_PI_ALLOW_SUBAGENT_TOOLS=1 restores full
	// flexibility. Stops a prompt-injected main loop from escalating a read-only
	// fan-out into arbitrary command/file execution under the user's identity.
	const allowDangerousTools = process.env.WEAVE_PI_ALLOW_SUBAGENT_TOOLS === "1";
	let tools: string[] | undefined;
	if (task.tools && task.tools.length > 0) {
		tools = task.tools;
	} else if (readOnly || !allowDangerousTools) {
		tools = DEFAULT_READONLY_TOOLS;
	} else {
		tools = undefined; // readOnly:false + opt-in => full toolset
	}
	if (tools && !allowDangerousTools) {
		// Trim first: pi's `--tools` parser trims each entry, so " bash" would slip
		// past a non-trimmed check here and then get re-enabled downstream.
		tools = tools.map((t) => t.trim()).filter((t) => t !== "" && !DANGEROUS_SUBAGENT_TOOLS.has(t.toLowerCase()));
		if (tools.length === 0) tools = DEFAULT_READONLY_TOOLS;
	}
	if (tools) args.push("--tools", tools.join(","));
	args.push(task.prompt);

	const identity = resolveIdentity();
	const env: NodeJS.ProcessEnv = {
		...process.env,
		WEAVE_PI_SUBAGENT: "1",
		WEAVE_PI_SUBAGENT_ID: randomUUID(),
		WEAVE_ROUTER_KEY: key,
		WEAVE_ROUTER_URL: getRouterBaseUrl(),
	};
	// Don't let the main loop's per-knob routing overrides leak into children:
	// knobsForRole prefers env values over the role preset, which would route
	// subagents on the main loop's quality knobs instead of the speed/cheap ones.
	delete env.WEAVE_ROUTING_ALPHA;
	delete env.WEAVE_ROUTING_SPEED_WEIGHT;
	delete env.WEAVE_ROUTING_OUTPUT_COST_RATIO;
	delete env.WEAVE_ROUTING_EXPECTED_OUTPUT_TOKENS;
	if (identity.email) env.WEAVE_USER_EMAIL = identity.email;
	if (identity.name) env.WEAVE_USER_NAME = identity.name;

	const result: ChildResult = { index, finalText: "", exitCode: 0 };
	const messages: Message[] = [];

	return new Promise<ChildResult>((resolve) => {
		const invocation = getPiInvocation(args);
		const proc = spawn(invocation.command, invocation.args, {
			cwd: task.cwd ?? defaultCwd,
			shell: false,
			stdio: ["ignore", "pipe", "pipe"],
			env,
		});

		let stdoutBuf = "";
		let stderrBuf = "";
		let settled = false;
		let timedOut = false;

		const processLine = (line: string) => {
			if (!line.trim()) return;
			let event: { type?: string; message?: Message };
			try {
				event = JSON.parse(line);
			} catch {
				return;
			}
			if ((event.type === "message_end" || event.type === "tool_result_end") && event.message) {
				messages.push(event.message);
			}
		};

		proc.stdout.on("data", (data) => {
			stdoutBuf += data.toString();
			const lines = stdoutBuf.split("\n");
			stdoutBuf = lines.pop() ?? "";
			for (const line of lines) processLine(line);
		});
		proc.stderr.on("data", (data) => {
			stderrBuf += data.toString();
		});

		const timer = setTimeout(() => {
			timedOut = true;
			proc.kill("SIGTERM");
			// proc.killed flips true the instant SIGTERM is *sent*, so escalate based
			// on whether the process actually exited (settled), not proc.killed.
			setTimeout(() => {
				if (!settled) proc.kill("SIGKILL");
			}, SIGKILL_GRACE_MS);
		}, SUBAGENT_TIMEOUT_MS);

		const onAbort = () => {
			proc.kill("SIGTERM");
			setTimeout(() => {
				if (!settled) proc.kill("SIGKILL");
			}, SIGKILL_GRACE_MS);
		};
		if (signal) {
			if (signal.aborted) onAbort();
			else signal.addEventListener("abort", onAbort, { once: true });
		}

		const settle = (exitCode: number) => {
			if (settled) return;
			settled = true;
			clearTimeout(timer);
			signal?.removeEventListener("abort", onAbort);
			result.exitCode = exitCode;
			// resolve() must always run — a parse error here would otherwise strand
			// this promise and block the whole dispatch via mapWithConcurrencyLimit.
			try {
				if (stdoutBuf.trim()) processLine(stdoutBuf);
				result.finalText = finalAssistantText(messages);
				result.routedModel = lastRoutedModel(stderrBuf);
				if (exitCode !== 0 && !result.finalText) {
					result.error = timedOut
						? `timed out after ${Math.round(SUBAGENT_TIMEOUT_MS / 1000)}s`
						: signal?.aborted
							? "aborted"
							: lastStderrLine(stderrBuf) || `exited with code ${exitCode}`;
				}
			} catch (err) {
				if (!result.error) result.error = `failed to read subagent output: ${(err as Error).message}`;
			}
			resolve(result);
		};

		// A null exit code means the child was killed by a signal (timeout / abort /
		// crash) — settle as failure (1), not success (0), so a killed subagent with
		// no output can't be counted as succeeded.
		proc.on("close", (code) => settle(code ?? 1));
		proc.on("error", (err) => {
			stderrBuf += `${err.message}\n`;
			settle(1);
		});
	});
}

function lastRoutedModel(stderr: string): string | undefined {
	let found: string | undefined;
	for (const line of stderr.split("\n")) {
		const idx = line.indexOf(ROUTED_MODEL_STDERR_PREFIX);
		if (idx !== -1) found = line.slice(idx + ROUTED_MODEL_STDERR_PREFIX.length).trim();
	}
	return found || undefined;
}

function lastStderrLine(stderr: string): string {
	const lines = stderr
		.split("\n")
		.map((l) => l.trim())
		.filter(Boolean);
	return lines.length > 0 ? lines[lines.length - 1] : "";
}

export function registerDispatch(pi: ExtensionAPI, selfPath: string): void {
	pi.registerTool({
		name: "dispatch",
		label: "Dispatch",
		description: [
			"Run tasks as parallel, context-isolated subagents (separate pi processes).",
			"Each subagent has its own context window and routes through the Weave Router on speed/cheap knobs;",
			"only its final answer returns here, so intermediate work never bloats this context.",
			"Subagents are read-only by default (read, grep, find, ls); pass per-task `tools` to widen.",
			"Use for fan-out investigation/search across independent questions.",
		].join(" "),
		parameters: DispatchParams,
		executionMode: "parallel",

		async execute(_toolCallId, params, signal, _onUpdate, ctx) {
			const key = resolveRouterKey();
			if (!key) {
				return {
					content: [
						{ type: "text", text: "Weave dispatch unavailable: no router key (set WEAVE_ROUTER_KEY or run the --pi installer)." },
					],
					details: { results: [] as ChildResult[] },
					isError: true,
				};
			}

			const readOnly = params.readOnly ?? true;
			const results = await mapWithConcurrencyLimit(params.tasks, DISPATCH_CONCURRENCY, (task, index) =>
				runChild(selfPath, task, readOnly, ctx.cwd, key, signal, index),
			);

			const succeeded = results.filter((r) => r.exitCode === 0 && !r.error).length;
			const blocks = results.map((r) => {
				const tags = [r.routedModel ? `routed: ${r.routedModel}` : null, r.exitCode === 0 && !r.error ? null : "FAILED"]
					.filter(Boolean)
					.join(" · ");
				const header = `## Subagent ${r.index + 1}${tags ? ` (${tags})` : ""}`;
				const body = r.error ? `Error: ${r.error}` : r.finalText || "(no output)";
				return `${header}\n${body}`;
			});

			return {
				content: [{ type: "text", text: `${succeeded}/${results.length} subagents succeeded\n\n${blocks.join("\n\n")}` }],
				details: { results },
				isError: succeeded === 0,
			};
		},

		renderCall(args, theme) {
			const tasks = args.tasks ?? [];
			let text = `${theme.fg("toolTitle", theme.bold("dispatch "))}${theme.fg("accent", `${tasks.length} subagent${tasks.length === 1 ? "" : "s"}`)}`;
			for (const t of tasks.slice(0, 3)) {
				const preview = t.prompt.length > 60 ? `${t.prompt.slice(0, 60)}...` : t.prompt;
				text += `\n  ${theme.fg("dim", preview)}`;
			}
			if (tasks.length > 3) text += `\n  ${theme.fg("muted", `… +${tasks.length - 3} more`)}`;
			return new Text(text, 0, 0);
		},
	});
}
