// Package server wires the HTTP engine: middleware, route registration, and
// (later) streaming-flush helpers.
package server

import (
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
)

// Register wires routes onto the engine. devModeNoAuth skips bearer-auth on
// /v1/* for local development.
func Register(engine *gin.Engine, authSvc *auth.Service, proxySvc *proxy.Service, devModeNoAuth bool) {
	engine.GET("/health", middleware.WithTimeout(healthTimeout), admin.HealthHandler)

	adminAuthed := engine.Group("", middleware.WithTimeout(validateTimeout), middleware.WithAuth(authSvc))
	adminAuthed.GET("/validate", admin.ValidateHandler)

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
	// Native Gemini ingress shares the chat-completion timeout and
	// middleware budget. The action suffix (:generateContent or
	// :streamGenerateContent) lives inside the modelAction parameter
	// because Gin treats `:` outside the leading position as a literal.
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
