// Package gemini holds HTTP handlers for the native Gemini
// generateContent surface. The route shape is
// POST /v1beta/models/{model}:generateContent (and :streamGenerateContent
// for SSE), which Gemini-native clients hit directly.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/server/middleware"
	"workweave/router/internal/translate"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

const maxBodyBytes = 10 * 1024 * 1024

// generateContentSuffix and streamGenerateContentSuffix are the action
// suffixes Gemini's REST surface accepts after `{model}`.
const (
	generateContentSuffix       = ":generateContent"
	streamGenerateContentSuffix = ":streamGenerateContent"
)

// GenerateContentHandler wires
// POST /v1beta/models/:modelAction to proxy.Service.ProxyGeminiGenerateContent.
// The colon-suffixed action lives inside a single Gin path parameter
// because Gin treats `:` outside the leading position as a literal.
// The handler parses out the model name and the streaming choice,
// injects them as synthetic body fields ("model", "stream") so the
// envelope's format-neutral accessors work, and forwards.
func GenerateContentHandler(svc *proxy.Service, authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		modelAction := c.Param("modelAction")
		model, stream, ok := splitModelAction(modelAction)
		if !ok {
			writeGeminiError(c, http.StatusNotFound, "INVALID_ARGUMENT",
				fmt.Sprintf("unknown action %q: expected :generateContent or :streamGenerateContent", modelAction))
			return
		}

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
		if err != nil {
			log.Debug("Failed to read request body", "err", err)
			writeGeminiError(c, http.StatusBadRequest, "INVALID_ARGUMENT", "failed to read request body")
			return
		}
		if len(body) > maxBodyBytes {
			writeGeminiError(c, http.StatusRequestEntityTooLarge, "INVALID_ARGUMENT", "request body too large")
			return
		}

		body, err = injectModelAndStream(body, model, stream)
		if err != nil {
			log.Debug("Failed to inject Gemini synthetic fields", "err", err)
			writeGeminiError(c, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be a JSON object")
			return
		}

		ctx := stashClientIdentity(c.Request.Context(), c.Request.Header)
		ctx = resolveUser(ctx, authSvc, middleware.InstallationFrom(c))
		c.Request = c.Request.WithContext(ctx)

		if err := svc.ProxyGeminiGenerateContent(c.Request.Context(), body, c.Writer, c.Request); err != nil {
			var statusErr *providers.UpstreamStatusError
			if errors.As(err, &statusErr) {
				if c.Writer.Written() {
					return
				}
				writeGeminiError(c, statusErr.Status, "UPSTREAM_ERROR", "upstream call failed")
				return
			}
			if c.Writer.Written() {
				log.Error("Proxy failed mid-stream", "err", err)
				return
			}
			if errors.Is(err, proxy.ErrGeminiCrossFormatUnsupported) {
				writeGeminiError(c, http.StatusNotImplemented, "UNIMPLEMENTED", err.Error())
				return
			}
			if errors.Is(err, providers.ErrNotImplemented) {
				writeGeminiError(c, http.StatusNotImplemented, "UNIMPLEMENTED", err.Error())
				return
			}
			if errors.Is(err, proxy.ErrProviderNotConfigured) {
				writeGeminiError(c, http.StatusBadGateway, "FAILED_PRECONDITION", err.Error())
				return
			}
			if errors.Is(err, translate.ErrNotJSONObject) {
				writeGeminiError(c, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be a JSON object")
				return
			}
			if errors.Is(err, cluster.ErrNoEligibleProvider) {
				log.Warn("No eligible provider for Gemini request", "err", err)
				writeGeminiError(c, http.StatusBadRequest, "FAILED_PRECONDITION",
					"no provider keys available for any deployed model: register a BYOK key or supply a provider Authorization header")
				return
			}
			if errors.Is(err, cluster.ErrClusterUnavailable) {
				log.Error("Cluster routing unavailable", "err", err)
				c.Header("Retry-After", "1")
				writeGeminiError(c, http.StatusServiceUnavailable, "UNAVAILABLE",
					"router unavailable: cluster scorer failed and no fallback is configured")
				return
			}
			log.Error("Gemini proxy failed", "err", err)
			writeGeminiError(c, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream call failed")
		}
	}
}

// splitModelAction splits "{model}:{action}" into the model name and a
// stream flag. Empty model or unknown action yields ok=false.
func splitModelAction(modelAction string) (model string, stream bool, ok bool) {
	switch {
	case strings.HasSuffix(modelAction, streamGenerateContentSuffix):
		model = strings.TrimSuffix(modelAction, streamGenerateContentSuffix)
		stream = true
	case strings.HasSuffix(modelAction, generateContentSuffix):
		model = strings.TrimSuffix(modelAction, generateContentSuffix)
	default:
		return "", false, false
	}
	if model == "" {
		return "", false, false
	}
	return model, stream, true
}

// injectModelAndStream rewrites body to add a synthetic top-level
// "model" field (extracted from the URL path) and a "stream" boolean
// (true for :streamGenerateContent). Both fields are stripped before
// the body reaches the upstream Google adapter — see emit_gemini.go's
// FormatGemini same-format passthrough branch.
func injectModelAndStream(body []byte, model string, stream bool) ([]byte, error) {
	out, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, err
	}
	out, err = sjson.SetBytes(out, "stream", stream)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// resolveUser upserts the router user keyed on (installation, email) and
// returns a ctx with the user_id stashed. No-op when authSvc, installation,
// or the email signal is missing.
func resolveUser(ctx context.Context, authSvc *auth.Service, installation *auth.Installation) context.Context {
	if authSvc == nil || installation == nil {
		return ctx
	}
	id := proxy.ClientIdentityFrom(ctx)
	if id.Email == "" {
		return ctx
	}
	return authSvc.ResolveAndStashUser(ctx, installation.ID, id.Email, id.AccountID)
}

// stashClientIdentity stashes user-identification signals from HTTP
// headers onto the context. Native Gemini requests don't carry an
// Anthropic-style metadata.user_id, so only headers contribute.
func stashClientIdentity(ctx context.Context, h http.Header) context.Context {
	id := proxy.ClientIdentity{
		SessionID: h.Get("X-Claude-Code-Session-Id"),
		Email:     proxy.NormalizeEmail(h.Get("X-Weave-User-Email")),
		UserAgent: h.Get("User-Agent"),
		ClientApp: h.Get("X-App"),
	}
	return context.WithValue(ctx, proxy.ClientIdentityContextKey{}, id)
}

// writeGeminiError emits a Google API-shaped error JSON. Format
// matches generativelanguage.googleapis.com so SDK clients surface
// the message naturally.
func writeGeminiError(c *gin.Context, status int, errStatus, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"code":    status,
			"message": message,
			"status":  errStatus,
		},
	})
}
