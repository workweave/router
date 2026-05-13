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
	geminiapi "workweave/router/internal/api/gemini"
	openaiapi "workweave/router/internal/api/openai"
	"workweave/router/internal/auth"
	"workweave/router/internal/proxy"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

const (
	healthTimeout   = 1 * time.Second
	validateTimeout = 1 * time.Second

	messagesTimeout       = 600 * time.Second
	chatCompletionTimeout = 600 * time.Second
	passthroughTimeout    = 10 * time.Second
	routeTimeout          = 5 * time.Second
	adminTimeout          = 10 * time.Second
)

// DeploymentMode gates whether the self-hoster admin dashboard and its
// /admin/v1/* API are mounted. In Weave-managed (SaaS) deployments the
// dashboard is redundant attack surface — keys, BYOK secrets, and config
// are owned by the Weave control plane.
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
// deployedModels is the source for the admin model-selection checklist. May
// be nil in tests; required in selfhosted production so the dashboard can
// render the universe of routable models.
func Register(engine *gin.Engine, authSvc *auth.Service, proxySvc *proxy.Service, deployedModels admin.DeployedModelsSource, mode DeploymentMode) {
	engine.GET("/health", middleware.WithTimeout(healthTimeout), admin.HealthHandler)

	// /validate is a token-validity probe used by clients (not the dashboard), so it stays mounted in both modes.
	adminAuthed := engine.Group("", middleware.WithTimeout(validateTimeout), middleware.WithAuth(authSvc))
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
		metrics := engine.Group("/admin/v1", middleware.WithTimeout(adminTimeout), middleware.WithAdminOrAuth(authSvc))
		metrics.GET("/metrics/summary", admin.MetricsSummaryHandler(proxySvc))
		metrics.GET("/metrics/timeseries", admin.MetricsTimeseriesHandler(proxySvc))
		metrics.GET("/metrics/details", admin.MetricsDetailsHandler(proxySvc))

		// Mutations: admin cookie REQUIRED. rk_ tokens are rejected so a leaked data-plane key can't mint fresh router keys or rotate provider credentials.
		mgmt := engine.Group("/admin/v1", middleware.WithTimeout(adminTimeout), middleware.WithAdminOnly(authSvc))
		mgmt.GET("/keys", admin.ListAPIKeysHandler(authSvc))
		mgmt.POST("/keys", admin.IssueAPIKeyHandler(authSvc))
		mgmt.POST("/keys/rotate", admin.RotateAPIKeyHandler(authSvc))
		mgmt.DELETE("/keys/:id", admin.DeleteAPIKeyHandler(authSvc))
		mgmt.GET("/provider-keys", admin.ListExternalKeysHandler(authSvc))
		mgmt.POST("/provider-keys", admin.UpsertExternalKeyHandler(authSvc))
		mgmt.DELETE("/provider-keys/:id", admin.DeleteExternalKeyHandler(authSvc))
		mgmt.GET("/config", admin.ConfigHandler)
		if deployedModels != nil {
			mgmt.GET("/excluded-models", admin.GetExcludedModelsHandler(authSvc, deployedModels, proxySvc))
			mgmt.PUT("/excluded-models", admin.UpdateExcludedModelsHandler(authSvc, deployedModels, proxySvc))
		}
	}

	messagesGroup := engine.Group("",
		middleware.WithTimingEntry(),
		middleware.WithTimeout(messagesTimeout),
		middleware.WithAuth(authSvc),
		middleware.WithEmbedLastUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
	)
	messagesGroup.POST("/v1/messages", anthropicapi.MessagesHandler(proxySvc, authSvc))

	chatCompletionGroup := engine.Group("",
		middleware.WithTimingEntry(),
		middleware.WithTimeout(chatCompletionTimeout),
		middleware.WithAuth(authSvc),
		middleware.WithEmbedLastUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
	)
	chatCompletionGroup.POST("/v1/chat/completions", openaiapi.ChatCompletionHandler(proxySvc, authSvc))
	// Action suffix (:generateContent or :streamGenerateContent) lives inside modelAction because Gin treats `:` outside the leading position as a literal.
	chatCompletionGroup.POST("/v1beta/models/:modelAction", geminiapi.GenerateContentHandler(proxySvc, authSvc))

	passthroughGroup := engine.Group("",
		middleware.WithTimeout(passthroughTimeout),
		middleware.WithAuth(authSvc),
	)
	passthroughGroup.POST("/v1/messages/count_tokens", anthropicapi.PassthroughHandler(proxySvc))
	passthroughGroup.GET("/v1/models", anthropicapi.PassthroughHandler(proxySvc))
	passthroughGroup.GET("/v1/models/:model", anthropicapi.PassthroughHandler(proxySvc))

	routeGroup := engine.Group("",
		middleware.WithTimeout(routeTimeout),
		middleware.WithAuth(authSvc),
		middleware.WithEmbedLastUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
	)
	routeGroup.POST("/v1/route", anthropicapi.RouteHandler(proxySvc))
}

// registerUIStatic mounts the exported Next.js dashboard at /ui with
// clean-URL semantics (no trailing slash, no .html extension visible).
//
// Next is configured with trailingSlash:false, so its static export writes
// files as `settings.html` rather than `settings/index.html`. We can't use
// gin.Static / http.FileServer directly: FileServer would either 404 on
// `/ui/settings` or redirect `/ui/settings` → `/ui/settings/` (when
// `settings/index.html` exists), neither of which is what we want here.
//
// Resolution order for a request to `/ui/<path>`:
//  1. If <path> ends with `/`, redirect to the slashless form (308).
//  2. If <path> is empty or `index`, serve `index.html`.
//  3. If `<path>` exists as a regular file under assets/ui, serve it.
//  4. If `<path>.html` exists, serve that.
//  5. Otherwise 404.
//
// All resolved paths are clamped under `root` via filepath.Clean to block
// `..` traversal regardless of what gin's wildcard captures.
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
