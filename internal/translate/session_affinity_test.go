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

// Makora and Together are serverless OpenAI-compat upstreams that were missing
// from the old literal affinity list, so their turns paid a cold-replica
// prefill. Keying the header off the OpenAI-compat family gives them replica
// stickiness with no per-provider edit.
func TestSessionAffinity_MakoraAndTogetherSetHeader(t *testing.T) {
	for _, provider := range []string{providers.ProviderMakora, providers.ProviderTogether} {
		t.Run(provider, func(t *testing.T) {
			env, err := translate.ParseAnthropic(anthropicSrc())
			require.NoError(t, err)

			out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
				TargetModel:     "deepseek/deepseek-v4-pro",
				TargetProvider:  provider,
				SessionAffinity: affinityKey,
			})
			require.NoError(t, err)

			assert.Equal(t, affinityKey, out.Headers.Get("x-session-affinity"))
			assert.Empty(t, out.Headers.Get("x-session-id"))
			_, hasBody := promptCacheKey(t, out.Body)
			assert.False(t, hasBody, "%s must not carry the OpenAI prompt_cache_key body field", provider)
		})
	}
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

func TestSessionAffinity_XAIUsesGrokConvIDHeader(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicSrc())
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:     "grok-4.5",
		TargetProvider:  providers.ProviderXAI,
		SessionAffinity: affinityKey,
	})
	require.NoError(t, err)

	assert.Equal(t, affinityKey, out.Headers.Get("x-grok-conv-id"))
	assert.Empty(t, out.Headers.Get("x-session-affinity"))
	assert.Empty(t, out.Headers.Get("x-session-id"))
	_, hasBody := promptCacheKey(t, out.Body)
	assert.False(t, hasBody, "xAI chat/completions must not carry prompt_cache_key")
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

// Without a session key, an OpenAI cross-format route still carries a
// prompt_cache_key — a stable hash of the cacheable prefix — so OpenAI's
// prompt caching engages instead of re-billing the full prefix every turn.
func TestSessionAffinity_OpenAIFallsBackToStablePrefixKey(t *testing.T) {
	src := []byte(`{"model":"claude-opus-4-7","system":"you are a helpful assistant","tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)

	prepare := func(t *testing.T) string {
		env, err := translate.ParseAnthropic(src)
		require.NoError(t, err)
		out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
			TargetModel:    "gpt-5.5",
			TargetProvider: providers.ProviderOpenAI,
		})
		require.NoError(t, err)
		v, ok := promptCacheKey(t, out.Body)
		require.True(t, ok, "OpenAI must carry a prompt_cache_key even without a session key")
		assert.NotEmpty(t, v)
		return v
	}

	first := prepare(t)
	second := prepare(t)
	assert.Equal(t, first, second, "fallback prompt_cache_key must be stable for the same prefix")
}

// A different cacheable prefix (different system + tools) yields a different
// fallback key, so unrelated conversations don't share one cache bucket.
func TestSessionAffinity_OpenAIFallbackKeyVariesByPrefix(t *testing.T) {
	keyFor := func(t *testing.T, system string) string {
		src := []byte(`{"model":"claude-opus-4-7","system":` + system + `,"messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
		env, err := translate.ParseAnthropic(src)
		require.NoError(t, err)
		out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
			TargetModel:    "gpt-5.5",
			TargetProvider: providers.ProviderOpenAI,
		})
		require.NoError(t, err)
		v, ok := promptCacheKey(t, out.Body)
		require.True(t, ok)
		return v
	}

	a := keyFor(t, `"prompt A"`)
	b := keyFor(t, `"prompt B"`)
	assert.NotEqual(t, a, b, "different cacheable prefixes must map to different cache keys")
}

// A same-format OpenAI caller that partitions caching with its own
// prompt_cache_key must keep it — the router's synthetic prefix hash must not
// clobber a deliberate caller key on the keyless fallback path.
func TestSessionAffinity_OpenAIPreservesCallerPromptCacheKey(t *testing.T) {
	const callerKey = "caller-partition-key"
	src := []byte(`{"model":"gpt-5.5","prompt_cache_key":"` + callerKey + `","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:    "gpt-5.5",
		TargetProvider: providers.ProviderOpenAI,
	})
	require.NoError(t, err)

	v, ok := promptCacheKey(t, out.Body)
	require.True(t, ok)
	assert.Equal(t, callerKey, v, "caller-supplied prompt_cache_key must be preserved")
}

// A keyless request with no cacheable prefix (no system, no tools) must stay
// unhinted: hashing the empty prefix would herd every such conversation onto
// one synthetic cache key.
func TestSessionAffinity_OpenAIEmptyPrefixStaysUnhinted(t *testing.T) {
	src := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:    "gpt-5.5",
		TargetProvider: providers.ProviderOpenAI,
	})
	require.NoError(t, err)

	_, ok := promptCacheKey(t, out.Body)
	assert.False(t, ok, "a prefix-less keyless request must not carry a synthetic prompt_cache_key")
}

// An empty tools array ("tools":[]) is not a cacheable prefix: gjson's
// .Exists() is true and .Raw is "[]" (non-empty), so a naive presence check
// would herd every such keyless request onto one synthetic key. It must stay
// unhinted just like a request with no tools field at all.
func TestSessionAffinity_OpenAIEmptyToolsArrayStaysUnhinted(t *testing.T) {
	src := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"tools":[]}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:    "gpt-5.5",
		TargetProvider: providers.ProviderOpenAI,
	})
	require.NoError(t, err)

	_, ok := promptCacheKey(t, out.Body)
	assert.False(t, ok, "an empty tools array must not count as a cacheable prefix")
}
