# db — AGENTS

> **Mirror notice.** Verbatim sync with [CLAUDE.md](CLAUDE.md). **Update both together** — divergence = bug.

Migrations + SQLC query sources. Read [root CLAUDE.md](../CLAUDE.md) first. Adapter that consumes these lives in [`../internal/postgres`](../internal/postgres) — see [its CLAUDE.md](../internal/postgres/CLAUDE.md).

## Layout

- `migrations/` — `NNNN_<name>.up.sql` + `.down.sql` pairs, sequential numbering.
- `queries/<table>.sql` — SQLC query sources.

## Migration conventions

- Always wrap migrations in `BEGIN; ... COMMIT;`.
- Never create migration files manually — use `make migrate-create NAME=<name>`.
- **Down migrations must be precise rollbacks.** No `IF EXISTS` guards. Don't separately drop indexes when dropping tables.
- `organization_id` + `created_by` are opaque external identifiers — **never add foreign keys to tables outside the router's own schema.** Such tables don't exist in this project.
- Soft-delete via `deleted_at TIMESTAMP` on tables that need lifecycle. Hot-path queries filter `WHERE deleted_at IS NULL`.

## Query conventions

- **Always named params** (`@param::varchar`), never numbered (`$1`).
- **Always include type casts** so SQLC inference is unambiguous.
- Query names use consistent prefixes: `Insert*`, `Upsert*`, `Get*`, `Update*`, `Delete*`.
- Every query gets an explanatory comment (SQLC turns it into godoc on the generated function).
- No-rows single-row queries return an error — caller checks `errors.Is(err, sql.ErrNoRows)`.

## Regeneration

After touching anything in `queries/`, run `make generate` and commit the regenerated `../internal/sqlc/` alongside the query change. CI fails if generated code drifts from sources.
