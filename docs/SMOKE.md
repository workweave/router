# Router pre-merge smoke suite

The smoke suite boots the real router (docker compose stack) and drives it with
deterministic, Claude-Code-shaped request fixtures, asserting the behavior that
in-process unit and conformance tests cannot see: HTTP status, response/usage
shape, prompt-cache accounting, decision headers, and (via a real gpt-5.x call)
tool-schema translation to OpenAI's structured-output format.

It exists because the regression class it targets is invisible to `go test`. Two
concrete examples that motivated it:

- #820 turned on router `cache_control` breakpoint injection for the
  Anthropic→Anthropic path and could emit breakpoint combinations that only the
  *real* Anthropic API rejects (a 5th breakpoint past the 4-cap, or a router `5m`
  breakpoint ordered before a client `ttl=1h` one → hard 400). #821 fixed it hours
  later. The conformance suite stops at `proxy.Service` with a mock upstream, so it
  never observed the 400.
- A tool with a genuinely typeless optional parameter (no `type`/`anyOf`/`enum`
  at all — by design, meaning "accept any JSON value") 400'd against the real
  OpenAI Responses API: `strictifyOpenAISchema`'s nullable-wrapping fallback
  wrapped the bare node in an invalid `anyOf` without checking it carried a
  strict-expressible type first. Caught unit-level in
  `internal/translate/strictify_openai_test.go`, and end-to-end in
  `smoke/openai_test.go` — the unit test proves the translator produces the
  right JSON; the smoke scenario proves the real API actually accepts it.

## Architecture: a record/replay proxy sits between the router and its providers

`smoke/mitmproxy/` is a small MITM (man-in-the-middle) forward proxy. The
router's HTTP transport already honors `http.ProxyFromEnvironment`
(`internal/providers/httputil`), so pointing the `server` container at it via
`HTTPS_PROXY` — and trusting its ephemeral CA via `SSL_CERT_DIR` — intercepts
every outbound call with **zero router code changes**. It mints a TLS leaf cert
per CONNECT-target hostname, so it's not Anthropic-specific: the same proxy
intercepts calls to `api.anthropic.com` and `api.openai.com` alike.

Three modes (`SMOKE_PROXY_MODE`):

| Mode | What it does | Needs a key? |
|---|---|---|
| `replay-only` (CI default) | Serves cassettes committed under `smoke/mitmproxy/cassettes/`; a cache miss is a clean 502, not a hang | No |
| `record` | Always calls the real API and (re)writes cassettes | Yes |
| `replay-or-record` (local default) | Serves from cache, falls back to live + record on a miss | Only for the first run of a new scenario |

Cassettes are keyed by `sha256(method + path + body)`. The fixtures are
byte-deterministic (`smoke/fixtures/system_prompt.txt` never changes), so a
given scenario hashes identically run to run — this is what makes `replay-only`
CI runs deterministic and free. Response headers are sanitized before a
cassette is written (`Authorization` / `x-api-key` / org identifiers / rate-limit
and request-id noise never get persisted), so it's safe for these files to be
committed and reviewed in a normal PR diff.

This means the CI job needs **no provider API keys at all** for its normal
path-gated run — it replays what's already checked in. Keys are only needed to
*record*, which happens locally or in a scheduled nightly refresh.

## When it runs

- **Not on every PR.** The CI job (`.github/workflows/smoke.yml`) is path-gated to
  the regression-prone surfaces: `internal/proxy/**`, `internal/translate/**`,
  `internal/providers/**`, `internal/router/catalog/**`, `cmd/router/**`,
  `smoke/**`, `docker-compose.yml`, `Dockerfile`. Docs, artifacts, the HMM
  sidecar, and the frontend never trigger it. It runs in `replay-only` mode —
  no secret needed.
- **On demand** via the workflow's `workflow_dispatch` button.
- **Locally** before merging a risky router change, or to refresh cassettes:
  `make smoke` (replay-only by default) or
  `ANTHROPIC_API_KEY=… SMOKE_PROXY_MODE=record make smoke` (real API, updates
  cassettes).

## Running locally

```bash
make smoke                                          # replay-only, no key needed
ANTHROPIC_API_KEY=sk-ant-… SMOKE_PROXY_MODE=record make smoke   # refresh Anthropic cassettes
ANTHROPIC_API_KEY=sk-ant-… OPENAI_API_KEY=sk-… SMOKE_PROXY_MODE=record make smoke   # refresh both providers' cassettes
```

That runs `scripts/smoke/run.sh`, which:

1. Writes an ephemeral `docker-compose.override.yml` that drops the pubsub
   `8085` host binding (avoids a clash with the monorepo's own emulator) and
   sets the router's own `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` (harmless
   placeholders in `replay-only` mode — no request reaches a real provider —
   or the real keys in `record`/`replay-or-record`).
2. `docker compose -f docker-compose.yml -f smoke/mitmproxy/docker-compose.yml
   up -d --build server mitmproxy` and waits for `/health`.
3. `docker compose run --rm seed` and parses the `rk_…` router key.
4. `go test -tags smoke -count=1 -v ./smoke/`.
5. On failure, dumps the last ~150 ANSI-stripped `server` + `mitmproxy` log
   lines (the `ProxyMessages complete` and `mitmproxy: … key=…` lines are the
   payload). Then tears the stack down.

Iterating on a scenario? Keep the stack up between runs:

```bash
SMOKE_KEEP_STACK=1 make smoke
# ...edit a scenario...
SMOKE_ROUTER_KEY=rk_… go test -tags smoke -count=1 -v ./smoke/ -run TestCaching
# tear down when done:
docker compose -f docker-compose.yml -f smoke/mitmproxy/docker-compose.yml down -v
rm -f docker-compose.override.yml
```

## Cost

`replay-only` runs make zero upstream calls. `record`/`replay-or-record` pin
every Anthropic scenario to the cheapest model (`claude-haiku-4-5`) and every
OpenAI scenario to the cheapest reasoning tier (`gpt-5.4-nano`), both via
`x-weave-force-model`, and cap `max_tokens` — a full refresh is ~15 real calls
across both providers, a few cents. Skip recording OpenAI by omitting
`OPENAI_API_KEY`; `smoke/openai_test.go` skips itself
(`SMOKE_OPENAI_ENABLED=0`, set automatically by `run.sh` in that case).

## What it covers

| File | Scenario |
|---|---|
| `smoke/boot_test.go` | `/health`, `/v1/version`, `/v1/router/models` respond and are well-formed |
| `smoke/basic_test.go` | `/force-model` command turn; non-stream turn (usage + decision headers); streamed turn well-ordered |
| `smoke/cache_test.go` | router-injected caching warms then reads; client-at-capacity doesn't over-inject; `ttl=1h` breakpoint not poisoned; overflow rejected cleanly by the router |
| `smoke/streaming_test.go` | tool-use stream lifecycle: balanced `content_block_start/stop`, exactly one `message_stop`, `stop_reason` present |
| `smoke/openai_test.go` | OpenAI Responses-API translation path (gpt-5.x + tools): a genuinely typeless optional tool param round-trips without a 400; basic turn served correctly |

## Regression proof

The suite is built to catch the #820 class. To confirm it does, revert the fix
and watch it fail (#821 lived entirely in `cache_control.go`):

```bash
# Restore the pre-#821 cache_control.go (the over-injecting version):
git show 3551eed7:internal/translate/cache_control.go > internal/translate/cache_control.go
make smoke   # replay-only against the existing cassettes: TestCaching capacity/ttl scenarios FAIL
git checkout internal/translate/cache_control.go
make smoke   # green again
```

## Adding a scenario

1. Build the request with `newRequest(userID)` in `smoke/request_builder_test.go`
   — chain `.tokens()`, `.streaming()`, `.sysCache()`, `.msgCache()`,
   `.toolCache()`, `.cachedTools()`, `.withTool()` etc. to shape breakpoints,
   turn size, and the tool registry. The large stable prefix
   (`smoke/fixtures/system_prompt.txt`) is prepended automatically so caching
   engages.
2. Add a `t.Run(...)` subtest to the relevant `*_test.go`, using `call(t, body)`
   (Anthropic, the suite-wide pin) or `callModel(t, body, model)` (any other
   model/provider — see `smoke/openai_test.go`) and the shared assertions
   (`requireOKMessage`, `assertServedByPin`/`assertServedByModel`,
   `assertStreamWellFormed`).
3. Keep it cheap: pin the cheap tier for whichever provider you're targeting,
   cap `max_tokens`, avoid multi-turn loops.
4. Record the new cassette: `ANTHROPIC_API_KEY=… [OPENAI_API_KEY=…]
   SMOKE_PROXY_MODE=record make smoke`, then review and commit the new
   file(s) under `smoke/mitmproxy/cassettes/`.

## Refreshing cassettes

Cassettes go stale when a fixture, scenario, or a provider's own response shape
changes. See `smoke/mitmproxy/cassettes/README.md`. Refresh with:

```bash
ANTHROPIC_API_KEY=sk-ant-… OPENAI_API_KEY=sk-… SMOKE_PROXY_MODE=record make smoke
git status smoke/mitmproxy/cassettes/   # review the diff, then commit
```

## Config (env)

| Var | Default | Meaning |
|---|---|---|
| `SMOKE_PROXY_MODE` | `replay-only` | `replay-only` \| `record` \| `replay-or-record` |
| `ANTHROPIC_API_KEY` | — | required only when `SMOKE_PROXY_MODE` isn't `replay-only` |
| `OPENAI_API_KEY` | — | optional even in `record` mode — omit to skip recording/refreshing the OpenAI-path scenarios |
| `SMOKE_PIN_MODEL` | `claude-haiku-4-5` | Anthropic model the default scenarios force |
| `SMOKE_OPENAI_PIN_MODEL` | `gpt-5.4-nano` | OpenAI model `smoke/openai_test.go` forces |
| `SMOKE_BASE_URL` | `http://localhost:8080` | router base URL |
| `SMOKE_KEEP_STACK` | `0` | leave the stack up after the run |
| `SMOKE_CI_CACHE` | `0` | layer-cache the server/mitmproxy builds via the GitHub Actions cache backend. **CI-only** — set only by `.github/workflows/smoke.yml`; hard-errors outside a real GitHub Actions runner, never set locally |

## CI build caching

The router's own `Dockerfile` is expensive to build cold: ONNX runtime +
tokenizer downloads, a Next.js build for the mini UI, then a CGO-linked Go
compile. `.github/workflows/smoke.yml` sets `SMOKE_CI_CACHE=1`, which makes
`run.sh` add a fourth compose overlay,
`smoke/mitmproxy/docker-compose.ci-cache.yml`, layering `cache_from`/`cache_to:
type=gha,mode=max` onto both the `server` and `mitmproxy` builds.

That overlay is deliberately **separate** from the always-on
`smoke/mitmproxy/docker-compose.yml` — `type=gha` needs
`ACTIONS_CACHE_URL`/`ACTIONS_RUNTIME_TOKEN`, which only exist inside an actual
GitHub Actions run, and it hard-errors (not silently skips) without them. Never
add `cache_from`/`cache_to: type=gha` to a compose file `make smoke` loads by
default.

Expected effect: a cold build (new runner, or a change touching the
Dockerfile/go.mod) still pays the full ~8-12 minutes. A warm-cache PR run
(most PRs — only the Go source under `internal/`/`cmd/` changed) drops to low
single-digit minutes, since `mode=max` caches every intermediate build stage,
not just the final layer.

## CI secret

The normal path-gated PR run needs **no secrets** — it replays committed
cassettes. `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` are only needed for a manual
`workflow_dispatch` run with `SMOKE_PROXY_MODE=record` (or a future scheduled
nightly refresh), and only maintainers of this repo can set those secrets, so
fork PRs are naturally locked out of ever recording.

## Relationship to `test-claude-locally`

The `.claude/skills/test-claude-locally` skill is the *interactive* version of
this: stand the stack up by hand and drive it with `claude -p` to reproduce a
one-off routing/translation bug — always against the real API. This suite is
the *automated* regression net, running mostly against recorded cassettes.
Reach for the skill to investigate; run `make smoke` to guard against
regressions before merge.
