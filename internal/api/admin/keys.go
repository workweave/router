package admin

import (
	"errors"
	"net/http"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/config"
	"workweave/router/internal/providers"

	"github.com/gin-gonic/gin"
)

type apiKeyResponse struct {
	ID              string     `json:"id"`
	Name            *string    `json:"name"`
	KeyPrefix       string     `json:"key_prefix"`
	KeySuffix       string     `json:"key_suffix"`
	LastUsedAt      *time.Time `json:"last_used_at"`
	CreatedAt       time.Time  `json:"created_at"`
	DefaultStrategy string     `json:"default_strategy"`
}

type issueAPIKeyRequest struct {
	Name string `json:"name"`
	// DefaultStrategy is the per-key routing-strategy default; see
	// auth.APIKey.DefaultStrategy. Empty (the common case) means no key
	// default -- the deployment default (cluster) applies unless the caller
	// sends x-weave-router-strategy.
	DefaultStrategy string `json:"default_strategy"`
}

type issueAPIKeyResponse struct {
	Key   apiKeyResponse `json:"key"`
	Token string         `json:"token"`
}

type updateAPIKeyDefaultStrategyRequest struct {
	DefaultStrategy string `json:"default_strategy"`
}

func toAPIKeyResponse(k *auth.APIKey) apiKeyResponse {
	return apiKeyResponse{
		ID:              k.ID,
		Name:            k.Name,
		KeyPrefix:       k.KeyPrefix,
		KeySuffix:       k.KeySuffix,
		LastUsedAt:      k.LastUsedAt,
		CreatedAt:       k.CreatedAt,
		DefaultStrategy: k.DefaultStrategy,
	}
}

// writeAPIKeyServiceError maps IssueAPIKey/RotateAPIKey/SetAPIKeyDefaultStrategy
// errors to HTTP responses. Callers pass the "action" fallback message (e.g.
// "issue", "rotate") used when the error isn't one of the recognized sentinels.
func writeAPIKeyServiceError(c *gin.Context, err error, action string) {
	if errors.Is(err, auth.ErrUnknownStrategy) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Unknown default_strategy. Must be one of: cluster, rl, hmm, bandit."})
		return
	}
	if errors.Is(err, auth.ErrAPIKeyNotFound) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "API key not found."})
		return
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to " + action + " API key."})
}

func ListAPIKeysHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		keys, err := authSvc.ListAPIKeys(c.Request.Context(), installation.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to list API keys."})
			return
		}
		out := make([]apiKeyResponse, 0, len(keys))
		for _, k := range keys {
			out = append(out, toAPIKeyResponse(k))
		}
		c.JSON(http.StatusOK, gin.H{"keys": out})
	}
}

// IssueAPIKeyHandler creates a new router API key for the installation. An
// installation may hold multiple active keys at a time; callers issue, rotate,
// and revoke them individually.
func IssueAPIKeyHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		var req issueAPIKeyRequest
		_ = c.ShouldBindJSON(&req)
		var name *string
		if req.Name != "" {
			name = &req.Name
		}
		key, rawToken, err := authSvc.IssueAPIKey(c.Request.Context(), installation.ID, name, req.DefaultStrategy, nil)
		if err != nil {
			writeAPIKeyServiceError(c, err, "issue")
			return
		}
		c.JSON(http.StatusCreated, issueAPIKeyResponse{
			Key:   toAPIKeyResponse(key),
			Token: rawToken,
		})
	}
}

// RotateAPIKeyHandler soft-deletes the specified key and issues a replacement
// against the same installation, carrying forward the previous key's name
// and default_strategy. 404 when the id is not owned by the caller's
// installation.
func RotateAPIKeyHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		id := c.Param("id")
		if id == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Missing ID."})
			return
		}
		key, rawToken, err := authSvc.RotateAPIKey(c.Request.Context(), installation.ID, id, nil)
		if err != nil {
			writeAPIKeyServiceError(c, err, "rotate")
			return
		}
		c.JSON(http.StatusCreated, issueAPIKeyResponse{
			Key:   toAPIKeyResponse(key),
			Token: rawToken,
		})
	}
}

// UpdateAPIKeyDefaultStrategyHandler flips a key's default_strategy without
// rotating (invalidating) the token itself -- the Cursor path: mint a key
// once, then flip its default strategy as needed. 404 when the id is not
// owned by the caller's installation; 400 for an unrecognized strategy.
func UpdateAPIKeyDefaultStrategyHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		id := c.Param("id")
		if id == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Missing ID."})
			return
		}
		var req updateAPIKeyDefaultStrategyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}
		if err := authSvc.SetAPIKeyDefaultStrategy(c.Request.Context(), installation.ID, id, req.DefaultStrategy); err != nil {
			writeAPIKeyServiceError(c, err, "update")
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// DeleteAPIKeyHandler soft-deletes a router API key. Returns 404 for keys
// owned by another installation so a tenant who learns a foreign key UUID
// cannot revoke it.
func DeleteAPIKeyHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		id := c.Param("id")
		if id == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Missing ID."})
			return
		}
		keys, err := authSvc.ListAPIKeys(c.Request.Context(), installation.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to look up API key."})
			return
		}
		owned := false
		for _, k := range keys {
			if k.ID == id {
				owned = true
				break
			}
		}
		if !owned {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "API key not found."})
			return
		}
		if err := authSvc.DeleteAPIKey(c.Request.Context(), installation.ID, id); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete API key."})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

type externalKeyResponse struct {
	ID         string     `json:"id"`
	Provider   string     `json:"provider"`
	Name       *string    `json:"name"`
	KeyPrefix  string     `json:"key_prefix"`
	KeySuffix  string     `json:"key_suffix"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type upsertExternalKeyRequest struct {
	Provider string  `json:"provider" binding:"required"`
	Key      string  `json:"key" binding:"required"`
	Name     *string `json:"name"`
}

func toExternalKeyResponse(k *auth.ExternalAPIKey) externalKeyResponse {
	return externalKeyResponse{
		ID:         k.ID,
		Provider:   k.Provider,
		Name:       k.Name,
		KeyPrefix:  k.KeyPrefix,
		KeySuffix:  k.KeySuffix,
		LastUsedAt: k.LastUsedAt,
		CreatedAt:  k.CreatedAt,
	}
}

func ListExternalKeysHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		keys, err := authSvc.ListExternalAPIKeys(c.Request.Context(), installation.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to list provider keys."})
			return
		}
		out := make([]externalKeyResponse, 0, len(keys))
		for _, k := range keys {
			out = append(out, toExternalKeyResponse(k))
		}
		c.JSON(http.StatusOK, gin.H{"keys": out})
	}
}

func UpsertExternalKeyHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		var req upsertExternalKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Provider and key are required."})
			return
		}
		// A provider configured via the deployment's env var (e.g. ANTHROPIC_API_KEY)
		// must not be shadowed by a dashboard BYOK key — credential resolution
		// prefers BYOK, so the stored key would silently win on every outbound call.
		// The frontend grays out env-keyed providers, but that guard is derived from
		// GET /admin/v1/config and fails open if that fetch errors; this is the only
		// backend enforcement. Mirrors the env-key check in ConfigHandler.
		if config.GetOr(providers.APIKeyEnvVar(req.Provider), "") != "" {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "Provider already configured via deployment environment variable. Remove the env var before adding a dashboard key."})
			return
		}
		key, err := authSvc.UpsertExternalAPIKey(c.Request.Context(), installation.ID, req.Provider, req.Key, req.Name, nil)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to save provider key."})
			return
		}
		c.JSON(http.StatusCreated, toExternalKeyResponse(key))
	}
}

func DeleteExternalKeyHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		id := c.Param("id")
		if id == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Missing ID."})
			return
		}
		if err := authSvc.DeleteExternalAPIKey(c.Request.Context(), installation.ID, id); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete provider key."})
			return
		}
		c.Status(http.StatusNoContent)
	}
}
