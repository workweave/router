// Package server wires the HTTP engine: middleware and route registration.
package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"weave-os/router/internal/analytics"
	"weave-os/router/internal/api/admin"
	analyticsapi "weave-os/router/internal/api/analytics"
	anthropicapi "weave-os/router/internal/api/anthropic"
	feedbackapi "weave-os/router/internal/api/feedback"
	geminiapi "weave-os/router/internal/api/gemini"
	openaiapi "weave-os/router/internal/api/openai"
	subscriptionsapi "weave-os/router/internal/api/subscriptions"
	"weave-os/router/internal/auth"
	"weave-os/router/internal/billing"
	"weave-os/router/internal/policyclient"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/hmm/rosterdata"
	"weave-os/router/internal/router/policy"
	"weave-os/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

const (
	healthTimeout    = 1 * time.Second
	readinessTimeout = 2 * time.Second
	// Cold-cache key probe costs 3 sequential Postgres round trips; 1s made that a credential rejection.
	validateTimeout = 5 * time.Second

	messagesTimeout       = 600 * time.Second
	chatCompletionTimeout = 600 * time.Second
	passthroughTimeout    = 10 * time.Second
	routeTimeout          = 5 * time.Second
	adminTimeout          = 10 * time.Second
	// catalogModelsTimeout bounds GET /v1/router/models; must exceed the HMM
	// sidecar client budget (policyclient.DefaultTimeout) or a cold cache 503s.
	catalogModelsTimeout = policyclient.DefaultTimeout * 2
	// feedbackTimeout bounds the no-login feedback link reads/writes. Both are
	// single-row Postgres ops plus an async span emit, so 5s is generous.
	feedbackTimeout = 5 * time.Second
	// analyticsTimeout bounds an export page. Keyset scans on a high-volume
	// telemetry table warrant a batch-job budget, not an interactive one.
	analyticsTimeout = 60 * time.Second
)

// DeploymentMode gates whether the self-hoster admin dashboard and its
// /admin/v1/* API are mounted. Managed (SaaS) deployments skip it since
// keys, BYOK secrets, and config are owned by the Weave control plane.
type DeploymentMode string

const (
	// DeploymentModeSelfHosted mounts the dashboard and /admin/v1/* API. Default when ROUTER_DEPLOYMENT_MODE is unset.
	DeploymentModeSelfHosted DeploymentMode = "selfhosted"
	// DeploymentModeManaged skips the dashboard and admin API entirely so misconfig can't expose a redundant control plane.
	DeploymentModeManaged DeploymentMode = "managed"
)

// Register wires routes onto the engine. In managed mode the dashboard +
// /admin/v1/* routes are not registered at all.
//
// deployedModels may be nil in tests; required in selfhosted prod so the
// dashboard can render the universe of routable models.
//
// hmmModels is optional; nil when no HMM sidecar is wired — falls back to the
// cluster registry.
//
// billingSvc is set only in managed mode when credit-billing is enabled; it
// gates every inference route on prepaid balance via WithBalanceCheck. nil
// leaves inference routes open (BYOK/platform key still controls upstream auth).
//
// readinessChecker gates /readyz only; /health remains process liveness.
//
// hmmRosterSource, when non-nil, mounts GET /v1/router/hmm-roster for the
// control plane's cluster allowlist UI.
//
// analyticsSvc, when non-nil, mounts the /v1/analytics/* export surface;
// nil leaves it unmounted (tests, deployments without telemetry storage).
func Register(engine *gin.Engine, authSvc *auth.Service, proxySvc *proxy.Service, deployedModels admin.DeployedModelsSource, hmmModels admin.HMMRosterSource, mode DeploymentMode, billingSvc *billing.Service, readinessChecker admin.HealthChecker, hmmRosterSource policy.RosterSource, analyticsSvc *analytics.Service, hmmDistributionRosters ...*rosterdata.Roster) {
	// Browser clients need an explicit expose list before fetch can read the
	// router's routing and cost metadata from a cross-origin response.
	engine.Use(func(c *gin.Context) {
		c.Header("Access-Control-Expose-Headers", strings.Join([]string{
			proxy.HeaderRouterDecision,
			proxy.HeaderRouterProvider,
			proxy.HeaderRouterModel,
			proxy.HeaderRouterContextWindow,
			proxy.HeaderRouterCache,
			proxy.HeaderRouterFallbackFrom,
			proxy.HeaderRouterFallbackAttempt,
			proxy.HeaderRouterCostUSD,
			proxy.HeaderRouterCostInputUSD,
			proxy.HeaderRouterCostOutputUSD,
			proxy.HeaderRouterCacheReadTokens,
			proxy.HeaderRouterCacheCreationTokens,
		}, ", "))
		c.Next()
	})
	// Managed mode: BYOK is opt-in per installation (see WithAuth).
	byokRequiresOptIn := mode == DeploymentModeManaged

	engine.GET("/health", middleware.WithTimeout(healthTimeout), admin.HealthHandler)
	engine.GET("/readyz", middleware.WithTimeout(readinessTimeout), admin.ReadinessHandler(readinessChecker))

	// /v1/version reports the binary's git commit + build time (via -ldflags),
	// used by the README's managed-deployment badge. Public build metadata, unauthed like /health.
	engine.GET("/v1/version", middleware.WithTimeout(healthTimeout), admin.VersionHandler)
	var registeredStrategies []router.Strategy
	if proxySvc != nil {
		registeredStrategies = proxySvc.RegisteredStrategies()
	}
	defaultStrategy := router.Strategy(strings.ToLower(strings.TrimSpace(os.Getenv("ROUTER_DEFAULT_STRATEGY"))))
	if defaultStrategy == "" {
		defaultStrategy = router.StrategyCluster
	}
	defaultStrategy = middleware.NormalizeRouterStrategyDefault(defaultStrategy, registeredStrategies...)
	engine.GET(
		"/v1/router/policies",
		middleware.WithTimeout(healthTimeout),
		admin.PolicyCatalogHandler(proxySvc, defaultStrategy),
	)

	// /v1/router/models lets the Weave control plane validate per-org exclusion
	// submissions against the live deployed-models universe instead of
	// hand-copying it per gitlink bump. Unauthed: read-only, and the list is
	// already public on the RouterArena leaderboard.
	if deployedModels != nil {
		engine.GET("/v1/router/models", middleware.WithTimeout(catalogModelsTimeout), admin.CatalogModelsHandler(deployedModels, hmmModels))

		// Projects the quality-vs-price dial's model mix across dial positions
		// for the dashboard's distribution preview. Same unauthed rationale as
		// /v1/router/models; the assertion skips sources that can't project one.
		if dist, ok := deployedModels.(admin.RoutingDistributionSource); ok {
			engine.GET("/v1/router/routing-distribution", middleware.WithTimeout(healthTimeout), admin.RoutingDistributionHandler(dist, hmmDistributionRosters...))
		}
	}

	// /v1/router/hmm-roster: frozen per-cluster arm roster mapped to catalog IDs.
	// Unauthed — read-only and non-sensitive, same rationale as /v1/router/models.
	if hmmRosterSource != nil {
		engine.GET("/v1/router/hmm-roster", middleware.WithTimeout(readinessTimeout), admin.HMMRosterHandler(hmmRosterSource))
	}

	// /internal/v1/*: control-plane-to-router calls, authed by a shared secret
	// and mounted only when one is configured. This is not a second admin API —
	// it carries only work the control plane cannot do itself because the
	// credential is minted here per request (key-pair, workload identity).
	if internalToken := strings.TrimSpace(os.Getenv("ROUTER_INTERNAL_SERVICE_TOKEN")); internalToken != "" {
		internalGroup := engine.Group("/internal/v1", middleware.WithTimeout(adminTimeout), middleware.WithInternalServiceAuth(internalToken))
		internalGroup.POST("/provider-keys/models", admin.InternalListUpstreamModelsHandler(authSvc, proxySvc))
	}

	// /validate is a token-validity probe used by clients (not the dashboard), so it stays mounted in both modes.
	adminAuthed := engine.Group("", middleware.WithTimeout(validateTimeout), middleware.WithAuth(authSvc, byokRequiresOptIn))
	adminAuthed.GET("/validate", admin.ValidateHandler)
	if authSvc.SubscriptionAccountsEnabled() {
		subscriptionGroup := engine.Group("/v1", middleware.WithTimeout(adminTimeout), middleware.WithAuth(authSvc, byokRequiresOptIn))
		subscriptionsapi.Register(subscriptionGroup, authSvc)
	}

	if mode == DeploymentModeSelfHosted {
		engine.GET("/", func(c *gin.Context) { c.Redirect(http.StatusFound, "/ui") })
		registerUIStatic(engine, "./assets/ui")

		// Public — mounting inside WithAuth would be a chicken-and-egg
		// deadlock for users who don't yet have a cookie.
		authPublic := engine.Group("/admin/v1/auth", middleware.WithTimeout(adminTimeout))
		authPublic.POST("/login", admin.LoginHandler(authSvc))
		authPublic.POST("/logout", admin.LogoutHandler())
		authPublic.GET("/me", admin.MeHandler(authSvc))

		// Read-only metrics: dashboard cookie OR rk_ bearer so an installation can fetch its own data for monitoring scripts. Per-installation scoping is enforced inside the handlers.
		metrics := engine.Group("/admin/v1", middleware.WithTimeout(adminTimeout), middleware.WithAdminOrAuth(authSvc, byokRequiresOptIn))
		metrics.GET("/metrics/summary", admin.MetricsSummaryHandler(proxySvc))
		metrics.GET("/metrics/timeseries", admin.MetricsTimeseriesHandler(proxySvc))
		metrics.GET("/metrics/details", admin.MetricsDetailsHandler(proxySvc))
		metrics.GET("/metrics/model-breakdown", admin.MetricsModelBreakdownHandler(proxySvc))

		// Mutations: admin cookie REQUIRED. rk_ tokens are rejected so a leaked data-plane key can't mint fresh router keys or rotate provider credentials.
		mgmt := engine.Group("/admin/v1", middleware.WithTimeout(adminTimeout), middleware.WithAdminOnly(authSvc))
		mgmt.GET("/keys", admin.ListAPIKeysHandler(authSvc))
		mgmt.POST("/keys", admin.IssueAPIKeyHandler(authSvc))
		mgmt.POST("/keys/:id/rotate", admin.RotateAPIKeyHandler(authSvc))
		mgmt.DELETE("/keys/:id", admin.DeleteAPIKeyHandler(authSvc))
		mgmt.GET("/provider-keys", admin.ListExternalKeysHandler(authSvc))
		mgmt.POST("/provider-keys", admin.UpsertExternalKeyHandler(authSvc, deployedModels))
		mgmt.PUT("/provider-keys/:id/model-aliases", admin.UpdateExternalKeyAliasesHandler(authSvc, deployedModels))
		// Discovery hits the provider endpoint itself; the group's adminTimeout
		// (10s) bounds that upstream GET /models call.
		mgmt.GET("/provider-keys/:id/models", admin.ListUpstreamModelsHandler(authSvc, proxySvc))
		mgmt.POST("/provider-keys/discover-models", admin.DiscoverModelsHandler(proxySvc))
		mgmt.DELETE("/provider-keys/:id", admin.DeleteExternalKeyHandler(authSvc))
		mgmt.GET("/config", admin.ConfigHandler)
		mgmt.GET("/onboarding", admin.OnboardingHandler(authSvc))
		mgmt.GET("/routing-preferences", admin.GetRoutingPreferencesHandler(authSvc))
		mgmt.PUT("/routing-preferences", admin.UpdateRoutingPreferencesHandler(authSvc))
		mgmt.GET("/content-capture", admin.GetContentCaptureHandler(authSvc, proxySvc))
		mgmt.PUT("/content-capture", admin.UpdateContentCaptureHandler(authSvc, proxySvc))
		mgmt.GET("/fast-mode-models", admin.GetFastModeModelsHandler(authSvc))
		mgmt.PUT("/fast-mode-models", admin.UpdateFastModeModelsHandler(authSvc))
		if deployedModels != nil {
			mgmt.GET("/excluded-models", admin.GetExcludedModelsHandler(authSvc, deployedModels, proxySvc))
			mgmt.PUT("/excluded-models", admin.UpdateExcludedModelsHandler(authSvc, deployedModels, proxySvc))
			mgmt.GET("/allowed-models", admin.GetAllowedModelsHandler(authSvc, deployedModels))
			mgmt.PUT("/allowed-models", admin.UpdateAllowedModelsHandler(authSvc, deployedModels, proxySvc))
			mgmt.GET("/excluded-providers", admin.GetExcludedProvidersHandler(authSvc, deployedModels, proxySvc))
			mgmt.PUT("/excluded-providers", admin.UpdateExcludedProvidersHandler(authSvc, deployedModels, proxySvc))
		}
	}

	messagesMiddleware := []gin.HandlerFunc{
		middleware.WithTimingEntry(),
		middleware.WithTimeout(messagesTimeout),
		middleware.WithAuth(authSvc, byokRequiresOptIn),
		middleware.WithAgentShadowEvaluation(),
	}
	if billingSvc != nil {
		messagesMiddleware = append(messagesMiddleware, middleware.WithBalanceCheck(billingSvc, billing.MinBalanceMicros), middleware.WithAPIKeySpendCap(billingSvc), middleware.WithOrgMonthlySpendCap(billingSvc))
	}
	messagesMiddleware = append(messagesMiddleware,
		middleware.WithEmbedOnlyUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
		middleware.WithRouterStrategyDefault(defaultStrategy, registeredStrategies...),
		middleware.WithPolicyDebugOverride(),
		middleware.WithAllowedModelsOverride(proxySvc),
		middleware.WithRoutingKnobsOverride(),
		middleware.WithForceEffortOverride(),
	)
	messagesGroup := engine.Group("", messagesMiddleware...)
	messagesGroup.POST("/v1/messages", anthropicapi.MessagesHandler(proxySvc, authSvc))

	chatCompletionMiddleware := []gin.HandlerFunc{
		middleware.WithTimingEntry(),
		middleware.WithTimeout(chatCompletionTimeout),
		middleware.WithAuth(authSvc, byokRequiresOptIn),
	}
	if billingSvc != nil {
		chatCompletionMiddleware = append(chatCompletionMiddleware, middleware.WithBalanceCheck(billingSvc, billing.MinBalanceMicros), middleware.WithAPIKeySpendCap(billingSvc), middleware.WithOrgMonthlySpendCap(billingSvc))
	}
	chatCompletionMiddleware = append(chatCompletionMiddleware,
		middleware.WithEmbedOnlyUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
		middleware.WithRouterStrategyDefault(defaultStrategy, registeredStrategies...),
		middleware.WithPolicyDebugOverride(),
		middleware.WithAllowedModelsOverride(proxySvc),
		middleware.WithRoutingKnobsOverride(),
		middleware.WithForceEffortOverride(),
	)
	chatCompletionGroup := engine.Group("", chatCompletionMiddleware...)
	chatCompletionGroup.POST("/v1/chat/completions", openaiapi.ChatCompletionHandler(proxySvc, authSvc))
	// Responses surface required by Codex CLI after wire_api="chat" was retired;
	// translated internally to chat completions so the turn loop is reused.
	chatCompletionGroup.POST("/v1/responses", openaiapi.ResponsesHandler(proxySvc, authSvc))
	// Action suffix (:generateContent or :streamGenerateContent) lives inside modelAction because Gin treats `:` outside the leading position as a literal.
	chatCompletionGroup.POST("/v1beta/models/:modelAction", geminiapi.GenerateContentHandler(proxySvc, authSvc))

	// Passthrough endpoints cost no upstream tokens, so they stay open even
	// with billing enabled — count_tokens is the SDK's pre-flight call before
	// /v1/messages, and gating it would break client negotiation.
	passthroughGroup := engine.Group("",
		middleware.WithTimeout(passthroughTimeout),
		middleware.WithAuth(authSvc, byokRequiresOptIn),
	)
	passthroughGroup.POST("/v1/messages/count_tokens", anthropicapi.PassthroughHandler(proxySvc))
	passthroughGroup.GET("/v1/models", openaiapi.ModelsHandler(anthropicapi.PassthroughHandler(proxySvc)))
	passthroughGroup.GET("/v1/models/:model", anthropicapi.PassthroughHandler(proxySvc))
	// Rides the passthrough group (cheap, no billing middleware) — read-only, no routing side-effects.
	passthroughGroup.GET("/v1/display-settings", admin.DisplaySettingsHandler)
	// Product surface (not admin): the Codex status hook's rk_ key needs the router's savings number.
	passthroughGroup.GET("/v1/sessions/:session_id/cost", admin.SessionCostHandler(proxySvc))

	routeMiddleware := []gin.HandlerFunc{
		middleware.WithTimeout(routeTimeout),
		middleware.WithAuth(authSvc, byokRequiresOptIn),
	}
	if billingSvc != nil {
		routeMiddleware = append(routeMiddleware, middleware.WithBalanceCheck(billingSvc, billing.MinBalanceMicros), middleware.WithAPIKeySpendCap(billingSvc), middleware.WithOrgMonthlySpendCap(billingSvc))
	}
	routeMiddleware = append(routeMiddleware,
		middleware.WithEmbedOnlyUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
		middleware.WithRouterStrategyDefault(defaultStrategy, registeredStrategies...),
		middleware.WithPolicyDebugOverride(),
		middleware.WithAllowedModelsOverride(proxySvc),
		middleware.WithRoutingKnobsOverride(),
		middleware.WithForceEffortOverride(),
	)
	routeGroup := engine.Group("", routeMiddleware...)
	routeGroup.POST("/v1/route", anthropicapi.RouteHandler(proxySvc))

	previewGroup := engine.Group("",
		middleware.WithTimingEntry(),
		middleware.WithTimeout(routeTimeout),
		middleware.WithAuth(authSvc, byokRequiresOptIn),
		middleware.WithEmbedOnlyUserMessageOverride(),
		middleware.WithRouterStrategyDefault(defaultStrategy, registeredStrategies...),
		middleware.WithPolicyDebugOverride(),
		middleware.WithAllowedModelsOverride(proxySvc),
		middleware.WithRoutingKnobsOverride(),
	)
	previewGroup.POST("/v1/route/preview", anthropicapi.PreviewRouteHandler(proxySvc))

	// Read-only routing-decision export. Product surface, so it mounts in both
	// modes; ra_ keys only, no spend path.
	if analyticsSvc != nil {
		analyticsGroup := engine.Group("/v1/analytics",
			middleware.WithTimeout(analyticsTimeout),
			middleware.WithAnalyticsKey(authSvc),
			middleware.WithAnalyticsRateLimit(middleware.AnalyticsRequestsPerMinute),
		)
		analyticsGroup.GET("/routing-decisions", analyticsapi.RoutingDecisionsHandler(analyticsSvc))
		analyticsGroup.GET("/models", analyticsapi.ModelsHandler())
		analyticsGroup.GET("/schema", analyticsapi.SchemaHandler())
	}

	// No-login feedback link: the signed HMAC token in the URL/body is the
	// sole credential, so no auth middleware. Mounted only when
	// ROUTER_FEEDBACK_LINK_SECRET is configured.
	if proxySvc.FeedbackEnabled() {
		feedbackGroup := engine.Group("/v1/feedback", middleware.WithTimeout(feedbackTimeout))
		feedbackGroup.GET("/link/:token", feedbackapi.GetContextHandler(proxySvc))
		feedbackGroup.POST("/link", feedbackapi.SubmitHandler(proxySvc))
		// One-click thumb links embedded in response footers.
		feedbackGroup.GET("/rate", feedbackapi.RateHandler(proxySvc))
		feedbackGroup.GET("/assets/wooly-wave.png", feedbackapi.WoolyWaveHandler())
		feedbackGroup.GET("/assets/weave.svg", feedbackapi.WeaveLogoHandler())
	}
}

// registerUIStatic mounts the exported Next.js dashboard at /ui with
// clean-URL semantics (no trailing slash, no .html extension).
//
// Next's static export (trailingSlash:false) writes `settings.html`, not
// `settings/index.html`, so plain gin.Static/http.FileServer would 404 or
// redirect wrong on `/ui/settings`. Resolution order for `/ui/<path>`:
//  1. Trailing slash -> redirect to slashless form (308).
//  2. Empty or `index` -> serve index.html.
//  3. `<path>` exists as a file -> serve it.
//  4. `<path>.html` exists -> serve that.
//  5. Otherwise 404.
//
// Resolved paths are clamped under `root` via filepath.Clean against `..` traversal.
func registerUIStatic(engine *gin.Engine, root string) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	handler := func(c *gin.Context) {
		raw := c.Param("filepath")
		raw = strings.TrimPrefix(raw, "/")

		// Strip trailing slash so bookmarked /ui/settings/ collapses to
		// /ui/settings. The matched param does not include the /ui prefix.
		if strings.HasSuffix(raw, "/") && raw != "" {
			target := "/ui/" + strings.TrimSuffix(raw, "/")
			c.Redirect(http.StatusPermanentRedirect, target)
			return
		}

		if raw == "" || raw == "index" {
			http.ServeFile(c.Writer, c.Request, filepath.Join(absRoot, "index.html"))
			return
		}

		cleaned := filepath.Clean("/" + raw)
		fullPath := filepath.Join(absRoot, cleaned)
		// Reject any path that escaped the root after cleaning.
		if !strings.HasPrefix(fullPath, absRoot+string(filepath.Separator)) && fullPath != absRoot {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		if info, statErr := os.Stat(fullPath); statErr == nil && !info.IsDir() {
			http.ServeFile(c.Writer, c.Request, fullPath)
			return
		}
		// Clean-URL fallback: /ui/settings → assets/ui/settings.html.
		htmlPath := fullPath + ".html"
		if info, statErr := os.Stat(htmlPath); statErr == nil && !info.IsDir() {
			http.ServeFile(c.Writer, c.Request, htmlPath)
			return
		}
		c.AbortWithStatus(http.StatusNotFound)
	}
	engine.GET("/ui", handler)
	engine.HEAD("/ui", handler)
	engine.GET("/ui/*filepath", handler)
	engine.HEAD("/ui/*filepath", handler)
}
