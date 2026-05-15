# internal/postgres — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

Postgres adapter: SQLC over pgx, plus the session-pin store impl. Read [root CLAUDE.md](../../CLAUDE.md) first.

## Layout

- [`repository.go`](repository.go) — generic repo for auth + installation domain.
- [`converters.go`](converters.go) — adapter-boundary mapping between `pgtype` / `uuid` and the domain types in [`../auth`](../auth) / [`../router/sessionpin`](../router/sessionpin).
- Sibling files implement narrower repos.

## Hard rules

- **Adapter depends only on the inner ring** + may import `internal/sqlc`. Adapters never import each other — `internal/postgres` doesn't know `internal/api/admin` etc.
- **Never write raw SQL outside `db/queries/`** or call `pgx.Pool` directly from anywhere except this package. SQLC is the only data mapper.
- **Domain types must not leak `pgtype` / `uuid` concerns.** `auth.Installation`, `auth.APIKey`, `sessionpin.Pin` are all converted at the adapter boundary in `converters.go`.

## Adding a column or query

1. **Migration first.** Add `db/migrations/NNNN_<name>.up.sql` + `.down.sql` in sequential numbering. Wrap in `BEGIN`/`COMMIT`. Down migration must be a precise rollback — no `IF EXISTS` guards. See [`../../db/CLAUDE.md`](../../db/CLAUDE.md).
2. **Add the query** to the appropriate `db/queries/<table>.sql`. Use named params with type casts (`@param::varchar`). Use `sqlc.embed(t)` for JOINs.
3. **Run `make generate`** to regenerate `internal/sqlc/`. Commit the generated code alongside changes.
4. **Update [`repository.go`](repository.go)** (and [`converters.go`](converters.go) if a new column needs domain mapping).

## SQLC conventions

- Always named params (`@param::varchar`), never numbered (`$1`).
- Always include type casts so SQLC inference is unambiguous.
- Query names use consistent prefixes: `Insert*`, `Upsert*`, `Get*`, `Update*`, `Delete*`.
- Every query gets an explanatory comment (SQLC turns it into godoc on the generated function).
- No-rows single-row queries return an error — check `errors.Is(err, sql.ErrNoRows)`.

## Never edit generated files

`internal/sqlc/` is generated. The "DO NOT EDIT" header is load-bearing. Regenerate with `make generate`.
