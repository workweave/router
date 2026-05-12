// Package server wires the HTTP engine: middleware and route registration.
package server

import (
	"net/http"
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

// Register wires routes onto the engine. devModeNoAuth skips bearer-auth on
// /v1/* for local development. In managed mode the dashboard + /admin/v1/*
// routes are not registered at all.
func Register(engine *gin.Engine, authSvc *auth.Service, proxySvc *proxy.Service, devModeNoAuth bool, mode DeploymentMode) {
	engine.GET("/health", middleware.WithTimeout(healthTimeout), admin.HealthHandler)

	// /validate is a token-validity probe used by clients (not the dashboard), so it stays mounted in both modes.
	adminAuthed := engine.Group("", middleware.WithTimeout(validateTimeout), middleware.WithAuth(authSvc))
	adminAuthed.GET("/validate", admin.ValidateHandler)

	if mode == DeploymentModeSelfHosted {
		engine.GET("/", func(c *gin.Context) { c.Redirect(http.StatusFound, "/ui/") })
		engine.Static("/ui", "./assets/ui")

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
	}

	messagesAuth := []gin.HandlerFunc{middleware.WithTimingEntry(), middleware.WithTimeout(messagesTimeout)}
	if !devModeNoAuth {
		messagesAuth = append(messagesAuth, middleware.WithAuth(authSvc))
	}
	messagesAuth = append(messagesAuth,
		middleware.WithEmbedLastUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
	)
	messagesGroup := engine.Group("", messagesAuth...)
	messagesGroup.POST("/v1/messages", anthropicapi.MessagesHandler(proxySvc, authSvc))

	chatCompletionAuth := []gin.HandlerFunc{middleware.WithTimingEntry(), middleware.WithTimeout(chatCompletionTimeout)}
	if !devModeNoAuth {
		chatCompletionAuth = append(chatCompletionAuth, middleware.WithAuth(authSvc))
	}
	chatCompletionAuth = append(chatCompletionAuth,
		middleware.WithEmbedLastUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
	)
	chatCompletionGroup := engine.Group("", chatCompletionAuth...)
	chatCompletionGroup.POST("/v1/chat/completions", openaiapi.ChatCompletionHandler(proxySvc, authSvc))
	// Action suffix (:generateContent or :streamGenerateContent) lives inside modelAction because Gin treats `:` outside the leading position as a literal.
	chatCompletionGroup.POST("/v1beta/models/:modelAction", geminiapi.GenerateContentHandler(proxySvc, authSvc))

	passthroughAuth := []gin.HandlerFunc{middleware.WithTimeout(passthroughTimeout)}
	if !devModeNoAuth {
		passthroughAuth = append(passthroughAuth, middleware.WithAuth(authSvc))
	}
	passthroughGroup := engine.Group("", passthroughAuth...)
	passthroughGroup.POST("/v1/messages/count_tokens", anthropicapi.PassthroughHandler(proxySvc))
	passthroughGroup.GET("/v1/models", anthropicapi.PassthroughHandler(proxySvc))
	passthroughGroup.GET("/v1/models/:model", anthropicapi.PassthroughHandler(proxySvc))

	routeAuth := []gin.HandlerFunc{middleware.WithTimeout(routeTimeout)}
	if !devModeNoAuth {
		routeAuth = append(routeAuth, middleware.WithAuth(authSvc))
	}
	routeAuth = append(routeAuth,
		middleware.WithEmbedLastUserMessageOverride(),
		middleware.WithClusterVersionOverride(),
	)
	routeGroup := engine.Group("", routeAuth...)
	routeGroup.POST("/v1/route", anthropicapi.RouteHandler(proxySvc))
}
