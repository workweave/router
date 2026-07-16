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

// DeployedModelsSource exposes the routable models the cluster scorer knows
// about. Implemented by *cluster.Multiversion; tests can pass a fake.
type DeployedModelsSource interface {
	DefaultDeployedModels() []cluster.DeployedEntry
}

// ExclusionOverrideSource reports the deployment-wide ROUTER_EXCLUDED_MODELS
// override, if active. Implemented by *proxy.Service.
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

// GetExcludedModelsHandler returns deployed models and the installation's
// exclusion list. `env_override_active` tells the UI to render read-only.
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
// 400 on unknown model IDs; 403 if the env override is active.
func UpdateExcludedModelsHandler(authSvc *auth.Service, models DeployedModelsSource, override ExclusionOverrideSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		if override != nil && override.HasExcludedModelsOverride() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "Exclusion list is pinned by ROUTER_EXCLUDED_MODELS; clear the env var to edit.",
			})
			return
		}

		var req updateExcludedModelsRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		allowed := make(map[string]struct{}, len(models.DefaultDeployedModels()))
		for _, e := range models.DefaultDeployedModels() {
			allowed[e.Model] = struct{}{}
		}

		stored, err := authSvc.SetInstallationExcludedModels(c.Request.Context(), installation.ExternalID, installation.ID, req.Excluded, allowed)
		if err != nil {
			if errors.Is(err, auth.ErrUnknownModel) {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "unknown model id in exclusion list"})
				return
			}
			log.Error("Failed to update excluded models", "err", err, "installation_id", installation.ID)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to update excluded models."})
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
