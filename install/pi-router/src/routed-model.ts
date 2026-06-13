/**
 * Surfaces which model the router actually picked for each request.
 *
 * The router sets `x-router-model` on every response (streaming, non-streaming,
 * and cache hits). In the interactive UI we show it in the status bar and
 * notify on change. In a headless child (print/RPC — e.g. a dispatch subagent)
 * there is no UI, so we print a marker to stderr that the parent dispatch tool
 * parses to attribute each subagent's work to a model.
 */

import type { ExtensionAPI, ExtensionContext } from "@mariozechner/pi-coding-agent";
import { ROUTED_MODEL_HEADER, ROUTED_MODEL_STDERR_PREFIX } from "./config.js";

const STATUS_KEY = "weave";

export function registerRoutedModel(pi: ExtensionAPI): void {
	let last: string | undefined;

	pi.on("after_provider_response", (event, ctx: ExtensionContext) => {
		const model = event.headers?.[ROUTED_MODEL_HEADER];
		if (!model || model === last) return;
		last = model;

		if (ctx.hasUI) {
			ctx.ui.setStatus(STATUS_KEY, `routed: ${model}`);
			ctx.ui.notify(`Weave Router routed to ${model}`, "info");
		} else {
			process.stderr.write(`${ROUTED_MODEL_STDERR_PREFIX} ${model}\n`);
		}
	});
}
