package translate_test

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const affinityKey = "0123456789abcdef0123456789abcdef"

func anthropicSrc() []byte {
	return []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
}

func promptCacheKey(t *testing.T, body []byte) (string, bool) {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	v, ok := doc["prompt_cache_key"].(string)
	return v, ok
}

func TestSessionAffinity_FireworksSetsHeaderNotBody(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicSrc())
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:     "deepseek/deepseek-v4-pro",
		TargetProvider:  providers.ProviderFireworks,
		SessionAffinity: affinityKey,
	})
	require.NoError(t, err)

	assert.Equal(t, affinityKey, out.Headers.Get("x-session-affinity"))
	assert.Empty(t, out.Headers.Get("x-session-id"))
	_, hasBody := promptCacheKey(t, out.Body)
	assert.False(t, hasBody, "Fireworks must not carry the OpenAI prompt_cache_key body field")
}

func TestSessionAffinity_DeepInfraSetsHeader(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicSrc())
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:     "deepseek/deepseek-v4-flash",
		TargetProvider:  providers.ProviderDeepInfra,
		SessionAffinity: affinityKey,
	})
	require.NoError(t, err)

	assert.Equal(t, affinityKey, out.Headers.Get("x-session-affinity"))
}

func TestSessionAffinity_OpenRouterUsesSessionIDHeader(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicSrc())
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:     "deepseek/deepseek-v4-pro",
		TargetProvider:  providers.ProviderOpenRouter,
		SessionAffinity: affinityKey,
	})
	require.NoError(t, err)

	assert.Equal(t, affinityKey, out.Headers.Get("x-session-id"))
	assert.Empty(t, out.Headers.Get("x-session-affinity"))
}

func TestSessionAffinity_OpenAIUsesPromptCacheKeyBody(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicSrc())
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:     "gpt-5.5",
		TargetProvider:  providers.ProviderOpenAI,
		SessionAffinity: affinityKey,
	})
	require.NoError(t, err)

	v, ok := promptCacheKey(t, out.Body)
	require.True(t, ok, "OpenAI must carry prompt_cache_key in the body")
	assert.Equal(t, affinityKey, v)
	assert.Empty(t, out.Headers.Get("x-session-affinity"))
	assert.Empty(t, out.Headers.Get("x-session-id"))
}

func TestSessionAffinity_BedrockGetsNoHint(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicSrc())
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:     "moonshotai/kimi-k2.5",
		TargetProvider:  providers.ProviderBedrock,
		SessionAffinity: affinityKey,
	})
	require.NoError(t, err)

	assert.Empty(t, out.Headers.Get("x-session-affinity"))
	assert.Empty(t, out.Headers.Get("x-session-id"))
	_, hasBody := promptCacheKey(t, out.Body)
	assert.False(t, hasBody)
}

func TestSessionAffinity_EmptyIsNoOp(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicSrc())
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:    "deepseek/deepseek-v4-pro",
		TargetProvider: providers.ProviderFireworks,
	})
	require.NoError(t, err)

	assert.Empty(t, out.Headers.Get("x-session-affinity"))
	_, hasBody := promptCacheKey(t, out.Body)
	assert.False(t, hasBody)
}
