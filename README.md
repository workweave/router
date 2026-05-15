<div align="center">

# 🧭 Weave Router

**One endpoint. Every model. Always the right one.**

A drop-in proxy for Anthropic, OpenAI, and Gemini that picks the best model
for *every* request — using a tiny on-box embedder, not a vibes-based prompt.

[![Weave Badge](https://img.shields.io/endpoint?url=https%3A%2F%2Fapp.workweave.ai%2Fapi%2Frepository%2Fbadge%2Forg_QWsHDcRQWQEs6RpkdEZrlFK8%2F805349704&cacheSeconds=3600)](https://app.workweave.ai/reports/repository/org_QWsHDcRQWQEs6RpkdEZrlFK8/805349704)
[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go)](go.mod)
[![Tests](https://github.com/workweave/router/actions/workflows/test.yml/badge.svg)](https://github.com/workweave/router/actions/workflows/test.yml)
[![License: ELv2](https://img.shields.io/badge/License-ELv2-00BFB3.svg)](https://www.elastic.co/licensing/elastic-license)

*Built by [Weave](https://www.workweave.ai) — the #1 engineering intelligence platform,
loved by Robinhood, PostHog & Reducto.*

</div>

---

## What it does

Point Claude Code, Cursor, or your own app at `localhost:8080`. The router:

- 🎯 **Routes per request** — an AvengersPro-derived cluster scorer picks the
  right model from your enabled providers, every turn.
- 🔌 **Speaks everyone's API** — Anthropic Messages, OpenAI Chat Completions,
  Gemini native. Streaming, tools, vision, the works.
- 🧠 **Knows OSS too** — DeepSeek, Kimi, GLM, Qwen, Llama, Mistral via
  OpenRouter (or any OpenAI-compatible endpoint).
- 🔒 **BYOK by default** — provider keys stay on your box, encrypted at rest.
- 📊 **Observable** — OTLP traces out of the box. Drop in Honeycomb, Datadog,
  Grafana, whatever.

No silent fallbacks. No vibes. Routing failures return 503 — loud by design.

## 60-second quickstart

```bash
# 1. Drop a provider key in. OpenRouter is the recommended baseline.
echo "OPENROUTER_API_KEY=sk-or-v1-..." >> .env.local

# 2. Boot the stack (Postgres + router on :8080, seeds an rk_ key).
make full-setup
```

That's it. The router is up at <http://localhost:8080>, the dashboard at
<http://localhost:8080/ui/> (password: `admin`), and your `rk_...` key is
printed in the logs.

```bash
# Call it like Anthropic
curl -sS http://localhost:8080/v1/messages \
  -H "Authorization: Bearer rk_..." \
  -d '{"model":"claude-sonnet-4-5","max_tokens":256,
       "messages":[{"role":"user","content":"hi"}]}'

# …or like OpenAI
curl -sS http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer rk_..." \
  -d '{"model":"gpt-4o-mini",
       "messages":[{"role":"user","content":"hi"}]}'

# Peek at the routing decision without proxying
curl -sS http://localhost:8080/v1/route -H "Authorization: Bearer rk_..." -d '...'
```

## Wire it into your tools

**Claude Code**

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_CUSTOM_HEADERS="X-Weave-Router-Key: rk_..."
claude
```

**Cursor** — Settings → Models → *Override OpenAI Base URL* →
`http://localhost:8080/v1`, paste `rk_...` as the API key.

> Two keys, don't mix them up:
> - `sk-or-...` / `sk-ant-...` / `sk-...` = your **upstream** provider key. Lives in `.env.local`.
> - `rk_...` = your **router** key. Clients send this as a Bearer token.

## Endpoints

| Endpoint                       | Format                                   |
| ------------------------------ | ---------------------------------------- |
| `POST /v1/messages`            | Anthropic Messages — routed              |
| `POST /v1/chat/completions`    | OpenAI Chat Completions — routed         |
| `POST /v1beta/models/:action`  | Gemini `generateContent` — routed        |
| `POST /v1/route`               | Returns the decision, no upstream call   |
| `GET /v1/models` &nbsp;·&nbsp; `POST /v1/messages/count_tokens` | Anthropic passthrough |
| `GET /health` &nbsp;·&nbsp; `GET /validate` | liveness + key check         |

## Deeper docs

- 📐 [**Configuration reference**](docs/CONFIGURATION.md) — every env var,
  BYOK encryption, OTel knobs, cluster routing.
- 🛠️ [**Contributing**](CONTRIBUTING.md) — layering rules, hot-reload dev,
  migrations, tests, the whole engineering loop.
- 🏗️ [**Architecture**](AGENTS.md) — package layout, import contracts,
  recipes for adding endpoints / providers / strategies.

## Roadmap

- Token-aware rate limiting (Redis sliding window per installation)
- Sub-installations for tenant hierarchies
- Speculative dispatch + hedging for tail latency

---

<div align="center">

Licensed under [ELv2](https://www.elastic.co/licensing/elastic-license) ·
[Report a security issue](SECURITY.md) ·
[Code of conduct](CODE_OF_CONDUCT.md)

</div>
