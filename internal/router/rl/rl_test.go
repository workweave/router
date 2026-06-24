package rl_test

import (
	"context"
	"errors"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/rl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDecider records the query it received and returns a canned result/error.
type fakeDecider struct {
	got    rl.Query
	result rl.Result
	err    error
}

func (f *fakeDecider) Decide(_ context.Context, q rl.Query) (rl.Result, error) {
	f.got = q
	return f.result, f.err
}

func deployed(ids ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

func enabled(names ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

func TestRouteMapsRosterChoiceBackToCatalogModel(t *testing.T) {
	// The policy picks by OpenRouter-style roster ID; the router must dispatch
	// the corresponding catalog model via its own provider.
	dec := &fakeDecider{result: rl.Result{Model: "anthropic/claude-opus-4-8", Score: 1.5, ScoreLabel: "DPO score", StateLabel: "implementing"}}
	r := rl.New(dec, deployed("claude-opus-4-8", "deepseek/deepseek-v4-flash"))

	decision, err := r.Route(context.Background(), router.Request{
		PromptText:       "refactor the auth module",
		EnabledProviders: enabled(providers.ProviderAnthropic, providers.ProviderDeepInfra),
	})
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-8", decision.Model)
	assert.Equal(t, providers.ProviderAnthropic, decision.Provider)
	assert.Contains(t, decision.Reason, "DPO score")
	assert.Contains(t, decision.Reason, "implementing")

	// The deepseek slash-form id passes through unchanged; the dotted/dashed
	// first-party slug is what the policy was offered for opus.
	rosterIDs := make(map[string]string, len(dec.got.Candidates))
	for _, c := range dec.got.Candidates {
		rosterIDs[c.RosterID] = c.Provider
	}
	assert.Equal(t, providers.ProviderAnthropic, rosterIDs["anthropic/claude-opus-4-8"])
	assert.Equal(t, providers.ProviderDeepInfra, rosterIDs["deepseek/deepseek-v4-flash"])
}

func TestRouteOmitsModelsWithNoEnabledProvider(t *testing.T) {
	dec := &fakeDecider{result: rl.Result{Model: "anthropic/claude-opus-4-8"}}
	r := rl.New(dec, deployed("claude-opus-4-8", "deepseek/deepseek-v4-flash"))

	_, err := r.Route(context.Background(), router.Request{
		PromptText:       "hi",
		EnabledProviders: enabled(providers.ProviderAnthropic),
	})
	require.NoError(t, err)
	for _, c := range dec.got.Candidates {
		assert.NotEqual(t, "deepseek/deepseek-v4-flash", c.RosterID,
			"deepinfra not enabled, so the deepseek model must not be offered")
	}
}

func TestRouteExcludesRequestedExclusions(t *testing.T) {
	dec := &fakeDecider{result: rl.Result{Model: "deepseek/deepseek-v4-flash"}}
	r := rl.New(dec, deployed("claude-opus-4-8", "deepseek/deepseek-v4-flash"))

	_, err := r.Route(context.Background(), router.Request{
		PromptText:       "hi",
		EnabledProviders: enabled(providers.ProviderAnthropic, providers.ProviderDeepInfra),
		ExcludedModels:   map[string]struct{}{"claude-opus-4-8": {}},
	})
	require.NoError(t, err)
	for _, c := range dec.got.Candidates {
		assert.NotEqual(t, "anthropic/claude-opus-4-8", c.RosterID)
	}
}

func TestRouteNoEligibleCandidatesIsUnavailable(t *testing.T) {
	dec := &fakeDecider{result: rl.Result{Model: "anthropic/claude-opus-4-8"}}
	r := rl.New(dec, deployed("claude-opus-4-8"))

	_, err := r.Route(context.Background(), router.Request{
		PromptText:       "hi",
		EnabledProviders: enabled(providers.ProviderOpenAI), // no binding for opus
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, rl.ErrPolicyUnavailable))
}

func TestRouteDeciderErrorIsUnavailable(t *testing.T) {
	dec := &fakeDecider{err: errors.New("sidecar down")}
	r := rl.New(dec, deployed("claude-opus-4-8"))

	_, err := r.Route(context.Background(), router.Request{
		PromptText:       "hi",
		EnabledProviders: enabled(providers.ProviderAnthropic),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, rl.ErrPolicyUnavailable))
}

func TestRouteNilEnabledProvidersIsUnrestricted(t *testing.T) {
	// nil EnabledProviders means "unrestricted" (router.Request contract); the
	// policy must still be offered the deployed models via their primary
	// provider, not an empty set.
	dec := &fakeDecider{result: rl.Result{Model: "anthropic/claude-opus-4-8"}}
	r := rl.New(dec, deployed("claude-opus-4-8", "deepseek/deepseek-v4-flash"))

	decision, err := r.Route(context.Background(), router.Request{
		PromptText:       "hi",
		EnabledProviders: nil,
	})
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-8", decision.Model)
	assert.Equal(t, providers.ProviderAnthropic, decision.Provider)
	assert.NotEmpty(t, dec.got.Candidates, "nil providers must not empty the candidate set")
}

func TestRouteToolTurnDropsToolUseLowFromCandidatesAndIndex(t *testing.T) {
	// qwen/qwen3-235b-a22b-2507 is ToolUseLow. On a tool turn it must be absent
	// from BOTH the offered candidates and the response-mapping index — a
	// sidecar that names it anyway is rejected, not dispatched.
	dec := &fakeDecider{result: rl.Result{Model: "qwen/qwen3-235b-a22b-2507"}}
	r := rl.New(dec, deployed("claude-opus-4-8", "qwen/qwen3-235b-a22b-2507"))

	_, err := r.Route(context.Background(), router.Request{
		PromptText:       "use a tool",
		HasTools:         true,
		EnabledProviders: nil,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, rl.ErrPolicyUnavailable))
	for _, c := range dec.got.Candidates {
		assert.NotEqual(t, "qwen/qwen3-235b-a22b-2507", c.RosterID,
			"ToolUseLow model must not be offered on a tool turn")
	}
}

func TestRouteImageTurnDropsImageUnsupported(t *testing.T) {
	// deepseek/deepseek-v4-flash is image-unsupported; opus is vision-capable.
	// An image turn must drop the text-only model when a capable one survives.
	dec := &fakeDecider{result: rl.Result{Model: "anthropic/claude-opus-4-8"}}
	r := rl.New(dec, deployed("claude-opus-4-8", "deepseek/deepseek-v4-flash"))

	_, err := r.Route(context.Background(), router.Request{
		PromptText:       "what is in this image",
		HasImages:        true,
		EnabledProviders: nil,
	})
	require.NoError(t, err)
	for _, c := range dec.got.Candidates {
		assert.NotEqual(t, "deepseek/deepseek-v4-flash", c.RosterID,
			"image-unsupported model must not be offered on an image turn")
	}
}

func TestRouteUnknownReturnedModelIsUnavailable(t *testing.T) {
	dec := &fakeDecider{result: rl.Result{Model: "openai/gpt-5.5"}} // never offered
	r := rl.New(dec, deployed("claude-opus-4-8"))

	_, err := r.Route(context.Background(), router.Request{
		PromptText:       "hi",
		EnabledProviders: enabled(providers.ProviderAnthropic),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, rl.ErrPolicyUnavailable))
}
