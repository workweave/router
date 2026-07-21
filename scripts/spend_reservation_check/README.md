# spend_reservation_check

Exercises spend-cap reservations against the real Postgres database used by
the local `docker compose` stack. It covers the #793 concurrent
reserve-then-settle regression, verifies that reservation and monthly spend
rows use the same Go-computed UTC month, and checks that the TTL sweeper
releases expired reservations.

Per [`../../AGENTS.md`](../../AGENTS.md) ("No DB-backed integration tests in
`internal/`"), this lives here as a runnable script rather than
`internal/postgres/*_test.go`. It is **not** a test (`package main`), so
`go test ./...` never touches Postgres. It is gated on
`ROUTER_TEST_DATABASE_URL` and is a no-op without it.

## Usage (from the repo root)

Bring up the local `docker compose` Postgres stack with router migrations
applied, then:

```bash
ROUTER_TEST_DATABASE_URL="postgres://router:router@localhost:5432/router?search_path=router" \
    go run ./scripts/spend_reservation_check
```

Exits non-zero and logs which check failed on any mismatch.
