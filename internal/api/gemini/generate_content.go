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
	"workweave/router/internal/proxy"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

const maxBodyBytes = 10 * 1024 * 1024

const (
	generateContentSuffix       = ":generateContent"
	streamGenerateContentSuffix = ":streamGenerateContent"
)

// GenerateContentHandler wires POST /v1beta/models/:modelAction to proxy.Service.ProxyGeminiGenerateContent.
// The colon-suffixed action lives inside a single Gin path parameter because Gin treats `:` outside the
// leading position as a literal.
func GenerateContentHandler(svc *proxy.Service, authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		modelAction := c.Param("modelAction")
		model, stream, ok := splitModelAction(modelAction)
		if !ok {
			writeGeminiError(c, http.StatusNotFound, "INVALID_ARGUMENT",
				fmt.Sprintf("Unknown action %q: expected :generateContent or :streamGenerateContent.", modelAction))
			return
		}

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
		if err != nil {
			log.Debug("Failed to read request body", "err", err)
			writeGeminiError(c, http.StatusBadRequest, "INVALID_ARGUMENT", "Failed to read request body.")
			return
		}
		if len(body) > maxBodyBytes {
			writeGeminiError(c, http.StatusRequestEntityTooLarge, "INVALID_ARGUMENT", "Request body too large.")
			return
		}

		body, err = injectModelAndStream(body, model, stream)
		if err != nil {
			log.Debug("Failed to inject Gemini synthetic fields", "err", err)
			writeGeminiError(c, http.StatusBadRequest, "INVALID_ARGUMENT", "Request body must be a JSON object.")
			return
		}

		ctx := context.WithValue(c.Request.Context(), proxy.ClientIdentityContextKey{}, proxy.ClientIdentityFromHeaders(c.Request.Header))
		ctx = proxy.ResolveUserFromContext(ctx, authSvc, middleware.InstallationFrom(c))
		c.Request = c.Request.WithContext(ctx)

		if err := svc.ProxyGeminiGenerateContent(c.Request.Context(), body, c.Writer, c.Request); err != nil {
			cls, ok := proxy.ClassifyDispatchError(err)
			if ok && cls.Kind == proxy.DispatchErrorUpstreamStatus {
				if c.Writer.Written() {
					return
				}
				writeGeminiError(c, cls.Status, "UPSTREAM_ERROR", cls.Message)
				return
			}
			if c.Writer.Written() {
				log.Error("Proxy failed mid-stream", "err", err)
				return
			}
			if errors.Is(err, proxy.ErrGeminiCrossFormatUnsupported) {
				writeGeminiError(c, http.StatusNotImplemented, "UNIMPLEMENTED", "Cross-format request not supported by the upstream Gemini provider.")
				return
			}
			if ok {
				proxy.LogDispatchErrorClass(log, cls, err)
				if cls.RetryAfter {
					c.Header("Retry-After", "1")
				}
				writeGeminiError(c, cls.Status, geminiErrorStatus(cls.Kind), cls.Message)
				return
			}
			log.Error("Gemini proxy failed", "err", err)
			writeGeminiError(c, http.StatusBadGateway, "UPSTREAM_ERROR", "Upstream call failed.")
		}
	}
}

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

// geminiErrorStatus maps a classified dispatch error to the Gemini native
// error envelope's "status" field. Gemini's taxonomy is finer-grained than
// Anthropic/OpenAI's two-way split, so this switches on Kind directly rather
// than reusing DispatchErrorKind.IsClientError.
func geminiErrorStatus(kind proxy.DispatchErrorKind) string {
	switch kind {
	case proxy.DispatchErrorNotImplemented:
		return "UNIMPLEMENTED"
	case proxy.DispatchErrorProviderNotConfigured:
		return "FAILED_PRECONDITION"
	case proxy.DispatchErrorRequestNotJSONObject:
		return "INVALID_ARGUMENT"
	case proxy.DispatchErrorNoEligibleProvider:
		return "FAILED_PRECONDITION"
	case proxy.DispatchErrorInvalidRoutingKnobs:
		return "INVALID_ARGUMENT"
	case proxy.DispatchErrorRLPolicyUnavailable, proxy.DispatchErrorBanditUnavailable, proxy.DispatchErrorClusterUnavailable:
		return "UNAVAILABLE"
	default:
		return "UPSTREAM_ERROR"
	}
}

func writeGeminiError(c *gin.Context, status int, errStatus, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"code":    status,
			"message": message,
			"status":  errStatus,
		},
	})
}
