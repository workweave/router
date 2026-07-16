---
name: test-claude-locally
description: Run the Weave router locally in docker compose and drive it with `claude -p` to reproduce and verify routing/translation behavior for a specific upstream model (e.g. GLM-5.1, DeepSeek, Qwen). Use when verifying a router fix end-to-end, reproducing a prod routing bug, confirming a model's streaming behavior (nudges, tool-call suppression, loop/no-progress breaks), or testing a `/force-model` route — without touching the user's global Claude Code config.
---

# Testing the router locally

Stand up the router in docker compose, point a one-off `claude -p` session at it via `--settings`, and read the local server logs to confirm behavior. Two upstream modes: the **real** provider API (needs a working key + credits) or a **mock** upstream that emits an exact SSE shape (deterministic, no credits).

## Critical gotchas (read first)

- **`~/.claude/settings.json` `env` overrides inherited env vars.** Setting `ANTHROPIC_BASE_URL` in the shell does NOT redirect `claude` — settings.json wins and the request silently goes to prod. Always redirect with `claude --settings <file>` (see `scripts/local-settings.json`). Never edit the user's global `~/.claude.json` or `~/.claude/settings.json` — that breaks their live session.
- **`claude -p` is stateless across invocations.** A standalone `/force-model` call does not persist to the next `claude -p`. Put `/force-model <model>` as the first line of the SAME prompt that contains the task.
- **The router ignores the request's `model` field** and routes via the cluster scorer. Two ways to pin a model: (a) `/force-model <model>` as the leading line of the last user message — but that turn returns a synthetic ack and does NOT forward upstream, so the pin only affects a *later* turn on the same session key; (b) the **`x-weave-force-model: <model>` request header**, which pins AND forwards on the SAME turn (built for headless/CI). For raw-curl testing prefer the header — it works without a Claude Code session and needs only one request. Aliases like `fable-5`, `haiku`, `sonnet-5` resolve to canonical IDs (see `forceModelAliases` in `internal/proxy/force_model.go`).
- **Port 8085 conflict.** The monorepo's pubsub emulator may already own host port 8085. Drop the router's host binding with a `docker-compose.override.yml` (see workflow). The server still reaches the emulator over the compose network.
- **No credits / no key = no reproduction.** If the real upstream returns an error (e.g. OpenRouter "Insufficient credits"), use the mock-upstream path instead.
- **To assert on the exact bytes the router FORWARDS upstream** (not just its response), point the provider base URL at a capture mock that logs the request body. This is the cleanest way to verify a `internal/translate` emit change end-to-end.
- **Anthropic upstream base URL is NOT env-configurable by default.** `cmd/router/main.go` passes `anthropic.DefaultBaseURL` literally (unlike OpenAI/Fireworks/etc. which read `<PROVIDER>_BASE_URL`). To redirect Anthropic to a mock, apply a temporary 1-line edit: `anthropic.NewClient(anthropicKey, config.GetOr("ANTHROPIC_BASE_URL", anthropic.DefaultBaseURL))`, then set `ANTHROPIC_BASE_URL` + `ANTHROPIC_API_KEY=sk-mock` (main.go panics if the Anthropic key is unset). Revert the edit in cleanup. The boot log line still prints the hardcoded `api.anthropic.com` (cosmetic) — confirm the redirect via the capture file, not the log.

## Workflow

```
- [ ] 1. Bring up the stack (handle port 8085)
- [ ] 2. Seed an API key
- [ ] 3. Choose upstream: real provider OR mock
- [ ] 4. Write a one-off local-settings.json
- [ ] 5. Drive with `claude -p --settings`, forcing the target model
- [ ] 6. Read local logs to verify behavior
- [ ] 7. Clean up
```

### 1. Bring up the stack

```bash
cd <router-repo>
# Drop the pubsub host-port binding to avoid an 8085 conflict:
cat > docker-compose.override.yml <<'EOF'
services:
  pubsub-emulator:
    ports: !reset []
EOF
docker compose up -d --build server   # --build picks up code changes
until curl -sf http://localhost:8080/health >/dev/null; do sleep 2; done
```

The override file is gitignored-by-intent scaffolding — delete it in cleanup.

### 2. Seed an API key

```bash
docker compose run --rm seed
```

Copy the `rk_...` key it prints.

### 3. Choose the upstream

**Real provider** — set the provider key in `.env.local` (e.g. `FIREWORKS_API_KEY=...`) and restart `docker compose up -d server`. Confirm the boot log shows `<Provider> provider enabled` with the real base_url. Use this to confirm a model genuinely produces the behavior.

**Mock upstream** — for a deterministic, credit-free repro of a precise SSE shape. Point the provider's base URL at a local mock and restart:

```bash
python3 scripts/mock_openai_upstream.py >/tmp/mock.log 2>&1 &   # serves :8099
# In docker-compose.override.yml under `server:`, add:
#   environment:
#     FIREWORKS_BASE_URL: http://host.docker.internal:8099/v1
#     FIREWORKS_API_KEY: sk-mock
#   extra_hosts: ["host.docker.internal:host-gateway"]
docker compose up -d server
```

Edit the mock's emitted chunks to match the upstream shape you're reproducing. Provider→env-var names live in `internal/providers/provider.go`; base-URL overrides are read in `cmd/router/main.go` (`<PROVIDER>_BASE_URL`).

### 4. One-off local settings

```bash
cat > /tmp/local-settings.json <<EOF
{ "env": {
  "ANTHROPIC_BASE_URL": "http://localhost:8080",
  "ANTHROPIC_CUSTOM_HEADERS": "X-Weave-Router-Key: rk_REPLACE_ME"
}}
EOF
```

### 5. Drive it

```bash
cd <scratch-dir-with-files-to-act-on>
env -u CLAUDE_CODE_SESSION_ID -u ANTHROPIC_BASE_URL -u ANTHROPIC_CUSTOM_HEADERS \
  claude -p 'First send exactly: /force-model z-ai/glm-5.1
Then <task that requires tool use>, then stop.' \
  --settings /tmp/local-settings.json --max-turns 10 --verbose
```

### 6. Verify via logs

The decision + completion log per turn carries the signals you need. Strip ANSI first:

```bash
docker compose logs server --since=3m 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g' \
  | grep 'ProxyMessages complete' | grep 'decision_model=z-ai/glm-5.1'
```

Useful fields: `decision_model`, `decision_provider`, `upstream_finish_reason`, `suppressed_tool_calls`, `text_only_turn_nudged`, `tool_use_blocks`, `resp_stop_reason`, `stop_reason_demoted`. Also grep for `recovery nudge`, `tool-call loop`, `no-progress`.

**Confirm the model was actually served** before drawing conclusions:
```bash
docker compose logs server --since=3m 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g' \
  | grep 'ProxyMessages complete' | grep -oE 'decision_model=[^ ]+' | sort | uniq -c
```
If you only see other models, `/force-model` didn't take — recheck step 5.

### 7. Clean up

```bash
pkill -f mock_openai_upstream.py 2>/dev/null
rm -f docker-compose.override.yml /tmp/local-settings.json
# `docker compose down` if you want to stop the stack
```

## Recipe: capture the forwarded wire body (raw curl, no Claude Code)

Best for verifying an `internal/translate` emit change (e.g. thinking/effort rewrites). Deterministic, credit-free, single request.

```bash
# 1. Capture mock: logs the exact forwarded request body, returns a minimal 200.
#    (For Anthropic, return an Anthropic Messages JSON; for OpenAI-compat, a chat.completion.)
# 2. Redirect the provider base URL to it. Anthropic needs the temp main.go edit
#    (see gotchas); OpenAI-compat providers read <PROVIDER>_BASE_URL natively.
# 3. Bring up + seed a key (steps 1-2 of the workflow), then:
curl -s -XPOST http://localhost:8080/v1/messages \
  -H 'content-type: application/json' \
  -H 'x-weave-router-key: rk_...' \
  -H 'x-weave-force-model: claude-fable-5' \
  -d '{"model":"claude-sonnet-4-6","max_tokens":64,"stream":false,
       "thinking":{"type":"disabled"},"messages":[{"role":"user","content":"say hi"}]}'
# 4. Assert on the captured body (what the router sent upstream), not the mock's reply.
```

For a **before/after contrast**, run once on your branch, then `git show origin/main:<file> > /tmp/x && cp /tmp/x <file>`, rebuild, rerun, and restore. Diff the captured bodies.

## Notes

- Local cluster version comes from `ROUTER_CLUSTER_VERSION` in `.env.local`; it may differ from prod, which is why `/force-model` (not the scorer) is the reliable way to hit one model.
- GLM-5.1's primary binding is Together (then Fireworks, then OpenRouter) — see `internal/router/catalog/catalog.go`.
- To confirm a deploy contains a given router commit: the prod Cloud Run revision name maps to a monorepo commit; `git ls-tree <monorepo-commit> router-internal/router` shows the pinned router submodule SHA.
