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
	s.subsidyEnabled = true
	s.subsidyEpsilon = epsilon
	s.subsidyGamma = gamma
	return s
}

// WithUsageObserver wires the subscription rate-limit observer WITHOUT enabling
// the cost discount. The composition root calls this so the per-installation
// usage-bypass gate works even when ROUTER_SUBSCRIPTION_AWARE_ROUTING is off;
// WithSubscriptionAwareRouting additionally turns on the discount.
func (s *Service) WithUsageObserver(obs *usage.Observer) *Service {
	s.usageObserver = obs
	return s
}

// presentSubscriptionTokens returns the caller's Codex and Claude subscription
// tokens, "" when absent. It sources each from BOTH the dedicated
// X-Weave-*-Subscription headers (the opencode dual-sub path) AND the inbound
// Authorization bearer (Claude Code's sk-ant-oat… / Codex CLI's JWT+account-id
// on their native harnesses) — mirroring resolveAndInjectCredentials so the
// subsidy works for all three harnesses, not just opencode. The token doubles as
// the usage-observer key, and equals the eventually-resolved credential, so
// record and read agree regardless of source.
func (s *Service) presentSubscriptionTokens(ctx context.Context, headers http.Header) (codex, anthropic string) {
	if codexSubscriptionFromContext(ctx) != nil {
		codex = openaiSubscriptionFromContext(ctx)
	} else if c := ExtractClientCredentials(providers.ProviderOpenAI, headers); c != nil && c.OAuth {
		codex = string(c.APIKey)
	}
	if t := anthropicSubscriptionFromContext(ctx); t != "" {
		anthropic = t
	} else if c := ExtractClientCredentials(providers.ProviderAnthropic, headers); c != nil && c.OAuth {
		anthropic = string(c.APIKey)
	}
	return codex, anthropic
}

// withUsageObserver installs a context header observer that records the present
// subscription's rate-limit headroom from upstream response headers. No-op
// (returns ctx) when the feature is off or no subscription is present.
func (s *Service) withUsageObserver(ctx context.Context, headers http.Header) context.Context {
	if s.usageObserver == nil {
		return ctx
	}
	codexTok, anthroTok := s.presentSubscriptionTokens(ctx, headers)
	if codexTok == "" && anthroTok == "" {
		return ctx
	}
	obs := func(callCtx context.Context, h http.Header) {
		// Record only when the call's RESOLVED credential is one of THIS request's
		// detected subscription tokens — not any incidental OAuth credential — and
		// key by that token so subsidyFactors reads the same key. Gating on the
		// resolved credential also skips internal calls that don't use the sub,
		// e.g. the handover summarizer's deployment-key Anthropic call after
		// clearCredentials. The parser follows the matched token's family, so this
		// works whether the sub arrived via a dedicated header or the inbound bearer.
		creds := CredentialsFromContext(callCtx)
		if creds == nil || !creds.OAuth {
			return
		}
		switch string(creds.APIKey) {
		case codexTok:
			if snap, ok := usage.ParseCodexHeaders(h); ok {
				s.usageObserver.Record(s.usageObserver.Key([]byte(codexTok)), snap)
			}
		case anthroTok:
			if snap, ok := usage.ParseAnthropicUnifiedHeaders(h); ok {
				s.usageObserver.Record(s.usageObserver.Key([]byte(anthroTok)), snap)
			}
		}
	}
	return providers.WithUpstreamHeaderObserver(ctx, obs)
}

// subsidyFactors computes the per-covered-model cost multiplier for this
// request from the present subscription(s). When headroom has been observed it
// uses the real factor; when a subscription is present but NO headroom has been
// observed yet, it OPTIMISTICALLY assumes slack (the epsilon floor) so the
// covered models are favored from the very first turn. This bootstraps the
// feature: otherwise the subscription would never serve a turn, so its headroom
// would never be observed, so the discount would never engage — a chicken-and-
// egg that pins routing to whatever wins at full price. The optimistic factor
// self-corrects to the real headroom once the first subscription-served response
// records it (including a 429's near-cap reading). Returns nil only when the
// feature is off or no subscription is present. Keyed identically to
// withUsageObserver so record and read agree across all three harnesses.
func (s *Service) subsidyFactors(ctx context.Context, headers http.Header) map[string]float64 {
	if s.usageObserver == nil || !s.subsidyEnabled {
		return nil
	}
	codexTok, anthroTok := s.presentSubscriptionTokens(ctx, headers)
	factors := make(map[string]float64)
	if codexTok != "" {
		f := s.observedOrOptimisticFactor(codexTok)
		for _, m := range codexCoveredModels {
			factors[m] = f
		}
	}
	if anthroTok != "" {
		f := s.observedOrOptimisticFactor(anthroTok)
		for _, m := range claudeCoveredModels() {
			factors[m] = f
		}
	}
	if len(factors) == 0 {
		return nil
	}
	return factors
}

// observedOrOptimisticFactor returns the cost factor for a present subscription:
// the real factor from observed headroom, or the optimistic slack floor
// (subsidyEpsilon) when none has been observed yet. The optimistic default is
// what lets the covered models win the FIRST turn and thereby get a chance to
// serve and record real headroom; without it the feature never bootstraps.
//
// The optimistic floor is safe here precisely because the observer keeps a
// reading alive for the life of its binding quota window (usage.Observer.freshFor),
// not a short TTL: a Snapshot miss therefore means the credential was genuinely
// never observed, or its window has since reset — both states where assuming
// slack is correct. A credential observed near its cap keeps returning that
// near-1.0 factor (no optimistic re-subsidy) until its window actually resets.
func (s *Service) observedOrOptimisticFactor(token string) float64 {
	if snap, ok := s.usageObserver.Snapshot(s.usageObserver.Key([]byte(token))); ok {
		return snap.CostFactor(s.subsidyEpsilon, s.subsidyGamma)
	}
	return s.subsidyEpsilon
}
