# @workweave/pi-router

A [pi](https://pi.dev) extension that routes every request through the
[WorkWeave Router](https://github.com/workweave/router) — a trained, per-request
LLM proxy that picks the most cost-efficient model that still solves each task.

Installed automatically by the Weave Router installer:

```bash
WEAVE_ROUTER_KEY=rk_… npx -y @workweave/router --pi          # user scope
WEAVE_ROUTER_KEY=rk_… npx -y @workweave/router --pi --local  # local router (http://localhost:8080)
```

That writes `~/.pi/agent/models.json` (the `weave` provider), adds
`npm:@workweave/pi-router` to `~/.pi/agent/settings.json` `packages`, and stores
the key in `~/.pi/agent/.weave_router_key`. pi auto-installs this package from
npm on next start.

## What it does

- **Automatic model selection.** All pi traffic flows through the router, which
  selects the model per request. You don't pick a model — the router does.
- **Per-process routing bias.** Static `x-weave-routing-*` knob headers bias the
  router: quality on the main loop, speed + cheap on subagents, cheapest on
  compaction.
- **Sticky sessions.** `metadata.user_id = "pi:<sessionId>"` pins the main loop
  to one model for the session; subagents get their own pins.
- **`dispatch` tool — parallel, context-isolated subagents.** pi has none
  natively. `dispatch` spawns child `pi` processes (read-only by default), runs
  them concurrently, and returns only each subagent's final answer — intermediate
  tool output stays in the child, so the main context stays small.
- **Routed-model display.** Shows the model the router actually chose
  (`x-router-model`) in the status bar, and opts the request out of the router's
  in-band routing badge (`X-Weave-Routing-Marker: off`) — pi can't render that
  separate marker text block inline, and the status bar already conveys the model.
- **Safety backstop.** Blocks a few catastrophic shell commands (`rm -rf /`,
  `mkfs`, `dd of=/dev/…`, fork bombs, force-push to main). Disable with
  `WEAVE_NO_SAFETY=1`.

## Configuration (environment)

| Variable | Default | Purpose |
|---|---|---|
| `WEAVE_ROUTER_URL` | `http://localhost:8080` | Router base URL (children inherit it) |
| `WEAVE_ROUTER_KEY` | — | Router key (else read from `.weave_router_key`) |
| `WEAVE_ROUTER_KEY_FILE` | `<agentDir>/.weave_router_key` | Override key file path |
| `WEAVE_USER_EMAIL` / `WEAVE_USER_NAME` | from `git config` | Identity headers for attribution |
| `WEAVE_PI_SUBAGENT_MODEL` | `claude-sonnet-4-6` | `weave/<model>` handle children launch with (router re-routes) |
| `WEAVE_PI_DISPATCH_CONCURRENCY` | `4` | Max concurrent subagents |
| `WEAVE_PI_SUBAGENT_TIMEOUT_MS` | `600000` | Per-subagent timeout |
| `WEAVE_PI_ALLOW_SUBAGENT_TOOLS` | unset | `1` lets `dispatch` grant subagents write/exec tools (bash, write, edit); default strips them |
| `WEAVE_ROUTING_ALPHA` / `…_SPEED_WEIGHT` / `…_OUTPUT_COST_RATIO` / `…_EXPECTED_OUTPUT_TOKENS` | role preset | Override individual routing knobs (main process only — children always use their role preset) |
| `WEAVE_NO_SAFETY` | unset | `1` disables the catastrophic-bash gate |
| `WEAVE_CHEAP_COMPACTION` | unset | `1` enables the (experimental) cheap-compaction path |

Internal: `WEAVE_PI_SUBAGENT=1` and `WEAVE_PI_SUBAGENT_ID` are set by `dispatch`
on child processes; don't set them yourself.

## Billing

Routing through the router switches pi from Claude **subscription OAuth** to
**per-token** billing on the router deployment's key (or your BYOK key). BYOK
skips cross-provider failover; deployment-key billing is the default.

## Notes

- Cheap compaction is currently a reserved flag — the handler defers to pi's
  built-in compaction until the router-routed path is validated end-to-end.
