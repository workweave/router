---
description: Route Claude Code directly to Anthropic again (turn the Weave Router off)
allowed-tools: Bash(npx:*)
---

Turn the Weave Router **off** for Claude Code by running:

`npx @workweave/router off --claude{{SCOPE}}`

Then tell me to fully quit and reopen Claude Code — it only reads the router setting at startup, so the change won't apply to the current session until it restarts.
