// Package feedback holds the HTTP handlers for the no-login feedback link
// surface (`/v1/feedback/link`). The signed HMAC token in the URL / body is the
// sole credential — these routes carry no auth middleware — and is verified via
// the proxy service, which also reads routing context and persists submissions.
package feedback

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	token "workweave/router/internal/feedback"
	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// maxBodyBytes caps the feedback submission payload. A rating + short comment is
// tiny; this only guards against a hostile client streaming an unbounded body.
const maxBodyBytes = 64 * 1024

type submittedFeedback struct {
	Rating  string  `json:"rating"`
	Comment *string `json:"comment,omitempty"`
}

// contextResponse is the JSON shape the public feedback page consumes
// (RouterFeedbackContext on the frontend). Field names are the contract.
type contextResponse struct {
	RequestID      string             `json:"request_id"`
	ChosenModel    string             `json:"chosen_model,omitempty"`
	ChosenProvider string             `json:"chosen_provider,omitempty"`
	ClientApp      string             `json:"client_app,omitempty"`
	RoutedAt       string             `json:"routed_at,omitempty"`
	Feedback       *submittedFeedback `json:"feedback"`
}

type submitRequest struct {
	Token   string  `json:"token"`
	Rating  string  `json:"rating"`
	Comment *string `json:"comment"`
}

// GetContextHandler serves GET /v1/feedback/link/:token: verify the token and
// return the request's routing context plus any prior feedback.
func GetContextHandler(svc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		claims, err := svc.VerifyFeedbackToken(c.Param("token"))
		if err != nil {
			writeTokenError(c, err)
			return
		}

		fctx, err := svc.GetFeedbackContext(c.Request.Context(), claims.InstallationID, claims.RequestID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Token is valid but no telemetry row exists (telemetry disabled or
			// pruned past retention). Render the page with just the request id
			// so the user can still leave feedback.
			c.JSON(http.StatusOK, contextResponse{RequestID: claims.RequestID})
			return
		}
		if err != nil {
			log.Error("Failed to load feedback context", "err", err, "request_id", claims.RequestID)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to load context"})
			return
		}
		c.JSON(http.StatusOK, toContextResponse(fctx))
	}
}

// SubmitHandler serves POST /v1/feedback/link: verify the token in the body and
// persist the rating + optional comment.
func SubmitHandler(svc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
		if err != nil || len(body) > maxBodyBytes {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
			return
		}
		var req submitRequest
		if jsonErr := json.Unmarshal(body, &req); jsonErr != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
			return
		}
		if req.Rating != "up" && req.Rating != "down" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_rating"})
			return
		}

		claims, err := svc.VerifyFeedbackToken(req.Token)
		if err != nil {
			writeTokenError(c, err)
			return
		}

		err = svc.SubmitFeedback(c.Request.Context(), proxy.SubmitFeedbackParams{
			InstallationID: claims.InstallationID,
			ExternalID:     claims.ExternalID,
			RequestID:      claims.RequestID,
			RouterUserID:   claims.RouterUserID,
			Rating:         req.Rating,
			Comment:        normalizeComment(req.Comment),
		})
		if err != nil {
			log.Error("Failed to submit feedback", "err", err, "request_id", claims.RequestID)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to submit feedback"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// RateHandler serves GET /v1/feedback/rate?t=<token>&r=up|down: the one-click
// thumb link embedded in a response footer. It records the rating straight from
// the GET (no form page) and returns a tiny HTML confirmation. The signed token
// is the sole credential, so these links carry no auth middleware. Note: a GET
// that mutates can be triggered by link prefetchers, but the token is
// unguessable and per-request, so the blast radius is one request's own rating.
func RateHandler(svc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		rating := c.Query("r")
		if rating != "up" && rating != "down" {
			c.Data(http.StatusBadRequest, "text/html; charset=utf-8", ratePageError("That rating link looks malformed."))
			return
		}

		claims, err := svc.VerifyFeedbackToken(c.Query("t"))
		if err != nil {
			msg := "This feedback link is invalid."
			if errors.Is(err, token.ErrExpiredToken) {
				msg = "This feedback link has expired."
			}
			status := http.StatusNotFound
			if errors.Is(err, token.ErrExpiredToken) {
				status = http.StatusGone
			}
			c.Data(status, "text/html; charset=utf-8", ratePageError(msg))
			return
		}

		err = svc.SubmitFeedback(c.Request.Context(), proxy.SubmitFeedbackParams{
			InstallationID: claims.InstallationID,
			ExternalID:     claims.ExternalID,
			RequestID:      claims.RequestID,
			RouterUserID:   claims.RouterUserID,
			Rating:         rating,
		})
		if err != nil {
			log.Error("Failed to record one-click feedback", "err", err, "request_id", claims.RequestID, "rating", rating)
			c.Data(http.StatusInternalServerError, "text/html; charset=utf-8", ratePageError("Sorry — we couldn't record that. Please try again."))
			return
		}

		c.Data(http.StatusOK, "text/html; charset=utf-8", ratePageSuccess())
	}
}

func toContextResponse(f proxy.FeedbackContext) contextResponse {
	resp := contextResponse{
		RequestID:      f.RequestID,
		ChosenModel:    f.ChosenModel,
		ChosenProvider: f.ChosenProvider,
		ClientApp:      f.ClientApp,
	}
	if !f.RoutedAt.IsZero() {
		resp.RoutedAt = f.RoutedAt.UTC().Format(time.RFC3339)
	}
	if f.Rating != "" {
		resp.Feedback = &submittedFeedback{Rating: f.Rating, Comment: f.Comment}
	}
	return resp
}

// normalizeComment trims whitespace and collapses an empty comment to nil so
// "" and "no comment" are indistinguishable downstream.
func normalizeComment(comment *string) *string {
	if comment == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*comment)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// writeTokenError maps token verification failures to the HTTP status codes the
// frontend distinguishes: 410 for an expired link, 404 for anything else.
func writeTokenError(c *gin.Context, err error) {
	if errors.Is(err, token.ErrExpiredToken) {
		c.AbortWithStatusJSON(http.StatusGone, gin.H{"error": "expired"})
		return
	}
	c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "invalid"})
}
