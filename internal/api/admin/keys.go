package admin

import (
	"net/http"
	"time"

	"workweave/router/internal/auth"

	"github.com/gin-gonic/gin"
)

// --- Router API keys ---

type apiKeyResponse struct {
	ID         string     `json:"id"`
	Name       *string    `json:"name"`
	KeyPrefix  string     `json:"key_prefix"`
	KeySuffix  string     `json:"key_suffix"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type issueAPIKeyRequest struct {
	Name string `json:"name"`
}

type issueAPIKeyResponse struct {
	Key   apiKeyResponse `json:"key"`
	Token string         `json:"token"`
}

func toAPIKeyResponse(k *auth.APIKey) apiKeyResponse {
	return apiKeyResponse{
		ID:         k.ID,
		Name:       k.Name,
		KeyPrefix:  k.KeyPrefix,
		KeySuffix:  k.KeySuffix,
		LastUsedAt: k.LastUsedAt,
		CreatedAt:  k.CreatedAt,
	}
}

// ListAPIKeysHandler returns all active router API keys for the authed installation.
func ListAPIKeysHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		keys, err := authSvc.ListAPIKeys(c.Request.Context(), installation.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to list api keys"})
			return
		}
		out := make([]apiKeyResponse, 0, len(keys))
		for _, k := range keys {
			out = append(out, toAPIKeyResponse(k))
		}
		c.JSON(http.StatusOK, gin.H{"keys": out})
	}
}

// IssueAPIKeyHandler creates a new router API key. Returns the raw token once.
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
		key, rawToken, err := authSvc.IssueAPIKey(c.Request.Context(), installation.ID, name, nil)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to issue api key"})
			return
		}
		c.JSON(http.StatusCreated, issueAPIKeyResponse{
			Key:   toAPIKeyResponse(key),
			Token: rawToken,
		})
	}
}

// DeleteAPIKeyHandler soft-deletes a router API key by ID.
func DeleteAPIKeyHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := resolveInstallation(c, authSvc); !ok {
			return
		}
		id := c.Param("id")
		if id == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing id"})
			return
		}
		if err := authSvc.DeleteAPIKey(c.Request.Context(), id); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to delete api key"})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// --- Provider (external) API keys ---

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

// ListExternalKeysHandler returns all active provider API keys for the authed installation.
func ListExternalKeysHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		keys, err := authSvc.ListExternalAPIKeys(c.Request.Context(), installation.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to list provider keys"})
			return
		}
		out := make([]externalKeyResponse, 0, len(keys))
		for _, k := range keys {
			out = append(out, toExternalKeyResponse(k))
		}
		c.JSON(http.StatusOK, gin.H{"keys": out})
	}
}

// UpsertExternalKeyHandler creates or replaces a provider API key.
func UpsertExternalKeyHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		var req upsertExternalKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "provider and key are required"})
			return
		}
		key, err := authSvc.UpsertExternalAPIKey(c.Request.Context(), installation.ID, req.Provider, req.Key, req.Name, nil)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to save provider key"})
			return
		}
		c.JSON(http.StatusCreated, toExternalKeyResponse(key))
	}
}

// DeleteExternalKeyHandler soft-deletes a provider API key.
func DeleteExternalKeyHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		id := c.Param("id")
		if id == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing id"})
			return
		}
		if err := authSvc.DeleteExternalAPIKey(c.Request.Context(), installation.ID, id); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to delete provider key"})
			return
		}
		c.Status(http.StatusNoContent)
	}
}
