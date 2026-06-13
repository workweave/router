/**
 * Registers the `weave` provider with the per-process header policy.
 *
 * Re-registered on each `session_start` (and once at load) so the right knob
 * headers are always live and the provider survives `/reload`. We register a
 * new provider named "weave" rather than overriding the built-in "anthropic"
 * provider — overriding "anthropic" would hijack the Claude OAuth token.
 */

import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import {
	getRole,
	getRouterBaseUrl,
	isSubagent,
	PROVIDER_NAME,
	providerHeaders,
	resolveRouterKey,
	WEAVE_MODELS,
} from "./config.js";

export function registerWeave(pi: ExtensionAPI): void {
	const key = resolveRouterKey();
	const role = getRole();

	if (!key) {
		// The main loop can still run off the installer-written models.json
		// provider. A subagent MUST apply the speed/cheap knobs, so a missing
		// key there is fatal rather than silently routing on quality knobs.
		if (isSubagent()) {
			throw new Error(
				"Weave Router: no router key found (set WEAVE_ROUTER_KEY or write ~/.pi/agent/.weave_router_key).",
			);
		}
		return;
	}

	pi.registerProvider(PROVIDER_NAME, {
		name: "Weave Router",
		// Root URL, no /v1: the anthropic-messages provider uses @anthropic-ai/sdk,
		// which appends /v1/messages to baseUrl. A /v1 here yields /v1/v1/messages.
		baseUrl: getRouterBaseUrl(),
		// Planted to satisfy pi's "is auth configured" check. The router ignores
		// it (auth runs off X-Weave-Router-Key); authHeader:false keeps
		// Authorization free for BYOK.
		apiKey: key,
		api: "anthropic-messages",
		authHeader: false,
		headers: providerHeaders(role, key),
		models: WEAVE_MODELS,
	});
}
