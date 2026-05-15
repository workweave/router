# Contributing

Thanks for your interest in contributing to router. This document covers
the things you'll need to know — layering, build/test workflow, commit
conventions, and PR process.

## Quick start

1. Fork and clone the repo.
2. Boot the stack: `make full-setup` (see [README](README.md)).
3. Run the test suite: `make check` (regenerates code + builds + tests).
4. Make your changes on a topic branch.
5. Open a PR.

## Code of Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
By participating, you agree to abide by its terms.

## Developer Certificate of Origin (DCO)

Every commit must be signed off, certifying you wrote the change (or have
the right to submit it under the project's license). Add `-s` to your
commit:

```bash
git commit -s -m "your message"
```

This appends a `Signed-off-by: Name <email>` trailer. The full DCO text is
at <https://developercertificate.org>. PRs without sign-off will be asked
to amend.

## Layering — read this before writing code

The router uses three concentric layers; **imports must flow inward
only**. The full rules live in [AGENTS.md](AGENTS.md). The summary:

- **Inner ring** (`internal/auth`, `internal/proxy`, `internal/router`,
  `internal/providers`, `internal/translate`) — pure domain logic, no
  I/O outside Service methods. `internal/router` and
  `internal/providers` are interface-only and never import each other.
- **Adapters** (`internal/postgres`, `internal/router/cluster`,
  `internal/providers/{anthropic,openai,google,openaicompat}`) — depend
  only on the inner ring. Adapters never import each other.
- **Presentation** (`internal/api/*`, `internal/server*`) — adapts HTTP
  to Services. Never imports `internal/postgres` or concrete provider
  packages.
- **Composition** — `cmd/router/main.go` is the only file that
  constructs concrete adapters and wires them in.

Cross-layer violations are review-blocking. AGENTS.md has step-by-step
recipes for adding endpoints, providers, migrations, queries, and
routing strategies — please read the relevant section before starting
non-trivial work.

## What we won't accept

- **Silent fallbacks.** When the cluster scorer can't run, the router
  returns HTTP 503 — it does *not* fall back to a default model. PRs
  re-introducing fail-open behavior will be rejected; silent degradation
  masked real regressions before, which is why the heuristic was retired.
- **Imports across layers.** A handler calling `*sqlc.Queries` directly,
  a repo calling another repo, or `internal/router` doing I/O are all
  layering violations. Surface a Service method instead.
- **Hand-edited generated code.** Files under `internal/sqlc/` are
  regenerated from `db/queries/` via `make generate`. Editing them
  directly will be reverted.
- **Magic strings for provider/model names.** Use the constants in
  `internal/providers` (`providers.ProviderAnthropic`, etc.) everywhere
  these appear.
- **DI containers, reflection-based wiring, or service locators.**
  Composition is plain Go function calls in `cmd/router/main.go`.

## Build / test

```bash
make generate     # regenerate SQLC + statusline (no DB required)
make build        # typecheck the whole module
make test         # run all tests
make check        # generate + build + test (CI-equivalent)
```

The CI required-checks gate is `make check`, plus `git diff --quiet`
after `make generate` (to catch uncommitted regenerated code).

### Hot-reload dev loop

For iterating on router code with `CompileDaemon`:

```bash
make db                                # start Postgres only (port 5433)
echo "DATABASE_URL=postgresql://router:router@localhost:5433/router?sslmode=disable" >> .env.local
make setup                             # init schema + migrate + seed an rk_ key
make dev                               # run the server with hot reload
```

Prerequisites: Go 1.25+, [golang-migrate](https://github.com/golang-migrate/migrate),
[CompileDaemon](https://github.com/githubnemo/CompileDaemon).

The cluster scorer uses an ONNX embedder; on Apple Silicon you also need:

```bash
# Populate ./assets/ first — see docs/CONFIGURATION.md → Cluster-routing artifacts.
echo "ROUTER_ONNX_ASSETS_DIR=$(pwd)/assets" >> .env.local
echo "CGO_LDFLAGS=-L/path/to/libtokenizers" >> .env.local
echo "ROUTER_ONNX_LIBRARY_DIR=/opt/homebrew/lib" >> .env.local
```

(`brew install onnxruntime`; `libtokenizers` from
[daulet/tokenizers releases](https://github.com/daulet/tokenizers/releases).)
Without these the cluster scorer fails at boot and the router refuses to
start. The Docker path bundles all of this — Apple Silicon CGO setup only
matters for the `make dev` flow.

### Tests

- Use in-memory fakes for repos / routers / provider clients. See
  `internal/auth/service_test.go` and `internal/proxy/service_test.go`
  for the canonical pattern.
- Real assertions only — compare a value the code produced to a value
  you chose. Tautological assertions (`x == x`, "constructor returns an
  instance", "mock was called") are rejected.
- No DB-backed integration tests in `internal/`. The Docker Compose
  stack is the runtime fixture for end-to-end work.

## Database changes

Migrations and queries are SQLC-driven. Don't write raw SQL outside
`db/queries/` and don't call `pgx.Pool` from anywhere outside
`internal/postgres/`.

```bash
make migrate-create NAME=add-xyz
$EDITOR db/migrations/<ts>_add-xyz.up.sql
$EDITOR db/migrations/<ts>_add-xyz.down.sql
make migrate-up
make generate     # regenerate SQLC after migration changes
```

Rules:

- Wrap migrations in `BEGIN; ... COMMIT;`.
- Down migrations must be precise rollbacks of the up — no `IF EXISTS`
  guards.
- Use named parameters (`@param::type`), never numbered (`$1`).
- `organization_id` and `created_by` are opaque external strings — do
  not add foreign keys to tables outside the router's own schema.
- Soft-delete via `deleted_at TIMESTAMP` on tables that need lifecycle.

## Logging

- Use `slog` via `observability.Get` / `observability.FromGin`. Never
  `fmt.Println` or `log.Print*`.
- Snake_case attribute keys (`api_key_id`, not `apiKeyID`).
- `Debug` for routine ops (auth checks, repo calls), `Info` for major
  business events (server start, key issuance), `Error` only for genuine
  failures that need on-call attention. An auth-401 is `Debug`, not
  `Error`.
- Never log raw bearer tokens or full hashes. The 8-char prefix + 4-char
  suffix (`KeyPrefix` / `KeySuffix` on `auth.APIKey`) are the safe form.

## Commit messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(proxy): add session pinning for multi-turn coherence
fix(auth): clear API key cache on installation deletion
refactor(translate): extract OpenAI envelope builder
docs: clarify ROUTER_ADMIN_PASSWORD requirement
```

Common types: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `ci`.

## Pull request process

1. Open the PR against `main`. Include the DCO sign-off on every commit.
2. Fill in the PR template — what changed, why, and how it was tested.
3. Make sure CI is green (`make check` + the migration-timestamp check).
4. A maintainer will review. Plan to iterate; reviews are usually 1-2
   rounds.

## Adding documentation

Project docs live under `docs/`. New top-level docs should include this
header before the H1:

```
Created: YYYY-MM-DD
Last edited: YYYY-MM-DD
```

If you add a doc, append a row to `docs/README.md` in the appropriate
section, sorted by `Created` ascending.

## Reporting security issues

**Do not** open a public issue for security vulnerabilities. See
[SECURITY.md](SECURITY.md) for the disclosure process.

## License

By contributing, you agree that your contributions will be licensed
under the project's [LICENSE](LICENSE).
