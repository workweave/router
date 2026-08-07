---
name: test-dashboard-locally
description: Run the Weave router self-hosted dashboard locally in docker compose, seed telemetry, and verify /ui dashboard charts and /admin/v1/metrics endpoints end-to-end. Use when testing dashboard UI changes, metrics API changes, or chart additions.
---

# Testing the self-hosted dashboard locally

## Bring up the stack

```bash
cd router
# If host port 8085 is taken, add a gitignored docker-compose.override.yml:
#   services:
#     pubsub-emulator:
#       ports: !reset []
docker compose up -d --build server
until curl -sf http://localhost:8080/health >/dev/null; do sleep 2; done
docker compose run --rm seed   # prints an rk_ API key
```

- Postgres is on host port **5433** (user/pass/db all `router` — local-only creds).
- Dashboard is at `http://localhost:8080/ui/dashboard`. With `ROUTER_ADMIN_PASSWORD` unset locally the login password defaults to `admin`.
- The dashboard's main content is an inner scroll container — page-level scrolling may do nothing; scroll the inner container element instead.

## Seeding telemetry for metrics/chart testing

Dashboard metrics read `router.model_router_request_telemetry` with `span_type = 'router.upstream'`. You can seed rows directly instead of driving live requests (fine when the change only *reads* telemetry; drive real requests via the test-claude-locally skill if the change touches telemetry *writing*):

- Cost columns (`actual_input_cost_usd` etc.) are **bigint micros** despite the `_usd` suffix (1_000_000 = $1).
- Get the installation id from `router.model_router_installations` (compose seed creates one, `external_id = '__router_admin__'`).
- `(installation_id, request_id, span_type)` must be unique.
- Seed at least 2–3 distinct `decision_model` values across multiple days so per-model charts and granularity switching (hour/day/week) are meaningfully testable.

## Verifying metrics endpoints

`/admin/v1/metrics/*` accepts either the dashboard admin cookie (all installations) or an `rk_` bearer key (scoped to its installation):

```bash
curl -s "http://localhost:8080/admin/v1/metrics/model-breakdown?granularity=day" \
  -H "Authorization: Bearer rk_..."
```

Cross-check aggregated API totals against the exact seeded values before trusting the UI.

## Gotchas

- `wmctrl`-based window maximizing may fail on this VM's window manager; the browser window may already be effectively full-screen — check a screenshot before fighting it.
- The "review" CI check on PRs may fail with "Workflow initiated by non-human actor" for bot-created PRs; that is workflow authorization, not a code failure.

## Devin Secrets Needed

None for local testing (local admin password defaults to `admin`; the seed command generates the rk_ key).
