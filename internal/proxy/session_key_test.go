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
	// Claude Code mutates the system prompt every turn, so turn2 carries a
	// different system block. The key must still be stable: it keys on the
	// (unchanging) first user message, not the volatile system text.
	turn1 := anthropicEnv(t, `{
		"model": "claude-sonnet-4-6",
		"system": "You are a careful coding assistant. <reminder>cwd=/a</reminder>",
		"messages": [
			{"role": "user", "content": "Help me refactor server.go"}
		]
	}`)
	turn2 := anthropicEnv(t, `{
		"model": "claude-sonnet-4-6",
		"system": "You are a careful coding assistant. <reminder>cwd=/a/b time=12:01</reminder>",
		"messages": [
			{"role": "user", "content": "Help me refactor server.go"},
			{"role": "assistant", "content": "Sure, what's broken?"},
			{"role": "user", "content": "Now also extract the dispatch loop"}
		]
	}`)

	k1 := proxy.DeriveSessionKey(turn1, "api-key-A")
	k2 := proxy.DeriveSessionKey(turn2, "api-key-A")

	assert.Equal(t, k1, k2, "key stable across turns: first user message unchanged, volatile system ignored")
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

func TestDeriveSessionKey_IgnoresSystemPrompt(t *testing.T) {
	// System text is volatile (Claude Code rewrites it every turn), so it must
	// NOT move the key. Two requests that differ only in system prompt — same
	// first user message — collide.
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

	assert.Equal(t, kA, kB, "system prompt is volatile and must not affect the key")
}

func TestDeriveSessionKey_SeparatesSubAgentsSharingUserID(t *testing.T) {
	// The fix: Claude Code sends ONE metadata.user_id for the main loop and all
	// of a session's sub-agents. Keying on user_id alone funneled them onto a
	// single pin that concurrent threads thrashed. The first user message (each
	// sub-agent's distinct dispatch prompt) must split them into separate pins.
	mainLoop := anthropicEnv(t, `{
		"metadata": {"user_id": "device=abc;account=u1;session=42"},
		"system": "main loop system",
		"messages": [{"role": "user", "content": "Refactor the dispatch loop in server.go"}]
	}`)
	subAgent := anthropicEnv(t, `{
		"metadata": {"user_id": "device=abc;account=u1;session=42"},
		"system": "explore sub-agent system",
		"messages": [{"role": "user", "content": "Find every .go file under internal/"}]
	}`)

	kMain := proxy.DeriveSessionKey(mainLoop, "api-key")
	kSub := proxy.DeriveSessionKey(subAgent, "api-key")

	assert.NotEqual(t, kMain, kSub, "same user_id but different first message (sub-agent) must get a distinct pin")
}

func TestDeriveSessionKey_SameUserIDAndFirstMessageCollide(t *testing.T) {
	// Stability counterpart: same user_id AND same first user message — across
	// a thread's turns, even as the system prompt mutates — stay on one pin.
	turn1 := anthropicEnv(t, `{
		"metadata": {"user_id": "device=abc;session=42"},
		"system": "system v1",
		"messages": [{"role": "user", "content": "first turn"}]
	}`)
	turn2 := anthropicEnv(t, `{
		"metadata": {"user_id": "device=abc;session=42"},
		"system": "system v2 — mutated",
		"messages": [
			{"role": "user", "content": "first turn"},
			{"role": "assistant", "content": "ok"},
			{"role": "user", "content": "keep going"}
		]
	}`)

	k1 := proxy.DeriveSessionKey(turn1, "api-key")
	k2 := proxy.DeriveSessionKey(turn2, "api-key")

	assert.Equal(t, k1, k2, "same user_id + same first message → one stable pin across turns")
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

func openAIEnv(t *testing.T, body string) *translate.RequestEnvelope {
	t.Helper()
	env, err := translate.ParseOpenAI([]byte(body))
	require.NoError(t, err)
	return env
}

func TestDeriveSessionKey_OpenAILeadingSystemDoesNotCollapse(t *testing.T) {
	// OpenAI-format bodies carry `system` inside messages[], so messages.0 is a
	// system role and FirstUserMessageText is empty. Without metadata.user_id,
	// distinct conversations must still get distinct pins — DeriveSessionKey
	// falls back to system text rather than collapsing every OpenAI session on
	// one API key onto a single pin.
	convoA := openAIEnv(t, `{
		"messages": [
			{"role": "system", "content": "You are assistant A."},
			{"role": "user", "content": "task one"}
		]
	}`)
	convoB := openAIEnv(t, `{
		"messages": [
			{"role": "system", "content": "You are assistant B."},
			{"role": "user", "content": "task two"}
		]
	}`)

	kA := proxy.DeriveSessionKey(convoA, "api-key")
	kB := proxy.DeriveSessionKey(convoB, "api-key")

	assert.NotEqual(t, kA, kB, "distinct OpenAI conversations must not share a pin when the first user message is empty")
}
