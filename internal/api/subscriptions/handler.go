// Package subscriptions exposes the data-plane enrollment API for the per-user
// subscription credential pool: POST/GET/DELETE /v1/subscriptions, authenticated
// by the installation's router (rk_) key. The npx login command runs the OAuth
// flow locally and pushes the resulting token bundle here.
package subscriptions

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/server/middleware"
	"workweave/router/internal/subscriptions"

	"github.com/gin-gonic/gin"
)

// subscriptionTokenPrefix marks a Claude subscription (Claude.ai OAuth) bearer.
const subscriptionTokenPrefix = "sk-ant-oat"

// Enroller is the pool operations the handlers need. Satisfied by
// *subscriptions.Service.
type Enroller interface {
	Enroll(ctx context.Context, p subscriptions.EnrollParams) (*subscriptions.Credential, error)
	List(ctx context.Context, installationID, userEmail string) ([]*subscriptions.Credential, error)
	Remove(ctx context.Context, installationID, userEmail, id string) error
}

type enrollRequest struct {
	Provider         string `json:"provider"`
	UserEmail        string `json:"user_email"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresAt        string `json:"expires_at"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	ClaudeAccountID  string `json:"claude_account_id"`
	AccountLabel     string `json:"account_label"`
}

// callerEmail is the authenticated caller's normalized identity, taken from the
// X-Weave-User-Email header — the same self-asserted identity the proxy scopes
// a turn's pooled lookup to. Enroll/list/delete bind to it (not an arbitrary
// request-supplied user_email) so one installation-key holder can't enumerate
// or delete another user's credentials. Header trust matches the turn path; it
// is not a cryptographic identity (closing that needs per-user auth).
func callerEmail(c *gin.Context) string {
	return proxy.ClientIdentityFromHeaders(c.Request.Header).Email
}

type credentialResponse struct {
	ID                 string     `json:"id"`
	Provider           string     `json:"provider"`
	AccountLabel       string     `json:"account_label,omitempty"`
	AccessTokenExpires *time.Time `json:"access_token_expires_at,omitempty"`
	LastUsedAt         *time.Time `json:"last_used_at,omitempty"`
	RefreshFailed      bool       `json:"refresh_failed"`
	CreatedAt          time.Time  `json:"created_at"`
}

func toResponse(c *subscriptions.Credential) credentialResponse {
	out := credentialResponse{
		ID:            c.ID,
		Provider:      c.Provider,
		AccountLabel:  c.AccountLabel,
		RefreshFailed: !c.RefreshFailedAt.IsZero(),
		CreatedAt:     c.CreatedAt,
	}
	if !c.ExpiresAt.IsZero() {
		t := c.ExpiresAt
		out.AccessTokenExpires = &t
	}
	if !c.LastUsedAt.IsZero() {
		t := c.LastUsedAt
		out.LastUsedAt = &t
	}
	return out
}

// EnrollHandler stores a new subscription credential in the caller's pool.
func EnrollHandler(svc Enroller) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation := middleware.InstallationFrom(c)
		if installation == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_key"})
			return
		}
		var req enrollRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		provider := subscriptions.NormalizeProvider(req.Provider)
		if provider == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Unsupported provider; expected 'anthropic' or 'openai'."})
			return
		}
		email := callerEmail(c)
		if email == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "A valid X-Weave-User-Email header is required."})
			return
		}
		if bodyEmail := proxy.NormalizeEmail(req.UserEmail); bodyEmail != "" && bodyEmail != email {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "user_email does not match the authenticated X-Weave-User-Email."})
			return
		}
		accessToken := strings.TrimSpace(req.AccessToken)
		refreshToken := strings.TrimSpace(req.RefreshToken)
		if accessToken == "" || refreshToken == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Both access_token and refresh_token are required."})
			return
		}
		if err := validateTokenShape(provider, accessToken, req.ChatGPTAccountID, req.ClaudeAccountID); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		expiresAt := parseExpiry(req.ExpiresAt)

		cred, err := svc.Enroll(c.Request.Context(), subscriptions.EnrollParams{
			InstallationID:   installation.ID,
			UserEmail:        email,
			Provider:         provider,
			AccountLabel:     strings.TrimSpace(req.AccountLabel),
			ChatGPTAccountID: strings.TrimSpace(req.ChatGPTAccountID),
			ClaudeAccountID:  strings.TrimSpace(req.ClaudeAccountID),
			AccessToken:      accessToken,
			RefreshToken:     refreshToken,
			ExpiresAt:        expiresAt,
			CreatedBy:        email,
		})
		if err != nil {
			observability.FromGin(c).Error("Failed to enroll subscription credential", "err", err, "provider", provider)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to enroll subscription."})
			return
		}
		c.JSON(http.StatusCreated, toResponse(cred))
	}
}

// ListHandler returns the caller's enrolled credentials for a user_email.
func ListHandler(svc Enroller) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation := middleware.InstallationFrom(c)
		if installation == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_key"})
			return
		}
		email := callerEmail(c)
		if email == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "A valid X-Weave-User-Email header is required."})
			return
		}
		if q := proxy.NormalizeEmail(c.Query("user_email")); q != "" && q != email {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "user_email does not match the authenticated X-Weave-User-Email."})
			return
		}
		creds, err := svc.List(c.Request.Context(), installation.ID, email)
		if err != nil {
			observability.FromGin(c).Error("Failed to list subscription credentials", "err", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to list subscriptions."})
			return
		}
		out := make([]credentialResponse, 0, len(creds))
		for _, cred := range creds {
			out = append(out, toResponse(cred))
		}
		c.JSON(http.StatusOK, gin.H{"subscriptions": out})
	}
}

// RemoveHandler soft-deletes one credential scoped to the caller's installation
// and user_email.
func RemoveHandler(svc Enroller) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation := middleware.InstallationFrom(c)
		if installation == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_key"})
			return
		}
		email := callerEmail(c)
		if email == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "A valid X-Weave-User-Email header is required."})
			return
		}
		if q := proxy.NormalizeEmail(c.Query("user_email")); q != "" && q != email {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "user_email does not match the authenticated X-Weave-User-Email."})
			return
		}
		id := c.Param("id")
		err := svc.Remove(c.Request.Context(), installation.ID, email, id)
		if errors.Is(err, subscriptions.ErrCredentialNotFound) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "Subscription credential not found."})
			return
		}
		if err != nil {
			observability.FromGin(c).Error("Failed to remove subscription credential", "err", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove subscription."})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// validateTokenShape rejects tokens that don't match the enrolled provider so a
// router key or cross-provider key can't be smuggled into an OAuth slot. Both
// providers must also supply their stable account id: fingerprinting keys on it
// so re-enrolling the same account replaces its row, and without it the
// fingerprint would fall back to the rotating refresh token and duplicate the
// pool on every fresh login.
func validateTokenShape(provider, accessToken, chatGPTAccountID, claudeAccountID string) error {
	if auth.HasAPIKeyPrefix(accessToken) {
		return errors.New("access_token must be a subscription OAuth token, not a router key")
	}
	switch provider {
	case providers.ProviderAnthropic:
		if !strings.HasPrefix(accessToken, subscriptionTokenPrefix) {
			return errors.New("Claude access_token must be a subscription OAuth token (sk-ant-oat…)")
		}
		if strings.TrimSpace(claudeAccountID) == "" {
			return errors.New("claude_account_id is required for an anthropic subscription")
		}
	case providers.ProviderOpenAI:
		if strings.HasPrefix(accessToken, "sk-") {
			return errors.New("ChatGPT access_token must be a subscription JWT, not an OpenAI API key")
		}
		if strings.TrimSpace(chatGPTAccountID) == "" {
			return errors.New("chatgpt_account_id is required for an openai subscription")
		}
	}
	return nil
}

// parseExpiry parses an RFC3339 expiry, returning the zero time on absence or
// parse failure (the pool refreshes proactively regardless).
func parseExpiry(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
