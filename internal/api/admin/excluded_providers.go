package admin

import (
	"errors"
	"net/http"
	"sort"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

// ProviderExclusionOverrideSource reports whether ROUTER_EXCLUDED_PROVIDERS
// (or an equivalent deployment-wide override) is in effect, and what its
// contents are. Implemented by *proxy.Service.
type ProviderExclusionOverrideSource interface {
	HasExcludedProvidersOverride() bool
	ExcludedProvidersOverride() []string
}

type excludedProvidersResponse struct {
	Available         []string `json:"available"`
	Excluded          []string `json:"excluded"`
	EnvOverrideActive bool     `json:"env_override_active"`
}

type updateExcludedProvidersRequest struct {
	Excluded []string `json:"excluded"`
}

// deployedProvidersDTO returns the distinct provider names behind the
// deployed-models registry, sorted. Centralized so the GET and PUT responses
// cannot drift apart.
func deployedProvidersDTO(models DeployedModelsSource) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, e := range models.DefaultDeployedModels() {
		if _, dup := seen[e.Provider]; dup {
			continue
		}
		seen[e.Provider] = struct{}{}
		out = append(out, e.Provider)
	}
	sort.Strings(out)
	return out
}

// GetExcludedProvidersHandler returns the deployed provider names and the
// installation's provider exclusion list. When a deployment-wide env override
// is active, `env_override_active` is true and the UI must render the
// checklist read-only.
func GetExcludedProvidersHandler(authSvc *auth.Service, models DeployedModelsSource, override ProviderExclusionOverrideSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		envActive := override != nil && override.HasExcludedProvidersOverride()
		var excluded []string
		if envActive {
			excluded = override.ExcludedProvidersOverride()
		} else {
			excluded = append([]string{}, installation.ExcludedProviders...)
			sort.Strings(excluded)
		}
		if excluded == nil {
			excluded = []string{}
		}

		c.JSON(http.StatusOK, excludedProvidersResponse{
			Available:         deployedProvidersDTO(models),
			Excluded:          excluded,
			EnvOverrideActive: envActive,
		})
	}
}

// UpdateExcludedProvidersHandler replaces the installation's provider
// exclusion list. Rejects unknown provider names with 400. Returns 403 when
// the env override is active so the UI never silently loses a save.
func UpdateExcludedProvidersHandler(authSvc *auth.Service, models DeployedModelsSource, override ProviderExclusionOverrideSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		if override != nil && override.HasExcludedProvidersOverride() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "Exclusion list is pinned by ROUTER_EXCLUDED_PROVIDERS; clear the env var to edit.",
			})
			return
		}

		var req updateExcludedProvidersRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		available := deployedProvidersDTO(models)
		allowed := make(map[string]struct{}, len(available))
		for _, p := range available {
			allowed[p] = struct{}{}
		}

		stored, err := authSvc.SetInstallationExcludedProviders(c.Request.Context(), installation.ExternalID, installation.ID, req.Excluded, allowed)
		if err != nil {
			if errors.Is(err, auth.ErrUnknownProvider) {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			log.Error("Failed to update excluded providers", "err", err, "installation_id", installation.ID)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to update excluded providers."})
			return
		}

		sort.Strings(stored)
		c.JSON(http.StatusOK, excludedProvidersResponse{
			Available: available,
			Excluded:  stored,
		})
	}
}

var _ ProviderExclusionOverrideSource = (*proxy.Service)(nil)
