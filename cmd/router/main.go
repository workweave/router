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
	"workweave/router/internal/router/cluster"
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

	// In managed mode every provider is registered unconditionally with no
	// deployment-level API key. The proxy service is also flipped into
	// BYOK-only mode below, so a request without BYOK or client-supplied
	// credentials for the chosen provider 400s at the scorer rather than
	// silently spending the platform's API budget on customer traffic.
	// Self-hosted mode keeps the historical per-env-key gating: an operator
	// who sets ANTHROPIC_API_KEY/etc. on their own deployment expects those
	// keys to serve traffic without per-installation BYOK plumbing.
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
		logger.Info("Anthropic provider enabled (router key)", "base_url", anthropic.DefaultBaseURL)
	default:
		logger.Info("Anthropic provider enabled (client auth passthrough)", "base_url", anthropic.DefaultBaseURL)
	}

	if byokOnly {
		openaiBaseURL := config.GetOr("OPENAI_BASE_URL", openaiProvider.DefaultBaseURL)
		providerMap[providers.ProviderOpenAI] = openaiProvider.NewClient("", openaiBaseURL)
		logger.Info("OpenAI provider enabled (BYOK only)", "base_url", openaiBaseURL)
	} else if openaiKey := config.GetOr("OPENAI_API_KEY", ""); openaiKey != "" {
		openaiBaseURL := config.GetOr("OPENAI_BASE_URL", openaiProvider.DefaultBaseURL)
		providerMap[providers.ProviderOpenAI] = openaiProvider.NewClient(openaiKey, openaiBaseURL)
		logger.Info("OpenAI provider enabled", "base_url", openaiBaseURL)
	}

	if byokOnly {
		openRouterBaseURL := config.GetOr("OPENROUTER_BASE_URL", openaiCompatProvider.DefaultBaseURL)
		providerMap[providers.ProviderOpenRouter] = openaiCompatProvider.NewClient("", openRouterBaseURL)
		logger.Info("OpenRouter provider enabled (BYOK only)", "base_url", openRouterBaseURL)
	} else if openRouterKey := config.GetOr("OPENROUTER_API_KEY", ""); openRouterKey != "" {
		openRouterBaseURL := config.GetOr("OPENROUTER_BASE_URL", openaiCompatProvider.DefaultBaseURL)
		providerMap[providers.ProviderOpenRouter] = openaiCompatProvider.NewClient(openRouterKey, openRouterBaseURL)
		logger.Info("OpenRouter provider enabled", "base_url", openRouterBaseURL)
	}

	if byokOnly {
		fireworksBaseURL := config.GetOr("FIREWORKS_BASE_URL", openaiCompatProvider.FireworksBaseURL)
		providerMap[providers.ProviderFireworks] = openaiCompatProvider.NewClient("", fireworksBaseURL)
		logger.Info("Fireworks provider enabled (BYOK only)", "base_url", fireworksBaseURL)
	} else if fireworksKey := config.GetOr("FIREWORKS_API_KEY", ""); fireworksKey != "" {
		fireworksBaseURL := config.GetOr("FIREWORKS_BASE_URL", openaiCompatProvider.FireworksBaseURL)
		providerMap[providers.ProviderFireworks] = openaiCompatProvider.NewClient(fireworksKey, fireworksBaseURL)
		logger.Info("Fireworks provider enabled", "base_url", fireworksBaseURL)
	}

	if byokOnly {
		// Native Generative Language REST surface — required for multi-turn
		// tool use against Gemini 3.x preview models, whose opaque
		// thought_signature field is not exposed by the OpenAI-compat
		// surface. See router/internal/providers/google/native_client.go.
		googleBaseURL := config.GetOr("GOOGLE_BASE_URL", googleProvider.NativeBaseURL)
		providerMap[providers.ProviderGoogle] = googleProvider.NewNativeClient("", googleBaseURL)
		logger.Info("Google (Gemini) native provider enabled (BYOK only)", "base_url", googleBaseURL)
	} else if googleKey := config.GetOr("GOOGLE_API_KEY", ""); googleKey != "" {
		googleBaseURL := config.GetOr("GOOGLE_BASE_URL", googleProvider.NativeBaseURL)
		providerMap[providers.ProviderGoogle] = googleProvider.NewNativeClient(googleKey, googleBaseURL)
		logger.Info("Google (Gemini) native provider enabled", "base_url", googleBaseURL)
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
	embedLastUser := config.GetOr("ROUTER_EMBED_LAST_USER_MESSAGE", "false") == "true"
	if embedLastUser {
		logger.Info("Cluster scorer embedding the last user message (ROUTER_EMBED_LAST_USER_MESSAGE=true)")
	}
	var stickyTTL time.Duration
	if v := config.GetOr("ROUTER_STICKY_DECISION_TTL_MS", "0"); v != "0" && v != "" {
		ms, parseErr := time.ParseDuration(v + "ms")
		if parseErr != nil || ms < 0 {
			logger.Warn("Invalid ROUTER_STICKY_DECISION_TTL_MS; sticky decisions disabled", "value", v)
		} else {
			stickyTTL = ms
			logger.Info("Sticky routing decisions enabled", "ttl_ms", ms.Milliseconds())
		}
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
	hardPinProvider, hardPinModel := resolveHardPinModel(availableProviders, logger)
	if hardPinExplore {
		logger.Info("Explore sub-agent hard-pin enabled", "provider", hardPinProvider, "model", hardPinModel)
	}
	logger.Info("Hard-pin model resolved", "provider", hardPinProvider, "model", hardPinModel)

	proxySvc := proxy.NewService(rtr, providerMap, emitter, embedLastUser, stickyTTL, semanticCache, pinStore, hardPinExplore, hardPinProvider, hardPinModel, repo.Telemetry).WithByokOnly(byokOnly)

	engine := gin.New()
	engine.UnescapePathValues = true
	engine.UseRawPath = true
	engine.Use(
		observability.Middleware(),
		observability.AccessLog(),
		gin.Recovery(),
	)

	server.Register(engine, authSvc, proxySvc, deploymentMode)

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

	versions, err := cluster.ListVersions()
	if err != nil {
		return nil, fmt.Errorf("list cluster versions: %w", err)
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
				"hint", "set "+envVarForProvider(prov)+" to keep these in the routing pool",
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

// envVarForProvider returns the env var name for a provider's API key.
func envVarForProvider(provider string) string {
	switch provider {
	case providers.ProviderAnthropic:
		return "ANTHROPIC_API_KEY"
	case providers.ProviderOpenAI:
		return "OPENAI_API_KEY"
	case providers.ProviderOpenRouter:
		return "OPENROUTER_API_KEY"
	case providers.ProviderFireworks:
		return "FIREWORKS_API_KEY"
	case providers.ProviderGoogle:
		return "GOOGLE_API_KEY"
	default:
		return "<unknown provider " + provider + ">"
	}
}
