# internal/auth — AGENTS

> **Mirror notice.** Verbatim sync with [CLAUDE.md](CLAUDE.md). **Update both together** — divergence = bug.

Identity domain. Types, repos, `Service.VerifyAPIKey`, `APIKeyCache`, ID/hashing helpers, Tink encryptor. Read [root CLAUDE.md](../../CLAUDE.md) first.

## Adding a method to `*auth.Service`

1. **Define method on `*auth.Service`** in [`service.go`](service.go). No I/O directly here — push into repo. Inner-ring imports (`router`, `providers`, `translate`, `observability`, `internal/router/*` helper packages, `internal/proxy/usage`) + small utility libs are fine.
2. **If you need new repo methods**, add to the interfaces in [`installation.go`](installation.go) / [`api_key.go`](api_key.go) / sibling files. Interface = contract; the Postgres adapter must satisfy it.
3. **Implement new repo method in [`../postgres/repository.go`](../postgres/repository.go)** (or sibling in `internal/postgres/`), adding the SQLC query in `db/queries/`. Run `make generate` to regenerate `internal/sqlc/`.
4. **Update matching `service_test.go` fakes** to satisfy the expanded interface. Tests use fakes; assert on real return values, not just that mocks were called.

## Conventions

- **Domain types must not leak `pgtype` / `uuid` concerns.** Convert at the adapter boundary in [`../postgres/converters.go`](../postgres/converters.go).
- **`fireMarkUsed` is the documented "log-and-continue" exception.** Best-effort, off the request path — see [`service.go`](service.go). Everywhere else, errors flow up.
- **Clock injection.** Use `auth.Clock = func() time.Time` rather than calling `time.Now()` directly — lets tests pin time.
- **Token safety.** Never log raw bearer tokens. 8-char prefix + 4-char suffix (`KeyPrefix` / `KeySuffix` columns on `auth.APIKey`) are the only safe form.
- **BYOK secrets at rest** go through `auth.Encryptor` (Tink AES-256-GCM). Plaintext only in memory for the request lifetime.

## Helpers live here

Auth-shaped helpers (token prefix, ID gen, hashing, encryption) belong in this package alongside the types they support — not in a generic `util/` package.
