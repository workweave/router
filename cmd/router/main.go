// Command router is the entry point for the router service. Composition root:
// the only place concrete repositories, routers, and provider clients are
// instantiated, then injected into auth.Service (identity) and proxy.Service
// (routing/dispatch).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/config"
	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/postgres"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/anthropic"
	googleProvider "workweave/router/internal/providers/google"
	openaiProvider "workweave/router/internal/providers/openai"
	openaiCompatProvider "workweave/router/internal/providers/openaicompat"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/cache"
	"workweave/router/internal/router/capability"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/router/planner"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/server"

	_ "time/tzdata"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	logger := observability.Get()

	cfg, err := pgxpool.ParseConfig(config.PostgresDSN())
	if err != nil {
		logger.Error("Failed to parse postgres DSN", "err", err)
		panic(err)
	}
	// Pin every new connection to the router schema. Defense-in-depth: even if
	// DATABASE_URL forgets `?search_path=router`, no query in the app can
	// accidentally read or write `public.*`. Migrations run through a separate
	// connection (golang-migrate) and need the same pin via the migrate URL.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO router, public")
		return err
	}
	// 6 conns covers MarkUsed writes (sparse, auth-cache absorbs most
	// reads) plus session-pin reads/writes when ROUTER_SESSION_PIN_ENABLED
	// is on (~1 read + 1 async write per pin miss). The 30s in-proc LRU
	// in proxy.Service collapses the steady-state read load. If pgxpool
	// wait p95 climbs above 1ms with the flag on, that's the
	// migrate-to-Memorystore signal — not a bigger pool.
	cfg.MaxConns = 6
	cfg.MinConns = 1
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 10 * time.Minute
	cfg.HealthCheckPeriod = 1 * time.Minute

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		logger.Error("Failed to construct postgres pool", "err", err)
		panic(err)
	}
	defer pool.Close()

	// Deployment mode gates the self-hoster dashboard + /admin/v1/* API.
	// Default is selfhosted so docker-compose / bare-binary deployments
	// "just work"; Weave-managed Cloud Run services explicitly set
	// ROUTER_DEPLOYMENT_MODE=managed to drop the redundant admin surface.
	deploymentMode := server.DeploymentMode(config.GetOr("ROUTER_DEPLOYMENT_MODE", string(server.DeploymentModeSelfHosted)))
	switch deploymentMode {
	case server.DeploymentModeSelfHosted, server.DeploymentModeManaged:
	default:
		err := fmt.Errorf("invalid ROUTER_DEPLOYMENT_MODE %q (expected %q or %q)", deploymentMode, server.DeploymentModeSelfHosted, server.DeploymentModeManaged)
		logger.Error("Refusing to boot with invalid deployment mode", "err", err)
		panic(err)
	}
	logger.Info("Router deployment mode", "mode", deploymentMode)

	// Load Tink keyset for external API key encryption. The keyset is optional:
	// if EXTERNAL_KEY_ENCRYPTION_KEY is unset the router boots with the no-op
	// encryptor and stores BYOK secrets unencrypted at rest. This is convenient
	// for self-hosters and local dev; for production deployments handling
	// customer BYOK secrets, set the keyset so ciphertext is at-rest encrypted.
	// A malformed keyset is still fail-closed — we only bypass when the var is
	// genuinely absent, never when it's set-but-broken.
	keysetJSON := config.GetOr("EXTERNAL_KEY_ENCRYPTION_KEY", "")
	var encryptor auth.Encryptor
	if keysetJSON == "" {
		logger.Warn("EXTERNAL_KEY_ENCRYPTION_KEY not set; BYOK secrets will be stored unencrypted at rest. Set EXTERNAL_KEY_ENCRYPTION_KEY to a Tink AES-256-GCM keyset to enable encryption.")
		encryptor = auth.NoOpEncryptor{}
	} else {
		encryptor, err = auth.NewTinkEncryptor(keysetJSON)
		if err != nil {
			logger.Error("Failed to create Tink encryptor from keyset", "err", err)
			os.Exit(1)
		}
	}

	repo := postgres.NewRepository(pool, encryptor)

	providerMap := make(map[string]providers.Client)
	// envKeyedProviders tracks providers whose deployment-level API key is
	// actually configured (env var present). This set feeds resolveHardPinModel
	// so compaction only lands on providers with real deployment auth — a
	// BYOK-only or passthrough-only provider would 401 on a request that
	// doesn't carry the matching credential.
	envKeyedProviders := make(map[string]struct{})
	// anthropicPassthroughEligible is true when Anthropic is registered with
	// no deployment env key but is reachable via client-supplied auth
	// (OAuth / x-api-key headers) — Claude Code's logged-in plan flow. It
	// must NOT taint envKeyedProviders since hard-pin can't rely on
	// every inbound request carrying Anthropic credentials.
	anthropicPassthroughEligible := false

	// In managed mode every provider is registered unconditionally with no
	// deployment-level API key. The proxy service is also flipped into
	// BYOK-only mode below, so a request without BYOK or client-supplied
	// credentials for the chosen provider 400s at the scorer rather than
	// silently spending the platform's API budget on customer traffic.
	byokOnly := deploymentMode == server.DeploymentModeManaged

	// Anthropic is always registered. With ANTHROPIC_API_KEY (selfhosted only)
	// the router uses its own key; otherwise the client's auth headers
	// (OAuth / x-api-key) are passed through to api.anthropic.com directly.
	anthropicKey := ""
	if !byokOnly {
		anthropicKey = config.GetOr("ANTHROPIC_API_KEY", "")
	}
	providerMap[providers.ProviderAnthropic] = anthropic.NewClient(anthropicKey, anthropic.DefaultBaseURL)
	switch {
	case byokOnly:
		logger.Info("Anthropic provider enabled (BYOK only)", "base_url", anthropic.DefaultBaseURL)
	case anthropicKey != "":
		envKeyedProviders[providers.ProviderAnthropic] = struct{}{}
		logger.Info("Anthropic provider enabled (router key)", "base_url", anthropic.DefaultBaseURL)
	default:
		// Anthropic in selfhosted with no env key serves the passthrough
		// path on client OAuth/x-api-key. It's eligible for routing but
		// must stay out of envKeyedProviders so resolveHardPinModel doesn't
		// pin compaction to a model that needs a credential the inbound
		// request might not carry.
		anthropicPassthroughEligible = true
		logger.Info("Anthropic provider enabled (client auth passthrough)", "base_url", anthropic.DefaultBaseURL)
	}

	{
		openaiBaseURL := config.GetOr("OPENAI_BASE_URL", openaiProvider.DefaultBaseURL)
		openaiKey := ""
		if !byokOnly {
			openaiKey = config.GetOr("OPENAI_API_KEY", "")
		}
		providerMap[providers.ProviderOpenAI] = openaiProvider.NewClient(openaiKey, openaiBaseURL)
		switch {
		case byokOnly:
			logger.Info("OpenAI provider enabled (BYOK only)", "base_url", openaiBaseURL)
		case openaiKey != "":
			envKeyedProviders[providers.ProviderOpenAI] = struct{}{}
			logger.Info("OpenAI provider enabled", "base_url", openaiBaseURL)
		default:
			logger.Info("OpenAI provider registered (BYOK only — set OPENAI_API_KEY for deployment-level use)", "base_url", openaiBaseURL)
		}
	}

	{
		openRouterBaseURL := config.GetOr("OPENROUTER_BASE_URL", openaiCompatProvider.DefaultBaseURL)
		openRouterKey := ""
		if !byokOnly {
			openRouterKey = config.GetOr("OPENROUTER_API_KEY", "")
		}
		providerMap[providers.ProviderOpenRouter] = openaiCompatProvider.NewClient(openRouterKey, openRouterBaseURL)
		switch {
		case byokOnly:
			logger.Info("OpenRouter provider enabled (BYOK only)", "base_url", openRouterBaseURL)
		case openRouterKey != "":
			envKeyedProviders[providers.ProviderOpenRouter] = struct{}{}
			logger.Info("OpenRouter provider enabled", "base_url", openRouterBaseURL)
		default:
			logger.Info("OpenRouter provider registered (BYOK only — set OPENROUTER_API_KEY for deployment-level use)", "base_url", openRouterBaseURL)
		}
	}

	{
		fireworksBaseURL := config.GetOr("FIREWORKS_BASE_URL", openaiCompatProvider.FireworksBaseURL)
		fireworksKey := ""
		if !byokOnly {
			fireworksKey = config.GetOr("FIREWORKS_API_KEY", "")
		}
		providerMap[providers.ProviderFireworks] = openaiCompatProvider.NewClient(fireworksKey, fireworksBaseURL)
		switch {
		case byokOnly:
			logger.Info("Fireworks provider enabled (BYOK only)", "base_url", fireworksBaseURL)
		case fireworksKey != "":
			envKeyedProviders[providers.ProviderFireworks] = struct{}{}
			logger.Info("Fireworks provider enabled", "base_url", fireworksBaseURL)
		default:
			logger.Info("Fireworks provider registered (BYOK only — set FIREWORKS_API_KEY for deployment-level use)", "base_url", fireworksBaseURL)
		}
	}

	{
		// Native Generative Language REST surface — required for multi-turn
		// tool use against Gemini 3.x preview models, whose opaque
		// thought_signature field is not exposed by the OpenAI-compat
		// surface. See router/internal/providers/google/native_client.go.
		googleBaseURL := config.GetOr("GOOGLE_BASE_URL", googleProvider.NativeBaseURL)
		googleKey := ""
		if !byokOnly {
			googleKey = config.GetOr("GOOGLE_API_KEY", "")
		}
		providerMap[providers.ProviderGoogle] = googleProvider.NewNativeClient(googleKey, googleBaseURL)
		switch {
		case byokOnly:
			logger.Info("Google (Gemini) native provider enabled (BYOK only)", "base_url", googleBaseURL)
		case googleKey != "":
			envKeyedProviders[providers.ProviderGoogle] = struct{}{}
			logger.Info("Google (Gemini) native provider enabled", "base_url", googleBaseURL)
		default:
			logger.Info("Google (Gemini) native provider registered (BYOK only — set GOOGLE_API_KEY for deployment-level use)", "base_url", googleBaseURL)
		}
	}

	availableProviders := make(map[string]struct{}, len(providerMap))
	for name := range providerMap {
		availableProviders[name] = struct{}{}
	}

	rtr, err := buildClusterScorer(availableProviders)
	if err != nil {
		// Ops alerts on Cloud Run boot failures; silent degradation would mask quality regressions.
		logger.Error("Cluster scorer failed to build; refusing to boot", "err", err)
		panic(err)
	}
	logger.Info("Routing via cluster scorer", "embedder", "jina-v2-base-code-int8")

	cache := auth.NewLRUAPIKeyCache(10000, 50000, 5*time.Minute, 60*time.Second)
	userCache := auth.NewLRUUserCache(50000, 10*time.Minute)
	authSvc := auth.NewService(repo.Installations, repo.APIKeys, repo.ExternalAPIKeys, repo.Users, cache, userCache, time.Now).WithEncryptor(encryptor)

	// Admin dashboard password. In managed mode the dashboard is not mounted
	// at all so the password is irrelevant. In selfhosted mode, fall back to
	// "admin" when unset and warn — operators that care about securing the
	// dashboard should always set ROUTER_ADMIN_PASSWORD explicitly.
	if deploymentMode == server.DeploymentModeSelfHosted {
		adminPassword := config.GetOr("ROUTER_ADMIN_PASSWORD", "")
		if adminPassword == "" {
			adminPassword = "admin"
			logger.Warn("ROUTER_ADMIN_PASSWORD not set; using default 'admin'. Set ROUTER_ADMIN_PASSWORD to secure the dashboard.")
		}
		authSvc.WithAdminPassword(adminPassword)
	}
	embedOnlyUser := config.GetOr("ROUTER_EMBED_ONLY_USER_MESSAGE", "true") == "true"
	if embedOnlyUser {
		logger.Info("Cluster scorer embedding user-role text only (ROUTER_EMBED_ONLY_USER_MESSAGE=true)")
	} else {
		logger.Info("Cluster scorer embedding concatenated stream (ROUTER_EMBED_ONLY_USER_MESSAGE=false)")
	}
	emitter, err := buildOtelEmitter()
	if err != nil {
		logger.Error("Failed to create OTel emitter", "err", err)
		panic(err)
	}

	semanticCache := buildSemanticCache(rtr)

	// Default-on: the cluster scorer's α-blend is baked at training time on
	// per-prompt cost numbers that don't account for prompt-cache continuity.
	// Without session pinning, mid-conversation provider switches discard the
	// cache prefix (Anthropic's cache is keyed per-model) and pay the cache-
	// write penalty on the new model — over a 30-turn agentic trajectory that
	// dominates routing-decision savings. Until cost models reflect cache-warm
	// economics, leaving pinning off makes the router optimize a number that
	// doesn't match production.
	//
	// Set ROUTER_SESSION_PIN_ENABLED=false to opt out (kill switch).
	var pinStore sessionpin.Store
	if config.GetOr("ROUTER_SESSION_PIN_ENABLED", "true") == "true" {
		pinStore = postgres.NewSessionPinRepo(pool)
		go runSessionPinSweep(context.Background(), pinStore)
		logger.Info("Session pin store enabled (sliding 1h TTL, hourly sweep)")
	}

	hardPinExplore := config.GetOr("ROUTER_HARD_PIN_EXPLORE", "false") == "true"
	// Pin to a model whose provider has actual deployment-level auth — a
	// BYOK-only registered provider would 401 here since hard-pin compaction
	// runs on every installation, including ones without their own BYOK.
	// Anthropic-passthrough is excluded for the same reason: an inbound
	// request that doesn't carry Anthropic client headers would 401.
	// In selfhosted mode the boot-time hard-pin is computed over providers
	// with deployment-level env keys — those are the only ones a hard-pin
	// can rely on across every installation. In managed/byokOnly mode there
	// is no provider with deployment auth, so any boot-time hard-pin would
	// 401 for installations that didn't BYOK that exact provider; we resolve
	// hard-pin per-request from the cluster bundle below instead.
	hardPinProvider, hardPinModel := resolveHardPinModel(envKeyedProviders, logger)
	if hardPinExplore {
		logger.Info("Explore sub-agent hard-pin enabled", "provider", hardPinProvider, "model", hardPinModel)
	}
	logger.Info("Hard-pin model resolved", "provider", hardPinProvider, "model", hardPinModel, "byok_only", byokOnly)

	// Per-request hard-pin resolver for byokOnly deployments. Loads the
	// default cluster bundle once and closes over its metadata/registry; the
	// resolver is then called from the proxy with the request's enabled-
	// providers set so compaction lands on the cheapest model the request
	// can actually authenticate to. Selfhosted mode leaves the resolver nil
	// — its boot-time hardPin{Provider,Model} is already correct.
	var hardPinResolver func(map[string]struct{}) (string, string, bool)
	if byokOnly {
		reqVersion := config.GetOr("ROUTER_CLUSTER_VERSION", cluster.LatestVersion)
		if version, vErr := cluster.ResolveVersion(reqVersion); vErr == nil {
			if bundle, bErr := cluster.LoadBundle(version); bErr == nil {
				meta, registry := bundle.Metadata, bundle.Registry
				hardPinResolver = func(enabled map[string]struct{}) (string, string, bool) {
					return cluster.CheapestModel(meta, registry, enabled)
				}
				logger.Info("Hard-pin resolver wired for byokOnly mode (per-request cheapest model from cluster bundle)", "version", version)
			} else {
				logger.Warn("Hard-pin resolver disabled: cluster bundle failed to load; byokOnly hard-pin will fall back to boot-time defaults", "err", bErr, "version", version)
			}
		} else {
			logger.Warn("Hard-pin resolver disabled: could not resolve cluster version; byokOnly hard-pin will fall back to boot-time defaults", "err", vErr)
		}
	}

	// Default-eligible set for proxy.Service: env-keyed providers + the
	// Anthropic passthrough path. BYOK and client-supplied credentials add
	// to this set per-request inside enabledProvidersForRequest.
	deploymentEligible := make(map[string]struct{}, len(envKeyedProviders)+1)
	for p := range envKeyedProviders {
		deploymentEligible[p] = struct{}{}
	}
	if anthropicPassthroughEligible {
		deploymentEligible[providers.ProviderAnthropic] = struct{}{}
	}

	// Planner + handover config (Prism-style cache-aware routing). Defaults
	// keep the kill switch on, $0.001 EV threshold, and a 3-turn horizon —
	// each can be overridden per deployment. The summarizer is only wired
	// when its provider client is registered; otherwise the orchestrator
	// falls back to handover.TrimLastN on switch turns.
	plannerEnabled := config.GetOr("ROUTER_PLANNER_ENABLED", "true") == "true"
	plannerCfg := planner.EVConfig{
		ThresholdUSD:           parseEnvFloat("ROUTER_SWITCH_EV_THRESHOLD_USD", proxy.DefaultPlannerThresholdUSD),
		ExpectedRemainingTurns: parseEnvInt("ROUTER_SWITCH_EXPECTED_REMAINING_TURNS", proxy.DefaultPlannerExpectedRemainingTurns),
		TierUpgradeEnabled:     config.GetOr("ROUTER_SWITCH_TIER_UPGRADE_ENABLED", boolDefault(proxy.DefaultPlannerTierUpgradeEnabled)) == "true",
	}
	handoverProviderName := config.GetOr("ROUTER_HANDOVER_PROVIDER", providers.ProviderAnthropic)
	handoverModel := config.GetOr("ROUTER_HANDOVER_MODEL", proxy.DefaultHandoverModel)
	handoverTimeout := parseEnvDurationMs("ROUTER_HANDOVER_TIMEOUT_MS", proxy.DefaultHandoverTimeout)
	// summarizer stays as the interface type so an unregistered provider
	// leaves it as a true nil interface — passing a typed-nil *ProviderSummarizer
	// through WithSummarizer would defeat the orchestrator's `!= nil` check.
	var summarizer handover.Summarizer
	if client, ok := providerMap[handoverProviderName]; ok {
		summarizer = proxy.NewProviderSummarizer(client, handoverModel, handoverTimeout)
		logger.Info("Handover summarizer wired", "provider", handoverProviderName, "model", handoverModel, "timeout_ms", handoverTimeout.Milliseconds())
	} else {
		logger.Info("Handover summarizer disabled (provider not registered); switch turns will fall back to TrimLastN", "requested_provider", handoverProviderName)
	}

	// Available-models set lets the planner force a switch when a pinned
	// model's provider has been removed. Sourced from the default cluster
	// bundle's deployed_models filtered by registered providers — same
	// logic as resolveHardPinModel, so a missing/unloadable bundle leaves
	// it nil (planner then treats every pin as still routable).
	availableModels := resolveAvailableModels(availableProviders, logger)

	proxySvc := proxy.NewService(rtr, providerMap, emitter, embedOnlyUser, semanticCache, pinStore, hardPinExplore, hardPinProvider, hardPinModel, repo.Telemetry).
		WithByokOnly(byokOnly).
		WithDeploymentKeyedProviders(deploymentEligible).
		WithHardPinResolver(hardPinResolver).
		WithPlannerEnabled(plannerEnabled).
		WithPlanner(plannerCfg).
		WithSummarizer(summarizer).
		WithAvailableModels(availableModels).
		WithDefaultBaselineModel(resolveDefaultBaselineModel())
	logger.Info("Planner configured", "enabled", plannerEnabled, "threshold_usd", plannerCfg.ThresholdUSD, "expected_remaining_turns", plannerCfg.ExpectedRemainingTurns, "tier_upgrade_enabled", plannerCfg.TierUpgradeEnabled, "available_models_count", len(availableModels))

	// Fail loud if a deployed model is missing from the tier table;
	// TierUnknown would silently disable the guard for that pair.
	if plannerCfg.TierUpgradeEnabled && len(availableModels) > 0 {
		deployed := make([]string, 0, len(availableModels))
		for m := range availableModels {
			deployed = append(deployed, m)
		}
		if err := capability.Validate(deployed); err != nil {
			logger.Error("Capability tier table incomplete; refusing to start with tier guard enabled", "err", err)
			panic(err)
		}
	}

	// ROUTER_EXCLUDED_MODELS pins a deployment-wide model exclusion list,
	// overriding per-installation DB state. Empty / unset → DB takes over.
	if excludedRaw := strings.TrimSpace(config.GetOr("ROUTER_EXCLUDED_MODELS", "")); excludedRaw != "" {
		parts := strings.Split(excludedRaw, ",")
		cleaned := make([]string, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				cleaned = append(cleaned, trimmed)
			}
		}
		proxySvc = proxySvc.WithExcludedModelsOverride(cleaned)
		logger.Info("Model exclusion override active", "excluded_models", cleaned)
	}

	engine := gin.New()
	engine.UnescapePathValues = true
	engine.UseRawPath = true
	engine.Use(
		observability.Middleware(),
		observability.AccessLog(),
		gin.Recovery(),
	)

	// Cast the router to *cluster.Multiversion so the admin model-selection
	// handler can surface the universe of deployed models. The fallback nil
	// keeps non-cluster routers (heuristic dev override, etc.) bootable.
	deployedModels, _ := rtr.(*cluster.Multiversion)
	server.Register(engine, authSvc, proxySvc, deployedModels, deploymentMode)

	srv := &http.Server{
		Addr:    ":" + config.GetOr("PORT", "8080"),
		Handler: engine,
		// Slowloris primitive. ReadTimeout/WriteTimeout would break
		// streaming bodies/responses, so per-route gin timeouts handle
		// non-streaming routes and this only bounds header read time.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	providerNames := make([]string, 0, len(providerMap))
	for name := range providerMap {
		providerNames = append(providerNames, name)
	}
	logger.Info("Starting Weave router", "providers", providerNames, "addr", srv.Addr)

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		logger.Error("Server exited with error", "err", err)
		return
	case sig := <-stop:
		logger.Info("Received shutdown signal; draining", "signal", sig.String())
	}

	// Cloud Run gives 10s between SIGTERM and SIGKILL; 8s leaves margin
	// for log flush and pool close after Shutdown returns.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("Graceful shutdown failed", "err", err)
	}
	// Emitter gets a dedicated drain budget so it can flush remaining spans
	// even if the server consumed the full shutdown timeout above.
	emitterCtx, emitterCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer emitterCancel()
	if err := emitter.Shutdown(emitterCtx); err != nil {
		logger.Warn("OTel emitter shutdown incomplete", "err", err)
	}
}

// parseOtelHeaders parses a comma-separated key=value string into a map.
func parseOtelHeaders(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && k != "" {
			out[k] = v
		}
	}
	return out
}

// buildClusterScorer constructs the cluster.Multiversion router. One ONNX
// embedder is shared across all artifact versions. Returns an error on any
// artifact, embedder, or warmup failure; the caller panics so the boot
// fails loud rather than silently degrading to a default model.
func buildClusterScorer(availableProviders map[string]struct{}) (router.Router, error) {
	logger := observability.Get()

	requestedVersion := config.GetOr("ROUTER_CLUSTER_VERSION", cluster.LatestVersion)
	defaultVersion, err := cluster.ResolveVersion(requestedVersion)
	if err != nil {
		return nil, fmt.Errorf("resolve cluster version %q: %w", requestedVersion, err)
	}

	// Default to building only the served version. Staging/eval deployments
	// opt in to building every committed bundle by setting
	// ROUTER_CLUSTER_BUILD_ALL_VERSIONS=true; that powers the eval harness's
	// per-request x-weave-cluster-version header A/B. Prod doesn't need the
	// other bundles in memory and exposing them via header override is a
	// foot-gun.
	buildAll := strings.EqualFold(config.GetOr("ROUTER_CLUSTER_BUILD_ALL_VERSIONS", "false"), "true")
	var versions []string
	if buildAll {
		versions, err = cluster.ListVersions()
		if err != nil {
			return nil, fmt.Errorf("list cluster versions: %w", err)
		}
	} else {
		versions = []string{defaultVersion}
	}

	embedder, err := cluster.NewEmbedder()
	if err != nil {
		return nil, err
	}

	cfg := cluster.DefaultConfig()
	if v := config.GetOr("ROUTER_CLUSTER_EMBED_TIMEOUT_MS", ""); v != "" {
		ms, parseErr := time.ParseDuration(v + "ms")
		if parseErr != nil || ms <= 0 {
			logger.Warn("Invalid ROUTER_CLUSTER_EMBED_TIMEOUT_MS; using default", "value", v, "default_ms", cfg.EmbedTimeout.Milliseconds())
		} else {
			cfg.EmbedTimeout = ms
			logger.Info("Cluster embed timeout overridden", "embed_timeout_ms", ms.Milliseconds())
		}
	}
	scorers := make(map[string]*cluster.Scorer, len(versions))
	for _, v := range versions {
		bundle, err := cluster.LoadBundle(v)
		if err != nil {
			_ = embedder.Close()
			return nil, fmt.Errorf("load bundle %s: %w", v, err)
		}

		missingProviders := map[string][]string{}
		for _, e := range bundle.Registry.DeployedModels {
			if _, ok := availableProviders[e.Provider]; !ok {
				missingProviders[e.Provider] = append(missingProviders[e.Provider], e.Model)
			}
		}
		for prov, models := range missingProviders {
			logger.Warn(
				"Cluster artifact references unregistered provider; affected models will be excluded from argmax",
				"cluster_version", v,
				"missing_provider", prov,
				"affected_models", models,
				"hint", "set "+envVarHint(prov)+" to keep these in the routing pool",
			)
		}

		scorer, err := cluster.NewScorer(bundle, cfg, embedder, availableProviders)
		if err != nil {
			logger.Warn("Cluster scorer version skipped", "cluster_version", v, "err", err)
			continue
		}
		scorers[v] = scorer
		logger.Info("Cluster scorer version built", "cluster_version", v, "models", bundle.Registry.Models())
	}

	if _, ok := scorers[defaultVersion]; !ok {
		_ = embedder.Close()
		return nil, fmt.Errorf("default cluster version %q failed to build (likely no registered provider covers its deployed_models); set ROUTER_CLUSTER_VERSION to a version that does, or register the missing provider key", defaultVersion)
	}

	multi, err := cluster.NewMultiversion(defaultVersion, scorers)
	if err != nil {
		_ = embedder.Close()
		return nil, fmt.Errorf("build multiversion router: %w", err)
	}
	logger.Info(
		"Cluster multiversion router ready",
		"default_version", defaultVersion,
		"built_versions", multi.Built(),
		"requested_version", requestedVersion,
		"build_all_versions", buildAll,
	)

	// Warmup: burn the lazy ONNX graph-optimization cost at boot.
	type warmupResult struct {
		err error
	}
	warmupDone := make(chan warmupResult, 1)
	go func() {
		_, err := embedder.Embed(context.Background(), "warmup")
		warmupDone <- warmupResult{err: err}
	}()
	select {
	case res := <-warmupDone:
		if res.err != nil {
			_ = embedder.Close()
			return nil, res.err
		}
	case <-time.After(5 * time.Second):
		// Drain the goroutine before closing to avoid use-after-free.
		go func() {
			<-warmupDone
			_ = embedder.Close()
		}()
		return nil, fmt.Errorf("cluster embedder warmup timed out after 5s")
	}
	logger.Info("Cluster embedder warmed", "embedder", "jina-v2-base-code-int8", "embed_dim", cluster.EmbedDim)

	return multi, nil
}

// buildOtelEmitter constructs the OTel span emitter from environment
// variables. Returns (nil, nil) when OTEL_EXPORTER_OTLP_ENDPOINT is unset.
func buildOtelEmitter() (*otel.Emitter, error) {
	logger := observability.Get()

	endpoint := config.GetOr("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if endpoint == "" {
		return nil, nil
	}

	cfg := otel.EmitterConfig{
		Endpoint:      endpoint,
		Headers:       parseOtelHeaders(config.GetOr("OTEL_EXPORTER_OTLP_HEADERS", "")),
		ServiceName:   config.GetOr("OTEL_SERVICE_NAME", "router"),
		ResourceAttrs: parseOtelHeaders(config.GetOr("OTEL_RESOURCE_ATTRIBUTES", "")),
		Workers:       parseEnvInt("OTEL_EXPORT_WORKERS", 2),
		QueueSize:     parseEnvInt("OTEL_BSP_MAX_QUEUE_SIZE", 1000),
		BatchSize:     parseEnvInt("OTEL_BSP_MAX_EXPORT_BATCH_SIZE", 50),
		FlushInterval: parseEnvDurationMs("OTEL_BSP_SCHEDULE_DELAY", 500*time.Millisecond),
		ExportTimeout: parseEnvDurationMs("OTEL_EXPORTER_OTLP_TIMEOUT", 10*time.Second),
	}

	emitter, err := otel.NewEmitter(cfg)
	if err != nil {
		return nil, err
	}

	safeEndpoint := endpoint
	if u, err := url.Parse(endpoint); err == nil {
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		safeEndpoint = u.String()
	}
	logger.Info("OTel export enabled",
		"endpoint", safeEndpoint,
		"workers", cfg.Workers,
		"queue_size", cfg.QueueSize,
		"batch_size", cfg.BatchSize,
		"flush_interval_ms", cfg.FlushInterval.Milliseconds(),
		"export_timeout_ms", cfg.ExportTimeout.Milliseconds(),
	)
	return emitter, nil
}

// parseEnvInt reads an env var as a positive integer. Returns fallback when
// the var is unset, empty, or unparseable. Logs a warning on bad values.
func parseEnvInt(key string, fallback int) int {
	raw := config.GetOr(key, "")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		observability.Get().Warn("Invalid env var; using default", "key", key, "value", raw, "default", fallback)
		return fallback
	}
	return n
}

// parseEnvFloat reads an env var as a float64. Returns fallback when the
// var is unset, empty, or unparseable. Logs a warning only on parse
// failure. Zero and negative values are valid: ROUTER_SWITCH_EV_THRESHOLD_USD
// uses a USD threshold in `expectedSavings - evictionCost > threshold`, so
// operators set it to <= 0 to make the planner switch aggressively (the PR
// test plan documents `-1` as the force-switch knob).
func parseEnvFloat(key string, fallback float64) float64 {
	raw := config.GetOr(key, "")
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		observability.Get().Warn("Invalid env var; using default", "key", key, "value", raw, "default", fallback)
		return fallback
	}
	return v
}

// boolDefault renders a bool default for config.GetOr on bool envs.
func boolDefault(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// parseEnvDurationMs reads an env var as a millisecond integer and returns
// it as a time.Duration. Returns fallback when unset, empty, or unparseable.
func parseEnvDurationMs(key string, fallback time.Duration) time.Duration {
	raw := config.GetOr(key, "")
	if raw == "" {
		return fallback
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		observability.Get().Warn("Invalid env var; using default", "key", key, "value", raw, "default_ms", fallback.Milliseconds())
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

// buildSemanticCache constructs the cross-request semantic cache, or
// returns nil when disabled. Wiring honors:
//
//	ROUTER_SEMANTIC_CACHE_ENABLED — "false" disables; default enabled.
//	ROUTER_SEMANTIC_CACHE_TTL_SEC — per-entry TTL in seconds (default 3600).
//	ROUTER_SEMANTIC_CACHE_BUCKET  — per-(installation, format, cluster)
//	                                 LRU capacity (default 1024).
//
// Per-cluster cosine thresholds are pulled from the default version's
// metadata.yaml `cache_config` block. Other built versions reuse the
// default's thresholds at runtime — keeping the cache config tied to
// the default version means promotion (flipping `artifacts/latest`)
// also flips cache thresholds atomically.
func buildSemanticCache(rtr router.Router) *cache.Cache {
	logger := observability.Get()
	if config.GetOr("ROUTER_SEMANTIC_CACHE_ENABLED", "true") != "true" {
		logger.Info("Semantic cache disabled (ROUTER_SEMANTIC_CACHE_ENABLED=false)")
		return nil
	}
	multi, ok := rtr.(*cluster.Multiversion)
	if !ok {
		// Defensive: main.go always wires *cluster.Multiversion now that
		// the heuristic fallback is removed, but keep the guard so a
		// future refactor that introduces another router shape doesn't
		// silently disable the cache.
		logger.Warn("Semantic cache disabled: router is not a cluster.Multiversion")
		return nil
	}
	defaultScorer, ok := multi.Versions[multi.Default]
	if !ok {
		// Defensive: NewMultiversion guarantees the default is built.
		logger.Warn("Semantic cache disabled: default cluster scorer not found", "default_version", multi.Default)
		return nil
	}
	perCluster, defaultThreshold := defaultScorer.CacheThresholds()

	cfg := cache.DefaultConfig()
	cfg.PerClusterThreshold = perCluster
	if defaultThreshold > 0 {
		cfg.DefaultThreshold = defaultThreshold
	}
	if v := config.GetOr("ROUTER_SEMANTIC_CACHE_TTL_SEC", ""); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs <= 0 {
			logger.Warn("Invalid ROUTER_SEMANTIC_CACHE_TTL_SEC; using default", "value", v, "default_sec", int(cfg.TTL.Seconds()))
		} else {
			cfg.TTL = time.Duration(secs) * time.Second
		}
	}
	if v := config.GetOr("ROUTER_SEMANTIC_CACHE_BUCKET", ""); v != "" {
		size, err := strconv.Atoi(v)
		if err != nil || size <= 0 {
			logger.Warn("Invalid ROUTER_SEMANTIC_CACHE_BUCKET; using default", "value", v, "default", cfg.BucketSize)
		} else {
			cfg.BucketSize = size
		}
	}
	logger.Info(
		"Semantic cache enabled",
		"default_threshold", cfg.DefaultThreshold,
		"per_cluster_overrides", len(cfg.PerClusterThreshold),
		"bucket_size", cfg.BucketSize,
		"ttl_sec", int(cfg.TTL.Seconds()),
		"max_body_kib", cfg.MaxBodyBytes/1024,
		"version", multi.Default,
	)
	return cache.New(cfg)
}

// runSessionPinSweep deletes pins that have been expired for >24h on
// an hourly cadence. Bounded: row count is one per active session.
// Runs under context.Background() so the sweep keeps draining stale
// rows even during a graceful shutdown — the work is idempotent and
// short.
func runSessionPinSweep(ctx context.Context, store sessionpin.Store) {
	logger := observability.Get()
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := store.SweepExpired(sweepCtx); err != nil {
				logger.Error("Session pin sweep failed", "err", err)
			}
			cancel()
		}
	}
}

// defaultHardPinProvider and defaultHardPinModel are the fallback (provider,
// model) used by resolveHardPinModel when the cluster bundle can't be loaded.
const (
	defaultHardPinProvider = providers.ProviderAnthropic
	defaultHardPinModel    = "claude-haiku-4-5"
)

// resolveDefaultBaselineModel returns the cost-comparison baseline used when
// the inbound RequestedModel has no pricing entry. Unset → default
// claude-sonnet-4-5; set to empty → no substitution. config.GetOr collapses
// the empty-set and unset cases, so we use os.LookupEnv directly to preserve
// the "" → disable contract documented in .env.example.
func resolveDefaultBaselineModel() string {
	v, ok := os.LookupEnv("ROUTER_DEFAULT_BASELINE_MODEL")
	if !ok {
		return "claude-sonnet-4-5"
	}
	return strings.TrimSpace(v)
}

// resolveHardPinModel returns the (provider, model) to use for compaction and
// Explore hard-pins. Operator override wins; otherwise the cheapest model in
// the default artifact bundle among available providers is selected.
// Falls back to (defaultHardPinProvider, defaultHardPinModel) when no bundle is loadable.
func resolveHardPinModel(available map[string]struct{}, logger *slog.Logger) (provider, model string) {
	if m := config.GetOr("ROUTER_HARD_PIN_MODEL", ""); m != "" {
		p := config.GetOr("ROUTER_HARD_PIN_PROVIDER", defaultHardPinProvider)
		return p, m
	}

	reqVersion := config.GetOr("ROUTER_CLUSTER_VERSION", cluster.LatestVersion)
	defaultVersion, err := cluster.ResolveVersion(reqVersion)
	if err != nil {
		logger.Warn("Hard-pin model: could not resolve cluster version; using default", "err", err, "default_model", defaultHardPinModel)
		return defaultHardPinProvider, defaultHardPinModel
	}
	bundle, err := cluster.LoadBundle(defaultVersion)
	if err != nil {
		logger.Warn("Hard-pin model: could not load bundle; using default", "err", err, "default_model", defaultHardPinModel)
		return defaultHardPinProvider, defaultHardPinModel
	}
	p, m, ok := cluster.CheapestModel(bundle.Metadata, bundle.Registry, available)
	if !ok {
		logger.Warn("Hard-pin model: no cost-annotated model found for available providers; using default", "default_model", defaultHardPinModel)
		return defaultHardPinProvider, defaultHardPinModel
	}
	return p, m
}

// resolveAvailableModels returns the boot-time set of routable model names,
// derived from the default cluster bundle's deployed_models intersected with
// the registered provider set. Returns nil on any load failure — the planner
// then treats every pin as still routable (best-effort behavior; matches
// resolveHardPinModel's fallback posture).
func resolveAvailableModels(availableProviders map[string]struct{}, logger *slog.Logger) map[string]struct{} {
	reqVersion := config.GetOr("ROUTER_CLUSTER_VERSION", cluster.LatestVersion)
	defaultVersion, err := cluster.ResolveVersion(reqVersion)
	if err != nil {
		logger.Warn("Available-models set: could not resolve cluster version; planner will treat every pin as routable", "err", err)
		return nil
	}
	bundle, err := cluster.LoadBundle(defaultVersion)
	if err != nil {
		logger.Warn("Available-models set: could not load bundle; planner will treat every pin as routable", "err", err)
		return nil
	}
	out := make(map[string]struct{}, len(bundle.Registry.DeployedModels))
	for _, e := range bundle.Registry.DeployedModels {
		if _, ok := availableProviders[e.Provider]; !ok {
			continue
		}
		out[e.Model] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// envVarHint returns the env var name for a provider's API key, formatted
// for log warnings. Wraps providers.APIKeyEnvVar so the "unknown provider"
// fallback is readable in operator-facing logs.
func envVarHint(provider string) string {
	if v := providers.APIKeyEnvVar(provider); v != "" {
		return v
	}
	return "<unknown provider " + provider + ">"
}
