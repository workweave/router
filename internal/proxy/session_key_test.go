package proxy_test

import (
	"testing"

	"workweave/router/internal/proxy"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func anthropicEnv(t *testing.T, body string) *translate.RequestEnvelope {
	t.Helper()
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)
	return env
}

func TestDeriveSessionKey_StableAcrossTurns(t *testing.T) {
	turn1 := anthropicEnv(t, `{
		"model": "claude-sonnet-4-6",
		"system": "You are a careful coding assistant.",
		"messages": [
			{"role": "user", "content": "Help me refactor server.go"}
		]
	}`)
	turn2 := anthropicEnv(t, `{
		"model": "claude-sonnet-4-6",
		"system": "You are a careful coding assistant.",
		"messages": [
			{"role": "user", "content": "Help me refactor server.go"},
			{"role": "assistant", "content": "Sure, what's broken?"},
			{"role": "user", "content": "Now also extract the dispatch loop"}
		]
	}`)

	k1 := proxy.DeriveSessionKey(turn1, "api-key-A")
	k2 := proxy.DeriveSessionKey(turn2, "api-key-A")

	assert.Equal(t, k1, k2, "key stable across turns: system + first user message don't change")
	assert.Len(t, k1, sessionpin.SessionKeyLen)
}

func TestDeriveSessionKey_DiffersAcrossAPIKeys(t *testing.T) {
	env := anthropicEnv(t, `{
		"system": "X",
		"messages": [{"role": "user", "content": "hello"}]
	}`)

	k1 := proxy.DeriveSessionKey(env, "api-key-A")
	k2 := proxy.DeriveSessionKey(env, "api-key-B")

	assert.NotEqual(t, k1, k2, "distinct callers must not collide on identical prompts")
}

func TestDeriveSessionKey_DiffersAcrossSystemPrompts(t *testing.T) {
	envA := anthropicEnv(t, `{
		"system": "You are agent A.",
		"messages": [{"role": "user", "content": "go"}]
	}`)
	envB := anthropicEnv(t, `{
		"system": "You are agent B.",
		"messages": [{"role": "user", "content": "go"}]
	}`)

	kA := proxy.DeriveSessionKey(envA, "api-key")
	kB := proxy.DeriveSessionKey(envB, "api-key")

	assert.NotEqual(t, kA, kB)
}

func TestDeriveSessionKey_PrefersMetadataUserID(t *testing.T) {
	// Same metadata.user_id, different prompt prefixes → must collide.
	env1 := anthropicEnv(t, `{
		"metadata": {"user_id": "device=abc;session=42"},
		"system": "irrelevant 1",
		"messages": [{"role": "user", "content": "first turn"}]
	}`)
	env2 := anthropicEnv(t, `{
		"metadata": {"user_id": "device=abc;session=42"},
		"system": "irrelevant 2 — different now",
		"messages": [{"role": "user", "content": "third turn"}]
	}`)

	k1 := proxy.DeriveSessionKey(env1, "api-key")
	k2 := proxy.DeriveSessionKey(env2, "api-key")

	assert.Equal(t, k1, k2, "metadata.user_id takes precedence over prompt-prefix fallback")
}

func TestDeriveSessionKey_DistinctMetadataUserIDsDoNotCollide(t *testing.T) {
	// Claude Code packs {device_id, account_uuid, session_id} into
	// metadata.user_id, so different sessions for the same human produce
	// different keys — that's the whole point of a *session* pin.
	envA := anthropicEnv(t, `{
		"metadata": {"user_id": "device=abc;session=1"},
		"messages": [{"role": "user", "content": "x"}]
	}`)
	envB := anthropicEnv(t, `{
		"metadata": {"user_id": "device=abc;session=2"},
		"messages": [{"role": "user", "content": "x"}]
	}`)

	kA := proxy.DeriveSessionKey(envA, "api-key")
	kB := proxy.DeriveSessionKey(envB, "api-key")

	assert.NotEqual(t, kA, kB)
}

func TestDeriveSessionKey_MetadataTierDoesNotCollideWithPromptTier(t *testing.T) {
	// Domain separation: metadata-tier and prompt-prefix-tier must not
	// collide even when metadata.user_id matches the prompt-prefix shape.
	envWithMeta := anthropicEnv(t, `{
		"metadata": {"user_id": "user_id:fake"},
		"messages": [{"role": "user", "content": "x"}]
	}`)
	envNoMeta := anthropicEnv(t, `{
		"system": "user_id:fake",
		"messages": [{"role": "user", "content": ""}]
	}`)

	kMeta := proxy.DeriveSessionKey(envWithMeta, "api-key")
	kNoMeta := proxy.DeriveSessionKey(envNoMeta, "api-key")

	assert.NotEqual(t, kMeta, kNoMeta)
}

func TestDeriveSessionKey_NilEnvelopeStillKeyedByAPIKey(t *testing.T) {
	// Defensive: parsing failure must not leak across callers via a shared pin.
	kA := proxy.DeriveSessionKey(nil, "api-key-A")
	kB := proxy.DeriveSessionKey(nil, "api-key-B")
	assert.NotEqual(t, kA, kB)
}
