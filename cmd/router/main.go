// Command router is the entry point for the router service. Composition root:
// the only place concrete repositories, routers, and provider clients are
// instantiated, then injected into auth.Service (identity) and proxy.Service
// (routing/dispatch).
package main

import (
	"context"
	"crypto/rand"
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
	"workweave/router/internal/billing"
	"workweave/router/internal/config"
	"workweave/router/internal/feedback"
	"workweave/router/internal/observability"
	"workweave/router/internal/observability/apm"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/postgres"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/anthropic"
	googleProvider "workweave/router/internal/providers/google"
	openaiProvider "workweave/router/internal/providers/openai"
	openaiCompatProvider "workweave/router/internal/providers/openaicompat"
	"workweave/router/internal/proxy"
	"workweave/router/internal/proxy/usage"
	routerpubsub "workweave/router/internal/pubsub"
	"workweave/router/internal/router"
	"workweave/router/internal/router/bandit"
	"workweave/router/internal/router/banditexplore"
	"workweave/router/internal/router/cache"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/router/planner"
	"workweave/router/internal/router/rl"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/server"
	"workweave/router/internal/translate"

	_ "time/tzdata"

	gcppubsub "cloud.google.com/go/pubsub/v2"
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
		err := fmt.Errorf("Invalid ROUTER_DEPLOYMENT_MODE %q (expected %q or %q)", deploymentMode, server.DeploymentModeSelfHosted, server.DeploymentModeManaged)
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
	// openaiPassthroughEligible mirrors anthropicPassthroughEligible for the
	// OpenAI provider — Codex's logged-in plan flow. Same invariant: must NOT
	// taint envKeyedProviders.
	openaiPassthroughEligible := false

	// Credit billing service: wired by default in managed mode. The
	// boot-time health check exists only to surface the "tables actually
	// missing" rollback path — if the check errors (timeout, transient
	// pool unreadiness on a cold replica), we default to billing-enabled
	// rather than silently falling into BYOK-only mode, which would 400
	// every request for "no provider keys available". Self-hosted
	// deployments never wire billing.
	var billingSvc *billing.Service
	if deploymentMode == server.DeploymentModeManaged {
		billingRepo := postgres.NewBillingRepo(pool)
		bootCtx, bootCancel := context.WithTimeout(context.Background(), 5*time.Second)
		tablesExist, billingCheckErr := billingRepo.BillingTablesExist(bootCtx)
		bootCancel()
		switch {
		case billingCheckErr != nil:
			logger.Warn("Boot billing health check errored; defaulting to billing-enabled in managed mode", "err", billingCheckErr)
			billingSvc = billing.NewService(billingRepo)
		case !tablesExist:
			logger.Warn("Billing tables missing from router schema; staying in BYOK-only mode. Apply db migration 0006_credit_billing to enable billing.")
		default:
			billingSvc = billing.NewService(billingRepo)
			logger.Info("Router billing enabled", "min_balance_usd_micros", billing.MinBalanceMicros)
		}
	}

	// In managed mode without billing we keep BYOK-only behavior (zero
	// active customers today, but we don't want to silently spend
	// platform-key budget if billing fails to wire). With billing on, we
	// flip to platform-key mode: the balance check gates spending and the
	// debit hook books each call. Self-hosted stays at byokOnly=false so
	// platform env keys work the way operators expect.
	byokOnly := deploymentMode == server.DeploymentModeManaged && billingSvc == nil

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
		// A caller's Codex (ChatGPT) subscription reroutes OpenAI turns to the
		// Codex backend (chatgpt.com/backend-api/codex) over the Responses API;
		// that switch lives in the OpenAI client, keyed off the resolved
		// subscription credential — no separate wiring here.
		providerMap[providers.ProviderOpenAI] = openaiProvider.NewClient(openaiKey, openaiBaseURL)
		switch {
		case byokOnly:
			logger.Info("OpenAI provider enabled (BYOK only)", "base_url", openaiBaseURL)
		case openaiKey != "":
			envKeyedProviders[providers.ProviderOpenAI] = struct{}{}
			logger.Info("OpenAI provider enabled", "base_url", openaiBaseURL)
		default:
			// OpenAI in selfhosted with no env key serves the passthrough
			// path on client Authorization headers (Codex plan flow).
			openaiPassthroughEligible = true
			logger.Info("OpenAI provider enabled (client auth passthrough)", "base_url", openaiBaseURL)
		}
	}

	{
		openRouterBaseURL := config.GetOr("OPENROUTER_BASE_URL", openaiCompatProvider.DefaultBaseURL)
		// Managed deploys don't use OpenRouter as a platform source by default:
		// we don't read our own OPENROUTER_API_KEY, so it never lands in
		// envKeyedProviders and the scorer + failover chain won't route platform
		// traffic to it. A self-hoster running in managed mode can opt back in
		// with ROUTER_OPENROUTER_PLATFORM_ENABLED=true; self-hosted mode reads the
		// key unconditionally as before. Either way the provider stays registered
		// so a caller's BYOK OpenRouter key still dispatches.
		openRouterPlatformEnabled := deploymentMode == server.DeploymentModeSelfHosted ||
			config.GetOr("ROUTER_OPENROUTER_PLATFORM_ENABLED", "false") == "true"
		openRouterKey := ""
		if !byokOnly && openRouterPlatformEnabled {
			openRouterKey = config.GetOr("OPENROUTER_API_KEY", "")
		}
		providerMap[providers.ProviderOpenRouter] = openaiCompatProvider.NewClient(openRouterKey, openRouterBaseURL)
		switch {
		case byokOnly:
			logger.Info("OpenRouter provider enabled (BYOK only)", "base_url", openRouterBaseURL)
		case openRouterKey != "":
			envKeyedProviders[providers.ProviderOpenRouter] = struct{}{}
			logger.Info("OpenRouter provider enabled", "base_url", openRouterBaseURL)
		case !openRouterPlatformEnabled:
			logger.Info("OpenRouter provider registered (BYOK only — managed deploys don't use OpenRouter as a platform source; set ROUTER_OPENROUTER_PLATFORM_ENABLED=true to opt in)", "base_url", openRouterBaseURL)
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
		providerMap[providers.ProviderFireworks] = openaiCompatProvider.NewClientWithModelIDMap(fireworksKey, fireworksBaseURL, upstreamIDsForProvider(providers.ProviderFireworks))
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
		// DeepInfra OpenAI-compatible surface. DeepInfra uses HuggingFace-form
		// model IDs while the router exposes slash-form slugs; modelIDMap is
		// derived from the catalog's per-binding UpstreamID at boot.
		deepInfraBaseURL := config.GetOr("DEEPINFRA_BASE_URL", openaiCompatProvider.DeepInfraBaseURL)
		deepInfraKey := ""
		if !byokOnly {
			deepInfraKey = config.GetOr("DEEPINFRA_API_KEY", "")
		}
		providerMap[providers.ProviderDeepInfra] = openaiCompatProvider.NewClientWithModelIDMap(deepInfraKey, deepInfraBaseURL, upstreamIDsForProvider(providers.ProviderDeepInfra))
		switch {
		case byokOnly:
			logger.Info("DeepInfra provider enabled (BYOK only)", "base_url", deepInfraBaseURL)
		case deepInfraKey != "":
			envKeyedProviders[providers.ProviderDeepInfra] = struct{}{}
			logger.Info("DeepInfra provider enabled", "base_url", deepInfraBaseURL)
		default:
			logger.Info("DeepInfra provider registered (BYOK only — set DEEPINFRA_API_KEY for deployment-level use)", "base_url", deepInfraBaseURL)
		}
	}

	{
		// Makora OpenAI-compatible surface. Agent-optimized inference platform
		// serving DeepSeek V4 (and other OSS models) at higher throughput;
		// uses DeepSeek-canonical model IDs while the router exposes slash-form
		// slugs, so modelIDMap is derived from the catalog's per-binding
		// UpstreamID at boot.
		makoraBaseURL := config.GetOr("MAKORA_BASE_URL", openaiCompatProvider.MakoraBaseURL)
		makoraKey := ""
		if !byokOnly {
			makoraKey = config.GetOr("MAKORA_API_KEY", "")
		}
		providerMap[providers.ProviderMakora] = openaiCompatProvider.NewClientWithModelIDMap(makoraKey, makoraBaseURL, upstreamIDsForProvider(providers.ProviderMakora))
		switch {
		case byokOnly:
			logger.Info("Makora provider enabled (BYOK only)", "base_url", makoraBaseURL)
		case makoraKey != "":
			envKeyedProviders[providers.ProviderMakora] = struct{}{}
			logger.Info("Makora provider enabled", "base_url", makoraBaseURL)
		default:
			logger.Info("Makora provider registered (BYOK only — set MAKORA_API_KEY for deployment-level use)", "base_url", makoraBaseURL)
		}
	}

	{
		// Together AI OpenAI-compatible surface. Serves the OSS pool at the top
		// of the artificialanalysis.ai throughput tables for several models we
		// route (DeepSeek V4 Pro, GLM-5.1, MiniMax M2.7); primary binding for
		// those, with the prior providers kept as ordered fallbacks. Together
		// uses "Org/Model" model IDs while the router exposes slash-form slugs,
		// so modelIDMap is derived from the catalog's per-binding UpstreamID at
		// boot.
		togetherBaseURL := config.GetOr("TOGETHER_BASE_URL", openaiCompatProvider.TogetherBaseURL)
		togetherKey := ""
		if !byokOnly {
			togetherKey = config.GetOr("TOGETHER_API_KEY", "")
		}
		providerMap[providers.ProviderTogether] = openaiCompatProvider.NewClientWithModelIDMap(togetherKey, togetherBaseURL, upstreamIDsForProvider(providers.ProviderTogether))
		switch {
		case byokOnly:
			logger.Info("Together provider enabled (BYOK only)", "base_url", togetherBaseURL)
		case togetherKey != "":
			envKeyedProviders[providers.ProviderTogether] = struct{}{}
			logger.Info("Together provider enabled", "base_url", togetherBaseURL)
		default:
			logger.Info("Together provider registered (BYOK only — set TOGETHER_API_KEY for deployment-level use)", "base_url", togetherBaseURL)
		}
	}

	{
		// Bedrock via the OpenAI-compatible "bedrock-mantle" surface
		// (https://bedrock-mantle.{region}.api.aws/v1). AWS recommends this
		// over the model-native bedrock-runtime/InvokeModel surface; both
		// Qwen3 and Kimi K2.5 model IDs are addressable through it directly.
		// Auth is a static long-term Bedrock API key (AWS_BEARER_TOKEN_BEDROCK),
		// not SigV4, so the standard openaicompat bearer flow applies. Bedrock
		// expects dot-form model IDs; modelIDMap is derived from the catalog
		// at boot.
		bedrockRegion := config.GetOr("AWS_REGION", "us-east-1")
		bedrockBaseURL := config.GetOr("BEDROCK_BASE_URL", openaiCompatProvider.BedrockMantleBaseURL(bedrockRegion))
		bedrockKey := ""
		if !byokOnly {
			bedrockKey = config.GetOr("AWS_BEARER_TOKEN_BEDROCK", "")
		}
		providerMap[providers.ProviderBedrock] = openaiCompatProvider.NewClientWithModelIDMap(bedrockKey, bedrockBaseURL, upstreamIDsForProvider(providers.ProviderBedrock))
		switch {
		case byokOnly:
			logger.Info("Bedrock provider enabled (BYOK only)", "base_url", bedrockBaseURL)
		case bedrockKey != "":
			envKeyedProviders[providers.ProviderBedrock] = struct{}{}
			logger.Info("Bedrock provider enabled", "base_url", bedrockBaseURL, "region", bedrockRegion)
		default:
			logger.Info("Bedrock provider registered (BYOK only — set AWS_BEARER_TOKEN_BEDROCK for deployment-level use)", "base_url", bedrockBaseURL)
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

	// Fail loud if any registered provider lacks a ProviderFamilies entry: an
	// inbound request routed to it would fall through every cross-format dispatch
	// switch to ErrProviderNotConfigured (a silent 502) even though the provider
	// looked "enabled" at boot. Panic here so the misconfiguration aborts the
	// process rather than surfacing in production traffic.
	registeredProviders := make([]string, 0, len(providerMap))
	for name := range providerMap {
		registeredProviders = append(registeredProviders, name)
	}
	if err := providers.ValidateDispatchable(registeredProviders); err != nil {
		logger.Error("Registered provider missing a translation family; refusing to boot", "err", err)
		panic(err)
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

	pubsubProjectID := config.MustGet("PUBSUB_PROJECT_ID")
	pubsubTopicID := config.MustGet("PUBSUB_TOPIC_ROUTER_INVALIDATION")
	// Treated as a prefix: each replica derives its own subscription
	// "<prefix>-<uuid>" so every replica receives every invalidation. A shared
	// subscription would load-balance, defeating cross-fleet cache broadcast.
	pubsubSubscriptionPrefix := config.MustGet("PUBSUB_SUBSCRIPTION_ROUTER_INVALIDATION")
	pubsubClient, err := gcppubsub.NewClient(context.Background(), pubsubProjectID)
	if err != nil {
		logger.Error("Failed to create Pub/Sub client", "err", err)
		panic(err)
	}
	defer pubsubClient.Close()

	publisher := pubsubClient.Publisher(pubsubTopicID)
	notifier := routerpubsub.NewInvalidationNotifier(publisher)
	defer notifier.Stop()
	authSvc := auth.NewService(repo.Installations, repo.APIKeys, repo.ExternalAPIKeys, repo.Users, cache, userCache, time.Now).
		WithEncryptor(encryptor).
		WithInstallationChangeNotifier(notifier)

	// Listener fans out Pub/Sub-published invalidations to this replica's cache so
	// settings changes are visible on the next request across the fleet. The 5-min
	// cache TTL is the safety net if the listener falls behind.
	subCtx, subCancel := context.WithTimeout(context.Background(), 30*time.Second)
	subscriptionName, deleteSubscription, err := routerpubsub.CreateReplicaSubscription(
		subCtx, pubsubClient, pubsubProjectID, pubsubTopicID, pubsubSubscriptionPrefix,
	)
	subCancel()
	if err != nil {
		logger.Error("Failed to create per-replica invalidation subscription", "err", err)
		panic(err)
	}
	defer deleteSubscription()
	logger.Info("Created per-replica invalidation subscription", "subscription", subscriptionName)

	listener := routerpubsub.NewInvalidationListener(pubsubClient.Subscriber(subscriptionName), cache)
	listenerCtx, listenerCancel := context.WithCancel(context.Background())
	defer func() {
		listenerCancel()
		listener.Wait()
	}()
	go listener.Run(listenerCtx)

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
	if config.GetOr("ROUTER_DEEPSEEK_ESCAPE_NORMALIZE", "false") == "true" {
		translate.EnableEditEscapeNormalize = true
		logger.Info("Edit-tool escape-sequence repair enabled (ROUTER_DEEPSEEK_ESCAPE_NORMALIZE=true)")
	}
	emitter, err := buildOtelEmitter(string(deploymentMode))
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

	hardPinExplore := config.GetOr("ROUTER_HARD_PIN_EXPLORE", "true") == "true"
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

	// Per-request hard-pin resolver. Loads the default cluster bundle once and
	// closes over its metadata/registry; the resolver is then called from the
	// proxy with the request's enabled-providers set and the installation's
	// excluded_models deny set. It serves two purposes:
	//   - byokOnly: the boot-time pin was computed over every registered
	//     provider, but the request may only BYOK a subset; resolving per
	//     request keeps the hard-pin on a provider the request can
	//     authenticate to.
	//   - all modes: the hard-pin tier bypasses the scorer, which is the only
	//     component that applies excluded_models. Passing the deny set here
	//     skips an excluded model on the title-gen/classifier/probe path, just
	//     as the scorer does on the main-loop path.
	// If the bundle fails to load the resolver stays nil and the proxy falls
	// back to the boot-time hardPin{Provider,Model}.
	//
	// An explicit ROUTER_HARD_PIN_MODEL operator override is absolute by design
	// (it also bypasses the tier ceiling downstream); we do NOT wire the
	// resolver in that case so the operator's chosen model is never silently
	// rewritten. excluded_models then can't redirect it, but an operator pin is
	// a deliberate opt-in — the proxy logs if it collides with the exclude list.
	var hardPinResolver func(enabled, denySet map[string]struct{}) (string, string, bool)
	if config.GetOr("ROUTER_HARD_PIN_MODEL", "") == "" {
		reqVersion := config.GetOr("ROUTER_CLUSTER_VERSION", cluster.LatestVersion)
		if version, vErr := cluster.ResolveVersion(reqVersion); vErr == nil {
			if bundle, bErr := cluster.LoadBundle(version); bErr == nil {
				meta, registry := bundle.Metadata, bundle.Registry
				hardPinResolver = func(enabled, denySet map[string]struct{}) (string, string, bool) {
					return cluster.FastestModelInSet(meta, registry, enabled, denySet, nil)
				}
				logger.Info("Hard-pin resolver wired (per-request fastest model from cluster bundle, applies excluded_models)", "version", version, "byok_only", byokOnly)
			} else {
				logger.Warn("Hard-pin resolver disabled: cluster bundle failed to load; hard-pin will fall back to boot-time defaults and cannot apply excluded_models", "err", bErr, "version", version)
			}
		} else {
			logger.Warn("Hard-pin resolver disabled: could not resolve cluster version; hard-pin will fall back to boot-time defaults and cannot apply excluded_models", "err", vErr)
		}
	} else {
		logger.Info("Hard-pin resolver not wired: ROUTER_HARD_PIN_MODEL operator override is set and absolute by design", "model", hardPinModel)
	}

	// Default-eligible set for proxy.Service: env-keyed providers only.
	// BYOK and client-supplied credentials add to this set per-request
	// inside enabledProvidersForRequest.
	deploymentEligible := make(map[string]struct{}, len(envKeyedProviders))
	for p := range envKeyedProviders {
		deploymentEligible[p] = struct{}{}
	}

	// Passthrough-eligible providers join the eligible set ONLY when the
	// inbound request matches their surface (see WithPassthroughEligibleProviders
	// in internal/proxy/service.go). Adding them to deploymentEligible above
	// would let an Anthropic-surface request route to OpenAI in passthrough
	// mode, which would forward the inbound `x-api-key` (an Anthropic token)
	// to api.openai.com — a cross-provider credential leak. Surface-scoping
	// keeps each provider's passthrough credentials inside their own trust
	// boundary.
	passthroughEligible := make(map[string]struct{}, 2)
	if anthropicPassthroughEligible {
		passthroughEligible[providers.ProviderAnthropic] = struct{}{}
	}
	if openaiPassthroughEligible {
		passthroughEligible[providers.ProviderOpenAI] = struct{}{}
	}

	// Planner + handover config (Prism-style cache-aware routing). Defaults
	// keep the kill switch on, $0.001 EV threshold, and a 3-turn horizon —
	// each can be overridden per deployment. The summarizer is only wired
	// when its provider client is registered; otherwise the orchestrator
	// falls back to handover.TrimLastN on switch turns.
	plannerEnabled := config.GetOr("ROUTER_PLANNER_ENABLED", "true") == "true"
	effortEscalation := config.GetOr("ROUTER_EFFORT_ESCALATION", "false") == "true"
	// Per-turn large-vs-small action-classifier swap. Off by default until the
	// Layer-2 extrinsic validation clears it; enabling loads the compiled-in head.
	bandSwapEnabled := config.GetOr("ROUTER_BAND_SWAP", "false") == "true"
	// Cyclic-loop escalate-to-opus kill switch + log-not-act holdout. Enabled
	// by default (the lever shipped enabled); flipping the switch off detaches
	// the escalation ACTION without losing detection telemetry. The holdout
	// assigns that percentage of loop-detected sessions to record-only, so the
	// self-recovery baseline can be subtracted from rescue-rate claims. Parsed
	// inline rather than via parseEnvInt because 0 (holdout off) is a
	// legitimate value that parseEnvInt would reject.
	loopEscalationEnabled := config.GetOr("ROUTER_LOOP_ESCALATION_ENABLED", "true") == "true"
	loopEscalationHoldoutPct := 10
	if raw := config.GetOr("ROUTER_LOOP_ESCALATION_HOLDOUT_PCT", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 100 {
			logger.Warn("Invalid env var; using default", "key", "ROUTER_LOOP_ESCALATION_HOLDOUT_PCT", "value", raw, "default", loopEscalationHoldoutPct)
		} else {
			loopEscalationHoldoutPct = n
		}
	}
	// Shadow-mode spiral detector kill switch. Shadow mode is log-only (no
	// routing action), so it ships enabled; the switch sheds the per-turn
	// signal-scan cost if it ever misbehaves.
	spiralShadowEnabled := config.GetOr("ROUTER_SPIRAL_SHADOW_ENABLED", "true") == "true"
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

	// Optional, OFF by default: wrap the router in the quality-tie-band
	// explorer to collect propensity-logged trajectories (Phase 1 of the
	// bandit foundation). rtr stays the *cluster.Multiversion the admin cast
	// and semantic cache reference; only the proxy's routing entrypoint is
	// wrapped. Prod leaves ROUTER_EXPLORE_ENABLED unset → deterministic argmax.
	routeEntry := buildExploringRouter(rtr, logger)

	// High-fidelity OTLP content capture. Off unless WV_CAPTURE_CONTENT is set
	// (opt-in for self-hosted/OSS); Weave-managed deploys set it to "full" in
	// their deploy config. Redactor is nil here (no-op); managed wires one.
	captureMode := proxy.ParseCaptureMode(config.GetOr("WV_CAPTURE_CONTENT", ""))
	captureMaxBytes := parseEnvInt("WV_CAPTURE_MAX_BYTES", 1<<20)
	logger.Info("Router content capture configured", "mode", captureMode.String(), "max_bytes", captureMaxBytes)

	// Signed no-login feedback link. The signer mints per-request tokens and
	// verifies them on the feedback endpoints; both the signer and the public
	// base URL must be set for the link header to be emitted on responses.
	feedbackSigner := feedback.NewSigner(config.GetOr("ROUTER_FEEDBACK_LINK_SECRET", ""), feedbackLinkTTL())
	feedbackBaseURL := config.GetOr("ROUTER_FEEDBACK_BASE_URL", "")
	switch {
	case feedbackSigner != nil && feedbackBaseURL != "":
		logger.Info("Feedback link enabled", "base_url", feedbackBaseURL)
	case feedbackSigner != nil:
		logger.Warn("Feedback link endpoints mounted but ROUTER_FEEDBACK_BASE_URL is unset; responses will not carry a feedback link header")
	default:
		logger.Info("Feedback link disabled (set ROUTER_FEEDBACK_LINK_SECRET and ROUTER_FEEDBACK_BASE_URL to enable)")
	}

	// Opt-in RL/DPO policy router. Wired only when ROUTER_RL_SIDECAR_URL points
	// at a running policy sidecar; the x-weave-router-strategy: rl header then
	// routes through it, selecting from the same deployed catalog candidates and
	// dispatching via Weave's own providers. Left unset, the header fails closed
	// with HTTP 503 rather than silently serving the cluster scorer.
	var rlRouter router.Router
	if rlSidecarURL := config.GetOr("ROUTER_RL_SIDECAR_URL", ""); rlSidecarURL != "" {
		rlTimeout := parseEnvDurationMs("ROUTER_RL_SIDECAR_TIMEOUT_MS", rl.DefaultTimeout)
		rlRouter = rl.New(rl.NewHTTPDecider(rlSidecarURL, nil, rlTimeout), availableModels, availableProviders)
		logger.Info("RL policy router wired", "sidecar_url", rlSidecarURL, "timeout_ms", rlTimeout.Milliseconds(), "candidate_models", len(availableModels))
	} else {
		logger.Info("RL policy router disabled (ROUTER_RL_SIDECAR_URL unset); x-weave-router-strategy: rl will return 503")
	}

	// Opt-in Thompson-sampling bandit. Wired only when ROUTER_BANDIT_POSTERIOR_FILE
	// points at a ts_posterior.json from train_thompson_posterior.py; the
	// x-weave-router-strategy: bandit header then routes through it. Wraps the
	// raw cluster scorer (not the explore wrapper). Unset path -> nil -> 503.
	var banditRouter router.Router
	if posteriorPath := strings.TrimSpace(config.GetOr("ROUTER_BANDIT_POSTERIOR_FILE", "")); posteriorPath != "" {
		post, loadErr := bandit.LoadPosterior(posteriorPath)
		if loadErr != nil {
			logger.Error(
				"Bandit posterior load failed; x-weave-router-strategy: bandit will return 503",
				"path", posteriorPath,
				"err", loadErr,
			)
		} else {
			banditRouter = bandit.New(rtr, post)
			logger.Info("Bandit strategy router wired", "posterior_file", posteriorPath)
		}
	} else {
		logger.Info("Bandit strategy router disabled (ROUTER_BANDIT_POSTERIOR_FILE unset); x-weave-router-strategy: bandit will return 503")
	}

	proxySvc := proxy.NewService(routeEntry, providerMap, emitter, embedOnlyUser, semanticCache, pinStore, hardPinExplore, hardPinProvider, hardPinModel, repo.Telemetry).
		WithRLRouter(rlRouter).
		WithBanditRouter(banditRouter).
		WithContentCapture(captureMode, captureMaxBytes, nil).
		WithFeedback(repo.Feedback, feedbackSigner, feedbackBaseURL).
		WithByokOnly(byokOnly).
		WithDeploymentKeyedProviders(deploymentEligible).
		WithPassthroughEligibleProviders(passthroughEligible).
		WithHardPinResolver(hardPinResolver).
		WithPlannerEnabled(plannerEnabled).
		WithEffortEscalation(effortEscalation).
		WithBandSwap(bandSwapEnabled).
		WithLoopEscalationConfig(loopEscalationEnabled, loopEscalationHoldoutPct).
		WithLoopEscalationStore(repo.Telemetry).
		WithSpiralShadowConfig(spiralShadowEnabled).
		WithSpiralShadowStore(repo.Telemetry).
		WithRouterFeedbackStore(repo.Telemetry).
		WithPlanner(plannerCfg).
		WithSummarizer(summarizer).
		WithAvailableModels(availableModels).
		WithDefaultBaselineModel(resolveDefaultBaselineModel()).
		WithBillingService(billingSvc)
	logger.Info("Effort escalation configured", "enabled", effortEscalation)
	logger.Info("Loop escalation configured", "enabled", loopEscalationEnabled, "holdout_pct", loopEscalationHoldoutPct)
	logger.Info("Spiral shadow detector configured", "enabled", spiralShadowEnabled)
	logger.Info("Planner configured", "enabled", plannerEnabled, "threshold_usd", plannerCfg.ThresholdUSD, "expected_remaining_turns", plannerCfg.ExpectedRemainingTurns, "tier_upgrade_enabled", plannerCfg.TierUpgradeEnabled, "available_models_count", len(availableModels))

	// Fail loud if a deployed model is missing from the tier table;
	// TierUnknown would silently disable the guard for that pair.
	if plannerCfg.TierUpgradeEnabled && len(availableModels) > 0 {
		deployed := make([]string, 0, len(availableModels))
		for m := range availableModels {
			deployed = append(deployed, m)
		}
		if err := catalog.ValidateDeployed(deployed); err != nil {
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

	// ROUTER_EXCLUDED_PROVIDERS pins a deployment-wide provider exclusion
	// list, overriding per-installation DB state. Empty / unset → DB takes over.
	if excludedRaw := strings.TrimSpace(config.GetOr("ROUTER_EXCLUDED_PROVIDERS", "")); excludedRaw != "" {
		parts := strings.Split(excludedRaw, ",")
		cleaned := make([]string, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				cleaned = append(cleaned, trimmed)
			}
		}
		proxySvc = proxySvc.WithExcludedProvidersOverride(cleaned)
		logger.Info("Provider exclusion override active", "excluded_providers", cleaned)
	}

	// ROUTER_SUBSCRIPTION_AWARE_ROUTING discounts a covered model's cost term by
	// the caller's observed subscription rate-limit headroom (see
	// internal/proxy/usage): ~epsilon when the window has slack, →1 (full price)
	// as it binds. Off by default — ships dark until validated.
	// Defaults ON: the discount only affects turns that present a subscription
	// AND have observed headroom (cold start / non-sub traffic = unchanged), so
	// the blast radius is narrow. Set ROUTER_SUBSCRIPTION_AWARE_ROUTING=false to
	// disable without a code change.
	// The subscription usage observer is always wired: it feeds BOTH the
	// subscription-aware cost discount (below, env-gated) AND the
	// per-installation usage-bypass gate (DB-gated, so the env can't know
	// whether any tenant enabled it). Recording is cheap and side-effect-free,
	// so installing it unconditionally is what lets the bypass work regardless
	// of the cost-discount flag.
	subscriptionTTL := 10 * time.Minute
	if v, err := time.ParseDuration(config.GetOr("ROUTER_SUBSCRIPTION_OBSERVATION_TTL", "10m")); err == nil {
		subscriptionTTL = v
	}
	observerSalt := make([]byte, 16)
	if _, err := rand.Read(observerSalt); err != nil {
		logger.Error("Failed to seed subscription usage observer salt", "err", err)
		panic(err)
	}
	usageObserver := usage.NewObserver(observerSalt, subscriptionTTL, time.Now)
	// Bound memory: evict expired observations periodically (the usage package
	// spawns no goroutines of its own).
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			usageObserver.Sweep()
		}
	}()
	proxySvc = proxySvc.WithUsageObserver(usageObserver)

	// ROUTER_SUBSCRIPTION_AWARE_ROUTING discounts a covered model's cost term by
	// the caller's observed subscription rate-limit headroom (see
	// internal/proxy/usage): ~epsilon when the window has slack, →1 (full price)
	// as it binds. Defaults ON: the discount only affects turns that present a
	// subscription AND have observed headroom (cold start / non-sub traffic =
	// unchanged), so the blast radius is narrow. Set to false to disable the
	// discount without a code change — the observer (and the usage-bypass gate)
	// stay wired.
	if config.GetOr("ROUTER_SUBSCRIPTION_AWARE_ROUTING", "true") == "true" {
		epsilon := 0.05
		if v, err := strconv.ParseFloat(config.GetOr("ROUTER_SUBSCRIPTION_COST_EPSILON", "0.05"), 64); err == nil {
			epsilon = v
		}
		gamma := 2.0
		if v, err := strconv.ParseFloat(config.GetOr("ROUTER_SUBSCRIPTION_COST_GAMMA", "2"), 64); err == nil {
			gamma = v
		}
		proxySvc = proxySvc.WithSubscriptionAwareRouting(usageObserver, epsilon, gamma)
		logger.Info("Subscription-aware routing configured", "epsilon", epsilon, "gamma", gamma, "observation_ttl", subscriptionTTL)
	} else {
		logger.Info("Usage observer wired; subscription-aware cost discount disabled", "observation_ttl", subscriptionTTL)
	}

	// APM (SigNoz) — adds standard HTTP server spans and Go runtime metrics
	// to the same OTel resource shape as the rest of the Weave services.
	// No-op when WV_APM_OTLP_ENDPOINT is unset. Flushed explicitly in the
	// graceful-shutdown path below; defer would run after SIGKILL.
	apm.Init()

	engine := gin.New()
	engine.UnescapePathValues = true
	engine.UseRawPath = true
	engine.Use(
		observability.Middleware(),
		observability.AccessLog(),
		apm.Middleware(),
		gin.Recovery(),
	)

	// Cast the router to *cluster.Multiversion so the admin model-selection
	// handler can surface the universe of deployed models. The fallback nil
	// keeps non-cluster routers (heuristic dev override, etc.) bootable.
	deployedModels, _ := rtr.(*cluster.Multiversion)
	server.Register(engine, authSvc, proxySvc, deployedModels, deploymentMode, billingSvc)

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
		// Flush APM here too: a ListenAndServe failure bypasses the SIGTERM
		// path below, so without an explicit shutdown the buffered SDK
		// traces + metrics describing the failure itself would never reach
		// SigNoz — exactly when they'd be most useful.
		apmFailCtx, apmFailCancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer apmFailCancel()
		apm.ShutdownWithContext(apmFailCtx)
		return
	case sig := <-stop:
		logger.Info("Received shutdown signal; draining", "signal", sig.String())
	}

	// Cloud Run gives 10s between SIGTERM and SIGKILL. Budget across three
	// flush stages so the SDK trace/metric batch actually exports before
	// SIGKILL — a defer on apm.Shutdown would never run in time.
	//
	//   srv.Shutdown:    6.0s — long-lived streams are the hard part
	//   emitter.Shutdown: 1.5s — custom OTLP/HTTP decision spans
	//   apm.Shutdown:    1.5s — SDK trace + metric batchers
	//                    ----
	//                    9.0s, leaving ~1s slack before SIGKILL
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("Graceful shutdown failed", "err", err)
	}
	emitterCtx, emitterCancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer emitterCancel()
	if err := emitter.Shutdown(emitterCtx); err != nil {
		logger.Warn("OTel emitter shutdown incomplete", "err", err)
	}
	apmCtx, apmCancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer apmCancel()
	apm.ShutdownWithContext(apmCtx)
}

// buildExploringRouter optionally wraps rtr in the quality-tie-band explorer.
// OFF unless ROUTER_EXPLORE_ENABLED=true; returns rtr unchanged otherwise so
// prod keeps serving the deterministic argmax. The band half-width comes from
// ROUTER_EXPLORE_EPSILON (default 0.05, score units). The model->provider
// resolver is sourced from the default bundle's deployed models, so the
// explorer can only ever switch to a known, deployed peer.
//
// Intended for staging / a small traffic slice while real propensities and
// exploration support accrue — never flip it on fleet-wide without a bake-off.
func buildExploringRouter(rtr router.Router, logger *slog.Logger) router.Router {
	if !strings.EqualFold(config.GetOr("ROUTER_EXPLORE_ENABLED", "false"), "true") {
		return rtr
	}
	multi, ok := rtr.(*cluster.Multiversion)
	if !ok {
		logger.Warn("ROUTER_EXPLORE_ENABLED set but router is not a cluster.Multiversion; exploration disabled")
		return rtr
	}

	providers := make(map[string]string)
	for _, e := range multi.DefaultDeployedModels() {
		// First binding wins; the default bundle lists a model's primary
		// provider first, which matches the scorer's default resolution.
		if _, seen := providers[e.Model]; !seen {
			providers[e.Model] = e.Provider
		}
	}
	providerFor := func(model string) (string, bool) {
		p, ok := providers[model]
		return p, ok
	}

	epsilon := float32(parseEnvFloat("ROUTER_EXPLORE_EPSILON", 0.05))
	logger.Info(
		"Quality-tie-band exploration ENABLED (non-prod / shadow expected)",
		"band_width", epsilon,
		"deployed_models", len(providers),
	)
	return banditexplore.New(rtr, providerFor, epsilon)
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
		return nil, fmt.Errorf("Resolve cluster version %q: %w", requestedVersion, err)
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
			return nil, fmt.Errorf("List cluster versions: %w", err)
		}
	} else {
		versions = []string{defaultVersion}
	}

	embedders, err := cluster.NewEmbedderSet()
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
	if v := config.GetOr("ROUTER_CLUSTER_TOP_P", ""); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil || n <= 0 {
			logger.Warn("Invalid ROUTER_CLUSTER_TOP_P; using default", "value", v, "default", cfg.TopP)
		} else {
			cfg.TopP = n
			logger.Info("Cluster top_p overridden", "top_p", n)
		}
	}
	scorers := make(map[string]*cluster.Scorer, len(versions))
	warmed := make(map[string]cluster.Embedder)
	for _, v := range versions {
		bundle, err := cluster.LoadBundle(v)
		if err != nil {
			_ = embedders.Close()
			return nil, fmt.Errorf("Load bundle %s: %w", v, err)
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

		// Lazily construct only the embedders the built versions need:
		// prod (single default version) loads exactly one model.
		embedder, err := embedders.Get(bundle.EmbedderID())
		if err != nil {
			// The default version's embedder must construct; sibling
			// versions degrade like any other per-version build failure.
			if v == defaultVersion {
				_ = embedders.Close()
				return nil, fmt.Errorf("Construct embedder %q for default cluster version %s: %w", bundle.EmbedderID(), v, err)
			}
			logger.Warn("Cluster scorer version skipped; embedder unavailable", "cluster_version", v, "embedder", bundle.EmbedderID(), "err", err)
			continue
		}
		warmed[embedder.ID()] = embedder

		scorer, err := cluster.NewScorer(bundle, cfg, embedder, availableProviders)
		if err != nil {
			logger.Warn("Cluster scorer version skipped", "cluster_version", v, "err", err)
			continue
		}
		scorers[v] = scorer
		logger.Info("Cluster scorer version built", "cluster_version", v, "embedder", bundle.EmbedderID(), "models", bundle.Registry.Models())
	}

	if _, ok := scorers[defaultVersion]; !ok {
		_ = embedders.Close()
		return nil, fmt.Errorf("Default cluster version %q failed to build (likely no registered provider covers its deployed_models); set ROUTER_CLUSTER_VERSION to a version that does, or register the missing provider key", defaultVersion)
	}

	multi, err := cluster.NewMultiversion(defaultVersion, scorers)
	if err != nil {
		_ = embedders.Close()
		return nil, fmt.Errorf("Build multiversion router: %w", err)
	}
	logger.Info(
		"Cluster multiversion router ready",
		"default_version", defaultVersion,
		"built_versions", multi.Built(),
		"built_embedders", embedders.Built(),
		"requested_version", requestedVersion,
		"build_all_versions", buildAll,
	)

	// Warmup: burn each embedder's lazy ONNX graph-optimization cost at boot.
	for id, embedder := range warmed {
		type warmupResult struct {
			err error
		}
		warmupDone := make(chan warmupResult, 1)
		go func(e cluster.Embedder) {
			_, err := e.Embed(context.Background(), "warmup")
			warmupDone <- warmupResult{err: err}
		}(embedder)
		select {
		case res := <-warmupDone:
			if res.err != nil {
				_ = embedders.Close()
				return nil, fmt.Errorf("Warm embedder %q: %w", id, res.err)
			}
		case <-time.After(15 * time.Second):
			// Drain the goroutine before closing to avoid use-after-free.
			go func() {
				<-warmupDone
				_ = embedders.Close()
			}()
			return nil, fmt.Errorf("Cluster embedder %q warmup timed out after 15s", id)
		}
		logger.Info("Cluster embedder warmed", "embedder", id, "embed_dim", embedder.Dim())
	}

	return multi, nil
}

// buildOtelEmitter constructs the OTel span emitter from environment
// variables. Returns (nil, nil) when OTEL_EXPORTER_OTLP_ENDPOINT is unset.
func buildOtelEmitter(deploymentMode string) (*otel.Emitter, error) {
	logger := observability.Get()

	endpoint := config.GetOr("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if endpoint == "" {
		return nil, nil
	}

	// router.deployment_mode lets the collector branch ingest behavior
	// (e.g. redaction / content-opt-out) without inspecting per-record attrs.
	resourceAttrs := parseOtelHeaders(config.GetOr("OTEL_RESOURCE_ATTRIBUTES", ""))
	if resourceAttrs == nil {
		resourceAttrs = map[string]string{}
	}
	resourceAttrs["router.deployment_mode"] = deploymentMode

	cfg := otel.EmitterConfig{
		Endpoint:      endpoint,
		Headers:       parseOtelHeaders(config.GetOr("OTEL_EXPORTER_OTLP_HEADERS", "")),
		ServiceName:   config.GetOr("OTEL_SERVICE_NAME", "router"),
		ResourceAttrs: resourceAttrs,
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

// feedbackLinkTTL returns the lifetime of a minted feedback-link token,
// from ROUTER_FEEDBACK_LINK_TTL_SEC (default 30 days). The link is a low-stakes
// rating affordance, so a long expiry trades little risk for the convenience of
// a user rating a routing decision they noticed hours later.
// feedbackLinkTTL resolves the feedback-link token lifetime from
// ROUTER_FEEDBACK_LINK_TTL_SEC. Unset, negative, or unparseable falls back to
// 30 days; an explicit 0 means "never expire" — matching feedback.NewSigner's
// non-positive-TTL contract. Parsed inline rather than via parseEnvInt, which
// rejects 0 and so would silently turn an operator's "never expire" into the
// 30-day default.
func feedbackLinkTTL() time.Duration {
	const defaultSec = 30 * 24 * 60 * 60
	raw := config.GetOr("ROUTER_FEEDBACK_LINK_TTL_SEC", "")
	if raw == "" {
		return defaultSec * time.Second
	}
	sec, err := strconv.Atoi(raw)
	if err != nil || sec < 0 {
		observability.Get().Warn("Invalid env var; using default", "key", "ROUTER_FEEDBACK_LINK_TTL_SEC", "value", raw, "default", defaultSec)
		return defaultSec * time.Second
	}
	return time.Duration(sec) * time.Second
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
// Explore hard-pins. Operator override wins; otherwise the fastest model (by
// measured tok/s) in the default artifact bundle among available providers is
// selected, falling back to cheapest when the bundle lacks speed annotations.
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
	p, m, ok := cluster.FastestModel(bundle.Metadata, bundle.Registry, available)
	if !ok {
		logger.Warn("Hard-pin model: no model found for available providers; using default", "default_model", defaultHardPinModel)
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
	out := cluster.RoutableModelSet(bundle.Registry, availableProviders)
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

// upstreamIDsForProvider walks the catalog and returns the map of public
// model ID → upstream model ID for every binding on the given provider that
// has a non-empty UpstreamID. Returns nil when no rewriting is needed (e.g.
// OpenRouter, where the public slug IS the upstream ID). Callers pass the
// result straight to openaicompat.NewClientWithModelIDMap.
func upstreamIDsForProvider(provider string) map[string]string {
	out := make(map[string]string)
	for _, m := range catalog.Models {
		for _, b := range m.Providers {
			if b.Provider == provider && b.UpstreamID != "" {
				out[m.ID] = b.UpstreamID
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
