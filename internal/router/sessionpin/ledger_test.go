package sessionpin_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"workweave/router/internal/router/sessionpin"
)

func TestLedgerKey(t *testing.T) {
	assert.Equal(t, "anthropic/claude-opus-4-8", sessionpin.LedgerKey("anthropic", "claude-opus-4-8"))
}

func TestWarmPrefixTokens(t *testing.T) {
	now := time.Now()
	ledger := sessionpin.CacheLedger{
		sessionpin.LedgerKey("anthropic", "claude-opus-4-8"): {
			LastTurnAt:      now.Add(-10 * time.Minute),
			LastInputTokens: 42_000,
		},
		sessionpin.LedgerKey("fireworks", "deepseek/deepseek-v4-pro"): {
			LastTurnAt:      now.Add(-30 * time.Minute),
			LastInputTokens: 9_000,
		},
	}

	hourTTL := time.Hour
	assert.Equal(t, 42_000, ledger.WarmPrefixTokens("anthropic", "claude-opus-4-8", now, hourTTL),
		"entry inside TTL reports its last input size as the warm prefix")

	fiveMinTTL := 5 * time.Minute
	assert.Equal(t, 0, ledger.WarmPrefixTokens("fireworks", "deepseek/deepseek-v4-pro", now, fiveMinTTL),
		"entry past the provider TTL is cold")

	assert.Equal(t, 0, ledger.WarmPrefixTokens("openai", "gpt-5.5", now, hourTTL),
		"unknown pair is cold")
	assert.Equal(t, 0, ledger.WarmPrefixTokens("anthropic", "claude-opus-4-8", now, 0),
		"non-positive TTL is cold")
}

func TestTTLRemainingFrac(t *testing.T) {
	now := time.Now()
	ledger := sessionpin.CacheLedger{
		sessionpin.LedgerKey("anthropic", "claude-opus-4-8"): {
			LastTurnAt: now.Add(-15 * time.Minute),
		},
	}

	frac := ledger.TTLRemainingFrac("anthropic", "claude-opus-4-8", now, time.Hour)
	assert.InDelta(t, 0.75, frac, 0.01, "15 of 60 minutes elapsed leaves 75% of the TTL")

	assert.Zero(t, ledger.TTLRemainingFrac("anthropic", "claude-opus-4-8", now, 10*time.Minute),
		"lapsed entry reports 0")
	assert.Zero(t, ledger.TTLRemainingFrac("openai", "gpt-5.5", now, time.Hour),
		"unknown pair reports 0")
}

func TestNilLedgerIsCold(t *testing.T) {
	var ledger sessionpin.CacheLedger
	now := time.Now()
	assert.Equal(t, 0, ledger.WarmPrefixTokens("anthropic", "claude-opus-4-8", now, time.Hour))
	assert.Zero(t, ledger.TTLRemainingFrac("anthropic", "claude-opus-4-8", now, time.Hour))
}
