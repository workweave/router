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
	log := observability.FromContext(ctx)
	requestStart := time.Now()
	requestID := uuid.New().String()
	buf := otel.NewBuffer(s.emitter)
	ctx = buf.WithContext(ctx)

	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	installationID := installationIDFromContext(ctx)
	clientID := ClientIdentityFrom(ctx)

	// Strip the one-click thumbs footer that prior streamed answers appended as a
	// trailing model text part. Clients echo it back in contents[] on the next
	// turn, so without this it (and its signed rate URLs) accumulates in upstream
	// context (see ProxyMessages for the symmetric Anthropic/OpenAI path).
	body, stripErr := translate.StripFeedbackFooterFromGeminiContents(body)
	if stripErr != nil {
		log.Error("Failed to strip feedback footer from Gemini contents", "err", stripErr)
		return fmt.Errorf("strip feedback footer: %w", stripErr)
	}

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

	ctx, log, _ = bindRequestLogger(ctx, env, apiKeyID, requestID, "gemini_generate_content")
	log.Info("ProxyGeminiGenerateContent start",
		"requested_model", feats.Model,
		"stream", env.Stream(),
		"message_count", feats.MessageCount,
		"has_tools", feats.HasTools,
		"total_input_tokens", feats.Tokens,
		"prompt_preview", preview(promptText, 200),
	)

	logInboundRequestDiagnostics(log, env)

	subAgentHint := r.Header.Get("x-weave-subagent-type")

	routeStart := time.Now()
	routeRes, err := s.runTurnLoop(ctx, env, feats, apiKeyID, installationID, subAgentHint, r.Header, router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		HasImages:            feats.HasImages,
		PromptText:           promptText,
		EnabledProviders:     s.enabledProvidersForRequest(ctx, providers.ProviderGoogle, r.Header),
		ExcludedModels:       s.excludedModelsForRequest(ctx),
		RoutingKnobs:         router.RoutingKnobsFromContext(ctx),
	})
	routeMs := time.Since(routeStart).Milliseconds()
	if err != nil {
		log.Error("Routing failed for Gemini request", "err", err, "route_ms", routeMs, "requested_model", feats.Model, "total_input_tokens", feats.Tokens)
		return err
	}
	routeRes.SuggestionMode = r.Header.Get("x-weave-suggestion-mode") == "true"
	decision := routeRes.Decision
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec
	s.logPlannerOutcome(ctx, routeRes)

	p, provErr := s.provider(decision.Provider)
	if provErr != nil {
		return provErr
	}

	w.Header().Set(HeaderRouterDecision, decision.Reason)
	w.Header().Set(HeaderRouterProvider, decision.Provider)
	w.Header().Set(HeaderRouterModel, decision.Model)
	// Gemini path does not resolve a router user, matching the decision span
	// below which omits router_user_id.
	s.setFeedbackLinkHeader(w, installationID, externalID, requestID, "")

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
		Float64("catalog.requested_input_per_1m", reqPricing.InputUSDPer1M).
		Float64("catalog.requested_output_per_1m", reqPricing.OutputUSDPer1M).
		Float64("catalog.actual_input_per_1m", actPricing.InputUSDPer1M).
		Float64("catalog.actual_output_per_1m", actPricing.OutputUSDPer1M).
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
		TargetProvider:     decision.Provider,
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
	// Append the one-click feedback thumbs as a trailing part on streaming
	// answers (see ProxyMessages for the rationale). The Gemini path resolves no
	// router user, matching the decision span and feedback header above.
	clientSink := w
	if env.Stream() {
		if footer := s.feedbackFooter(ClientIdentityFrom(ctx).ClientApp, installationID, externalID, requestID, ""); footer != "" {
			clientSink = translate.NewGeminiRoutingFooterWriter(w, footer)
		}
	}
	contentSink, contentCap := s.maybeCaptureResponse(clientSink)
	var sink http.ResponseWriter = contentSink
	if marker := suppressMarkerIfRequested(r.Header, routingMarkerFor(routeRes)); marker != "" {
		mw := translate.NewGeminiRoutingMarkerWriter(sink, marker)
		// Flush marker + HTTP 200 immediately so TTFB is decoupled from
		// upstream prefill. Locks status to 200.
		if err := mw.Prelude(env.Stream()); err != nil {
			log.Error("Gemini routing-marker prelude failed", "err", err)
		}
		sink = mw
	}
	if s.usageRequired() {
		extractor = otel.NewUsageExtractor(sink, decision.Provider)
		sink = extractor
	}
	proxyErr := p.Proxy(ctx, decision, prep, sink, r)
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
		String("requested.model", feats.Model).
		String("decision.model", decision.Model).
		String("decision.provider", decision.Provider).
		String("decision.reason", decision.Reason).
		String("routing.turn_type", string(tt)).
		Int64("usage.input_tokens", int64(in)).
		Int64("usage.output_tokens", int64(out)).
		Int64("usage.cache_creation_input_tokens", int64(cacheCreation)).
		Int64("usage.cache_read_input_tokens", int64(cacheRead)).
		Float64("cost.requested_input_usd", catalog.EffectiveInputCost(in, cacheCreation, cacheRead, reqPricing.InputUSDPer1M, reqPricing, decision.Provider)).
		Float64("cost.requested_output_usd", catalog.EffectiveOutputCost(out, reqPricing.OutputUSDPer1M)).
		Float64("cost.actual_input_usd", catalog.EffectiveInputCost(in, cacheCreation, cacheRead, actPricing.InputUSDPer1M, actPricing, decision.Provider)).
		Float64("cost.actual_output_usd", catalog.EffectiveOutputCost(out, actPricing.OutputUSDPer1M)).
		Bool("cost.subscription_served", servedOnSubscription(ctx)).
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
	respBody, respTrunc := capturedResponse(contentCap)
	s.recordCallLog(ctx, geminiUpstreamBuilder.Build(), proxyErr != nil, body, respBody, respTrunc)
	otel.Flush(ctx)

	// Persist last-turn usage to the pin row so the next turn's planner
	// has cache-hit evidence. Off the request path; drops on saturation.
	s.recordTurnUsage(routeRes, decision.Model, in, out, cacheCreation, cacheRead)

	if proxyErr == nil {
		s.emitBilling(ctx, requestID, externalID, decision, actPricing, routeRes, in, out, cacheCreation, cacheRead)
	}

	log.Info("ProxyGeminiGenerateContent complete", "requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "decision_reason", decision.Reason, "embedded_tokens", len(promptText)/4, "total_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "embed_input", embedInput, "sticky_hit", stickyHit, "pin_tier", pinTier, "turn_type", string(tt), "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	return proxyErr
}
