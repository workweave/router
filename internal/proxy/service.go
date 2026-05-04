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
	"github.com/hashicorp/golang-lru/v2/expirable"
)

// Service orchestrates routing decisions and provider dispatch.
type Service struct {
	router               router.Router
	providers            map[string]providers.Client
	emitter              *otel.Emitter
	embedLastUserMessage bool
	stickyDecisions      *expirable.LRU[string, router.Decision]
	decisionLog          *DecisionLog
}

// APIKeyIDContextKey is the request-context key for the authenticated api_key_id.
type APIKeyIDContextKey struct{}

// ExternalIDContextKey is the request-context key for the installation's external_id.
type ExternalIDContextKey struct{}

// NewService constructs the proxy service.
func NewService(r router.Router, providerMap map[string]providers.Client, emitter *otel.Emitter, embedLastUserMessage bool, stickyDecisionTTL time.Duration, decisionLog *DecisionLog) *Service {
	var sticky *expirable.LRU[string, router.Decision]
	if stickyDecisionTTL > 0 {
		sticky = expirable.NewLRU[string, router.Decision](10000, nil, stickyDecisionTTL)
	}
	return &Service{
		router:               r,
		providers:            providerMap,
		emitter:              emitter,
		embedLastUserMessage: embedLastUserMessage,
		stickyDecisions:      sticky,
		decisionLog:          decisionLog,
	}
}

// ErrProviderNotConfigured is returned when a routing decision selects a
// provider that is not present in the registry.
var ErrProviderNotConfigured = errors.New("provider not configured")

// EmbedLastUserMessageContextKey is the context key for the per-request embed flag override.
type EmbedLastUserMessageContextKey struct{}

// embedLastUserMessageOverride reads the per-request embed flag from context, if set by the eval middleware.
func embedLastUserMessageOverride(ctx context.Context) (bool, bool) {
	v, ok := ctx.Value(EmbedLastUserMessageContextKey{}).(bool)
	return v, ok
}

func (s *Service) provider(name string) (providers.Client, error) {
	p, ok := s.providers[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotConfigured, name)
	}
	return p, nil
}

// Route exposes the underlying router strategy for callers that need a
// decision without dispatching (e.g. admin endpoints).
func (s *Service) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	return s.router.Route(ctx, req)
}

// Dispatch sends a request to the provider named in the routing decision.
func (s *Service) Dispatch(ctx context.Context, decision router.Decision, req providers.Request) (providers.Response, error) {
	p, err := s.provider(decision.Provider)
	if err != nil {
		return providers.Response{}, err
	}
	return p.Complete(ctx, req)
}

// PassthroughToProvider forwards a non-routing-path request to the default
// provider ("anthropic") for backward compatibility with existing Anthropic
// metadata endpoints (count_tokens, models).
func (s *Service) PassthroughToProvider(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	return s.PassthroughToNamedProvider(ctx, "anthropic", body, w, r)
}

// PassthroughToNamedProvider forwards a non-routing-path request to a specific
// provider by name. No model rewriting; no routing decision. For Anthropic
// targets the body is parsed into an envelope to scrub unsupported fields
// and derive filtered headers; other providers receive the body verbatim.
func (s *Service) PassthroughToNamedProvider(ctx context.Context, providerName string, body []byte, w http.ResponseWriter, r *http.Request) error {
	log := observability.Get()
	p, err := s.provider(providerName)
	if err != nil {
		return err
	}

	var prep providers.PreparedRequest
	if providerName == "anthropic" && len(body) > 0 {
		env, parseErr := translate.ParseAnthropic(body)
		if parseErr == nil {
			prep, err = env.PrepareAnthropicPassthrough(r.Header)
			if err != nil {
				return fmt.Errorf("prepare passthrough: %w", err)
			}
		} else {
			prep = providers.PreparedRequest{Body: body, Headers: translate.AnthropicPassthroughHeaders(r.Header)}
		}
	} else if providerName == "anthropic" {
		prep = providers.PreparedRequest{Body: body, Headers: translate.AnthropicPassthroughHeaders(r.Header)}
	} else {
		prep = providers.PreparedRequest{Body: body, Headers: make(http.Header)}
	}

	proxyStart := time.Now()
	proxyErr := p.Passthrough(ctx, prep, w, r)
	proxyMs := time.Since(proxyStart).Milliseconds()
	log.Info("PassthroughToProvider complete", "provider", providerName, "path", r.URL.Path, "method", r.Method, "proxy_ms", proxyMs, "proxy_err", proxyErr)
	return proxyErr
}

// ProxyMessages routes a raw Anthropic-Messages request body and streams the
// upstream response back. The routing decision is reflected in `x-router-*`
// response headers for client-side debugging.
func (s *Service) ProxyMessages(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	log := observability.Get()
	requestStart := time.Now()
	requestID := uuid.New().String()
	buf := otel.NewBuffer(s.emitter)
	ctx = buf.WithContext(ctx)

	env, parseErr := translate.ParseAnthropic(body)
	if parseErr != nil {
		log.Error("Failed to parse Anthropic request", "err", parseErr)
		return fmt.Errorf("parse request: %w", parseErr)
	}

	embedFlag := s.embedLastUserMessage
	if v, ok := embedLastUserMessageOverride(ctx); ok {
		embedFlag = v
	}
	feats := env.RoutingFeatures(embedFlag)
	promptText := feats.PromptText
	embedInput := "concatenated_stream"
	if embedFlag && feats.LastUserMessageText != "" {
		promptText = feats.LastUserMessageText
		embedInput = "last_user_message"
	}

	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	bypassSticky := hasEvalOverrideHeader(r)
	var (
		decision   router.Decision
		stickyHit  bool
		routeStart = time.Now()
	)
	if s.stickyDecisions != nil && apiKeyID != "" && !bypassSticky {
		if d, ok := s.stickyDecisions.Get(apiKeyID); ok {
			decision = d
			stickyHit = true
		}
	}
	if !stickyHit {
		var err error
		decision, err = s.router.Route(ctx, router.Request{
			RequestedModel:       feats.Model,
			EstimatedInputTokens: feats.Tokens,
			HasTools:             feats.HasTools,
			PromptText:           promptText,
		})
		if err != nil {
			log.Error("Routing failed", "err", err, "route_ms", time.Since(routeStart).Milliseconds(), "requested_model", feats.Model, "estimated_input_tokens", feats.Tokens)
			return err
		}
		if s.stickyDecisions != nil && apiKeyID != "" && !bypassSticky {
			s.stickyDecisions.Add(apiKeyID, decision)
		}
	}
	routeMs := time.Since(routeStart).Milliseconds()

	w.Header().Set("x-router-decision", decision.Reason)
	w.Header().Set("x-router-provider", decision.Provider)
	w.Header().Set("x-router-model", decision.Model)

	p, provErr := s.provider(decision.Provider)
	if provErr != nil {
		return provErr
	}

	reqPricing := otel.Lookup(feats.Model)
	actPricing := otel.Lookup(decision.Model)
	otel.Record(ctx, otel.Span{
		Name: "router.decision", Start: requestStart, End: time.Now(),
		Attrs: map[string]any{
			"request_id":                      requestID,
			"external_id":                     externalID,
			"requested.model":                 feats.Model,
			"decision.model":                  decision.Model,
			"decision.provider":               decision.Provider,
			"decision.reason":                 decision.Reason,
			"routing.sticky_hit":              stickyHit,
			"routing.embed_input":             embedInput,
			"routing.estimated_input_tokens":  feats.Tokens,
			"pricing.requested_input_per_1m":  reqPricing.InputUSDPer1M,
			"pricing.requested_output_per_1m": reqPricing.OutputUSDPer1M,
			"pricing.actual_input_per_1m":     actPricing.InputUSDPer1M,
			"pricing.actual_output_per_1m":    actPricing.OutputUSDPer1M,
			"latency.route_ms":                routeMs,
		},
	})
	otel.Flush(ctx)

	opts := translate.EmitOptions{
		TargetModel:        decision.Model,
		Capabilities:       router.Lookup(decision.Model),
		IncludeStreamUsage: s.emitter != nil,
	}

	proxyStart := time.Now()
	var proxyErr error
	crossFormat := false
	var extractor *otel.UsageExtractor

	switch decision.Provider {
	case "anthropic":
		prep, emitErr := env.PrepareAnthropic(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to emit Anthropic body", "err", emitErr)
			return fmt.Errorf("emit body: %w", emitErr)
		}
		proxyWriter := w
		if s.emitter != nil {
			extractor = otel.NewUsageExtractor(w, decision.Provider)
			proxyWriter = extractor
		}
		proxyErr = p.Proxy(ctx, decision, prep, proxyWriter, r)
	case "openai", "google":
		crossFormat = true
		prep, emitErr := env.PrepareOpenAI(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate Anthropic request to OpenAI format", "err", emitErr, "decision_provider", decision.Provider)
			return fmt.Errorf("translate anthropic request: %w", emitErr)
		}
		var sink otel.UsageSink
		if s.emitter != nil {
			extractor = otel.NewUsageExtractor(nil, decision.Provider)
			sink = extractor
		}
		translator := translate.NewAnthropicSSETranslator(w, decision.Model, sink)
		proxyErr = p.Proxy(ctx, decision, prep, translator, r)
		if proxyErr == nil {
			proxyErr = translator.Finalize()
		}
	default:
		return fmt.Errorf("%w: %s (no translation path defined for inbound Anthropic Messages)", ErrProviderNotConfigured, decision.Provider)
	}

	proxyMs := time.Since(proxyStart).Milliseconds()

	in, out := extractor.Tokens()
	upstreamAttrs := map[string]any{
		"request_id":                requestID,
		"external_id":               externalID,
		"usage.input_tokens":        in,
		"usage.output_tokens":       out,
		"cost.requested_input_usd":  float64(in) / 1_000_000 * reqPricing.InputUSDPer1M,
		"cost.requested_output_usd": float64(out) / 1_000_000 * reqPricing.OutputUSDPer1M,
		"cost.actual_input_usd":     float64(in) / 1_000_000 * actPricing.InputUSDPer1M,
		"cost.actual_output_usd":    float64(out) / 1_000_000 * actPricing.OutputUSDPer1M,
		"latency.upstream_ms":       proxyMs,
		"latency.total_ms":          time.Since(requestStart).Milliseconds(),
		"upstream.status_code":      upstreamStatus(proxyErr),
		"routing.cross_format":      crossFormat,
	}
	for k, v := range timingAttrs(ctx) {
		upstreamAttrs[k] = v
	}
	otel.Record(ctx, otel.Span{
		Name: "router.upstream", Start: proxyStart, End: time.Now(),
		Attrs: upstreamAttrs,
	})
	otel.Flush(ctx)

	if reqID := w.Header().Get("request-id"); reqID != "" {
		s.decisionLog.Append(DecisionLogEntry{
			RequestID:        reqID,
			RequestedModel:   feats.Model,
			DecisionModel:    decision.Model,
			DecisionReason:   decision.Reason,
			DecisionProvider: decision.Provider,
		})
	}

	log.Info("ProxyMessages complete", "requested_model", feats.Model, "decision_model", decision.Model, "decision_provider", decision.Provider, "decision_reason", decision.Reason, "estimated_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "message_count", feats.MessageCount, "last_kind", feats.LastKind, "last_preview", feats.LastPreview, "embed_input", embedInput, "cross_format", crossFormat, "sticky_hit", stickyHit, "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	return proxyErr
}

// hasEvalOverrideHeader reports whether the request carries any eval-harness override headers.
func hasEvalOverrideHeader(r *http.Request) bool {
	if r == nil {
		return false
	}
	return r.Header.Get("x-weave-disable-cluster") != "" ||
		r.Header.Get("x-weave-cluster-version") != "" ||
		r.Header.Get("x-weave-embed-last-user-message") != ""
}

// timingAttrs returns derived latency attributes from the request Timing, or nil if absent.
func timingAttrs(ctx context.Context) map[string]any {
	t := otel.TimingFrom(ctx)
	if t == nil {
		return nil
	}
	upstreamTotal := t.Ms(&t.UpstreamRequestNanos, &t.UpstreamEOFNanos)
	fullE2E := t.MsSince(&t.EntryNanos)

	var overhead int64
	if upstreamTotal > 0 {
		overhead = fullE2E - upstreamTotal
	}

	return map[string]any{
		"latency.full_e2e_ms":            fullE2E,
		"latency.preupstream_ms":         t.Ms(&t.EntryNanos, &t.UpstreamRequestNanos),
		"latency.upstream_headers_ms":    t.Ms(&t.UpstreamRequestNanos, &t.UpstreamHeadersNanos),
		"latency.upstream_first_byte_ms": t.Ms(&t.UpstreamRequestNanos, &t.UpstreamFirstByteNanos),
		"latency.upstream_total_ms":      upstreamTotal,
		"latency.postupstream_ms":        t.MsSince(&t.UpstreamEOFNanos),
		"latency.router_overhead_ms":     overhead,
	}
}

// upstreamStatus extracts the HTTP status from an UpstreamStatusError, or 0.
func upstreamStatus(err error) int {
	var e *providers.UpstreamStatusError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// ProxyOpenAIChatCompletion routes an OpenAI Chat Completion request, translating
// cross-format when the decision selects a non-OpenAI provider.
func (s *Service) ProxyOpenAIChatCompletion(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	log := observability.Get()
	requestStart := time.Now()
	requestID := uuid.New().String()
	buf := otel.NewBuffer(s.emitter)
	ctx = buf.WithContext(ctx)

	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)

	env, parseErr := translate.ParseOpenAI(body)
	if parseErr != nil {
		log.Error("Failed to parse OpenAI request", "err", parseErr)
		return fmt.Errorf("parse request: %w", parseErr)
	}
	feats := env.RoutingFeatures(false)

	routeStart := time.Now()
	decision, err := s.router.Route(ctx, router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		PromptText:           feats.PromptText,
	})
	routeMs := time.Since(routeStart).Milliseconds()
	if err != nil {
		log.Error("Routing failed for OpenAI request", "err", err, "route_ms", routeMs, "requested_model", feats.Model, "estimated_input_tokens", feats.Tokens)
		return err
	}

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
		Name: "router.decision", Start: requestStart, End: time.Now(),
		Attrs: map[string]any{
			"request_id":                      requestID,
			"external_id":                     externalID,
			"requested.model":                 feats.Model,
			"decision.model":                  decision.Model,
			"decision.provider":               decision.Provider,
			"decision.reason":                 decision.Reason,
			"routing.estimated_input_tokens":  feats.Tokens,
			"pricing.requested_input_per_1m":  reqPricing.InputUSDPer1M,
			"pricing.requested_output_per_1m": reqPricing.OutputUSDPer1M,
			"pricing.actual_input_per_1m":     actPricing.InputUSDPer1M,
			"pricing.actual_output_per_1m":    actPricing.OutputUSDPer1M,
			"latency.route_ms":                routeMs,
		},
	})
	otel.Flush(ctx)

	opts := translate.EmitOptions{
		TargetModel:        decision.Model,
		Capabilities:       router.Lookup(decision.Model),
		IncludeStreamUsage: s.emitter != nil,
	}

	proxyStart := time.Now()
	var proxyErr error
	crossFormat := false
	var extractor *otel.UsageExtractor

	switch decision.Provider {
	case "openai", "google":
		prep, emitErr := env.PrepareOpenAI(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to emit OpenAI body", "err", emitErr)
			return fmt.Errorf("emit body: %w", emitErr)
		}
		proxyWriter := w
		if s.emitter != nil {
			extractor = otel.NewUsageExtractor(w, decision.Provider)
			proxyWriter = extractor
		}
		proxyErr = p.Proxy(ctx, decision, prep, proxyWriter, r)
	case "anthropic":
		crossFormat = true
		prep, emitErr := env.PrepareAnthropic(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate OpenAI request to Anthropic format", "err", emitErr)
			return fmt.Errorf("translate openai request: %w", emitErr)
		}
		var sink otel.UsageSink
		if s.emitter != nil {
			extractor = otel.NewUsageExtractor(nil, "anthropic")
			sink = extractor
		}
		translator := translate.NewSSETranslator(w, decision.Model, sink)
		proxyErr = p.Proxy(ctx, decision, prep, translator, r)
		if proxyErr == nil {
			proxyErr = translator.Finalize()
		}
	default:
		return fmt.Errorf("%w: %s (no translation path defined)", ErrProviderNotConfigured, decision.Provider)
	}

	proxyMs := time.Since(proxyStart).Milliseconds()

	in, out := extractor.Tokens()
	openaiUpstreamAttrs := map[string]any{
		"request_id":                requestID,
		"external_id":               externalID,
		"usage.input_tokens":        in,
		"usage.output_tokens":       out,
		"cost.requested_input_usd":  float64(in) / 1_000_000 * reqPricing.InputUSDPer1M,
		"cost.requested_output_usd": float64(out) / 1_000_000 * reqPricing.OutputUSDPer1M,
		"cost.actual_input_usd":     float64(in) / 1_000_000 * actPricing.InputUSDPer1M,
		"cost.actual_output_usd":    float64(out) / 1_000_000 * actPricing.OutputUSDPer1M,
		"latency.upstream_ms":       proxyMs,
		"latency.total_ms":          time.Since(requestStart).Milliseconds(),
		"upstream.status_code":      upstreamStatus(proxyErr),
		"routing.cross_format":      crossFormat,
	}
	for k, v := range timingAttrs(ctx) {
		openaiUpstreamAttrs[k] = v
	}
	otel.Record(ctx, otel.Span{
		Name: "router.upstream", Start: proxyStart, End: time.Now(),
		Attrs: openaiUpstreamAttrs,
	})
	otel.Flush(ctx)

	log.Info("ProxyOpenAIChatCompletion complete", "requested_model", feats.Model, "decision_model", decision.Model, "decision_reason", decision.Reason, "estimated_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "cross_format", crossFormat, "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	return proxyErr
}
