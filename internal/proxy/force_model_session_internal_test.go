package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type forceModelMapStore struct {
	pins    map[string]sessionpin.Pin
	upserts []sessionpin.Pin
}

func newForceModelMapStore() *forceModelMapStore {
	return &forceModelMapStore{pins: make(map[string]sessionpin.Pin)}
}

func forceModelStoreKey(key [sessionpin.SessionKeyLen]byte, role string) string {
	return string(key[:]) + ":" + role
}

func (s *forceModelMapStore) Get(_ context.Context, key [sessionpin.SessionKeyLen]byte, role string) (sessionpin.Pin, bool, error) {
	pin, ok := s.pins[forceModelStoreKey(key, role)]
	if !ok {
		return sessionpin.Pin{}, false, nil
	}
	pin.SessionKey = key
	pin.Role = role
	return pin, true, nil
}

func (s *forceModelMapStore) Upsert(_ context.Context, p sessionpin.Pin) error {
	s.upserts = append(s.upserts, p)
	s.pins[forceModelStoreKey(p.SessionKey, p.Role)] = p
	return nil
}

func (s *forceModelMapStore) UpdateUsage(context.Context, [sessionpin.SessionKeyLen]byte, string, sessionpin.Usage) error {
	return nil
}

func (s *forceModelMapStore) IncrementUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) (int, error) {
	return 0, nil
}

func (s *forceModelMapStore) ResetUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) error {
	return nil
}

func (s *forceModelMapStore) SweepExpired(context.Context) error { return nil }

type forceModelRouter struct {
	calls int
}

func (r *forceModelRouter) Route(context.Context, router.Request) (router.Decision, error) {
	r.calls++
	return router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "fresh"}, nil
}

func TestForceModelSessionKeySharedAcrossSubAgents(t *testing.T) {
	mainEnv, err := translate.ParseAnthropic([]byte(`{
		"model":"claude-opus-4-8",
		"metadata":{"user_id":"{\"session_id\":\"0190a58d-0000-7000-8000-000000000001\",\"account_id\":\"acct\"}"},
		"messages":[{"role":"user","content":"implement the feature"}]
	}`))
	require.NoError(t, err)
	subEnv, err := translate.ParseAnthropic([]byte(`{
		"model":"claude-opus-4-8",
		"metadata":{"user_id":"{\"session_id\":\"0190a58d-0000-7000-8000-000000000001\",\"account_id\":\"acct\"}"},
		"messages":[{"role":"user","content":"<transcript>find related files</transcript>"}]
	}`))
	require.NoError(t, err)

	assert.NotEqual(t, DeriveSessionKey(mainEnv, "api-key"), DeriveSessionKey(subEnv, "api-key"))
	assert.Equal(t, DeriveForceModelSessionKey(mainEnv, "api-key"), DeriveForceModelSessionKey(subEnv, "api-key"))
}

func TestRunTurnLoop_SubAgentDispatchUsesSessionForceModelBeforeHardPin(t *testing.T) {
	store := newForceModelMapStore()
	svc := NewService(
		&forceModelRouter{},
		nil,
		nil,
		false,
		nil,
		store,
		true,
		providers.ProviderAnthropic,
		"claude-haiku-4-5",
		nil,
	)
	env, err := translate.ParseAnthropic([]byte(`{
		"model":"claude-opus-4-8",
		"metadata":{"user_id":"device=abc;session=shared"},
		"messages":[{"role":"user","content":"<transcript>inspect the repo</transcript>"}]
	}`))
	require.NoError(t, err)
	forceKey := DeriveForceModelSessionKey(env, "api-key")
	store.pins[forceModelStoreKey(forceKey, forceModelSessionRole)] = sessionpin.Pin{
		SessionKey:     forceKey,
		Role:           forceModelSessionRole,
		InstallationID: uuid.New(),
		Provider:       providers.ProviderAnthropic,
		Model:          "claude-opus-4-8",
		Reason:         translate.ReasonUserForceModel,
		PinnedUntil:    time.Now().Add(time.Hour),
	}

	feats := env.RoutingFeatures(false)
	res, err := svc.runTurnLoop(
		context.Background(),
		env,
		feats,
		"api-key",
		uuid.New(),
		"Explore",
		http.Header{},
		router.Request{RequestedModel: feats.Model},
	)

	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-8", res.Decision.Model)
	assert.Equal(t, translate.ReasonUserForceModel, res.Decision.Reason)
	assert.True(t, res.StickyHit)
	assert.False(t, res.HardPinned)
	assert.Equal(t, forceKey, res.SessionKey)
	assert.Equal(t, forceModelSessionRole, res.PinRole)
}
