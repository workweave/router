package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/observability/otel"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/sessionpin"
	"weave-os/router/internal/router/turntype"
	"weave-os/router/internal/translate"
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
	if managedSubscriptionEnrollmentUnavailable(ctx) {
		return ErrSubscriptionPoolUnavailable
	}
	ctx, err := s.checkUserMonthlySpendLimit(ctx, r.Header, r.URL.Path)
	if err != nil {
		return err
	}
	ctx = s.withPlanAwareSubscriptionModels(ctx, r.Header)
	log := observability.FromContext(ctx)
	requestStart := time.Now()
	requestID := requestIDFor(ctx)
	buf := s.newTelemetryBuffer()
	ctx = buf.WithContext(ctx)

	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	installationID := installationIDFromContext(ctx)
	clientID := ClientIdentityFrom(ctx)

	// Strip the one-click thumbs footer that prior streamed answers appended as a
	// trailing model text part. Clients echo it back in contents[] on the next
	// turn, so without this it (and its signed rate URLs) accumulates in upstream
	// context (see ProxyMessages for the symmetric Anthropic/OpenAI path).
	// Best-effort: a strip failure returns a nil body, so we log-and-continue
	// with the original bytes rather than aborting the turn over cosmetic
	// cleanup — matching the OpenAI chat path's feedback-footer strip.
	if strippedBody, stripErr := translate.StripFeedbackFooterFromGeminiContents(body); stripErr != nil {
		log.Error("Failed to strip feedback footer from Gemini contents", "err", stripErr)
	} else {
		body = strippedBody
	}

	env, parseErr := translate.ParseGemini(body)
	if parseErr != nil {
		log.Error("Failed to parse Gemini request", "err", parseErr)
		return fmt.Errorf("parse request: %w", parseErr)
	}
	var responseBuffer *responseCostBuffer
	if !env.Stream() {
		responseBuffer = newResponseCostBuffer(w)
		w = responseBuffer
		defer func() {
			if flushErr := responseBuffer.FlushToClient(); flushErr != nil {
				log.Error("Failed to flush buffered response", "err", flushErr)
			}
		}()
	}
	embedFlag := s.ResolveEmbedOnlyUserMessage(ctx)
	feats := env.RoutingFeatures(embedFlag)
	promptText := feats.PromptText
	embedInput := "concatenated_stream"
	if embedFlag && feats.OnlyUserMessageText != "" {
		promptText = feats.OnlyUserMessageText
		embedInput = "only_user_message"
	}

	var sessionKey [sessionpin.SessionKeyLen]byte
	ctx, log, sessionKey = bindRequestLogger(ctx, env, apiKeyID, requestID, "gemini_generate_content")
	log.Info("ProxyGeminiGenerateContent start",
		"requested_model", feats.Model,
		"stream", env.Stream(),
		"message_count", feats.MessageCount,
		"has_tools", feats.HasTools,
		"total_input_tokens", feats.Tokens,
	)

	logInboundRequestDiagnostics(log, env)

	subAgentHint := r.Header.Get("x-weave-subagent-type")

	forceCluster, forceErr := applyForceClusterHeader(ctx, r)
	if forceErr != nil {
		return forceErr
	}

	enabledProviders := s.enabledProvidersForRequest(ctx, providers.ProviderGoogle, r.Header)
	excluded := s.excludedModelsForRequest(ctx)

	// Proactive context-window compaction, as in ProxyMessages.
	outputReserve := contextWindowOutputReserve
	if feats.MaxTokens > outputReserve {
		outputReserve = feats.MaxTokens
	}
	maxEligibleWindow := s.maxEligibleContextWindow(excluded, enabledProviders, env.SignatureTokenSavings())
	compRes, compErr := s.maybeCompact(ctx, env, compactionInput{
		TurnType:       turntype.DetectFromEnvelope(env, feats, subAgentHint),
		OutputReserve:  outputReserve,
		MaxWindow:      maxEligibleWindow,
		RequestedModel: feats.Model,
		ClientApp:      clientID.ClientApp,
		PreferredSummarizer: func() string {
			return s.compactionPreferredSummarizer(ctx, sessionKey, roleForTier(catalog.TierFor(feats.Model)))
		},
		Headers: r.Header,
	})
	if compErr != nil {
		log.Warn("Compaction could not fit request to any eligible model",
			"err", compErr, "final_estimate", compRes.FinalEstimate, "max_window", maxEligibleWindow, "requested_model", feats.Model)
		return compErr
	}
	if compRes.Applied {
		feats = env.RoutingFeatures(embedFlag)
		log.Info("Proactive compaction applied",
			"tool_results_cleared", compRes.ToolResultsCleared,
			"summarized", compRes.Summarized,
			"summary_model", compRes.SummaryModel,
			"trimmed_to_recent", compRes.TrimmedToRecent,
			"final_estimate", compRes.FinalEstimate,
		)
	}

	routeRequest := router.Request{
		RequestedModel:               feats.Model,
		ForceCluster:                 forceCluster,
		EstimatedInputTokens:         feats.Tokens,
		HasTools:                     feats.HasTools,
		HasImages:                    feats.HasImages,
		TranslationRequirements:      env.TranslationRequirements(router.EndpointGeminiGenerate),
		ReasoningConfigurationSHA256: env.ReasoningConfigurationSHA256(),
		ToolConfigurationSHA256:      env.ToolConfigurationSHA256(),
		PromptText:                   promptText,
		ConversationMessages:         conversationMessagesForRouting(env),
		AvailableTools:               availableToolsForRouting(env),
		Tools:                        toolsForRouting(env),
		HistoryTruncated:             compRes.Applied,
		ClientSessionID:              clientSessionIDForRequest(ctx, env),
		EnabledProviders:             enabledProviders,
		CustomBindings:               s.customBindingsForRequest(ctx),
		GatewayProviders:             s.gatewayProvidersForRequest(ctx),
		ExcludedModels:               excluded,
		AllowedModels:                allowedModelsForRequest(ctx),
		PreferredModels:              s.preferredModelsForRequest(ctx),
		RoutingKnobs:                 router.RoutingKnobsFromContext(ctx),
		ClusterArmOverrides:          clusterArmOverridesForRequest(ctx),
	}
	routeStart := time.Now()
	routeRes, err := s.runTurnLoop(ctx, env, feats, apiKeyID, installationID, subAgentHint, r.Header, routeRequest)
	routeMs := time.Since(routeStart).Milliseconds()
	if err != nil {
		log.Error("Routing failed for Gemini request", "err", err, "route_ms", routeMs, "requested_model", feats.Model, "total_input_tokens", feats.Tokens)
		return err
	}
	if len(routeRes.SessionDisabledProviders) > 0 {
		ctx = context.WithValue(ctx, SessionDisabledProvidersContextKey{}, routeRes.SessionDisabledProviders)
	}
	routeRes.SuggestionMode = r.Header.Get("x-weave-suggestion-mode") == "true"
	decision := routeRes.Decision
	s.firePolicyShadowForServingDecision(ctx, decision, routeRequest)
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec
	s.logPlannerOutcome(ctx, routeRes)

	w.Header().Set(HeaderRouterDecision, decision.Reason)
	w.Header().Set(HeaderRouterProvider, decision.Provider)
	w.Header().Set(HeaderRouterModel, decision.Model)
	w.Header().Set(HeaderRouterContextWindow, strconv.Itoa(contextWindowForRequest(decision.Model, decision.Provider)))
	// Gemini path does not resolve a router user, matching the decision span
	// below which omits router_user_id.
	s.setFeedbackLinkHeader(ctx, w, installationID, externalID, requestID, "")

	reqPricing := otel.Lookup(s.baselineFor(feats.Model))
	actPricing := otel.Lookup(decision.Model)
	reqDecisionPricing := reqPricing.ForInputTokens(feats.Tokens)
	actDecisionPricing := actPricing.ForInputTokens(feats.Tokens)
	geminiDecisionBuilder := otel.NewAttrBuilder(45).
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
		Bool("routing.policy_fallback", routeRes.PolicyFallback).
		Bool("routing.sticky_hit", stickyHit).
		Bool("routing.session_pin_hit", pinTier == "in_proc" || pinTier == "postgres").
		String("routing.session_pin_tier", pinTier).
		Int64("routing.session_pin_age_s", pinAgeSec).
		String("routing.turn_type", string(tt)).
		String("routing.embed_input", embedInput).
		Int64("routing.estimated_input_tokens", int64(feats.Tokens)).
		Float64("catalog.requested_input_per_1m", reqDecisionPricing.InputUSDPer1M).
		Float64("catalog.requested_output_per_1m", reqDecisionPricing.OutputUSDPer1M).
		Float64("catalog.actual_input_per_1m", actDecisionPricing.InputUSDPer1M).
		Float64("catalog.actual_output_per_1m", actDecisionPricing.OutputUSDPer1M).
		Int64("latency.route_ms", routeMs)
	applySidecarAttrs(geminiDecisionBuilder, routeRes)
	applyPlannerAttrs(geminiDecisionBuilder, routeRes)
	applyRoutingStateAttrs(geminiDecisionBuilder, routeRes, decision.ServedIdentity(), sessionKey)
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
	effortServed := s.resolveEffort(ctx, decision, opts.Capabilities, routeRes.EscalateEffort)
	effortServed.apply(&opts)
	ctx = resolveAndInjectCredentials(ctx, decision.Provider, decision.Model, r.Header)

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
		if footer := s.feedbackFooter(ctx, ClientIdentityFrom(ctx).ClientApp, routeRes.TurnType, false); footer != "" {
			clientSink = translate.NewGeminiRoutingFooterWriter(w, footer)
		}
	}
	contentSink, contentCap := s.maybeCaptureResponse(ctx, clientSink)
	// preludeBuf delays commit so a 429 or empty stream stays retryable.
	preludeBuf := newPreludeBuffer(contentSink)
	marker := suppressMarkerIfRequested(ctx, r.Header, routingMarkerFor(routeRes))
	bindings := s.resolveBindingsForDispatch(ctx, decision)
	attempt := func(actx context.Context, d router.Decision, p providers.Client) error {
		attemptSink := http.ResponseWriter(preludeBuf)
		if marker != "" {
			mw := translate.NewGeminiRoutingMarkerWriter(attemptSink, marker)
			mw.Prepare(env.Stream())
			attemptSink = mw
		}
		proxySink := attemptSink
		if s.usageRequired() {
			extractor = otel.NewUsageExtractor(attemptSink, d.Provider)
			proxySink = extractor
		}
		preludeBuf.Seal()
		err := p.Proxy(actx, d, prep, proxySink, r)
		if err == nil && env.Stream() && !preludeBuf.Committed() {
			return translate.ErrStreamEmpty
		}
		if err != nil && env.Stream() && preludeBuf.Committed() {
			emitGeminiSSEErrorEvent(contentSink)
		}
		return err
	}
	winnerIdx, proxyErr := s.dispatchWithFallback(ctx, failoverInputs{
		w:               contentSink,
		buf:             preludeBuf,
		initialDecision: decision,
		bindings:        bindings,
		attempt:         attempt,
		flushErr:        flushBufferedIfPresent,
	})
	proxyMs := time.Since(proxyStart).Milliseconds()
	finalProvider := decision.Provider
	if winnerIdx >= 0 && winnerIdx < len(bindings) {
		finalProvider = bindings[winnerIdx].Provider
	}

	in, out := extractor.Tokens()
	cacheCreation, cacheRead := extractor.CacheTokens()
	if responseBuffer != nil && proxyErr == nil {
		setRouterCostHeaders(w.Header(), routerResponseCostFromPricing(actPricing, decision.Provider, in, out, cacheCreation, cacheRead))
	}
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
		Float64("cost.requested_input_usd", catalog.EffectiveInputCost(in, cacheCreation, cacheRead, reqPricing, decision.Provider)).
		Float64("cost.requested_output_usd", catalog.EffectiveOutputCost(in, out, reqPricing)).
		Float64("cost.actual_input_usd", catalog.EffectiveInputCost(in, cacheCreation, cacheRead, actPricing, decision.Provider)).
		Float64("cost.actual_output_usd", catalog.EffectiveOutputCost(in, out, actPricing)).
		Bool("cost.subscription_served", servedOnSubscription(ctx)).
		Int64("latency.upstream_ms", proxyMs).
		Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
		Int64("upstream.status_code", int64(upstreamStatus(proxyErr))).
		Bool("routing.cross_format", false)
	applyPlannerAttrs(geminiUpstreamBuilder, routeRes)
	applyRoutingStateAttrs(geminiUpstreamBuilder, routeRes, decision.ServedIdentity(), sessionKey)
	applyEffortAttrs(geminiUpstreamBuilder, effortServed)
	addTimingAttrs(ctx, geminiUpstreamBuilder)
	otel.Record(ctx, otel.Span{
		Name:  "router.upstream",
		Start: proxyStart,
		End:   time.Now(),
		Attrs: geminiUpstreamBuilder.Build(),
	})
	respBody, respTrunc := capturedResponse(contentCap)
	s.recordCallLog(ctx, geminiUpstreamBuilder.Build(), routeMs, proxyErr != nil, body, respBody, respTrunc)
	otel.Flush(ctx)

	// Persist last-turn usage to the pin row so the next turn's planner
	// has cache-hit evidence. Off the request path; drops on saturation.
	s.recordTurnUsage(routeRes, finalProvider, decision.ServedIdentity(), in, out, cacheCreation, cacheRead)

	if proxyErr == nil {
		s.emitBilling(ctx, requestID, externalID, decision, actPricing, routeRes, in, out, cacheCreation, cacheRead)
		if compRes.Summarized {
			s.billCompactionSummary(ctx, requestID, externalID, compRes.SummaryUsage)
		}
	}

	// Two-strike provider disable: see ProxyMessages. Gemini rarely produces a
	// real 529, but covers a future translate-layer path that might synthesize one.
	s.maybeDisableProviderAfterOverload(ctx, stickyHit, proxyErr, finalProvider, decision.Reason, installationID, routeRes.SessionKey, stickyStateRole(routeRes), routeRes.PinRole)

	log.Info("ProxyGeminiGenerateContent complete", append([]any{"requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "decision_reason", decision.Reason, "embedded_tokens", len(promptText) / 4, "total_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "embed_input", embedInput, "sticky_hit", stickyHit, "pin_tier", pinTier, "turn_type", string(tt), "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr)}, plannerLogFields(routeRes)...)...)
	s.reportPolicyOutcome(ctx, routeRes, decision, effortServed, decision.Provider, false, feats.Tokens, in, out, cacheCreation, cacheRead, routeMs, proxyMs, proxyErr, nil)
	return proxyErr
}
