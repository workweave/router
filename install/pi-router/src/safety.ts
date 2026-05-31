/**
 * Last-resort guard against a handful of catastrophic, effectively
 * irreversible shell commands. pi already confirms normal tool calls; this
 * matters most in non-interactive subagents where there is no human to
 * confirm. It blocks ONLY the obviously-destructive forms below — it is a
 * backstop, not a sandbox. Disable with WEAVE_NO_SAFETY=1.
 */

import { type ExtensionAPI, type ExtensionContext, isToolCallEventType, type ToolCallEvent } from "@mariozechner/pi-coding-agent";

interface Rule {
	test: (cmd: string) => boolean;
	reason: string;
}

const hasRecursive = (c: string) => /(?:\s|^)-\w*r/i.test(c) || /--recursive\b/.test(c);
const hasForce = (c: string) => /(?:\s|^)-\w*f/i.test(c) || /--force\b/.test(c);
const targetsRoot = (c: string) =>
	/--no-preserve-root\b/.test(c) || /(?:\s|^)(?:\/|~|\$HOME|\/\*)(?:\s|$)/.test(c);

const RULES: Rule[] = [
	{
		test: (c) => /\brm\b/.test(c) && hasRecursive(c) && hasForce(c) && targetsRoot(c),
		reason: "recursive force-remove of a root/home path",
	},
	{
		test: (c) => /\bmkfs(\.\w+)?\b/.test(c),
		reason: "filesystem format (mkfs)",
	},
	{
		test: (c) => /\bdd\b[^\n]*\bof=\/dev\//.test(c),
		reason: "dd writing directly to a device",
	},
	{
		test: (c) => />\s*\/dev\/(?:sd|nvme|disk|hd|mapper)/.test(c),
		reason: "redirect overwriting a raw block device",
	},
	{
		test: (c) => /:\s*\(\s*\)\s*\{[^}]*\|[^}]*&[^}]*\}\s*;\s*:/.test(c),
		reason: "fork bomb",
	},
	{
		test: (c) =>
			/\bgit\s+push\b/.test(c) &&
			(/(?:--force(?!-with-lease)|\s-f\b)/.test(c) || /\+(?:main|master)\b/.test(c)) &&
			/\b(?:main|master)\b/.test(c),
		reason: "force-push to main/master",
	},
];

function catastrophicReason(command: string): string | undefined {
	const cmd = command.trim();
	for (const rule of RULES) {
		if (rule.test(cmd)) return rule.reason;
	}
	return undefined;
}

export function registerSafety(pi: ExtensionAPI): void {
	pi.on("tool_call", (event: ToolCallEvent, _ctx: ExtensionContext) => {
		if (!isToolCallEventType("bash", event)) return undefined;
		const reason = catastrophicReason(event.input.command ?? "");
		if (!reason) return undefined;
		return { block: true, reason: `Weave safety: blocked ${reason}. Set WEAVE_NO_SAFETY=1 to override.` };
	});
}
