# Frozen HMM sidecar — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). Claude Code reads `CLAUDE.md`; Cursor + generic agents read `AGENTS.md`. **Update both together** — divergence = bug.

Scoped guide for the public, self-hosted frozen HMM sidecar. Read the repository-root [CLAUDE.md](../../CLAUDE.md) and [README.md](README.md) first.

## Boundary and invariants

- This directory is an inference-only companion to the public router. Keep training, online learning, private registries, managed credentials, and WorkWeave-only packages out of it.
- Runtime code must never unpickle. A trusted offline exporter may read a pickle, but its published archive must be data-only and pass `scripts/verify_artifact.py`.
- Reproduce the embedding space declared by the artifact exactly. Matching only the vector dimension is insufficient. Keep the startup reference-vector probe fail-closed.
- Fetch artifacts from an immutable, versioned URL with an exact SHA-256. Never use `latest`, floating tags, or replace an already-published asset in place.
- Preserve safe extraction and cache behavior: reject traversal, links, unexpected members, oversized files/archives, hash mismatches, and cache-key collisions.
- The sidecar is optional. Its absence or startup failure must not prevent the core router from serving its default cluster-routing strategy.
- Capability discovery must fail conservatively and retry in the background; do not make core router startup depend on the sidecar healthcheck.
- Keep lifecycle commands symmetric: `make down` must include the `hmm` profile so containers created by `make up-hmm` are not orphaned.
- Put image defaults in the Dockerfile or application. Do not add `${HMM_*:-default}` values to the Compose service's `environment`; those values override `.env.local` from `env_file` and break self-host overrides.
- Keep the `policy_router_v1` contract aligned with `cmd/router/main.go`. The frozen policy advertises callback capabilities as false; `/outcome` and `/feedback` still exist and return `204` for protocol completeness.
- Do not retain or log raw prompts, responses, embeddings, callback bodies, or other customer content.

## Updating the released artifact

1. Export from the trusted training environment with `scripts/export_artifact.py`.
2. Verify the archive with `scripts/verify_artifact.py`, inspect its members, and confirm its manifest contains no prompts, training rows, embedding caches, local paths, credentials, or private registry records.
3. Publish it under a new immutable release/tag and record the asset's SHA-256. Do not overwrite an existing release asset.
4. Update the URL and digest together in:
   - `sidecars/hmm/Dockerfile`
   - `.env.example`
   - `.github/workflows/test.yml`
5. Do not add the pins to `docker-compose.yml`; Compose must allow `.env.local` to override the image defaults.
6. If the schema, model roster, embedding model, or probe changes, update `sidecars/hmm/README.md`, `docs/CONFIGURATION.md`, policy tests, and the Go contract/capability tests in the same change.

## Required validation

From `sidecars/hmm/`:

```bash
poetry install --no-interaction
poetry run black --check hmm_sidecar scripts tests
poetry run pytest -q
poetry run python -m scripts.verify_artifact /path/to/artifact.tar.gz --sha256 <digest>
HMM_TEST_PACKAGE=/path/to/artifact.tar.gz poetry run pytest -q tests/test_policy.py
```

From the repository root:

```bash
go build -o /dev/null ./...
go test -count=1 ./...
go vet ./...
docker compose --profile hmm config --quiet
docker build -f sidecars/hmm/Dockerfile .
```

Before merging, confirm the real release artifact exercises `/route` in CI, the published bytes match every checked-in digest, OpenAI-compatible `.env.local` overrides survive Compose rendering, and an unavailable sidecar does not block router startup.
