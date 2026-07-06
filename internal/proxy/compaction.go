package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"workweave/router/internal/billing"
	"workweave/router/internal/observability"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/router/turntype"
	"workweave/router/internal/translate"
)

// ErrContextWindowExceeded is returned after the full compaction cascade
// (tool-result cleanup, summarization, trim) still can't fit any eligible
// model's window. Maps to HTTP 413, distinct from ErrNoEligibleProvider.
var ErrContextWindowExceeded = errors.New("proxy: request context exceeds every eligible model's window")

const (
	// DefaultCompactionTriggerPct is the fraction of the largest eligible
	// model's window at which the cascade engages. Mirrors Claude Code's own
	// ~0.85 auto-compact trigger: compacting below the window (not at overflow)
	// keeps the pre-summary history small enough for a summarizer to ingest.
	DefaultCompactionTriggerPct = 0.85
	// compactionRecentTurns is how many trailing non-system messages survive a
	// summarization rewrite, so the model keeps immediate working context.
	compactionRecentTurns = 12
	// compactionToolResultKeep is how many trailing tool results Tier-1 cleanup
	// leaves intact; older ones are replaced with a placeholder.
	compactionToolResultKeep = 5
	// compactionSummaryOutputReserve is headroom (summary output + margin) the
	// selected summarizer model needs above the history it must ingest.
	compactionSummaryOutputReserve = DefaultCompactionMaxTokens + 8_000
	// largeWindowSummarizerModel is the big-context Anthropic-family model used
	// to summarize histories too large for the cheap default summarizer. It is
	// Anthropic-family so the Anthropic-only ProviderSummarizer can target it
	// with no cross-format translation.
	largeWindowSummarizerModel = "claude-fable-5"
)

// compactionSummarizerModels are the Anthropic-family models the cascade may
// summarize with, ordered cheapest-first. The selector picks the first whose
// context window can ingest the (post-Tier-1) history plus summary headroom.
var compactionSummarizerModels = []string{DefaultHandoverModel, largeWindowSummarizerModel}

// CompactionSummarizer summarizes prior conversation with the structured
// compaction prompt against an explicit model. Implemented by
// *ProviderSummarizer; declared here so the Service depends on the behavior,
// not the concrete type.
type CompactionSummarizer interface {
	SummarizeForCompaction(ctx context.Context, env *translate.RequestEnvelope, model string, maxTokens int) (string, handover.Usage, error)
	Provider() string
}

// compactionResult records what the cascade did, for logging and billing.
type compactionResult struct {
	Applied            bool
	ToolResultsCleared int
	Summarized         bool
	SummaryModel       string
	SummaryUsage       handover.Usage
	TrimmedToRecent    int
	FinalEstimate      int
}

// maxEligibleContextWindow returns the largest effective context window among
// available routing models that are not policy-excluded. A signature-stripping
// (non-Anthropic) target gets sigSavings added to its window: the translator
// drops base64 thought-signature blocks before dispatch, so that model can
// serve sigSavings more of this request's estimated tokens — mirroring the
// per-model discount in excludeContextOverflowModels, so a signature-heavy
// session isn't falsely 413'd when a stripping model would still fit. Zero when
// none are known (availableModels unset), which disables compaction.
func (s *Service) maxEligibleContextWindow(policyExcluded map[string]struct{}, sigSavings int) int {
	maxWindow := 0
	for model := range s.availableModels {
		if _, excluded := policyExcluded[model]; excluded {
			continue
		}
		w := contextWindowForRequest(model)
		if sigSavings > 0 && modelStripsAnthropicSignatures(model) {
			w += sigSavings
		}
		if w > maxWindow {
			maxWindow = w
		}
	}
	return maxWindow
}

// selectCompactionSummarizer returns the cheapest configured summarizer model
// whose context window can ingest historyTokens plus summary headroom, or ""
// when none can (caller falls back to trimming).
func (s *Service) selectCompactionSummarizer(historyTokens int) string {
	need := historyTokens + compactionSummaryOutputReserve
	for _, m := range compactionSummarizerModels {
		if catalog.ContextWindowFor(m) >= need {
			return m
		}
	}
	return ""
}

// maybeCompact runs the compaction cascade when needed ≥ compactionTriggerPct
// of maxWindow: (1) clear old tool results, (2) summarize with a window-aware
// model, (3) progressive trim. Mutates env in place — caller MUST recompute
// estimates when res.Applied is true. Returns ErrContextWindowExceeded if the
// history overflows even after all tiers; no-ops when pct is zero/unset, below
// threshold, or the turn is hard-pinned (Claude Code's own compaction turn must
// not be rewritten, and probe/title-gen/classifier turns bypass the scorer).
func (s *Service) maybeCompact(ctx context.Context, env *translate.RequestEnvelope, tt turntype.TurnType, outputReserve, maxWindow int, reqHeaders http.Header) (compactionResult, error) {
	log := observability.FromContext(ctx)
	var res compactionResult
	if s.compactionTriggerPct <= 0 || maxWindow <= 0 || env == nil || s.isHardPinnedTurn(ctx, tt) {
		return res, nil
	}

	needed := func() int { return env.ContextOverflowTokenEstimate() + outputReserve }
	fits := func() bool { return needed() <= maxWindow }
	trigger := int(float64(maxWindow) * s.compactionTriggerPct)
	if needed() < trigger {
		return res, nil
	}
	log.Info("Compaction cascade engaged",
		"needed", needed(),
		"trigger", trigger,
		"max_window", maxWindow,
	)

	// Tier 1: clear stale tool results (cheap, local, no model call).
	if n := env.ClearOldToolResults(compactionToolResultKeep); n > 0 {
		res.Applied = true
		res.ToolResultsCleared = n
		log.Info("Compaction Tier-1: cleared old tool results", "cleared", n, "needed_after", needed())
	}
	if fits() {
		res.FinalEstimate = needed()
		return res, nil
	}

	// Tier 3: structured summarization with a window-aware model.
	if s.compactionSummarizer != nil {
		if summary, usage, model, ok := s.runCompactionSummary(ctx, env, reqHeaders); ok {
			env.RewriteForCompaction(summary, compactionRecentTurns)
			res.Applied = true
			res.Summarized = true
			res.SummaryModel = model
			res.SummaryUsage = usage
			log.Info("Compaction Tier-3: history summarized", "summary_model", model, "needed_after", needed())
		}
	}
	if fits() {
		res.FinalEstimate = needed()
		return res, nil
	}

	// Rescue: trim recent turns progressively until the request fits.
	for _, n := range []int{compactionRecentTurns, 6, 3, 1} {
		if env.TrimLastNMessages(n) > 0 {
			res.Applied = true
			res.TrimmedToRecent = n
		}
		if fits() {
			res.FinalEstimate = needed()
			log.Info("Compaction rescue: trimmed to recent turns", "kept_recent", n, "needed_after", needed())
			return res, nil
		}
	}

	// Floor: even the last user turn overflows the largest window.
	res.FinalEstimate = needed()
	return res, fmt.Errorf("context ~%d tokens over largest window %d: %w", res.FinalEstimate, maxWindow, ErrContextWindowExceeded)
}

// billCompactionSummary debits the compaction summary call as its own ledger
// row (mirrors the switch-handover summary billing). No-ops when billing is
// unwired or the usage carries no tokens.
func (s *Service) billCompactionSummary(ctx context.Context, requestID, externalID string, usage handover.Usage) {
	if usage.Model == "" || (usage.InputTokens == 0 && usage.OutputTokens == 0) {
		return
	}
	sumPricing, _ := catalog.PrimaryPriceFor(usage.Model)
	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	s.fireBilling(ctx, billing.DebitInferenceParams{
		OrganizationID:  externalID,
		RouterRequestID: requestID + "_precompaction_summary",
		Model:           usage.Model,
		Provider:        usage.Provider,
		InputTokens:     usage.InputTokens,
		OutputTokens:    usage.OutputTokens,
		CacheCreation:   usage.CacheCreation,
		CacheRead:       usage.CacheRead,
		Pricing:         sumPricing,
		HasOverride:     billing.HasOverrideFromContext(ctx),
		APIKeyID:        apiKeyID,
	})
}

// runCompactionSummary picks a window-aware summarizer model and dispatches the
// structured summary call, honoring the tenant-boundary credential rules used
// by the switch-handover path. Returns ok=false (and logs) when no summarizer
// fits the history, the tenant boundary forbids the call, or the call fails —
// in every such case the caller falls through to trimming.
func (s *Service) runCompactionSummary(ctx context.Context, env *translate.RequestEnvelope, reqHeaders http.Header) (string, handover.Usage, string, bool) {
	log := observability.FromContext(ctx)

	model := s.selectCompactionSummarizer(env.ContextOverflowTokenEstimate())
	if model == "" {
		log.Info("Compaction Tier-3 skipped: history exceeds every summarizer window", "history", env.ContextOverflowTokenEstimate())
		return "", handover.Usage{}, "", false
	}

	sumProvider := s.compactionSummarizer.Provider()
	sumCreds := resolveSummarizerCreds(ctx, sumProvider, reqHeaders)
	if sumCreds == nil && s.requestUsesNonDeploymentCreds(ctx, reqHeaders) {
		log.Info("Compaction Tier-3 skipped: would cross tenant boundary", "sum_provider", sumProvider)
		return "", handover.Usage{}, "", false
	}
	summCtx := ctx
	if sumCreds != nil {
		summCtx = context.WithValue(ctx, CredentialsContextKey{}, sumCreds)
	} else {
		summCtx = clearCredentials(ctx)
	}

	summary, usage, err := s.compactionSummarizer.SummarizeForCompaction(summCtx, env, model, DefaultCompactionMaxTokens)
	if err != nil {
		log.Warn("Compaction summarizer failed; falling back to trim", "err", err, "model", model)
		return "", handover.Usage{}, "", false
	}
	if summary == "" {
		log.Warn("Compaction summarizer returned empty; falling back to trim", "model", model)
		return "", handover.Usage{}, "", false
	}
	return summary, usage, model, true
}
