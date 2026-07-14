// Package server wires the HTTP engine: middleware and route registration.
package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workweave/router/internal/api/admin"
	anthropicapi "workweave/router/internal/api/anthropic"
	feedbackapi "workweave/router/internal/api/feedback"
	geminiapi "workweave/router/internal/api/gemini"
	openaiapi "workweave/router/internal/api/openai"
	"workweave/router/internal/auth"
	"workweave/router/internal/billing"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

const (
	healthTimeout    = 1 * time.Second
	readinessTimeout = 2 * time.Second
	validateTimeout  = 1 * time.Second

	messagesTimeout       = 600 * time.Second
	chatCompletionTimeout = 600 * time.Second
	passthroughTimeout    = 10 * time.Second
	routeTimeout          = 5 * time.Second
	adminTimeout          = 10 * time.Second
	// feedbackTimeout bounds the no-login feedback link reads/writes. Both are
	// single-row Postgres ops plus an async span emit, so 5s is generous.
	feedbackTimeout = 5 * time.Second
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
// billingSvc is set only in managed mode when credit-billing is enabled; it
// gates every inference route on prepaid balance via WithBalanceCheck. nil
// leaves inference routes open (BYOK/platform key still controls upstream auth).
//
// readinessChecker gates /readyz only; /health remains process liveness.
func Register(engine *gin.Engine, authSvc *auth.Service, proxySvc *proxy.Service, deployedModels admin.DeployedModelsSource, mode DeploymentMode, billingSvc *billing.Service, readinessChecker admin.HealthChecker) {
	// Managed mode bills via platform-key credits; a leftover BYOK row would
	// double-charge (upstream provider + Weave credits), so drop it here.
	byokDisabled := mode == DeploymentModeManaged

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
		engine.GET("/v1/router/models", middleware.WithTimeout(healthTimeout), admin.CatalogModelsHandler(deployedModels))

		// Projects the quality-vs-price dial's model mix across dial positions
		// for the dashboard's distribution preview. Same unauthed rationale as
		// /v1/router/models; the assertion skips sources that can't project one.
		if dist, ok := deployedModels.(admin.RoutingDistributionSource); ok {
			engine.GET("/v1/router/routing-distribution", middleware.WithTimeout(healthTimeout), admin.RoutingDistributionHandler(dist))
		}
	}

	// /validate is a token-validity probe used by clients (not the dashboard), so it stays mounted in both modes.
	adminAuthed := engine.Group("", middleware.WithTimeout(validateTimeout), middleware.WithAuth(authSvc, byokDisabled))
	adminAuthed.GET("/validate", admin.ValidateHandler)

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
		metrics := engine.Group("/admin/v1", middleware.WithTimeout(adminTimeout), middleware.WithAdminOrAuth(authSvc, byokDisabled))
		metrics.GET("/metrics/summary", admin.MetricsSummaryHandler(proxySvc))
		metrics.GET("/metrics/timeseries", admin.MetricsTimeseriesHandler(proxySvc))
		metrics.GET("/metrics/details", admin.MetricsDetailsHandler(proxySvc))

		// Mutations: admin cookie REQUIRED. rk_ tokens are rejected so a leaked data-plane key can't mint fresh router keys or rotate provider credentials.
		mgmt := engine.Group("/admin/v1", middleware.WithTimeout(adminTimeout), middleware.WithAdminOnly(authSvc))
		mgmt.GET("/keys", admin.ListAPIKeysHandler(authSvc))
		mgmt.POST("/keys", admin.IssueAPIKeyHandler(authSvc))
		mgmt.POST("/keys/:id/rotate", admin.RotateAPIKeyHandler(authSvc))
		mgmt.DELETE("/keys/:id", admin.DeleteAPIKeyHandler(authSvc))
		mgmt.GET("/provider-keys", admin.ListExternalKeysHandler(authSvc))
		mgmt.POST("/provider-keys", admin.UpsertExternalKeyHandler(authSvc))
		mgmt.DELETE("/provider-keys/:id", admin.DeleteExternalKeyHandler(authSvc))
		mgmt.GET("/config", admin.ConfigHandler)
		mgmt.GET("/routing-preferences", admin.GetRoutingPreferencesHandler(authSvc))
		mgmt.PUT("/routing-preferences", admin.UpdateRoutingPreferencesHandler(authSvc))
		if deployedModels != nil {
			mgmt.GET("/excluded-models", admin.GetExcludedModelsHandler(authSvc, deployedModels, proxySvc))
			mgmt.PUT("/excluded-models", admin.UpdateExcludedModelsHandler(authSvc, deployedModels, proxySvc))
			mgmt.GET("/excluded-providers", admin.GetExcludedProvidersHandler(authSvc, deployedModels, proxySvc))
			mgmt.PUT("/excluded-providers", admin.UpdateExcludedProvidersHandler(authSvc, deployedModels, proxySvc))
		}
	}

	messagesMiddleware := []gin.HandlerFunc{
		middleware.WithTimingEntry(),
		middleware.WithTimeout(messagesTimeout),
		middleware.WithAuth(authSvc, byokDisabled),
	}
	if billingSvc != nil {
		messagesMiddleware = append(messagesMiddleware, middleware.WithBalanceCheck(billingSvc, billing.MinBalanceMicros), middleware.WithAPIKeySpendCap(billingSvc))
	}
	messagesMiddleware = append(messagesMiddleware,
		middleware.WithEmbedOnlyUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
		middleware.WithRouterStrategyDefault(defaultStrategy, registeredStrategies...),
		middleware.WithPolicyDebugOverride(),
		middleware.WithRoutingKnobsOverride(),
	)
	messagesGroup := engine.Group("", messagesMiddleware...)
	messagesGroup.POST("/v1/messages", anthropicapi.MessagesHandler(proxySvc, authSvc))

	chatCompletionMiddleware := []gin.HandlerFunc{
		middleware.WithTimingEntry(),
		middleware.WithTimeout(chatCompletionTimeout),
		middleware.WithAuth(authSvc, byokDisabled),
	}
	if billingSvc != nil {
		chatCompletionMiddleware = append(chatCompletionMiddleware, middleware.WithBalanceCheck(billingSvc, billing.MinBalanceMicros), middleware.WithAPIKeySpendCap(billingSvc))
	}
	chatCompletionMiddleware = append(chatCompletionMiddleware,
		middleware.WithEmbedOnlyUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
		middleware.WithRouterStrategyDefault(defaultStrategy, registeredStrategies...),
		middleware.WithPolicyDebugOverride(),
		middleware.WithRoutingKnobsOverride(),
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
		middleware.WithAuth(authSvc, byokDisabled),
	)
	passthroughGroup.POST("/v1/messages/count_tokens", anthropicapi.PassthroughHandler(proxySvc))
	passthroughGroup.GET("/v1/models", anthropicapi.PassthroughHandler(proxySvc))
	passthroughGroup.GET("/v1/models/:model", anthropicapi.PassthroughHandler(proxySvc))

	routeMiddleware := []gin.HandlerFunc{
		middleware.WithTimeout(routeTimeout),
		middleware.WithAuth(authSvc, byokDisabled),
	}
	if billingSvc != nil {
		routeMiddleware = append(routeMiddleware, middleware.WithBalanceCheck(billingSvc, billing.MinBalanceMicros), middleware.WithAPIKeySpendCap(billingSvc))
	}
	routeMiddleware = append(routeMiddleware,
		middleware.WithEmbedOnlyUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
		middleware.WithRouterStrategyDefault(defaultStrategy, registeredStrategies...),
		middleware.WithPolicyDebugOverride(),
		middleware.WithRoutingKnobsOverride(),
	)
	routeGroup := engine.Group("", routeMiddleware...)
	routeGroup.POST("/v1/route", anthropicapi.RouteHandler(proxySvc))

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
