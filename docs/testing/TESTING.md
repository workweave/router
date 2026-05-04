Created: 2026-05-01
Last edited: 2026-05-03

# End-to-end testing: Router OTel → Weave ingestion

This guide walks through running the router side-by-side with the Weave
backend stack and verifying the full OTel trace ingestion pipeline:

```
Router → Webhook (OTLP/HTTP) → Pub/Sub → Worker → Postgres
```

## Prerequisites

- The Weave backend stack is running locally (`wv db up` has been run,
  Postgres + Redis + ClickHouse + Pub/Sub emulator are healthy)
- Go 1.25+ and [golang-migrate](https://github.com/golang-migrate/migrate)
  are installed
- Docker is available (for the router's own Postgres)
- At least one provider API key (e.g. `ANTHROPIC_API_KEY`) — the router
  refuses to boot without one

## 1. Start the router's Postgres

The router uses its own Postgres instance on host port **5433** to avoid
colliding with Weave's database on 5432.

```bash
cd router
make db
```

Verify it's healthy:

```bash
docker compose ps postgres
# STATE should be "running (healthy)"
```

## 2. Configure the router's `.env.local`

Create `router/.env.local` with the database connection, a provider key,
and OTel export pointed at the Weave webhook service:

```
DATABASE_URL=postgresql://router:router@localhost:5433/router?sslmode=disable
ANTHROPIC_API_KEY=<your-anthropic-key>
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:8081/api/ingest
OTEL_EXPORTER_OTLP_HEADERS=x-router-token=local-router-dev-token
OTEL_SERVICE_NAME=weave-router-local
```

The `ANTHROPIC_API_KEY` can be copied from `backend/.env.local` (pulled
via `wv pull-secrets`). The `x-router-token` value must match
`ROUTER_INGEST_TOKEN` in the Weave backend environment (step 4).

## 3. Bootstrap the router database

```bash
cd router
make setup
```

This runs `initdb` → `migrate-up` → `seed` in order.

By default, the seed creates an installation with `external_id=local-dev`.
To link the router to a specific Weave organization, pass `SEED_EXTERNAL_ID`:

```bash
cd router
SEED_EXTERNAL_ID=<weave-org-id> make seed
```

**Save the printed API token** — it's shown only once. You'll use it to
send test requests to the router.

## 4. Ensure Weave backend environment is configured

The following variables must be set in the Weave backend environment
(they should already be in `backend/.env.development`):

```
PUBSUB_TOPIC_ROUTER_TRACES=webhook-router-traces
PUBSUB_SUBSCRIPTION_ROUTER_TRACES=webhook-router-traces-worker
ROUTER_INGEST_TOKEN=local-router-dev-token
```

The Pub/Sub emulator init script (`db/pubsub/entrypoint.sh`) already
creates the `webhook-router-traces` topic and
`webhook-router-traces-worker` subscription. If you added these after the
emulator was started, restart it so the topic/subscription are created:

```bash
wv db down && wv db up
```

## 5. Ensure a matching organization exists in Weave

The worker validates that each span's `external_id` attribute matches a
real organization in Weave's `organizations` table. If your local DB is
empty (fresh setup), insert a test org matching the `external_id` you
seeded in step 3:

```bash
wv db psql
```

```sql
INSERT INTO organizations (id, name)
VALUES ('<external-id-from-step-3>', 'Router E2E Test Org')
ON CONFLICT (id) DO NOTHING;
```

For example, if you used `SEED_EXTERNAL_ID=test-router-org`:

```sql
INSERT INTO organizations (id, name)
VALUES ('test-router-org', 'Router E2E Test Org')
ON CONFLICT (id) DO NOTHING;
```

Skip this step if your local DB already has the organization (e.g. from
`wv get-started`).

## 6. Start the Weave services

You need three Weave processes running. Start each in a separate
terminal:

### Webhook service (receives OTLP traces from the router)

```bash
cd backend
wv be webhook
```

The webhook service listens on port **8081** and registers
`POST /api/ingest/v1/traces`.

### Worker (consumes Pub/Sub messages, writes to Postgres)

```bash
cd backend
wv be worker
```

The worker subscribes to the `webhook-router-traces-worker` Pub/Sub
subscription and writes parsed spans to the `router_routing_events`
table.

### Backend (optional — only needed if testing the GraphQL API or frontend)

```bash
wv backend
```

## 7. Start the router

```bash
cd router
ROUTER_DISABLE_CLUSTER=true PORT=8082 make dev
```

`ROUTER_DISABLE_CLUSTER=true` skips the ONNX cluster scorer, which
requires a downloaded model file. The heuristic fallback router is
sufficient for testing the OTel pipeline.

You should see in the router's output:

```
OTel export enabled  endpoint=http://localhost:8081/api/ingest  workers=2  ...
```

## 8. Send a test request

```bash
curl -s -X POST http://localhost:8082/v1/messages \
  -H "Authorization: Bearer <token-from-step-3>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-20250514",
    "max_tokens": 50,
    "messages": [{"role": "user", "content": "Say hello in exactly one word"}]
  }'
```

## 9. Verify the pipeline

### Check the webhook logs

You should see lines like:

```
Router traces request  content_type=application/x-protobuf  ...
Published router traces to Pub/Sub  message_id=...  resource_span_count=1
```

### Check the worker logs

You should see:

```
Processing router traces message  ...
Batch inserted router routing events  count=2  ...
```

The count of 2 corresponds to the two spans per request:
`router.decision` and `router.upstream`.

### Query Postgres

```bash
wv db psql
```

```sql
SELECT request_id, span_type, requested_model, decision_model,
       decision_provider, external_id
FROM router_routing_events
ORDER BY created_at DESC
LIMIT 10;
```

You should see two rows sharing the same `request_id` — one with
`span_type='router.decision'` and one with `span_type='router.upstream'`.

## Troubleshooting

### Router boot fails with "no provider API keys configured"

Set at least one of `ANTHROPIC_API_KEY`, `OPENAI_PROVIDER_API_KEY`, or
`GOOGLE_PROVIDER_API_KEY` in `router/.env.local`.

### Webhook returns 401 on router trace POST

The `x-router-token` header value in the router's
`OTEL_EXPORTER_OTLP_HEADERS` doesn't match `ROUTER_INGEST_TOKEN` in the
backend environment. Make sure both sides use the same token
(`local-router-dev-token` by default).

### Worker drops spans with "Unknown organization" warning

The `external_id` on the spans doesn't match any row in Weave's
`organizations` table. Either:

- Insert the matching org (step 5), or
- Re-seed the router with `SEED_EXTERNAL_ID` set to an org that already
  exists in your local DB

### No rows in `router_routing_events`

1. Check that the Pub/Sub emulator created the topic: look for
   `webhook-router-traces` in the emulator startup logs
2. Check that the worker subscribed: look for
   `webhook-router-traces-worker` in the worker startup logs
3. Check webhook logs for errors publishing to Pub/Sub

### Port collisions

| Service            | Default port | Notes                              |
| ------------------ | ------------ | ---------------------------------- |
| Router Postgres    | 5433         | Avoids Weave's Postgres on 5432   |
| Weave Postgres     | 5432         | Standard local dev                 |
| Webhook            | 8081         | OTLP traces land here             |
| Router             | 8082         | Client-facing proxy                |
| Pub/Sub emulator   | 8085         | Weave's local emulator             |

If you're running a multi-instance Weave setup (e.g. `i4`), ports may
differ — check your instance's `.env` overrides.
