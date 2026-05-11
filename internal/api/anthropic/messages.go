// Package anthropic holds HTTP handlers for the Anthropic Messages surface.
// The handler is intentionally thin: it adapts gin ↔ proxy.Service and shapes
// errors back into Anthropic's wire format.
package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime/debug"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/server/middleware"
	"workweave/router/internal/translate"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var writerTraceEnabled = os.Getenv("ROUTER_DEBUG_WRITER_TRACE") == "true"

type tracingWriter struct {
	gin.ResponseWriter
}

func (t *tracingWriter) WriteHeader(code int) {
	observability.Get().Debug("ResponseWriter WriteHeader called",
		"code", code,
		"already_written", t.ResponseWriter.Written(),
		"current_status", t.ResponseWriter.Status(),
		"stack", string(debug.Stack()),
	)
	t.ResponseWriter.WriteHeader(code)
}

func (t *tracingWriter) Write(b []byte) (int, error) {
	if !t.ResponseWriter.Written() {
		observability.Get().Debug("ResponseWriter implicit-200 via Write",
			"bytes", len(b),
			"stack", string(debug.Stack()),
		)
	}
	return t.ResponseWriter.Write(b)
}

const maxBodyBytes = 10 * 1024 * 1024

// MessagesHandler wires POST /v1/messages to proxy.Service.ProxyMessages.
// authSvc is used to upsert the end-user identity (router.model_router_users)
// once email is parsed from the body; pass nil to skip user resolution (tests).
func MessagesHandler(svc *proxy.Service, authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
		if err != nil {
			log.Debug("Failed to read request body", "err", err)
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}
		if len(body) > maxBodyBytes {
			writeAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
			return
		}

		ctx := stashClientIdentity(c.Request.Context(), c.Request.Header, body)
		ctx = proxy.ResolveUserFromContext(ctx, authSvc, middleware.InstallationFrom(c))
		c.Request = c.Request.WithContext(ctx)

		var w http.ResponseWriter = c.Writer
		if writerTraceEnabled {
			w = &tracingWriter{ResponseWriter: c.Writer}
		}
		if err := svc.ProxyMessages(c.Request.Context(), body, w, c.Request); err != nil {
			var statusErr *providers.UpstreamStatusError
			if errors.As(err, &statusErr) {
				if c.Writer.Written() {
					return
				}
				writeAnthropicError(c, statusErr.Status, "api_error", "upstream call failed")
				return
			}
			if c.Writer.Written() {
				log.Error("Proxy failed mid-stream", "err", err)
				return
			}
			if errors.Is(err, providers.ErrNotImplemented) {
				writeAnthropicError(c, http.StatusNotImplemented, "api_error", "provider not configured")
				return
			}
			if errors.Is(err, translate.ErrNotJSONObject) {
				writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
				return
			}
			if errors.Is(err, cluster.ErrNoEligibleProvider) {
				log.Warn("No eligible provider for request", "err", err)
				writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "no provider keys available for any deployed model: register a BYOK key or supply a provider Authorization header")
				return
			}
			if errors.Is(err, cluster.ErrClusterUnavailable) {
				log.Error("Cluster routing unavailable", "err", err)
				c.Header("Retry-After", "1")
				writeAnthropicError(c, http.StatusServiceUnavailable, "api_error", "router unavailable: cluster scorer failed and no fallback is configured")
				return
			}
			log.Error("Proxy failed", "err", err)
			writeAnthropicError(c, http.StatusBadGateway, "api_error", "upstream call failed")
			return
		}
	}
}

// stashClientIdentity extracts user identification signals from HTTP headers
// and the Anthropic metadata.user_id body field, then stashes them on the
// context for downstream OTEL spans and the decision sidecar log.
func stashClientIdentity(ctx context.Context, h http.Header, body []byte) context.Context {
	metaRaw := gjson.GetBytes(body, "metadata.user_id").String()
	meta := proxy.ParseClaudeCodeMetadata(metaRaw)
	sessionID := meta.SessionID
	if sessionID == "" {
		sessionID = h.Get("X-Claude-Code-Session-Id")
	}
	email := proxy.NormalizeEmail(meta.Email)
	if email == "" {
		email = proxy.NormalizeEmail(h.Get("X-Weave-User-Email"))
	}
	id := proxy.ClientIdentity{
		DeviceID:  proxy.NormalizeClientIdentifier(meta.DeviceID),
		AccountID: proxy.NormalizeClientIdentifier(meta.AccountID),
		SessionID: proxy.NormalizeClientIdentifier(sessionID),
		Email:     email,
		UserAgent: h.Get("User-Agent"),
		ClientApp: h.Get("X-App"),
	}
	return context.WithValue(ctx, proxy.ClientIdentityContextKey{}, id)
}

func writeAnthropicError(c *gin.Context, status int, errType, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}
