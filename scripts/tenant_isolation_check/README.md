# tenant_isolation_check

Runs the four tenant-ownership regression checks against live Postgres using
the real `postgres.NewSessionPinRepo` and `postgres.NewBillingRepo`.

Per [`../../CLAUDE.md`](../../CLAUDE.md) ("No DB-backed integration tests in
`internal/`"), this lives under `scripts/` rather than as a `*_test.go` file.
It is a separate `package main`, so `go test ./...` never touches Postgres.
The check is gated on `ROUTER_TEST_DATABASE_URL` and falls back to
`DATABASE_URL`; it is a no-op when neither variable is set.

## Usage (from the repo root)

Bring up the local `docker compose` Postgres stack with router migrations
applied, then:

```bash
ROUTER_TEST_DATABASE_URL="postgres://router:router@localhost:5432/router?search_path=router" \
    go run ./scripts/tenant_isolation_check
```

Exits non-zero and prints which check failed on any mismatch.
