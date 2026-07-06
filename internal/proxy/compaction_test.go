package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/router/turntype"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCompactionSummarizer struct {
	summary   string
	usage     handover.Usage
	err       error
	calls     int
	lastModel string
}

func (f *fakeCompactionSummarizer) SummarizeForCompaction(_ context.Context, _ *translate.RequestEnvelope, model string, _ int) (string, handover.Usage, error) {
	f.calls++
	f.lastModel = model
	return f.summary, f.usage, f.err
}

func (f *fakeCompactionSummarizer) Provider() string { return providers.ProviderAnthropic }

// alternatingAnthropicBody builds an Anthropic body of nMsgs user/assistant
// messages (starting with user), each padded to ~perMsgPad content bytes.
func alternatingAnthropicBody(nMsgs, perMsgPad int) []byte {
	pad := strings.Repeat("x", perMsgPad)
	var sb strings.Builder
	sb.WriteString(`{"model":"claude-opus-4-8","system":"sys","messages":[`)
	for i := range nMsgs {
		if i > 0 {
			sb.WriteString(",")
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sb.WriteString(`{"role":"` + role + `","content":"` + pad + `"}`)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// toolHeavyAnthropicBody builds nPairs of (assistant tool_use, user tool_result)
// with each tool_result carrying contentBytes of payload.
func toolHeavyAnthropicBody(nPairs, contentBytes int) []byte {
	pad := strings.Repeat("y", contentBytes)
	var sb strings.Builder
	sb.WriteString(`{"model":"claude-opus-4-8","messages":[`)
	for i := range nPairs {
		if i > 0 {
			sb.WriteString(",")
		}
		id := fmt.Sprintf("t%d", i)
		sb.WriteString(`{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"read","input":{}}]},`)
		sb.WriteString(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"` + pad + `"}]}`)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

func TestMaybeCompact_UnderThresholdIsNoop(t *testing.T) {
	s := &Service{compactionTriggerPct: DefaultCompactionTriggerPct, compactionSummarizer: &fakeCompactionSummarizer{}}
	env, err := translate.ParseAnthropic(alternatingAnthropicBody(2, 20))
	require.NoError(t, err)
	before := env.ContextOverflowTokenEstimate()

	res, err := s.maybeCompact(context.Background(), env, turntype.MainLoop, 0, 1_000_000, http.Header{})
	require.NoError(t, err)
	assert.False(t, res.Applied, "a small request must not be compacted")
	assert.Equal(t, before, env.ContextOverflowTokenEstimate(), "env must be untouched below threshold")
}

func TestMaybeCompact_DisabledWhenPctZero(t *testing.T) {
	s := &Service{} // compactionTriggerPct == 0 disables the cascade
	env, err := translate.ParseAnthropic(alternatingAnthropicBody(20, 200))
	require.NoError(t, err)
	res, err := s.maybeCompact(context.Background(), env, turntype.MainLoop, 0, 500, http.Header{})
	require.NoError(t, err)
	assert.False(t, res.Applied)
}

func TestMaybeCompact_Tier1ClearsToolResults(t *testing.T) {
	s := &Service{compactionTriggerPct: DefaultCompactionTriggerPct} // nil summarizer
	env, err := translate.ParseAnthropic(toolHeavyAnthropicBody(20, 300))
	require.NoError(t, err)
	before := env.ContextOverflowTokenEstimate()
	// maxWindow between post-Tier-1 and pre-Tier-1 estimates so Tier-1 alone fits.
	maxWindow := before * 3 / 4

	res, err := s.maybeCompact(context.Background(), env, turntype.MainLoop, 0, maxWindow, http.Header{})
	require.NoError(t, err)
	assert.True(t, res.Applied)
	assert.Positive(t, res.ToolResultsCleared, "old tool results should be cleared")
	assert.False(t, res.Summarized, "nil summarizer must not summarize")
	assert.LessOrEqual(t, env.ContextOverflowTokenEstimate(), maxWindow, "must fit after Tier-1")
}

func TestMaybeCompact_Tier3Summarizes(t *testing.T) {
	fake := &fakeCompactionSummarizer{summary: "SHORT STRUCTURED SUMMARY"}
	s := &Service{compactionTriggerPct: DefaultCompactionTriggerPct, compactionSummarizer: fake}
	env, err := translate.ParseAnthropic(alternatingAnthropicBody(20, 200))
	require.NoError(t, err)

	// Window that Tier-1 (no tool results here) can't satisfy but a
	// summarize + recent-12 rewrite can.
	res, err := s.maybeCompact(context.Background(), env, turntype.MainLoop, 0, 900, http.Header{})
	require.NoError(t, err)
	assert.True(t, res.Applied)
	assert.True(t, res.Summarized)
	assert.Equal(t, DefaultHandoverModel, res.SummaryModel, "small history summarizes with the cheap model")
	assert.Equal(t, 1, fake.calls)
	assert.Equal(t, DefaultHandoverModel, fake.lastModel)
}

func TestMaybeCompact_ExceedsFloorReturnsSentinel(t *testing.T) {
	s := &Service{compactionTriggerPct: DefaultCompactionTriggerPct} // nil summarizer
	env, err := translate.ParseAnthropic(alternatingAnthropicBody(4, 400))
	require.NoError(t, err)

	// A window so small that even trimming to a single (large) message overflows.
	_, err = s.maybeCompact(context.Background(), env, turntype.MainLoop, 0, 30, http.Header{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrContextWindowExceeded))
}

func TestMaybeCompact_SkipsHardPinnedTurns(t *testing.T) {
	fake := &fakeCompactionSummarizer{summary: "x"}
	s := &Service{compactionTriggerPct: DefaultCompactionTriggerPct, compactionSummarizer: fake}
	env, err := translate.ParseAnthropic(alternatingAnthropicBody(20, 200))
	require.NoError(t, err)
	before := env.ContextOverflowTokenEstimate()

	// A Compaction turn is Claude Code's own compaction request — the router
	// must not rewrite it, even when it's over threshold.
	res, err := s.maybeCompact(context.Background(), env, turntype.Compaction, 0, 900, http.Header{})
	require.NoError(t, err)
	assert.False(t, res.Applied, "hard-pinned turns must skip compaction")
	assert.Equal(t, 0, fake.calls, "summarizer must not be called for a Compaction turn")
	assert.Equal(t, before, env.ContextOverflowTokenEstimate(), "env must be untouched")
}

func TestWithCompaction_ZeroPctDisables(t *testing.T) {
	// ROUTER_COMPACTION_PCT=0 must disable, not fall back to the default.
	s := (&Service{}).WithCompaction(nil, 0)
	assert.Equal(t, 0.0, s.compactionTriggerPct)
	// A negative/out-of-range value falls back to the default.
	s = (&Service{}).WithCompaction(nil, -1)
	assert.Equal(t, DefaultCompactionTriggerPct, s.compactionTriggerPct)
	s = (&Service{}).WithCompaction(nil, 2)
	assert.Equal(t, DefaultCompactionTriggerPct, s.compactionTriggerPct)
}

func TestSelectCompactionSummarizer_WindowAware(t *testing.T) {
	s := &Service{}
	assert.Equal(t, DefaultHandoverModel, s.selectCompactionSummarizer(1_000), "small history → cheap model")
	assert.Equal(t, largeWindowSummarizerModel, s.selectCompactionSummarizer(300_000), "history over the cheap model's window → large-window model")
	assert.Equal(t, "", s.selectCompactionSummarizer(5_000_000), "history over every window → none")
}

func TestMaxEligibleContextWindow(t *testing.T) {
	s := &Service{availableModels: map[string]struct{}{"claude-haiku-4-5": {}}}
	assert.Equal(t, 200_000, s.maxEligibleContextWindow(nil, 0))
	assert.Equal(t, 200_000, s.maxEligibleContextWindow(nil, 5_000), "Anthropic (signature-keeping) models ignore signature savings")
	assert.Equal(t, 0, s.maxEligibleContextWindow(map[string]struct{}{"claude-haiku-4-5": {}}, 0), "policy-excluding the only model leaves no window")

	// A signature-stripping (non-Anthropic) model gets sigSavings added to its
	// effective window, matching the context-overflow pre-filter's discount.
	sStrip := &Service{availableModels: map[string]struct{}{"gpt-5.5": {}}}
	assert.Equal(t, 1_050_000, sStrip.maxEligibleContextWindow(nil, 0))
	assert.Equal(t, 1_050_000+5_000, sStrip.maxEligibleContextWindow(nil, 5_000), "stripping model gains signature savings as headroom")
}

func TestClassifyDispatchError_ContextWindowExceeded(t *testing.T) {
	cls, ok := ClassifyDispatchError(fmt.Errorf("wrapped: %w", ErrContextWindowExceeded))
	require.True(t, ok)
	assert.Equal(t, http.StatusRequestEntityTooLarge, cls.Status)
	assert.Equal(t, DispatchErrorContextWindowExceeded, cls.Kind)
	assert.True(t, cls.Kind.IsClientError())
}
