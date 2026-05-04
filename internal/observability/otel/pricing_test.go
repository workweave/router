package otel_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"workweave/router/internal/observability/otel"
)

func TestLookup(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		wantInput  float64
		wantOutput float64
	}{
		// ── Anthropic ──────────────────────────────────────────
		{name: "claude-opus-4-7", model: "claude-opus-4-7", wantInput: 15.00, wantOutput: 75.00},
		{name: "claude-sonnet-4-5", model: "claude-sonnet-4-5", wantInput: 3.00, wantOutput: 15.00},
		{name: "claude-haiku-4-5", model: "claude-haiku-4-5", wantInput: 0.80, wantOutput: 4.00},

		// ── OpenAI GPT-5.5 ─────────────────────────────────────
		{name: "gpt-5.5", model: "gpt-5.5", wantInput: 5.00, wantOutput: 40.00},
		{name: "gpt-5.5-pro", model: "gpt-5.5-pro", wantInput: 30.00, wantOutput: 120.00},
		{name: "gpt-5.5-mini", model: "gpt-5.5-mini", wantInput: 0.50, wantOutput: 2.50},
		{name: "gpt-5.5-nano", model: "gpt-5.5-nano", wantInput: 0.15, wantOutput: 0.60},

		// ── OpenAI GPT-5.4 ─────────────────────────────────────
		{name: "gpt-5.4", model: "gpt-5.4", wantInput: 3.00, wantOutput: 12.00},
		{name: "gpt-5.4-pro", model: "gpt-5.4-pro", wantInput: 20.00, wantOutput: 80.00},
		{name: "gpt-5.4-mini", model: "gpt-5.4-mini", wantInput: 0.40, wantOutput: 1.60},
		{name: "gpt-5.4-nano", model: "gpt-5.4-nano", wantInput: 0.10, wantOutput: 0.40},

		// ── OpenAI GPT-5 ───────────────────────────────────────
		{name: "gpt-5", model: "gpt-5", wantInput: 2.50, wantOutput: 10.00},
		{name: "gpt-5-chat", model: "gpt-5-chat", wantInput: 2.50, wantOutput: 10.00},
		{name: "gpt-5-mini", model: "gpt-5-mini", wantInput: 0.50, wantOutput: 2.00},
		{name: "gpt-5-nano", model: "gpt-5-nano", wantInput: 0.10, wantOutput: 0.40},

		// ── OpenAI GPT-4.x (legacy) ───────────────────────────
		{name: "gpt-4.1", model: "gpt-4.1", wantInput: 2.00, wantOutput: 8.00},
		{name: "gpt-4.1-mini", model: "gpt-4.1-mini", wantInput: 0.40, wantOutput: 1.60},
		{name: "gpt-4.1-nano", model: "gpt-4.1-nano", wantInput: 0.10, wantOutput: 0.40},
		{name: "gpt-4o", model: "gpt-4o", wantInput: 2.50, wantOutput: 10.00},
		{name: "gpt-4o-mini", model: "gpt-4o-mini", wantInput: 0.15, wantOutput: 0.60},

		// ── Google Gemini 3.x ──────────────────────────────────
		{name: "gemini-3-pro-preview", model: "gemini-3-pro-preview", wantInput: 2.00, wantOutput: 8.00},
		{name: "gemini-3.1-pro-preview", model: "gemini-3.1-pro-preview", wantInput: 2.00, wantOutput: 8.00},
		{name: "gemini-3-flash-preview", model: "gemini-3-flash-preview", wantInput: 0.50, wantOutput: 2.00},
		{name: "gemini-3.1-flash-lite-preview", model: "gemini-3.1-flash-lite-preview", wantInput: 0.10, wantOutput: 0.40},

		// ── Google Gemini 2.x (legacy) ─────────────────────────
		{name: "gemini-2.5-pro", model: "gemini-2.5-pro", wantInput: 1.25, wantOutput: 5.00},
		{name: "gemini-2.5-flash", model: "gemini-2.5-flash", wantInput: 0.30, wantOutput: 1.20},
		{name: "gemini-2.5-flash-lite", model: "gemini-2.5-flash-lite", wantInput: 0.10, wantOutput: 0.40},
		{name: "gemini-2.0-flash", model: "gemini-2.0-flash", wantInput: 0.10, wantOutput: 0.40},
		{name: "gemini-2.0-flash-lite", model: "gemini-2.0-flash-lite", wantInput: 0.075, wantOutput: 0.30},

		// ── Dated variants (8-digit suffix normalization) ──────
		{name: "claude-haiku-4-5-20251001", model: "claude-haiku-4-5-20251001", wantInput: 0.80, wantOutput: 4.00},
		{name: "claude-sonnet-4-5-20260315", model: "claude-sonnet-4-5-20260315", wantInput: 3.00, wantOutput: 15.00},
		{name: "claude-opus-4-7-20260101", model: "claude-opus-4-7-20260101", wantInput: 15.00, wantOutput: 75.00},
		{name: "gpt-5-12345678", model: "gpt-5-12345678", wantInput: 2.50, wantOutput: 10.00},
		{name: "gpt-4o-20250101", model: "gpt-4o-20250101", wantInput: 2.50, wantOutput: 10.00},
		{name: "gpt-4o-mini-20260601", model: "gpt-4o-mini-20260601", wantInput: 0.15, wantOutput: 0.60},
		{name: "gpt-4.1-mini-20251231", model: "gpt-4.1-mini-20251231", wantInput: 0.40, wantOutput: 1.60},
		{name: "gemini-2.5-pro-20260401", model: "gemini-2.5-pro-20260401", wantInput: 1.25, wantOutput: 5.00},
		{name: "gemini-2.5-flash-20260101", model: "gemini-2.5-flash-20260101", wantInput: 0.30, wantOutput: 1.20},

		// ── Unknown models ─────────────────────────────────────
		{name: "completely unknown", model: "nonexistent-model", wantInput: 0, wantOutput: 0},
		{name: "unknown with date suffix", model: "unknown-model-20251001", wantInput: 0, wantOutput: 0},
		{name: "Weave virtual model", model: "Weave", wantInput: 0, wantOutput: 0},
		{name: "empty string", model: "", wantInput: 0, wantOutput: 0},

		// ── Suffix edge cases (should NOT strip) ───────────────
		{name: "7-digit suffix not stripped", model: "claude-haiku-4-5-2025100", wantInput: 0, wantOutput: 0},
		{name: "9-digit suffix not stripped", model: "claude-haiku-4-5-202510011", wantInput: 0, wantOutput: 0},
		{name: "suffix with letters not stripped", model: "claude-haiku-4-5-2025abcd", wantInput: 0, wantOutput: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := otel.Lookup(tc.model)
			assert.Equal(t, tc.wantInput, got.InputUSDPer1M)
			assert.Equal(t, tc.wantOutput, got.OutputUSDPer1M)
		})
	}
}
