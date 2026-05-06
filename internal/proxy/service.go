package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/cache"
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
	// semanticCache short-circuits non-streaming requests on a
	// cosine-similarity hit against a stored response. Nil disables
	// the cache entirely. Always nil-check before use.
	semanticCache *cache.Cache
}

// APIKeyIDContextKey is the request-context key for the authenticated api_key_id.
type APIKeyIDContextKey struct{}

// ExternalIDContextKey is the request-context key for the installation's external_id.
type ExternalIDContextKey struct{}

// CredentialsContextKey is the request-context key for resolved per-request credentials.
type CredentialsContextKey struct{}

// CredentialsFromContext retrieves the resolved credentials stashed on a
// request context by the proxy service. Returns nil when no credentials have
// been stashed (plan-based auth path).
func CredentialsFromContext(ctx context.Context) *Credentials {
	v := ctx.Value(CredentialsContextKey{})
	if v == nil {
		return nil
	}
	creds, _ := v.(*Credentials)
	return creds
}

// NewService constructs the proxy service.
func NewService(r router.Router, providerMap map[string]providers.Client, emitter *otel.Emitter, embedLastUserMessage bool, stickyDecisionTTL time.Duration, decisionLog *DecisionLog, semanticCache *cache.Cache) *Service {
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
		semanticCache:        semanticCache,
	}
}

// ErrProviderNotConfigured is returned when a routing decision selects a
// provider that is not present in the registry.
var ErrProviderNotConfigured = errors.New("provider not configured")

// semanticCacheMaxBodyBytes caps how large a non-streaming response
// the cache will store. Bodies larger than this stream through to the
// client unchanged, but the post-Proxy Store call is skipped to keep
// peak memory bounded. 1 MiB covers typical Anthropic Messages and
// OpenAI Chat Completions responses; the long-tail of large bodies
// pays full provider cost on subsequent identical prompts.
const semanticCacheMaxBodyBytes = 1 << 20

// headersToSkipOnHit lists response headers the cache must NOT replay
// on a hit. request-id ties to a specific upstream call and would
// confuse downstream consumers (decisionLog, OTel correlation) if
// reused. x-router-* are set fresh on the hit path so the client
// always sees the current decision rather than a stale one.
var headersToSkipOnHit = map[string]struct{}{
	"Request-Id":        {},
	"X-Request-Id":      {},
	"X-Router-Decision": {},
	"X-Router-Provider": {},
	"X-Router-Model":    {},
	"X-Router-Cache":    {},
}

// cloneCacheHeaders snapshots a header set for storage, dropping
// transient identifiers that must not survive replay (see
// headersToSkipOnHit).
func cloneCacheHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		if _, skip := headersToSkipOnHit[http.CanonicalHeaderKey(k)]; skip {
			continue
		}
		copied := make([]string, len(vs))
		copy(copied, vs)
		out[k] = copied
	}
	return out
}

// writeCachedResponse emits a stored CachedResponse to the client.
// Caller-set router headers (x-router-*) are written from the live
// decision (not the cached entry) so the client always sees an
// accurate routing trace; the x-router-cache marker advertises the
// hit. Body is written verbatim — already in the inbound wire format.
func (s *Service) writeCachedResponse(w http.ResponseWriter, resp cache.CachedResponse, decision router.Decision) {
	for k, vs := range resp.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("x-router-decision", decision.Reason)
	w.Header().Set("x-router-provider", decision.Provider)
	w.Header().Set("x-router-model", decision.Model)
	w.Header().Set("x-router-cache", "hit")
	if resp.StatusCode != 0 && resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
	}
	_, _ = w.Write(resp.Body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

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
	clientID := ClientIdentityFrom(ctx)
	bypassEval := hasEvalOverrideHeader(r)
	var (
		decision   router.Decision
		stickyHit  bool
		routeStart = time.Now()
	)
	if s.stickyDecisions != nil && apiKeyID != "" && !bypassEval {
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
		if s.stickyDecisions != nil && apiKeyID != "" && !bypassEval {
			s.stickyDecisions.Add(apiKeyID, decision)
		}
	}
	routeMs := time.Since(routeStart).Milliseconds()

	// Semantic-cache lookup. Eligible when the cache is configured at
	// boot, the request is non-streaming, the routing decision carries
	// metadata (always set by the cluster scorer on success), and the
	// caller has an externalID for per-tenant isolation. Eval-harness
	// traffic bypasses the cache so per-prompt accuracy attribution
	// isn't polluted by cosine-near-neighbor replays of an unrelated
	// stored response.
	cacheEligible := s.semanticCache != nil && !env.Stream() && decision.Metadata != nil && externalID != "" && !bypassEval
	if cacheEligible {
		if resp, hit := s.semanticCache.Lookup(externalID, cache.FormatAnthropic, decision.Metadata.Embedding, decision.Metadata.ClusterIDs); hit {
			s.writeCachedResponse(w, resp, decision)
			otel.Record(ctx, otel.Span{
				Name:  "router.cache_hit",
				Start: requestStart,
				End:   time.Now(),
				Attrs: otel.NewAttrBuilder(7).
					String("request_id", requestID).
					String("external_id", externalID).
					String("decision.model", decision.Model).
					String("decision.provider", decision.Provider).
					Bool("cache.hit", true).
					String("cache.format", string(cache.FormatAnthropic)).
					Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
					Build(),
			})
			otel.Flush(ctx)
			log.Info("ProxyMessages cache hit", "requested_model", feats.Model, "decision_model", decision.Model, "decision_provider", decision.Provider, "external_id", externalID, "total_ms", time.Since(requestStart).Milliseconds())
			return nil
		}
	}

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
		Name:  "router.decision",
		Start: requestStart,
		End:   time.Now(),
		Attrs: otel.NewAttrBuilder(19).
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
			String("routing.embed_input", embedInput).
			Int64("routing.estimated_input_tokens", int64(feats.Tokens)).
			Float64("pricing.requested_input_per_1m", reqPricing.InputUSDPer1M).
			Float64("pricing.requested_output_per_1m", reqPricing.OutputUSDPer1M).
			Float64("pricing.actual_input_per_1m", actPricing.InputUSDPer1M).
			Float64("pricing.actual_output_per_1m", actPricing.OutputUSDPer1M).
			Int64("latency.route_ms", routeMs).
			Build(),
	})
	otel.Flush(ctx)

	opts := translate.EmitOptions{
		TargetModel:        decision.Model,
		Capabilities:       router.Lookup(decision.Model),
		IncludeStreamUsage: s.emitter != nil,
	}

	// Resolve per-request credentials for the chosen provider. When BYOK keys
	// are configured for this installation they take precedence; otherwise the
	// inbound request headers supply the credentials (plan-based auth).
	ctx = resolveAndInjectCredentials(ctx, decision.Provider, r.Header)

	// Wrap w with a captureWriter when the cache is eligible so the
	// post-translation wire bytes get mirrored into a buffer for
	// post-Proxy storage. captureW.captured() is the source of truth
	// for whether storage should happen.
	var captureW *captureWriter
	var sink http.ResponseWriter = w
	if cacheEligible {
		captureW = newCaptureWriter(w, semanticCacheMaxBodyBytes)
		sink = captureW
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
		proxyWriter := sink
		if s.emitter != nil {
			extractor = otel.NewUsageExtractor(sink, decision.Provider)
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
		var usage otel.UsageSink
		if s.emitter != nil {
			extractor = otel.NewUsageExtractor(nil, decision.Provider)
			usage = extractor
		}
		translator := translate.NewAnthropicSSETranslator(sink, decision.Model, usage)
		proxyErr = p.Proxy(ctx, decision, prep, translator, r)
		if proxyErr == nil {
			proxyErr = translator.Finalize()
		}
	default:
		return fmt.Errorf("%w: %s (no translation path defined for inbound Anthropic Messages)", ErrProviderNotConfigured, decision.Provider)
	}

	// Cache store. Only on success and when the captured body fits
	// within MaxBodyBytes (captureW.captured returns false on
	// overflow). Use the smallest top-p cluster id for storage; the
	// LRU.Lookup path scans every top-p cluster, so any one is fine.
	if cacheEligible && proxyErr == nil && captureW != nil {
		if body, status, ok := captureW.captured(); ok && status == http.StatusOK {
			storeResp := cache.CachedResponse{
				StatusCode: status,
				Headers:    cloneCacheHeaders(w.Header()),
				Body:       body,
			}
			s.semanticCache.Store(externalID, cache.FormatAnthropic, decision.Metadata.Embedding, decision.Metadata.ClusterIDs[0], storeResp)
		}
	}

	proxyMs := time.Since(proxyStart).Milliseconds()

	in, out := extractor.Tokens()
	upstreamBuilder := otel.NewAttrBuilder(24).
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
		Bool("routing.cross_format", crossFormat)
	addTimingAttrs(ctx, upstreamBuilder)
	otel.Record(ctx, otel.Span{
		Name:  "router.upstream",
		Start: proxyStart,
		End:   time.Now(),
		Attrs: upstreamBuilder.Build(),
	})
	otel.Flush(ctx)

	if reqID := w.Header().Get("request-id"); reqID != "" {
		s.decisionLog.Append(DecisionLogEntry{
			RequestID:        reqID,
			RequestedModel:   feats.Model,
			DecisionModel:    decision.Model,
			DecisionReason:   decision.Reason,
			DecisionProvider: decision.Provider,
			DeviceID:         clientID.DeviceID,
			SessionID:        clientID.SessionID,
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
	return r.Header.Get("x-weave-cluster-version") != "" ||
		r.Header.Get("x-weave-embed-last-user-message") != ""
}

// externalKeysFromContext reads the external API keys stashed by the auth
// middleware. Returns nil when none are present (plan-based auth path).
func externalKeysFromContext(ctx context.Context) []*auth.ExternalAPIKey {
	v := ctx.Value(ExternalAPIKeysContextKey{})
	if v == nil {
		return nil
	}
	keys, _ := v.([]*auth.ExternalAPIKey)
	return keys
}

// resolveAndInjectCredentials builds a BYOK credentials map from the external
// keys on ctx, resolves the best credentials for provider, and returns a
// context with the credentials stashed under CredentialsContextKey. When no
// credentials are available for the provider, ctx is returned unchanged.
func resolveAndInjectCredentials(ctx context.Context, provider string, headers http.Header) context.Context {
	byok := BuildCredentialsMap(externalKeysFromContext(ctx))
	creds := ResolveCredentials(provider, byok, headers)
	if creds != nil {
		return context.WithValue(ctx, CredentialsContextKey{}, creds)
	}
	return ctx
}

// addTimingAttrs appends derived latency attributes from the request Timing
// to the builder. No-op when no Timing is attached (middleware not wired or
// OTel disabled).
func addTimingAttrs(ctx context.Context, b *otel.AttrBuilder) {
	t := otel.TimingFrom(ctx)
	if t == nil {
		return
	}
	upstreamTotal := t.Ms(&t.UpstreamRequestNanos, &t.UpstreamEOFNanos)
	fullE2E := t.MsSince(&t.EntryNanos)

	var overhead int64
	if upstreamTotal > 0 {
		overhead = fullE2E - upstreamTotal
	}

	b.Int64("latency.full_e2e_ms", fullE2E).
		Int64("latency.preupstream_ms", t.Ms(&t.EntryNanos, &t.UpstreamRequestNanos)).
		Int64("latency.upstream_headers_ms", t.Ms(&t.UpstreamRequestNanos, &t.UpstreamHeadersNanos)).
		Int64("latency.upstream_first_byte_ms", t.Ms(&t.UpstreamRequestNanos, &t.UpstreamFirstByteNanos)).
		Int64("latency.upstream_total_ms", upstreamTotal).
		Int64("latency.postupstream_ms", t.MsSince(&t.UpstreamEOFNanos)).
		Int64("latency.router_overhead_ms", overhead)
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
	clientID := ClientIdentityFrom(ctx)

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

	// Semantic-cache lookup — same eligibility rules as ProxyMessages
	// (see that handler for rationale). Inbound format is FormatOpenAI
	// so an Anthropic-stored response is never replayed here. Eval-
	// harness traffic bypasses the cache; see ProxyMessages.
	bypassEval := hasEvalOverrideHeader(r)
	cacheEligible := s.semanticCache != nil && !env.Stream() && decision.Metadata != nil && externalID != "" && !bypassEval
	if cacheEligible {
		if resp, hit := s.semanticCache.Lookup(externalID, cache.FormatOpenAI, decision.Metadata.Embedding, decision.Metadata.ClusterIDs); hit {
			s.writeCachedResponse(w, resp, decision)
			otel.Record(ctx, otel.Span{
				Name:  "router.cache_hit",
				Start: requestStart,
				End:   time.Now(),
				Attrs: otel.NewAttrBuilder(7).
					String("request_id", requestID).
					String("external_id", externalID).
					String("decision.model", decision.Model).
					String("decision.provider", decision.Provider).
					Bool("cache.hit", true).
					String("cache.format", string(cache.FormatOpenAI)).
					Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
					Build(),
			})
			otel.Flush(ctx)
			log.Info("ProxyOpenAIChatCompletion cache hit", "requested_model", feats.Model, "decision_model", decision.Model, "decision_provider", decision.Provider, "external_id", externalID, "total_ms", time.Since(requestStart).Milliseconds())
			return nil
		}
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
		Name:  "router.decision",
		Start: requestStart,
		End:   time.Now(),
		Attrs: otel.NewAttrBuilder(17).
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
			Int64("routing.estimated_input_tokens", int64(feats.Tokens)).
			Float64("pricing.requested_input_per_1m", reqPricing.InputUSDPer1M).
			Float64("pricing.requested_output_per_1m", reqPricing.OutputUSDPer1M).
			Float64("pricing.actual_input_per_1m", actPricing.InputUSDPer1M).
			Float64("pricing.actual_output_per_1m", actPricing.OutputUSDPer1M).
			Int64("latency.route_ms", routeMs).
			Build(),
	})
	otel.Flush(ctx)

	opts := translate.EmitOptions{
		TargetModel:        decision.Model,
		Capabilities:       router.Lookup(decision.Model),
		IncludeStreamUsage: s.emitter != nil,
	}

	// Resolve per-request credentials for the chosen provider.
	ctx = resolveAndInjectCredentials(ctx, decision.Provider, r.Header)

	// Wrap w with a captureWriter when the cache is eligible so the
	// post-translation wire bytes get mirrored into a buffer for
	// post-Proxy storage.
	var captureW *captureWriter
	var sink http.ResponseWriter = w
	if cacheEligible {
		captureW = newCaptureWriter(w, semanticCacheMaxBodyBytes)
		sink = captureW
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
		proxyWriter := sink
		if s.emitter != nil {
			extractor = otel.NewUsageExtractor(sink, decision.Provider)
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
		var usage otel.UsageSink
		if s.emitter != nil {
			extractor = otel.NewUsageExtractor(nil, "anthropic")
			usage = extractor
		}
		translator := translate.NewSSETranslator(sink, decision.Model, usage)
		proxyErr = p.Proxy(ctx, decision, prep, translator, r)
		if proxyErr == nil {
			proxyErr = translator.Finalize()
		}
	default:
		return fmt.Errorf("%w: %s (no translation path defined)", ErrProviderNotConfigured, decision.Provider)
	}

	if cacheEligible && proxyErr == nil && captureW != nil {
		if body, status, ok := captureW.captured(); ok && status == http.StatusOK {
			storeResp := cache.CachedResponse{
				StatusCode: status,
				Headers:    cloneCacheHeaders(w.Header()),
				Body:       body,
			}
			s.semanticCache.Store(externalID, cache.FormatOpenAI, decision.Metadata.Embedding, decision.Metadata.ClusterIDs[0], storeResp)
		}
	}

	proxyMs := time.Since(proxyStart).Milliseconds()

	in, out := extractor.Tokens()
	openaiUpstreamBuilder := otel.NewAttrBuilder(24).
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
		Bool("routing.cross_format", crossFormat)
	addTimingAttrs(ctx, openaiUpstreamBuilder)
	otel.Record(ctx, otel.Span{
		Name:  "router.upstream",
		Start: proxyStart,
		End:   time.Now(),
		Attrs: openaiUpstreamBuilder.Build(),
	})
	otel.Flush(ctx)

	log.Info("ProxyOpenAIChatCompletion complete", "requested_model", feats.Model, "decision_model", decision.Model, "decision_reason", decision.Reason, "estimated_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "cross_format", crossFormat, "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	return proxyErr
}
