/**
 * Injects `metadata.user_id` into the request body so the router can:
 *   - keep the main loop on a sticky session pin ("pi:<sessionId>"), and
 *   - detect subagents ("subagent:<uuid>") for an independent pin + server-side
 *     SubAgentDispatch handling.
 *
 * This is the one body-level signal we control (the session pin key derives
 * from metadata.user_id when present; subagent detection on the Anthropic
 * ingress path keys off a "subagent:" prefix). Headers can't carry it because
 * before_provider_request can't set headers.
 */

import { randomUUID } from "node:crypto";
import type { ExtensionAPI, ExtensionContext } from "@mariozechner/pi-coding-agent";
import { isSubagent } from "./config.js";

// One id per child process so all of a subagent's requests share a single pin.
// The parent passes WEAVE_PI_SUBAGENT_ID; a standalone child falls back to a uuid.
const SUBAGENT_USER_ID = `subagent:${process.env.WEAVE_PI_SUBAGENT_ID?.trim() || randomUUID()}`;

export function registerMetadata(pi: ExtensionAPI): void {
	pi.on("before_provider_request", (event, ctx: ExtensionContext) => {
		const body = event.payload as { metadata?: { user_id?: string } } | undefined;
		if (!body || typeof body !== "object") return undefined;

		const userId = isSubagent() ? SUBAGENT_USER_ID : `pi:${ctx.sessionManager.getSessionId()}`;
		if (body.metadata?.user_id === userId) return undefined;

		body.metadata = { ...(body.metadata ?? {}), user_id: userId };
		return body;
	});
}
