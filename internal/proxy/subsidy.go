package proxy

import (
	"context"
	"net/http"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy/usage"
	"workweave/router/internal/router/catalog"
)

// codexCoveredModels is the conservative set of GPT models a ChatGPT/Codex
// subscription actually pays for via the Codex backend (chatgpt.com/backend-api/
// codex). Deliberately a curated allowlist, not "every OpenAI model": the plan
// only bills the Codex-served GPT family, so subsidizing an OpenAI model the
// backend won't bill would bias toward a turn that then 4xxs. Extend as the
// catalog's Codex-billable GPT set grows.
var codexCoveredModels = []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini"}

// claudeCoveredModels returns the catalog models a Claude (Pro/Max) subscription
// covers — every Anthropic-primary model. Derived from the catalog so it tracks
// model additions without a second source of truth.
func claudeCoveredModels() []string {
	out := make([]string, 0, 8)
	for _, m := range catalog.Models {
		if m.PrimaryProvider() == providers.ProviderAnthropic {
			out = append(out, m.ID)
		}
	}
	return out
}

// WithSubscriptionAwareRouting turns on subscription-aware cost discounting.
// The composition root calls it (from ROUTER_SUBSCRIPTION_AWARE_ROUTING) with a
// constructed usage.Observer and the epsilon/gamma knobs. Left unset, the
// feature is fully off: no header observer is installed and no cost factors are
// computed, so routing is byte-for-byte today's behavior.
func (s *Service) WithSubscriptionAwareRouting(obs *usage.Observer, epsilon, gamma float64) *Service {
	s.usageObserver = obs
	s.subsidyEpsilon = epsilon
	s.subsidyGamma = gamma
	return s
}

// withUsageObserver installs a context header observer that records the present
// subscription credentials' rate-limit headroom from upstream response headers.
// Keyed by the dedicated-header subscription tokens (the opencode dual-sub path);
// the Claude Code inbound-bearer path is a follow-up. No-op (returns ctx) when
// the feature is off or no subscription is present.
func (s *Service) withUsageObserver(ctx context.Context) context.Context {
	if s.usageObserver == nil {
		return ctx
	}
	// Gate the Codex path on a usable Codex subscription (token + account-id),
	// mirroring subsidyFactors exactly — otherwise we'd record observations under
	// a key subsidyFactors never reads (token without account-id), silently
	// accumulating until TTL eviction.
	codexTok := ""
	if codexSubscriptionFromContext(ctx) != nil {
		codexTok = openaiSubscriptionFromContext(ctx)
	}
	anthroTok := anthropicSubscriptionFromContext(ctx)
	if codexTok == "" && anthroTok == "" {
		return ctx
	}
	obs := func(callCtx context.Context, h http.Header) {
		// Only record headers from a response actually served on the caller's
		// subscription — keyed by the call's RESOLVED credential, not the request's
		// stashed token. This skips internal calls on the same request that don't
		// use the sub (e.g. the handover summarizer's deployment-key Anthropic
		// call after clearCredentials), which would otherwise poison the headroom
		// snapshot with deployment-key rate-limit headers.
		creds := CredentialsFromContext(callCtx)
		if creds == nil || !creds.OAuth {
			return
		}
		tok := string(creds.APIKey)
		switch {
		case codexTok != "" && tok == codexTok:
			if snap, ok := usage.ParseCodexHeaders(h); ok {
				s.usageObserver.Record(s.usageObserver.Key([]byte(codexTok)), snap)
			}
		case anthroTok != "" && tok == anthroTok:
			if snap, ok := usage.ParseAnthropicUnifiedHeaders(h); ok {
				s.usageObserver.Record(s.usageObserver.Key([]byte(anthroTok)), snap)
			}
		}
	}
	return providers.WithUpstreamHeaderObserver(ctx, obs)
}

// subsidyFactors computes the per-covered-model cost multiplier for this
// request, from the present subscriptions' observed rate-limit headroom. Returns
// nil when the feature is off, no subscription is present, or no headroom has
// been observed yet for the present subscription(s) — in which case routing is
// unaffected (cold start = full price until the first response populates the
// observer). Keyed identically to withUsageObserver so record and read agree.
func (s *Service) subsidyFactors(ctx context.Context) map[string]float64 {
	if s.usageObserver == nil {
		return nil
	}
	factors := make(map[string]float64)
	if codexSubscriptionFromContext(ctx) != nil {
		if tok := openaiSubscriptionFromContext(ctx); tok != "" {
			if snap, ok := s.usageObserver.Snapshot(s.usageObserver.Key([]byte(tok))); ok {
				f := snap.CostFactor(s.subsidyEpsilon, s.subsidyGamma)
				for _, m := range codexCoveredModels {
					factors[m] = f
				}
			}
		}
	}
	if tok := anthropicSubscriptionFromContext(ctx); tok != "" {
		if snap, ok := s.usageObserver.Snapshot(s.usageObserver.Key([]byte(tok))); ok {
			f := snap.CostFactor(s.subsidyEpsilon, s.subsidyGamma)
			for _, m := range claudeCoveredModels() {
				factors[m] = f
			}
		}
	}
	if len(factors) == 0 {
		return nil
	}
	return factors
}
