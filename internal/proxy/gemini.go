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

// ErrGeminiCrossFormatUnsupported is returned when a Gemini-source request
// lands on a non-Google provider. Cross-format emit is deferred until
// traffic asks for it.
var ErrGeminiCrossFormatUnsupported = errors.New("gemini cross-format emit not implemented")

// ProxyGeminiGenerateContent routes a native Gemini generateContent request.
// Only same-format passthrough (Gemini-in → Google-out) is supported;
// cross-format returns ErrGeminiCrossFormatUnsupported.
//
// The handler must inject synthetic top-level "model" (URL :model segment)
// and "stream" (true for :streamGenerateContent) fields into body before
// calling; both are stripped before forwarding upstream.
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
	embedFlag := s.embedOnlyUserMessage
	if v, ok := embedOnlyUserMessageOverride(ctx); ok {
		embedFlag = v
	}
	feats := env.RoutingFeatures(embedFlag)
	promptText := feats.PromptText
	embedInput := "concatenated_stream"
	if embedFlag && feats.OnlyUserMessageText != "" {
		promptText = feats.OnlyUserMessageText
		embedInput = "only_user_message"
	}

	subAgentHint := r.Header.Get("x-weave-subagent-type")

	routeStart := time.Now()
	routeRes, err := s.runTurnLoop(ctx, env, feats, apiKeyID, installationID, subAgentHint, r.Header, router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		PromptText:           promptText,
		EnabledProviders:     s.enabledProvidersForRequest(ctx, providers.ProviderGoogle, r.Header),
		ExcludedModels:       s.excludedModelsForRequest(ctx),
	})
	routeMs := time.Since(routeStart).Milliseconds()
	if err != nil {
		log.Error("Routing failed for Gemini request", "err", err, "route_ms", routeMs, "requested_model", feats.Model, "total_input_tokens", feats.Tokens)
		return err
	}
	decision := routeRes.Decision
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec
	s.logPlannerOutcome(routeRes)

	p, provErr := s.provider(decision.Provider)
	if provErr != nil {
		return provErr
	}

	w.Header().Set("x-router-decision", decision.Reason)
	w.Header().Set("x-router-provider", decision.Provider)
	w.Header().Set("x-router-model", decision.Model)

	reqPricing := otel.Lookup(s.baselineFor(feats.Model))
	actPricing := otel.Lookup(decision.Model)
	geminiDecisionBuilder := otel.NewAttrBuilder(40).
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
		String("routing.embed_input", embedInput).
		Int64("routing.estimated_input_tokens", int64(feats.Tokens)).
		Float64("pricing.requested_input_per_1m", reqPricing.InputUSDPer1M).
		Float64("pricing.requested_output_per_1m", reqPricing.OutputUSDPer1M).
		Float64("pricing.actual_input_per_1m", actPricing.InputUSDPer1M).
		Float64("pricing.actual_output_per_1m", actPricing.OutputUSDPer1M).
		Int64("latency.route_ms", routeMs)
	applyPlannerAttrs(geminiDecisionBuilder, routeRes)
	otel.Record(ctx, otel.Span{
		Name:  "router.decision",
		Start: requestStart,
		End:   time.Now(),
		Attrs: geminiDecisionBuilder.Build(),
	})
	otel.Flush(ctx)

	// Cross-format from a Gemini envelope is deferred; handler maps to HTTP 501.
	if decision.Provider != providers.ProviderGoogle {
		return fmt.Errorf("%w: decision picked provider %q for Gemini-source request", ErrGeminiCrossFormatUnsupported, decision.Provider)
	}

	opts := translate.EmitOptions{
		TargetModel:        decision.Model,
		Capabilities:       router.Lookup(decision.Model),
		IncludeStreamUsage: s.usageRequired(),
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
	if s.usageRequired() {
		extractor = otel.NewUsageExtractor(w, decision.Provider)
		proxyWriter = extractor
	}
	proxyErr := p.Proxy(ctx, decision, prep, proxyWriter, r)
	proxyMs := time.Since(proxyStart).Milliseconds()

	in, out := extractor.Tokens()
	cacheCreation, cacheRead := extractor.CacheTokens()
	geminiUpstreamBuilder := otel.NewAttrBuilder(40).
		String("request_id", requestID).
		String("external_id", externalID).
		String("client.device_id", clientID.DeviceID).
		String("client.account_id", clientID.AccountID).
		String("client.session_id", clientID.SessionID).
		String("client.user_agent", clientID.UserAgent).
		String("client.app", clientID.ClientApp).
		Int64("usage.input_tokens", int64(in)).
		Int64("usage.output_tokens", int64(out)).
		Int64("usage.cache_creation_input_tokens", int64(cacheCreation)).
		Int64("usage.cache_read_input_tokens", int64(cacheRead)).
		Float64("cost.requested_input_usd", float64(in)/1_000_000*reqPricing.InputUSDPer1M).
		Float64("cost.requested_output_usd", float64(out)/1_000_000*reqPricing.OutputUSDPer1M).
		Float64("cost.actual_input_usd", float64(in)/1_000_000*actPricing.InputUSDPer1M).
		Float64("cost.actual_output_usd", float64(out)/1_000_000*actPricing.OutputUSDPer1M).
		Int64("latency.upstream_ms", proxyMs).
		Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
		Int64("upstream.status_code", int64(upstreamStatus(proxyErr))).
		Bool("routing.cross_format", false)
	applyPlannerAttrs(geminiUpstreamBuilder, routeRes)
	addTimingAttrs(ctx, geminiUpstreamBuilder)
	otel.Record(ctx, otel.Span{
		Name:  "router.upstream",
		Start: proxyStart,
		End:   time.Now(),
		Attrs: geminiUpstreamBuilder.Build(),
	})
	otel.Flush(ctx)

	// Persist last-turn usage to the pin row so the next turn's planner
	// has cache-hit evidence. Off the request path; drops on saturation.
	s.recordTurnUsage(routeRes, in, out, cacheCreation, cacheRead)

	log.Info("ProxyGeminiGenerateContent complete", "requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "decision_reason", decision.Reason, "embedded_tokens", len(promptText)/4, "total_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "embed_input", embedInput, "sticky_hit", stickyHit, "pin_tier", pinTier, "turn_type", string(tt), "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	return proxyErr
}
