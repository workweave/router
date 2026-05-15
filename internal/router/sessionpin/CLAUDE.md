# internal/router/sessionpin — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

`Pin` type + `Store` interface for sticky per-session routing. Read [root CLAUDE.md](../../../CLAUDE.md) first.

## Surface

- `Pin` value type.
- `Store` interface (inner-ring contract) — implemented by `postgres.SessionPinRepository` in [`../../postgres`](../../postgres).
- Keyed by `(api_key_id, session_key, role)` where `session_key` = 16-byte sha256 truncation derived from the inbound request (see [`../../proxy/session_key.go`](../../proxy/session_key.go)).

## Roles

Stage 1 emits `role="default"` only. The column exists so a turn-type detector can land role-conditioned pinning later without a schema change.

## TTL sweep

`runSessionPinSweep` in `cmd/router/main.go` runs the TTL sweep loop. The store interface lives here; the loop and the Postgres adapter live outside the inner ring.

## Invariants

- **Interface in inner ring; impl in `internal/postgres`.** Proxy service is unit-tested with an in-memory fake; the Postgres adapter is exercised end-to-end via the docker-compose stack.
- **No I/O in this package.** Just types + interface.
