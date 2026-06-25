package proxy

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/translate"
)

// usageBypassEngaged reports whether this Anthropic-Messages request should skip
// cluster routing entirely and pass straight through to the requested model on
// the caller's own Claude subscription. It engages only when:
//
//   - the installation has turned the gate on (usageBypassFromContext),
//   - the subscription usage observer is wired (it drives the threshold read),
//   - the request presents a Claude subscription credential (the turn is paid
//     for by the customer's own plan, so there's nothing for the router to save
//     and nothing for us to bill),
//   - the requested model is Anthropic-served (the only thing a Claude
//     subscription can actually serve — a cross-vendor request can't bypass
//     onto the subscription), and
//   - observed utilization is still below the threshold, OR nothing has been
//     observed yet (cold start: serve the first turn on the subscription so its
//     response primes the observer, mirroring the subsidy bootstrap).
//
// Once observed utilization crosses the threshold the gate disengages and the
// normal routing path (scorer + subscription-aware cost discounting) takes over,
// so the caller starts conserving their remaining quota.
func (s *Service) usageBypassEngaged(ctx context.Context, headers http.Header, requestedModel string) bool {
	cfg, ok := usageBypassFromContext(ctx)
	if !ok || s.usageObserver == nil {
		return false
	}
	if m, found := catalog.ByID(requestedModel); !found || m.PrimaryProvider() != providers.ProviderAnthropic {
		return false
	}
	_, anthroTok := s.presentSubscriptionTokens(ctx, headers)
	if anthroTok == "" {
		return false
	}
	threshold := defaultUsageBypassThreshold
	if cfg.Threshold != nil {
		threshold = *cfg.Threshold
	}
	snap, observed := s.usageObserver.Snapshot(s.usageObserver.Key([]byte(anthroTok)))
	if !observed {
		return true
	}
	util := max(snap.Primary.UsedPercent, snap.Secondary.UsedPercent)
	return util < threshold
}

// bypassToAnthropic proxies an inbound Anthropic-Messages request straight to
// the Anthropic provider with the caller-requested model. It deliberately skips
// the cluster scorer, planner, session pin, semantic cache, AND billing: the
// turn runs on the customer's own subscription quota, so there is no model
// substitution to make and no usage to charge. The Anthropic adapter still
// observes the response's rate-limit headers (via the ctx observer installed by
// withUsageObserver), keeping the gate primed for the next request.
func (s *Service) bypassToAnthropic(
	ctx context.Context,
	env *translate.RequestEnvelope,
	feats translate.RoutingFeatures,
	requestStart time.Time,
	requestID, externalID string,
	r *http.Request,
	w http.ResponseWriter,
) error {
	log := observability.FromContext(ctx)
	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    feats.Model,
		Reason:   "usage_bypass",
	}
	w.Header().Set(HeaderRouterDecision, decision.Reason)
	w.Header().Set(HeaderRouterProvider, decision.Provider)
	w.Header().Set(HeaderRouterModel, decision.Model)

	p, provErr := s.provider(providers.ProviderAnthropic)
	if provErr != nil {
		return provErr
	}

	// Resolve credentials onto ctx so the Anthropic adapter's setAuth picks up
	// the subscription (or BYOK / client) credential exactly as a routed turn
	// would, and so servedOnSubscription / the usage observer key off the same
	// token the upstream call sends.
	ctx = resolveAndInjectCredentials(ctx, decision.Provider, r.Header)

	outputReserve := contextWindowOutputReserve
	if feats.MaxTokens > outputReserve {
		outputReserve = feats.MaxTokens
	}
	opts := translate.EmitOptions{
		TargetModel:           decision.Model,
		TargetProvider:        decision.Provider,
		Capabilities:          router.Lookup(decision.Model),
		IncludeStreamUsage:    s.usageRequired(),
		EnableExtendedContext: shouldEnableExtendedContext(env.FullTokenEstimate(), outputReserve),
	}
	prep, emitErr := env.PrepareAnthropic(r.Header, opts)
	if emitErr != nil {
		log.Error("Failed to emit Anthropic body on usage-bypass path", "err", emitErr)
		return fmt.Errorf("emit bypass body: %w", emitErr)
	}

	proxyStart := time.Now()
	proxyErr := p.Proxy(ctx, decision, prep, w, r)
	otel.Record(ctx, otel.Span{
		Name:  "router.usage_bypass",
		Start: requestStart,
		End:   time.Now(),
		Attrs: otel.NewAttrBuilder(6).
			String("request_id", requestID).
			String("external_id", externalID).
			String("decision.model", decision.Model).
			String("decision.provider", decision.Provider).
			String("decision.reason", decision.Reason).
			Bool("cost.subscription_served", servedOnSubscription(ctx)).
			Build(),
	})
	otel.Flush(ctx)
	log.Info("ProxyMessages usage-bypass complete",
		"request_id", requestID,
		"external_id", externalID,
		"requested_model", feats.Model,
		"decision_model", decision.Model,
		"proxy_ms", time.Since(proxyStart).Milliseconds(),
		"total_ms", time.Since(requestStart).Milliseconds(),
		"proxy_err", proxyErr,
	)
	return proxyErr
}
