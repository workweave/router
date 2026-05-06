// Command router is the entry point for the router service. Composition root:
// the only place concrete repositories, routers, and provider clients are
// instantiated, then injected into auth.Service (identity) and proxy.Service
// (routing/dispatch).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
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
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/cache"
	"workweave/router/internal/router/cluster"
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
	// 4 conns at ~5ms/UPDATE handles ~800 RPS per instance; the auth cache
	// absorbs almost all reads, so this pool is sized for MarkUsed writes.
	cfg.MaxConns = 4
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

	repo := postgres.NewRepository(pool)

	providerMap := make(map[string]providers.Client)

	if anthropicKey := config.GetOr("ANTHROPIC_API_KEY", ""); anthropicKey != "" {
		providerMap["anthropic"] = anthropic.NewClient(anthropicKey, anthropic.DefaultBaseURL)
		logger.Info("Anthropic provider enabled", "base_url", anthropic.DefaultBaseURL)
	}

	if openaiKey := config.GetOr("OPENAI_PROVIDER_API_KEY", ""); openaiKey != "" {
		openaiBaseURL := config.GetOr("OPENAI_PROVIDER_BASE_URL", openaiProvider.DefaultBaseURL)
		providerMap["openai"] = openaiProvider.NewClient(openaiKey, openaiBaseURL)
		logger.Info("OpenAI provider enabled", "base_url", openaiBaseURL)
	}

	if googleKey := config.GetOr("GOOGLE_PROVIDER_API_KEY", ""); googleKey != "" {
		googleBaseURL := config.GetOr("GOOGLE_PROVIDER_BASE_URL", googleProvider.DefaultBaseURL)
		providerMap["google"] = googleProvider.NewClient(googleKey, googleBaseURL)
		logger.Info("Google (Gemini) provider enabled", "base_url", googleBaseURL)
	}

	if len(providerMap) == 0 {
		err := errors.New("no provider API keys configured: set at least one of ANTHROPIC_API_KEY, OPENAI_PROVIDER_API_KEY, GOOGLE_PROVIDER_API_KEY")
		logger.Error("No upstream provider available; refusing to boot", "err", err)
		panic(err)
	}

	availableProviders := make(map[string]struct{}, len(providerMap))
	for name := range providerMap {
		availableProviders[name] = struct{}{}
	}

	rtr, err := buildClusterScorer(availableProviders)
	if err != nil {
		// Fail loud at boot rather than serve a degraded heuristic.
		// Ops alerts on Cloud Run boot failures; silent degradation
		// would mask quality regressions for hours.
		logger.Error("Cluster scorer failed to build; refusing to boot", "err", err)
		panic(err)
	}
	logger.Info("Routing via cluster scorer", "embedder", "jina-v2-base-code-int8")

	cache := auth.NewLRUAPIKeyCache(10000, 50000, 5*time.Minute, 60*time.Second)
	authSvc := auth.NewService(repo.Installations, repo.APIKeys, cache, time.Now)
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

	decisionsLogPath := config.GetOr("ROUTER_DECISIONS_LOG_PATH", "")
	if decisionsLogPath == "" {
		if home, hErr := os.UserHomeDir(); hErr == nil {
			decisionsLogPath = filepath.Join(home, ".weave-router", "decisions.jsonl")
		}
	}
	if decisionsLogPath == "off" {
		decisionsLogPath = ""
	}
	decisionLog := proxy.NewDecisionLog(decisionsLogPath)
	if decisionLog != nil {
		logger.Info("Decision sidecar log enabled", "path", decisionsLogPath)
	}

	semanticCache := buildSemanticCache(rtr)
	proxySvc := proxy.NewService(rtr, providerMap, emitter, embedLastUser, stickyTTL, decisionLog, semanticCache)

	engine := gin.New()
	engine.UnescapePathValues = true
	engine.UseRawPath = true
	engine.Use(
		observability.Middleware(),
		observability.AccessLog(),
		gin.Recovery(),
	)

	devModeNoAuth := config.GetOr("ROUTER_DEV_MODE", "false") == "true"
	if devModeNoAuth {
		logger.Info("ROUTER_DEV_MODE=true; bypassing bearer auth on /v1/* (DO NOT use in production)")
	}
	server.Register(engine, authSvc, proxySvc, devModeNoAuth)

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

// envVarForProvider returns the env var name for a provider's API key.
func envVarForProvider(provider string) string {
	switch provider {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openai":
		return "OPENAI_PROVIDER_API_KEY"
	case "google":
		return "GOOGLE_PROVIDER_API_KEY"
	default:
		return "<unknown provider " + provider + ">"
	}
}
