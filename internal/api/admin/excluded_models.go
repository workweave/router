package admin

import (
	"errors"
	"net/http"
	"sort"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router/cluster"

	"github.com/gin-gonic/gin"
)

// DeployedModelsSource exposes the universe of routable models the cluster
// scorer knows about. The admin endpoint surfaces this so the UI can render
// a checkbox per model. Implemented by *cluster.Multiversion in production;
// callers can pass a fake in tests.
type DeployedModelsSource interface {
	DefaultDeployedModels() []cluster.DeployedEntry
}

// ExclusionOverrideSource reports whether ROUTER_EXCLUDED_MODELS (or an
// equivalent deployment-wide override) is in effect, and what its contents
// are. Implemented by *proxy.Service.
type ExclusionOverrideSource interface {
	HasExcludedModelsOverride() bool
	ExcludedModelsOverride() []string
}

type deployedModelDTO struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

type excludedModelsResponse struct {
	Available         []deployedModelDTO `json:"available"`
	Excluded          []string           `json:"excluded"`
	EnvOverrideActive bool               `json:"env_override_active"`
}

type updateExcludedModelsRequest struct {
	Excluded []string `json:"excluded"`
}

// deployedModelsDTO converts the deployed-models list to sorted DTO form,
// centralized so the GET and PUT responses cannot drift apart.
func deployedModelsDTO(models DeployedModelsSource) []deployedModelDTO {
	entries := models.DefaultDeployedModels()
	out := make([]deployedModelDTO, 0, len(entries))
	for _, e := range entries {
		out = append(out, deployedModelDTO{Model: e.Model, Provider: e.Provider})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// GetExcludedModelsHandler returns deployed models and the installation's exclusion list.
// When a deployment-wide env override is active, `env_override_active` is true and the
// UI must render the checklist read-only.
func GetExcludedModelsHandler(authSvc *auth.Service, models DeployedModelsSource, override ExclusionOverrideSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		out := deployedModelsDTO(models)

		envActive := override != nil && override.HasExcludedModelsOverride()
		var excluded []string
		if envActive {
			excluded = override.ExcludedModelsOverride()
		} else {
			excluded = append([]string{}, installation.ExcludedModels...)
			sort.Strings(excluded)
		}
		if excluded == nil {
			excluded = []string{}
		}

		c.JSON(http.StatusOK, excludedModelsResponse{
			Available:         out,
			Excluded:          excluded,
			EnvOverrideActive: envActive,
		})
	}
}

// UpdateExcludedModelsHandler replaces the installation's exclusion list.
// Rejects unknown model IDs with 400. Returns 403 when the env override is
// active so the UI never silently loses a save.
func UpdateExcludedModelsHandler(authSvc *auth.Service, models DeployedModelsSource, override ExclusionOverrideSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		if override != nil && override.HasExcludedModelsOverride() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "exclusion list is pinned by ROUTER_EXCLUDED_MODELS; clear the env var to edit",
			})
			return
		}

		var req updateExcludedModelsRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		allowed := make(map[string]struct{}, len(models.DefaultDeployedModels()))
		for _, e := range models.DefaultDeployedModels() {
			allowed[e.Model] = struct{}{}
		}

		stored, err := authSvc.SetInstallationExcludedModels(c.Request.Context(), installation.ExternalID, installation.ID, req.Excluded, allowed)
		if err != nil {
			if errors.Is(err, auth.ErrUnknownModel) {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			log.Error("Failed to update excluded models", "err", err, "installation_id", installation.ID)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to update excluded models"})
			return
		}

		sort.Strings(stored)
		c.JSON(http.StatusOK, excludedModelsResponse{
			Available: deployedModelsDTO(models),
			Excluded:  stored,
		})
	}
}

var (
	_ DeployedModelsSource    = (*cluster.Multiversion)(nil)
	_ ExclusionOverrideSource = (*proxy.Service)(nil)
)
