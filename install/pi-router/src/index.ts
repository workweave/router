/**
 * @workweave/router — route the pi coding agent through the Weave Router.
 *
 * Wiring (all on the existing router surface — no router source change beyond
 * the installer):
 *   - provider:     register `weave` with per-process knob headers (quality on
 *                   the main loop, speed/cheap in subagents).
 *   - metadata:     stamp body.metadata.user_id for sticky sessions + subagent
 *                   detection.
 *   - routed-model: show which model the router actually picked.
 *   - safety:       block catastrophic bash (unless WEAVE_NO_SAFETY=1).
 *   - compaction:   experimental cheap path (only when WEAVE_CHEAP_COMPACTION=1).
 *   - dispatch:     parallel, context-isolated subagents — top-level process
 *                   only (no grandchildren).
 *
 * The same module loads in dispatched children via `-e <self>`; WEAVE_PI_SUBAGENT
 * flips the provider knobs and suppresses the dispatch tool so fan-out doesn't recurse.
 */

import { fileURLToPath } from "node:url";
import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import { isSubagent } from "./config.js";
import { registerCheapCompaction } from "./compaction.js";
import { registerDispatch } from "./dispatch.js";
import { registerMetadata } from "./metadata.js";
import { registerRoutedModel } from "./routed-model.js";
import { registerSafety } from "./safety.js";
import { registerWeave } from "./provider.js";

const SELF_PATH = fileURLToPath(import.meta.url);

export default function (pi: ExtensionAPI): void {
	// Register at load so the provider is available for `--list-models` and
	// print mode (dispatched children), and again on session_start so the right
	// knob headers survive `/reload` and new/resumed sessions.
	registerWeave(pi);
	pi.on("session_start", () => registerWeave(pi));

	registerMetadata(pi);
	registerRoutedModel(pi);

	if (process.env.WEAVE_NO_SAFETY !== "1") registerSafety(pi);
	if (process.env.WEAVE_CHEAP_COMPACTION === "1") registerCheapCompaction(pi);

	// Only the top-level process fans out. Children (WEAVE_PI_SUBAGENT=1) load
	// this same extension but get no dispatch tool, so subagents can't spawn
	// grandchildren.
	if (!isSubagent()) registerDispatch(pi, SELF_PATH);
}
