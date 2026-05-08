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
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

// ErrGeminiCrossFormatUnsupported is returned when a Gemini-source
// request lands on a non-Google provider. Cross-format emit from a
// Gemini envelope (Gemini-in → Anthropic-out, Gemini-in → OpenAI-out)
// is intentionally deferred until traffic asks for it; the helper
// presently routes only through the same-format passthrough path.
var ErrGeminiCrossFormatUnsupported = errors.New("gemini cross-format emit not implemented")

// ProxyGeminiGenerateContent routes a native Gemini generateContent
// request. Same-format passthrough (Gemini-in → Google-out) is fully
// supported; cross-format directions return
// ErrGeminiCrossFormatUnsupported (handler maps to HTTP 501) until a
// downstream consumer needs them.
//
// The handler must inject a synthetic top-level "model" field (from
// the URL path :model segment) and a synthetic "stream" boolean (true
// when the URL action is :streamGenerateContent) into body before
// calling this method. Both fields get stripped before the body is
// forwarded upstream — see emit_gemini.go's same-format branch.
func (s *Service) ProxyGeminiGenerateContent(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	log := observability.Get()
	requestStart := time.Now()
	requestID := uuid.New().String()
	buf := otel.NewBuffer(s.emitter)
	ctx = buf.WithContext(ctx)

	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	installationID := installationIDFromContext(ctx)
	clientID := ClientIdentityFrom(ctx)

	env, parseErr := translate.ParseGemini(body)
	if parseErr != nil {
		log.Error("Failed to parse Gemini request", "err", parseErr)
		return fmt.Errorf("parse request: %w", parseErr)
	}
	feats := env.RoutingFeatures(false)

	bypassEval := hasEvalOverrideHeader(r)
	bypassLegacySticky := bypassEval

	subAgentHint := r.Header.Get("x-weave-subagent-type")

	routeStart := time.Now()
	routeRes, err := s.routeWithSession(ctx, env, feats, apiKeyID, installationID, subAgentHint, bypassLegacySticky, router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		PromptText:           feats.PromptText,
		EnabledProviders:     s.enabledProvidersForRequest(ctx, r.Header),
	})
	routeMs := time.Since(routeStart).Milliseconds()
	if err != nil {
		log.Error("Routing failed for Gemini request", "err", err, "route_ms", routeMs, "requested_model", feats.Model, "estimated_input_tokens", feats.Tokens)
		return err
	}
	decision := routeRes.Decision
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec

	p, provErr := s.provider(decision.Provider)
	if provErr != nil {
		return provErr
	}

	w.Header().Set("x-router-decision", decision.Reason)
	w.Header().Set("x-router-provider", decision.Provider)
	w.Header().Set("x-router-model", decision.Model)

	reqPricing := otel.Lookup(feats.Model)
	actPricing := otel.Lookup(decision.Model)
	otel.Record(ctx, otel.Span{
		Name:  "router.decision",
		Start: requestStart,
		End:   time.Now(),
		Attrs: otel.NewAttrBuilder(22).
			String("request_id", requestID).
			String("external_id", externalID).
			String("client.device_id", clientID.DeviceID).
			String("client.account_id", clientID.AccountID).
			String("client.session_id", clientID.SessionID).
			String("client.user_agent", clientID.UserAgent).
			String("client.app", clientID.ClientApp).
			String("requested.model", feats.Model).
			String("decision.model", decision.Model).
			String("decision.provider", decision.Provider).
			String("decision.reason", decision.Reason).
			Bool("routing.sticky_hit", stickyHit).
			Bool("routing.session_pin_hit", pinTier == "in_proc" || pinTier == "postgres").
			String("routing.session_pin_tier", pinTier).
			Int64("routing.session_pin_age_s", pinAgeSec).
			String("routing.turn_type", string(tt)).
			Int64("routing.estimated_input_tokens", int64(feats.Tokens)).
			Float64("pricing.requested_input_per_1m", reqPricing.InputUSDPer1M).
			Float64("pricing.requested_output_per_1m", reqPricing.OutputUSDPer1M).
			Float64("pricing.actual_input_per_1m", actPricing.InputUSDPer1M).
			Float64("pricing.actual_output_per_1m", actPricing.OutputUSDPer1M).
			Int64("latency.route_ms", routeMs).
			Build(),
	})
	otel.Flush(ctx)

	// Cross-format emit from a Gemini envelope is deferred. Reject early
	// with a typed error the handler can map to HTTP 501. The Google
	// branch below uses PrepareGemini's same-format passthrough.
	if decision.Provider != providers.ProviderGoogle {
		return fmt.Errorf("%w: decision picked provider %q for Gemini-source request", ErrGeminiCrossFormatUnsupported, decision.Provider)
	}

	opts := translate.EmitOptions{
		TargetModel:        decision.Model,
		Capabilities:       router.Lookup(decision.Model),
		IncludeStreamUsage: s.emitter != nil,
	}
	ctx = resolveAndInjectCredentials(ctx, decision.Provider, r.Header)

	prep, emitErr := env.PrepareGemini(r.Header, opts)
	if emitErr != nil {
		log.Error("Failed to emit Gemini body", "err", emitErr)
		return fmt.Errorf("emit body: %w", emitErr)
	}

	proxyStart := time.Now()
	var extractor *otel.UsageExtractor
	proxyWriter := http.ResponseWriter(w)
	if s.emitter != nil {
		extractor = otel.NewUsageExtractor(w, decision.Provider)
		proxyWriter = extractor
	}
	proxyErr := p.Proxy(ctx, decision, prep, proxyWriter, r)
	proxyMs := time.Since(proxyStart).Milliseconds()

	in, out := extractor.Tokens()
	geminiUpstreamBuilder := otel.NewAttrBuilder(20).
		String("request_id", requestID).
		String("external_id", externalID).
		String("client.device_id", clientID.DeviceID).
		String("client.account_id", clientID.AccountID).
		String("client.session_id", clientID.SessionID).
		String("client.user_agent", clientID.UserAgent).
		String("client.app", clientID.ClientApp).
		Int64("usage.input_tokens", int64(in)).
		Int64("usage.output_tokens", int64(out)).
		Float64("cost.requested_input_usd", float64(in)/1_000_000*reqPricing.InputUSDPer1M).
		Float64("cost.requested_output_usd", float64(out)/1_000_000*reqPricing.OutputUSDPer1M).
		Float64("cost.actual_input_usd", float64(in)/1_000_000*actPricing.InputUSDPer1M).
		Float64("cost.actual_output_usd", float64(out)/1_000_000*actPricing.OutputUSDPer1M).
		Int64("latency.upstream_ms", proxyMs).
		Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
		Int64("upstream.status_code", int64(upstreamStatus(proxyErr))).
		Bool("routing.cross_format", false)
	addTimingAttrs(ctx, geminiUpstreamBuilder)
	otel.Record(ctx, otel.Span{
		Name:  "router.upstream",
		Start: proxyStart,
		End:   time.Now(),
		Attrs: geminiUpstreamBuilder.Build(),
	})
	otel.Flush(ctx)

	log.Info("ProxyGeminiGenerateContent complete", "requested_model", feats.Model, "decision_model", decision.Model, "decision_provider", decision.Provider, "decision_reason", decision.Reason, "estimated_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "sticky_hit", stickyHit, "pin_tier", pinTier, "turn_type", string(tt), "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	return proxyErr
}
