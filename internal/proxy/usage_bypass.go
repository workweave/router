package proxy

import (
	"context"
	"errors"
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

// usageBypassDecision returns the passthrough decision when the subscription
// usage-bypass gate should engage for this turn, and false otherwise. It is
// consulted inside runTurnLoop only at the point a FRESH scorer decision would
// be made — after the hard-pin, user-forced-pin, and tool-result sticky
// branches have already returned — so those higher-precedence paths are never
// preempted by the bypass. The req-level exclusion sets (EnabledProviders,
// ExcludedModels) it reads already carry installation provider/model denylists
// and the per-request context-overflow filter, so a policy-blocked or
// over-capacity model can't be served via the bypass.
func (s *Service) usageBypassDecision(ctx context.Context, headers http.Header, req router.Request) (router.Decision, bool) {
	if !s.usageBypassEngaged(ctx, headers, req) {
		return router.Decision{}, false
	}
	return router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    req.RequestedModel,
		Reason:   "usage_bypass",
	}, true
}

// usageBypassEngaged reports whether the requested model should be served
// straight through to the caller's own Claude subscription instead of routed.
// It engages only when:
//
//   - the installation has turned the gate on (usageBypassFromContext),
//   - the subscription usage observer is wired (it drives the threshold read),
//   - the requested model is Anthropic-served (the only thing a Claude
//     subscription can serve) and is neither provider- nor model-excluded for
//     this request (denylist or context-overflow filter),
//   - the request presents a Claude subscription credential (the turn is paid
//     for by the customer's own plan — nothing for the router to save, nothing
//     for us to bill), and
//   - observed utilization is still below the threshold, OR nothing has been
//     observed yet (cold start: serve the first turn on the subscription so its
//     response primes the observer, mirroring the subsidy bootstrap).
//
// Once observed utilization crosses the threshold the gate disengages and the
// normal routing path (scorer + subscription-aware cost discounting) takes over,
// so the caller starts conserving their remaining quota.
func (s *Service) usageBypassEngaged(ctx context.Context, headers http.Header, req router.Request) bool {
	cfg, ok := usageBypassFromContext(ctx)
	if !ok || s.usageObserver == nil {
		return false
	}
	model := req.RequestedModel
	if m, found := catalog.ByID(model); !found || m.PrimaryProvider() != providers.ProviderAnthropic {
		return false
	}
	if req.EnabledProviders != nil {
		if _, enabled := req.EnabledProviders[providers.ProviderAnthropic]; !enabled {
			return false
		}
	}
	if _, excluded := req.ExcludedModels[model]; excluded {
		return false
	}
	_, anthroTok := s.presentSubscriptionTokens(ctx, headers)
	if anthroTok == "" {
		return false
	}
	threshold := defaultUsageBypassThreshold
	if cfg.Threshold != nil {
		threshold = min(1, max(0, *cfg.Threshold))
	}
	snap, observed := s.usageObserver.Snapshot(s.usageObserver.Key([]byte(anthroTok)))
	if !observed {
		return true
	}
	// Never bypass onto a spent subscription: a passthrough would inject a token
	// the upstream 429s. Disengage regardless of the installation threshold (which
	// may sit above exhaustedFraction) so the turn takes the routed path, where the
	// exhaustion failover serves it on the deployment / BYOK Anthropic key.
	if snap.Exhausted() {
		return false
	}
	util := max(snap.Primary.UsedPercent, snap.Secondary.UsedPercent)
	return util < threshold
}

// claudeSubscriptionExhausted reports whether the caller's present Claude
// subscription has bound its plan window — the upstream will 429 any further
// turn until it resets. True only when: the usage observer is wired, a Claude
// subscription token is present on this request, its most-recent observed
// snapshot is exhausted, AND a non-subscription Anthropic key exists to serve the
// turn instead. The token key is derived identically to withUsageObserver /
// usageBypassEngaged so this read agrees with what the observer recorded. When
// true the caller suppresses the subscription credential (withSuppressedSubscription)
// so the turn serves on the Weave / BYOK key rather than the spent subscription.
func (s *Service) claudeSubscriptionExhausted(ctx context.Context, headers http.Header) bool {
	if s.usageObserver == nil {
		return false
	}
	_, anthroTok := s.presentSubscriptionTokens(ctx, headers)
	if anthroTok == "" {
		return false
	}
	if !s.anthropicFallbackKeyAvailable(ctx) {
		return false
	}
	snap, ok := s.usageObserver.Snapshot(s.usageObserver.Key([]byte(anthroTok)))
	return ok && snap.Exhausted()
}

// anthropicFallbackKeyAvailable reports whether a non-subscription Anthropic
// credential is configured to serve a Claude turn when the caller's subscription
// is spent: a per-request BYOK Anthropic key, or the deployment's own
// ANTHROPIC_API_KEY (tracked in deploymentKeyedProviders). Without one, dropping
// the subscription token would leave the turn with no Anthropic credential and
// 400 — strictly worse than the 429 — so the caller keeps using the subscription.
func (s *Service) anthropicFallbackKeyAvailable(ctx context.Context) bool {
	if byok := BuildCredentialsMap(externalKeysFromContext(ctx)); byok != nil {
		if _, ok := byok[providers.ProviderAnthropic]; ok {
			return true
		}
	}
	if s.deploymentKeyedProviders != nil {
		if _, ok := s.deploymentKeyedProviders[providers.ProviderAnthropic]; ok {
			return true
		}
	}
	return false
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
	modelSwitched bool,
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
		// When the session previously served a different model, strip thinking
		// blocks whose signatures the requested model would reject (else
		// Anthropic 400s on the stale signature).
		ModelSwitched: modelSwitched,
	}
	prep, emitErr := env.PrepareAnthropic(r.Header, opts)
	if emitErr != nil {
		log.Error("Failed to emit Anthropic body on usage-bypass path", "err", emitErr)
		return fmt.Errorf("emit bypass body: %w", emitErr)
	}

	proxyStart := time.Now()
	proxyErr := p.Proxy(ctx, decision, prep, w, r)
	// The Anthropic adapter returns a buffered *UpstreamErrorResponse on 4xx/5xx
	// without writing to w (the routed path flushes it via dispatchWithFallback).
	// Render it as the real upstream status+body so the caller sees e.g. a 429
	// with its retry headers instead of a generic 502; treat the turn as served.
	var upstreamErr *providers.UpstreamErrorResponse
	if errors.As(proxyErr, &upstreamErr) {
		flushUpstreamErrorAsAnthropic(w, proxyErr)
		proxyErr = nil
	}
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
