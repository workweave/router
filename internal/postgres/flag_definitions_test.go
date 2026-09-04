package postgres_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/flags"
	"weave-os/router/internal/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These exercise the SQL that unit tests can't: the flag-definition upsert/prune
// and the flag_overrides JSONB write-then-read round-trip through a real
// Postgres. Gated on ROUTER_TEST_DATABASE_URL because CI's `go test` runs without
// a database; run locally with, for example:
//
//	ROUTER_TEST_DATABASE_URL='postgres://...?search_path=router' \
//	  go test ./internal/postgres/ -run FlagOverrides -v
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ROUTER_TEST_DATABASE_URL not set; skipping database-backed test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(context.Background()))
	return pool
}

func TestFlagDefinitionsPublishRoundTrip(t *testing.T) {
	pool := testPool(t)
	repo := postgres.NewFlagDefinitionRepo(pool)
	ctx := context.Background()

	published := make([]flags.PublishedDefinition, 0, len(flags.Registry))
	for _, def := range flags.Registry {
		published = append(published, flags.PublishedDefinition{Definition: def, DeploymentDefault: "true"})
	}
	require.NoError(t, repo.Publish(ctx, published))

	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM router.flag_definitions").Scan(&count))
	assert.Equal(t, len(flags.Registry), count, "every registry entry should be published")

	// Idempotent: a second boot must converge, not duplicate or error.
	require.NoError(t, repo.Publish(ctx, published))
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM router.flag_definitions").Scan(&count))
	assert.Equal(t, len(flags.Registry), count)

	// A retired flag is pruned rather than left to haunt the admin UI.
	require.NoError(t, repo.Publish(ctx, published[:1]))
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM router.flag_definitions").Scan(&count))
	assert.Equal(t, 1, count)

	// Restore so a later local run starts from a full registry.
	require.NoError(t, repo.Publish(ctx, published))
}

func TestInstallationFlagOverridesRoundTrip(t *testing.T) {
	pool := testPool(t)
	repo := postgres.NewRepository(pool, auth.NoOpEncryptor{})
	ctx := context.Background()

	const externalID = "flag-override-roundtrip-test"
	t.Cleanup(func() {
		_, err := pool.Exec(ctx, "DELETE FROM router.model_router_installations WHERE external_id = $1", externalID)
		require.NoError(t, err)
	})

	created, err := repo.Installations.Create(ctx, auth.CreateInstallationParams{
		ExternalID: externalID,
		Name:       "flag override round trip",
	})
	require.NoError(t, err)

	// A fresh installation inherits every deployment default.
	assert.True(t, created.FlagOverrides.IsEmpty(), "new installation should carry no overrides")

	want := flags.Overrides{
		Bools:   map[flags.Key]bool{flags.KeyStruggleShadowEnabled: false, flags.KeySubscriptionPlanAwareRouting: true},
		Ints:    map[flags.Key]int{flags.KeyLoopEscalationHoldoutPct: 42},
		Strings: map[flags.Key]string{flags.KeyCyberRefusalFallback: "claude-opus-5"},
	}
	require.NoError(t, repo.Installations.UpdateFlagOverrides(ctx, externalID, created.ID, want))

	reread, err := repo.Installations.Get(ctx, externalID, created.ID)
	require.NoError(t, err)
	assert.Equal(t, want, reread.FlagOverrides, "overrides must survive the JSONB round-trip")

	// Clearing writes an empty object, not null, so the column default holds.
	require.NoError(t, repo.Installations.UpdateFlagOverrides(ctx, externalID, created.ID, flags.Overrides{}))
	reread, err = repo.Installations.Get(ctx, externalID, created.ID)
	require.NoError(t, err)
	assert.True(t, reread.FlagOverrides.IsEmpty())

	var raw []byte
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT flag_overrides FROM router.model_router_installations WHERE id = $1", created.ID,
	).Scan(&raw))
	assert.JSONEq(t, `{}`, string(raw))

	// A payload naming a retired/unknown flag degrades to "no overrides" rather
	// than failing the read, so a stale row can't take an org's traffic down.
	bad, err := json.Marshal(map[string]bool{"not_a_flag": true})
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		"UPDATE router.model_router_installations SET flag_overrides = $1 WHERE id = $2", bad, created.ID)
	require.NoError(t, err)

	reread, err = repo.Installations.Get(ctx, externalID, created.ID)
	require.NoError(t, err, "an unparseable payload must not fail the read")
	assert.True(t, reread.FlagOverrides.IsEmpty(), "unparseable payload falls back to deployment defaults")
}
