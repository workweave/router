/**
 * EXPERIMENTAL, off by default (enable with WEAVE_CHEAP_COMPACTION=1).
 *
 * Intent: run context compaction through the cheapest routing knobs. The
 * catch is that compaction bypasses provider hooks and reuses the session's
 * static provider headers (the main loop's quality knobs), and
 * `session_before_compact` is the only per-turn lever. Producing a fully
 * router-routed cheap CompactionResult here is not yet validated, so this
 * handler currently defers to pi's built-in compaction (returns no override)
 * and only reserves the flag + surfaces that it's active. Promote to a real
 * cheap path once validated end-to-end.
 */

import type { ExtensionAPI, ExtensionContext, SessionBeforeCompactEvent } from "@mariozechner/pi-coding-agent";

export function registerCheapCompaction(pi: ExtensionAPI): void {
	let announced = false;
	pi.on("session_before_compact", (_event: SessionBeforeCompactEvent, ctx: ExtensionContext) => {
		if (ctx.hasUI && !announced) {
			announced = true;
			ctx.ui.setStatus("weave-compaction", "cheap compaction: experimental (using built-in)");
		}
		// Defer to built-in compaction until the router-routed cheap path is validated.
		return undefined;
	});
}
