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
	"workweave/router/internal/router/turntype"
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

// InstallationIDContextKey is the request-context key for the authed
// installation's UUID. Stashed by middleware.WithAuth and read by
// ProxyMessages so session pins can be FK'd to the installation.
type InstallationIDContextKey struct{}

// NewService constructs the proxy service. pinStore may be nil when the
// session-pin feature flag is off; the tiered lookup transparently
// short-circuits past tiers 1–2 in that case.
func NewService(r router.Router, providerMap map[string]providers.Client, emitter *otel.Emitter, embedLastUserMessage bool, stickyDecisionTTL time.Duration, decisionLog *DecisionLog, semanticCache *cache.Cache, pinStore sessionpin.Store, hardPinExplore bool, hardPinProvider, hardPinModel string) *Service {
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

	tt := turntype.Detect(body)

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
	installationID, _ := ctx.Value(InstallationIDContextKey{}).(string)
	clientID := ClientIdentityFrom(ctx)
	bypassEval := hasEvalOverrideHeader(r)
	bypassSticky := bypassEval

	// §3.4: hard pins for turn types whose optimal model is known a priori.
	// These bypass all session-pin tiers and the cluster scorer, and must NOT
	// write a pin upsert (they must not overwrite the session's main-loop model).
	//   Compaction — always Haiku (short-out-of-long-in cost profile).
	//   SubAgentDispatch — Haiku when ROUTER_HARD_PIN_EXPLORE is set; gated
	//     until one week of shadow validation confirms no quality regression.
	var (
		decision        router.Decision
		stickyHit       bool
		pinTier         = "miss"
		pinAgeSec       int64
		sessionKey      [sessionpin.SessionKeyLen]byte
		pinCacheKey     string
		routeStart      = time.Now()
		// expiredPinModel is the model from a recently-expired session pin.
		// Even though the pin has lapsed, the upstream prompt cache may
		// still be warm (Anthropic cache TTL matches pinSessionTTL). We
		// pass it to the scorer so it can prefer the warm model rather than
		// switching mid-session on cost grounds alone.
		expiredPinModel string
	)
	if tt == turntype.Compaction || (tt == turntype.SubAgentDispatch && s.hardPinExplore) {
		decision = router.Decision{
			Provider: s.hardPinProvider,
			Model:    s.hardPinModel,
			Reason:   string(tt) + "_hard_pin",
		}
		stickyHit = true
		pinTier = string(tt) + "_hard_pin"
	}

	// Tiered routing-decision lookup (see docs/plans/SESSION_PIN.md §5).
	//   Tier 1 — pinCache (in-proc, 30s):   absorbs same-instance burst.
	//   Tier 2 — pinStore (Postgres, 1h):   survives Cloud Run restarts.
	//   Tier 3 — stickyDecisions (legacy):  apiKeyID-keyed LRU; kept
	//                                        during rollout, removed in
	//                                        a follow-up.
	// Tier 3 is only consulted on a tier-2 *miss*, never on a tier-2
	// *error* — papering over store errors with the legacy cache would
	// mask the migrate-to-Memorystore trigger criteria.
	// pinEligible is false for hard-pinned turns (stickyHit already set above),
	// which also suppresses the async pin upsert for those turns.
	pinEligible := s.pinStore != nil && !bypassSticky && !stickyHit
	if pinEligible {
		sessionKey = DeriveSessionKey(env, apiKeyID)
		pinCacheKey = sessionPinCacheKey(sessionKey, sessionpin.DefaultRole)

		if s.pinCache != nil {
			if pin, ok := s.pinCache.Get(pinCacheKey); ok {
				decision = pinDecision(pin)
				stickyHit = true
				pinTier = "in_proc"
				pinAgeSec = pinAge(pin)
			}
		}
		if !stickyHit {
			pin, found, err := s.pinStore.Get(ctx, sessionKey, sessionpin.DefaultRole)
			if err != nil {
				log.Error("session pin store unavailable; falling through to cluster scorer", "err", err)
			} else if found {
				if pin.PinnedUntil.After(time.Now()) {
					decision = pinDecision(pin)
					stickyHit = true
					pinTier = "postgres"
					pinAgeSec = pinAge(pin)
					if s.pinCache != nil {
						s.pinCache.Add(pinCacheKey, pin)
					}
				} else {
					// Pin has expired but the model's prompt cache may still
					// be warm (Anthropic cache TTL == pinSessionTTL). Record
					// the model so the scorer can prefer it over a cold
					// alternative with the same quality score.
					expiredPinModel = pin.Model
				}
			}
		}
	}

	// Tier 3: legacy apiKeyID LRU. Consulted only on tier-2 miss (or
	// when pinStore is nil).
	if !stickyHit && s.stickyDecisions != nil && apiKeyID != "" && !bypassSticky {
		if d, ok := s.stickyDecisions.Get(apiKeyID); ok {
			decision = d
			stickyHit = true
			pinTier = "legacy_apikey"
		}
	}

	if !stickyHit {
		var cacheWarmModels map[string]bool
		if expiredPinModel != "" {
			cacheWarmModels = map[string]bool{expiredPinModel: true}
		}
		var err error
		decision, err = s.router.Route(ctx, router.Request{
			RequestedModel:       feats.Model,
			EstimatedInputTokens: feats.Tokens,
			HasTools:             feats.HasTools,
			PromptText:           promptText,
			EnabledProviders:     s.enabledProvidersForRequest(ctx, r.Header),
			CacheWarmModels:      cacheWarmModels,
		})
		if err != nil {
			log.Error("Routing failed", "err", err, "route_ms", time.Since(routeStart).Milliseconds(), "requested_model", feats.Model, "estimated_input_tokens", feats.Tokens)
			return err
		}
		if s.stickyDecisions != nil && apiKeyID != "" && !bypassSticky {
			s.stickyDecisions.Add(apiKeyID, decision)
		}
	}

	// §3.3: annotate pin tier when a tool-result turn short-circuits to an
	// existing session pin, so the routing.session_pin_tier OTel attribute
	// reflects the bypass in dashboards.
	if stickyHit && tt == turntype.ToolResult {
		pinTier += "_tool_result_sc"
	}

	// Async upsert on every routed request — refreshes the sliding TTL
	// for sticky hits, records new pins for fresh routes. Uses
	// context.Background() per repo convention so a request cancel
	// never drops the pin write (would force re-route on the next turn
	// and break cache continuity).
	if pinEligible && installationID != "" {
		instUUID, parseErr := uuid.Parse(installationID)
		if parseErr == nil {
			pin := sessionpin.Pin{
				SessionKey:     sessionKey,
				Role:           sessionpin.DefaultRole,
				InstallationID: instUUID,
				Provider:       decision.Provider,
				Model:          decision.Model,
				Reason:         decision.Reason,
				TurnCount:      1, // ON CONFLICT increments; only meaningful on first insert
				PinnedUntil:    time.Now().Add(pinSessionTTL),
			}
			select {
			case s.pinWriteSem <- struct{}{}:
				go func(p sessionpin.Pin) {
					defer func() { <-s.pinWriteSem }()
					if err := s.pinStore.Upsert(context.Background(), p); err != nil {
						observability.Get().Error("session pin upsert failed", "err", err)
					}
				}(pin)
			default:
				observability.Get().Debug("session pin upsert dropped: semaphore full")
			}
			if s.pinCache != nil {
				s.pinCache.Add(pinCacheKey, pin)
			}
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
		Attrs: otel.NewAttrBuilder(24).
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
	case providers.ProviderOpenAI, providers.ProviderGoogle:
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
		EnabledProviders:     s.enabledProvidersForRequest(ctx, r.Header),
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
	case providers.ProviderOpenAI, providers.ProviderGoogle:
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
