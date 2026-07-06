// Package anthropic holds HTTP handlers for the Anthropic Messages surface.
package anthropic

import (
	"context"
	"io"
	"net/http"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const maxBodyBytes = 10 * 1024 * 1024

func MessagesHandler(svc *proxy.Service, authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
		if err != nil {
			log.Debug("Failed to read request body", "err", err)
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body.")
			return
		}
		if len(body) > maxBodyBytes {
			writeAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large.")
			return
		}

		msgs := gjson.GetBytes(body, "messages")
		log.Debug("inbound anthropic request",
			"body_bytes", len(body),
			"message_count", int(msgs.Get("#").Int()),
			"system_chars", len(gjson.GetBytes(body, "system").String()),
			"model", gjson.GetBytes(body, "model").String(),
			"max_tokens", gjson.GetBytes(body, "max_tokens").Int(),
			"session_id", c.Request.Header.Get("X-Claude-Code-Session-Id"),
		)

		ctx := stashClientIdentity(c.Request.Context(), c.Request.Header, body)
		ctx = proxy.ResolveUserFromContext(ctx, authSvc, middleware.InstallationFrom(c))
		c.Request = c.Request.WithContext(ctx)

		if err := svc.ProxyMessages(c.Request.Context(), body, c.Writer, c.Request); err != nil {
			cls, ok := proxy.ClassifyDispatchError(err)
			if ok && cls.Kind == proxy.DispatchErrorUpstreamStatus {
				if c.Writer.Written() {
					return
				}
				writeAnthropicError(c, cls.Status, "api_error", cls.Message)
				return
			}
			if c.Writer.Written() {
				log.Error("Proxy failed mid-stream", "err", err)
				return
			}
			if ok {
				proxy.LogDispatchErrorClass(log, cls, err)
				if cls.RetryAfter {
					c.Header("Retry-After", "1")
				}
				writeAnthropicError(c, cls.Status, anthropicErrorType(cls.Kind), cls.Message)
				return
			}
			log.Error("Proxy failed", "err", err)
			writeAnthropicError(c, http.StatusBadGateway, "api_error", "Upstream call failed.")
			return
		}
	}
}

func stashClientIdentity(ctx context.Context, h http.Header, body []byte) context.Context {
	metaRaw := gjson.GetBytes(body, "metadata.user_id").String()
	meta := proxy.ParseClaudeCodeMetadata(metaRaw)

	// Start from the header-only identity, then overlay the body-derived
	// fields Claude Code's metadata.user_id carries that no other surface
	// sends: DeviceID/AccountID always win; SessionID/Email only override
	// when the body actually has a value, else the header-derived one from
	// ClientIdentityFromHeaders stands.
	id := proxy.ClientIdentityFromHeaders(h)
	id.DeviceID = proxy.NormalizeClientIdentifier(meta.DeviceID)
	id.AccountID = proxy.NormalizeClientIdentifier(meta.AccountID)
	if meta.SessionID != "" {
		id.SessionID = proxy.NormalizeClientIdentifier(meta.SessionID)
	}
	if metaEmail := proxy.NormalizeEmail(meta.Email); metaEmail != "" {
		id.Email = metaEmail
	}
	observability.Get().Debug("anthropic stashClientIdentity",
		"meta_raw_len", len(metaRaw),
		"meta_raw_preview", observability.Preview(metaRaw, 200),
		"parsed_email_present", meta.Email != "",
		"parsed_account_present", meta.AccountID != "",
		"parsed_device_present", meta.DeviceID != "",
		"parsed_account_len", len(meta.AccountID),
		"final_email_present", id.Email != "",
		"final_account_present", id.AccountID != "",
		"final_name_present", id.DisplayName != "",
		"header_email_present", h.Get("X-Weave-User-Email") != "",
		"header_name_present", h.Get("X-Weave-User-Name") != "",
	)
	return context.WithValue(ctx, proxy.ClientIdentityContextKey{}, id)
}

// anthropicErrorType maps a classified dispatch error to the Anthropic
// Messages error envelope's "type" field. Anthropic only distinguishes
// client-input problems ("invalid_request_error") from everything else
// ("api_error").
func anthropicErrorType(kind proxy.DispatchErrorKind) string {
	if kind.IsClientError() {
		return "invalid_request_error"
	}
	return "api_error"
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
