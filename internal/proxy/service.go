package proxy

import (
	"context"
	"encoding/hex"
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
	"workweave/router/internal/router/sessionpin"
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
	// pinStore persists session-sticky routing decisions across Cloud
	// Run instance restarts (see docs/plans/SESSION_PIN.md). Nil when
	// the ROUTER_SESSION_PIN_ENABLED flag is off; tiered lookup
	// degrades to the legacy apiKeyID LRU below.
	pinStore sessionpin.Store
	// pinCache absorbs the hot path so a 50-turn session keyed on the
	// same instance only hits Postgres ~5–10 times. 30s TTL is short
	// enough that pinned_until-driven invalidation in the pin store
	// remains the source of truth for "is this still valid"; the LRU
	// is pure latency optimization.
	pinCache *expirable.LRU[string, sessionpin.Pin]
	// pinWriteSem bounds concurrent async pin-upsert goroutines. Writes
	// are dropped (non-blocking select) when the semaphore is full;
	// pins are best-effort so a dropped write degrades gracefully to a
	// fresh route on the next turn rather than accumulating goroutines
	// under slow/unavailable Postgres.
	pinWriteSem chan struct{}
	// hardPinExplore gates the §3.4 Explore sub-agent hard-pin.
	// Off by default; enable via ROUTER_HARD_PIN_EXPLORE=true after one
	// week of shadow validation (see docs/plans/AGENTIC_CODING.md §3.4).
	hardPinExplore bool
	// hardPinProvider and hardPinModel are the (provider, model) routed to
	// for compaction and (when hardPinExplore is on) Explore sub-agent turns.
	// Derived at boot from the cheapest available model in the cluster bundle;
	// overridable via ROUTER_HARD_PIN_PROVIDER / ROUTER_HARD_PIN_MODEL.
	hardPinProvider string
	hardPinModel    string
	// telemetry is an optional repository for persisting per-request telemetry.
	// Nil disables persistence (e.g. no DB in some test environments).
	telemetry TelemetryRepository
}

// pinSessionTTL is the sliding TTL written into pinned_until on every
// pin upsert. Mirrors Anthropic's prompt-cache TTL on Sonnet 4.5+ /
// Haiku 4.5+ / Opus 4.5+ so the pin lifecycle tracks the cache it's
// trying to keep warm.
const pinSessionTTL = time.Hour

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

// NewService constructs the proxy service. pinStore may be nil when the
// session-pin feature flag is off; the tiered lookup transparently
// short-circuits past tiers 1–2 in that case.
func NewService(r router.Router, providerMap map[string]providers.Client, emitter *otel.Emitter, embedLastUserMessage bool, stickyDecisionTTL time.Duration, decisionLog *DecisionLog, semanticCache *cache.Cache, pinStore sessionpin.Store, hardPinExplore bool, hardPinProvider, hardPinModel string, telemetry TelemetryRepository) *Service {
	var sticky *expirable.LRU[string, router.Decision]
	if stickyDecisionTTL > 0 {
		sticky = expirable.NewLRU[string, router.Decision](10000, nil, stickyDecisionTTL)
	}
	var pinCache *expirable.LRU[string, sessionpin.Pin]
	var pinWriteSem chan struct{}
	if pinStore != nil {
		pinCache = expirable.NewLRU[string, sessionpin.Pin](10000, nil, 30*time.Second)
		pinWriteSem = make(chan struct{}, 64)
	}
	return &Service{
		router:               r,
		providers:            providerMap,
		emitter:              emitter,
		embedLastUserMessage: embedLastUserMessage,
		stickyDecisions:      sticky,
		decisionLog:          decisionLog,
		semanticCache:        semanticCache,
		pinStore:             pinStore,
		pinCache:             pinCache,
		pinWriteSem:          pinWriteSem,
		hardPinExplore:       hardPinExplore,
		hardPinProvider:      hardPinProvider,
		hardPinModel:         hardPinModel,
		telemetry:            telemetry,
	}
}

// MetricsSummary returns aggregated cost/token totals for the given installation and time window.
func (s *Service) MetricsSummary(ctx context.Context, installationID string, from, to time.Time) (TelemetrySummary, error) {
	if s.telemetry == nil {
		return TelemetrySummary{}, nil
	}
	return s.telemetry.GetTelemetrySummary(ctx, installationID, from, to)
}

// MetricsTimeseries returns per-bucket cost rows for the cost savings chart.
func (s *Service) MetricsTimeseries(ctx context.Context, installationID string, from, to time.Time, granularity string) ([]TelemetryBucket, error) {
	if s.telemetry == nil {
		return nil, nil
	}
	return s.telemetry.GetTelemetryTimeseries(ctx, installationID, from, to, granularity)
}

// MetricsSummaryAll aggregates totals across every installation. Used by
// admin-cookie sessions on the dashboard.
func (s *Service) MetricsSummaryAll(ctx context.Context, from, to time.Time) (TelemetrySummary, error) {
	if s.telemetry == nil {
		return TelemetrySummary{}, nil
	}
	return s.telemetry.GetTelemetrySummaryAll(ctx, from, to)
}

// MetricsTimeseriesAll returns per-bucket cost rows aggregated across every
// installation. Admin-only counterpart to MetricsTimeseries.
func (s *Service) MetricsTimeseriesAll(ctx context.Context, from, to time.Time, granularity string) ([]TelemetryBucket, error) {
	if s.telemetry == nil {
		return nil, nil
	}
	return s.telemetry.GetTelemetryTimeseriesAll(ctx, from, to, granularity)
}

// MetricsRows returns individual telemetry rows for the given installation in
// [from, to). Used by the dashboard drill-down modal.
func (s *Service) MetricsRows(ctx context.Context, installationID string, from, to time.Time, limit int32) ([]TelemetryRow, error) {
	if s.telemetry == nil {
		return nil, nil
	}
	return s.telemetry.GetTelemetryRows(ctx, installationID, from, to, limit)
}

// MetricsRowsAll returns individual telemetry rows across every installation.
// Admin-only counterpart to MetricsRows.
func (s *Service) MetricsRowsAll(ctx context.Context, from, to time.Time, limit int32) ([]TelemetryRow, error) {
	if s.telemetry == nil {
		return nil, nil
	}
	return s.telemetry.GetTelemetryRowsAll(ctx, from, to, limit)
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

// PassthroughToProvider forwards a non-routing-path request to the default
// provider ("anthropic") for backward compatibility with existing Anthropic
// metadata endpoints (count_tokens, models).
func (s *Service) PassthroughToProvider(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	return s.PassthroughToNamedProvider(ctx, providers.ProviderAnthropic, body, w, r)
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
	if providerName == providers.ProviderAnthropic && len(body) > 0 {
		env, parseErr := translate.ParseAnthropic(body)
		if parseErr == nil {
			prep, err = env.PrepareAnthropicPassthrough(r.Header)
			if err != nil {
				return fmt.Errorf("prepare passthrough: %w", err)
			}
		} else {
			prep = providers.PreparedRequest{Body: body, Headers: translate.AnthropicPassthroughHeaders(r.Header)}
		}
	} else if providerName == providers.ProviderAnthropic {
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
	installationID := installationIDFromContext(ctx)
	clientID := ClientIdentityFrom(ctx)
	bypassEval := hasEvalOverrideHeader(r)
	// Tier-1/2 (session-key-based) pinning stays enabled for eval traffic. The
	// session_key is derived from the system text + first user message, so each
	// eval prompt produces a unique key; pinning per-session is exactly what an
	// agentic harness needs to keep multi-turn tool-use on a single provider
	// (mid-conversation provider switches break Gemini, which rejects function
	// calls without thoughtSignature emitted by other providers). Tier-3 (the
	// legacy apiKeyID LRU) stays bypassed because the eval harness shares one
	// apiKeyID across all 500 instances — without that bypass, the first
	// decision would stick across unrelated instances.
	bypassLegacySticky := bypassEval

	// Anthropic clients pack sub-agent identity into metadata.user_id; the
	// x-weave-subagent-type header is for non-Anthropic ingress, so this
	// call passes "".
	routeStart := time.Now()
	routeRes, routeErr := s.routeWithSession(ctx, env, feats, apiKeyID, installationID, "", bypassLegacySticky, router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		PromptText:           promptText,
		EnabledProviders:     s.enabledProvidersForRequest(ctx, r.Header),
	})
	if routeErr != nil {
		log.Error("Routing failed", "err", routeErr, "route_ms", time.Since(routeStart).Milliseconds(), "requested_model", feats.Model, "estimated_input_tokens", feats.Tokens)
		return routeErr
	}
	decision := routeRes.Decision
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec
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
		Attrs: otel.NewAttrBuilder(26).
			String("request_id", requestID).
			String("external_id", externalID).
			String("router_user_id", auth.UserIDFrom(ctx)).
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
			IntSlice("routing.cluster_ids", clusterIDsFromDecision(decision)).
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
	case providers.ProviderAnthropic:
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
	case providers.ProviderOpenAI, providers.ProviderOpenRouter, providers.ProviderFireworks:
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
	case providers.ProviderGoogle:
		crossFormat = true
		prep, emitErr := env.PrepareGemini(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate Anthropic request to Gemini format", "err", emitErr)
			return fmt.Errorf("translate anthropic request to gemini: %w", emitErr)
		}
		var usage otel.UsageSink
		if s.emitter != nil {
			extractor = otel.NewUsageExtractor(nil, decision.Provider)
			usage = extractor
		}
		// Chain: Gemini SSE → OpenAI SSE → Anthropic SSE.
		anthropicTr := translate.NewAnthropicSSETranslator(sink, decision.Model, usage)
		geminiTr := translate.NewGeminiToOpenAISSETranslator(anthropicTr, decision.Model, nil)
		proxyErr = p.Proxy(ctx, decision, prep, geminiTr, r)
		if proxyErr == nil {
			proxyErr = geminiTr.Finalize()
		}
		if proxyErr == nil {
			proxyErr = anthropicTr.Finalize()
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
	cacheCreation, cacheRead := extractor.CacheTokens()
	upstreamBuilder := otel.NewAttrBuilder(27).
		String("request_id", requestID).
		String("external_id", externalID).
		String("router_user_id", auth.UserIDFrom(ctx)).
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

	if installationID != uuid.Nil {
		s.fireTelemetry(InsertTelemetryParams{
			InstallationID:         installationID.String(),
			RequestID:              requestID,
			SpanType:               "router.upstream",
			TraceID:                requestID,
			Timestamp:              requestStart,
			RequestedModel:         feats.Model,
			DecisionModel:          decision.Model,
			DecisionProvider:       decision.Provider,
			DecisionReason:         decision.Reason,
			EstimatedInputTokens:   int32(feats.Tokens),
			StickyHit:              stickyHit,
			EmbedInput:             embedInput,
			InputTokens:            int32(in),
			OutputTokens:           int32(out),
			RequestedInputCostUSD:  float64(in) / 1_000_000 * reqPricing.InputUSDPer1M,
			RequestedOutputCostUSD: float64(out) / 1_000_000 * reqPricing.OutputUSDPer1M,
			ActualInputCostUSD:     float64(in) / 1_000_000 * actPricing.InputUSDPer1M,
			ActualOutputCostUSD:    float64(out) / 1_000_000 * actPricing.OutputUSDPer1M,
			RouteLatencyMs:         routeMs,
			UpstreamLatencyMs:      proxyMs,
			TotalLatencyMs:         time.Since(requestStart).Milliseconds(),
			CrossFormat:            crossFormat,
			UpstreamStatusCode:     int32(upstreamStatus(proxyErr)),
		})
	}

	log.Info("ProxyMessages complete", "requested_model", feats.Model, "decision_model", decision.Model, "decision_provider", decision.Provider, "decision_reason", decision.Reason, "estimated_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "message_count", feats.MessageCount, "last_kind", feats.LastKind, "last_preview", feats.LastPreview, "embed_input", embedInput, "cross_format", crossFormat, "sticky_hit", stickyHit, "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	return proxyErr
}

// sessionPinCacheKey produces the in-proc LRU key for a (session_key,
// role) pair. Hex-encoding the key keeps the LRU's string-keyed
// generic API; the role suffix preserves the schema's role dimension
// for when §3.3 starts emitting non-default roles.
func sessionPinCacheKey(key [sessionpin.SessionKeyLen]byte, role string) string {
	return hex.EncodeToString(key[:]) + ":" + role
}

// pinDecision rehydrates a router.Decision from a stored pin.
// Metadata is intentionally nil — the cluster scorer's embedding
// is not persisted, so semantic-cache lookups won't fire on a
// pinned-route turn. Acceptable: cache hits are an optimization, the
// pin already short-circuits the routing decision.
func pinDecision(p sessionpin.Pin) router.Decision {
	return router.Decision{
		Provider: p.Provider,
		Model:    p.Model,
		Reason:   p.Reason,
	}
}

// clusterIDsFromDecision returns the cluster ids stamped on a routing
// decision, or nil for decisions without metadata (pinned-route turns,
// heuristic fallback).
func clusterIDsFromDecision(d router.Decision) []int {
	if d.Metadata == nil {
		return nil
	}
	return d.Metadata.ClusterIDs
}

// pinAge returns seconds since first_pinned_at; zero if the timestamp
// is unset (fresh pin).
func pinAge(p sessionpin.Pin) int64 {
	if p.FirstPinnedAt.IsZero() {
		return 0
	}
	d := time.Since(p.FirstPinnedAt)
	if d < 0 {
		return 0
	}
	return int64(d.Seconds())
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

// enabledProvidersForRequest returns the set of provider names whose
// credentials are resolvable for this request: any provider the router
// has a boot-time env key for, any provider with a BYOK key on the
// installation, and any provider whose client-supplied header carries a
// non-router bearer/x-api-key. The cluster scorer intersects this set
// with its boot-time candidates so argmax never picks a model the
// upstream call would 401 on.
func (s *Service) enabledProvidersForRequest(ctx context.Context, headers http.Header) map[string]struct{} {
	out := make(map[string]struct{}, len(s.providers))
	for p := range s.providers {
		out[p] = struct{}{}
	}
	for _, k := range externalKeysFromContext(ctx) {
		out[k.Provider] = struct{}{}
	}
	for _, p := range []string{providers.ProviderAnthropic, providers.ProviderOpenAI, providers.ProviderGoogle, providers.ProviderOpenRouter} {
		if _, already := out[p]; already {
			continue
		}
		if ExtractClientCredentials(p, headers) != nil {
			out[p] = struct{}{}
		}
	}
	return out
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

// fireTelemetry persists a telemetry row asynchronously so it never blocks
// the response path. Errors are logged at Debug — telemetry loss is acceptable.
func (s *Service) fireTelemetry(p InsertTelemetryParams) {
	if s.telemetry == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.telemetry.InsertRequestTelemetry(ctx, p); err != nil {
			observability.Get().Debug("Telemetry insert failed", "err", err, "request_id", p.RequestID)
		}
	}()
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

	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	installationID := installationIDFromContext(ctx)
	clientID := ClientIdentityFrom(ctx)

	env, parseErr := translate.ParseOpenAI(body)
	if parseErr != nil {
		log.Error("Failed to parse OpenAI request", "err", parseErr)
		return fmt.Errorf("parse request: %w", parseErr)
	}
	feats := env.RoutingFeatures(false)

	bypassEval := hasEvalOverrideHeader(r)
	bypassLegacySticky := bypassEval

	// OpenAI clients can't pack subagent identity into metadata.user_id;
	// they signal it via the x-weave-subagent-type header instead.
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
		log.Error("Routing failed for OpenAI request", "err", err, "route_ms", routeMs, "requested_model", feats.Model, "estimated_input_tokens", feats.Tokens)
		return err
	}
	decision := routeRes.Decision
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec

	// Semantic-cache lookup — same eligibility rules as ProxyMessages
	// (see that handler for rationale). Inbound format is FormatOpenAI
	// so an Anthropic-stored response is never replayed here. Eval-
	// harness traffic bypasses the cache; see ProxyMessages.
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
		Attrs: otel.NewAttrBuilder(24).
			String("request_id", requestID).
			String("external_id", externalID).
			String("router_user_id", auth.UserIDFrom(ctx)).
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
			IntSlice("routing.cluster_ids", clusterIDsFromDecision(decision)).
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
	case providers.ProviderOpenAI, providers.ProviderOpenRouter, providers.ProviderFireworks:
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
	case providers.ProviderGoogle:
		crossFormat = true
		prep, emitErr := env.PrepareGemini(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate OpenAI request to Gemini format", "err", emitErr)
			return fmt.Errorf("translate openai request to gemini: %w", emitErr)
		}
		var usage otel.UsageSink
		if s.emitter != nil {
			extractor = otel.NewUsageExtractor(nil, decision.Provider)
			usage = extractor
		}
		translator := translate.NewGeminiToOpenAISSETranslator(sink, decision.Model, usage)
		proxyErr = p.Proxy(ctx, decision, prep, translator, r)
		if proxyErr == nil {
			proxyErr = translator.Finalize()
		}
	case providers.ProviderAnthropic:
		crossFormat = true
		prep, emitErr := env.PrepareAnthropic(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate OpenAI request to Anthropic format", "err", emitErr)
			return fmt.Errorf("translate openai request: %w", emitErr)
		}
		var usage otel.UsageSink
		if s.emitter != nil {
			extractor = otel.NewUsageExtractor(nil, providers.ProviderAnthropic)
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
	cacheCreation, cacheRead := extractor.CacheTokens()
	openaiUpstreamBuilder := otel.NewAttrBuilder(27).
		String("request_id", requestID).
		String("external_id", externalID).
		String("router_user_id", auth.UserIDFrom(ctx)).
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
		Bool("routing.cross_format", crossFormat)
	addTimingAttrs(ctx, openaiUpstreamBuilder)
	otel.Record(ctx, otel.Span{
		Name:  "router.upstream",
		Start: proxyStart,
		End:   time.Now(),
		Attrs: openaiUpstreamBuilder.Build(),
	})
	otel.Flush(ctx)

	installationIDOAI, _ := ctx.Value(InstallationIDContextKey{}).(string)
	if installationIDOAI != "" {
		s.fireTelemetry(InsertTelemetryParams{
			InstallationID:         installationIDOAI,
			RequestID:              requestID,
			SpanType:               "router.upstream",
			TraceID:                requestID,
			Timestamp:              requestStart,
			RequestedModel:         feats.Model,
			DecisionModel:          decision.Model,
			DecisionProvider:       decision.Provider,
			DecisionReason:         decision.Reason,
			EstimatedInputTokens:   int32(feats.Tokens),
			StickyHit:              stickyHit,
			InputTokens:            int32(in),
			OutputTokens:           int32(out),
			RequestedInputCostUSD:  float64(in) / 1_000_000 * reqPricing.InputUSDPer1M,
			RequestedOutputCostUSD: float64(out) / 1_000_000 * reqPricing.OutputUSDPer1M,
			ActualInputCostUSD:     float64(in) / 1_000_000 * actPricing.InputUSDPer1M,
			ActualOutputCostUSD:    float64(out) / 1_000_000 * actPricing.OutputUSDPer1M,
			RouteLatencyMs:         routeMs,
			UpstreamLatencyMs:      proxyMs,
			TotalLatencyMs:         time.Since(requestStart).Milliseconds(),
			CrossFormat:            crossFormat,
			UpstreamStatusCode:     int32(upstreamStatus(proxyErr)),
		})
	}

	log.Info("ProxyOpenAIChatCompletion complete", "requested_model", feats.Model, "decision_model", decision.Model, "decision_provider", decision.Provider, "decision_reason", decision.Reason, "estimated_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "cross_format", crossFormat, "sticky_hit", stickyHit, "pin_tier", pinTier, "turn_type", string(tt), "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	return proxyErr
}
