package admin

import (
	"errors"
	"net/http"
	"sort"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"

	"github.com/gin-gonic/gin"
)

type modelStatusDTO struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
}

type providerStatusDTO struct {
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
}

type preferredModelsResponse struct {
	Preferred []string `json:"preferred"`
}

type modelSelectionItemRequest struct {
	Model string `json:"model"`
}

type providerSelectionItemRequest struct {
	Provider string `json:"provider"`
}

// GetModelsHandler returns every deployed model with its effective enabled state.
func GetModelsHandler(authSvc *auth.Service, models DeployedModelsSource, override ExclusionOverrideSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		excluded := installation.ExcludedModels
		if override != nil && override.HasExcludedModelsOverride() {
			excluded = override.ExcludedModelsOverride()
		}
		excludedSet := stringsSet(excluded)
		available := deployedModelsDTO(models)
		out := make([]modelStatusDTO, 0, len(available))
		for _, model := range available {
			_, isExcluded := excludedSet[model.Model]
			out = append(out, modelStatusDTO{
				Model:    model.Model,
				Provider: model.Provider,
				Enabled:  !isExcluded,
			})
		}
		c.JSON(http.StatusOK, out)
	}
}

// GetProvidersHandler returns every deployed provider with its effective enabled state.
func GetProvidersHandler(authSvc *auth.Service, models DeployedModelsSource, override ProviderExclusionOverrideSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		excluded := installation.ExcludedProviders
		if override != nil && override.HasExcludedProvidersOverride() {
			excluded = override.ExcludedProvidersOverride()
		}
		excludedSet := stringsSet(excluded)
		available := deployedProvidersDTO(models)
		out := make([]providerStatusDTO, 0, len(available))
		for _, provider := range available {
			_, isExcluded := excludedSet[provider]
			out = append(out, providerStatusDTO{
				Provider: provider,
				Enabled:  !isExcluded,
			})
		}
		c.JSON(http.StatusOK, out)
	}
}

// GetPreferredModelsHandler returns the installation's ordered model priority ranking.
func GetPreferredModelsHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		preferred := append([]string{}, installation.PreferredModels...)
		if preferred == nil {
			preferred = []string{}
		}
		c.JSON(http.StatusOK, preferredModelsResponse{Preferred: preferred})
	}
}

// UpdatePreferredModelsHandler replaces the installation's ordered model priority ranking.
func UpdatePreferredModelsHandler(authSvc *auth.Service, models DeployedModelsSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		var req preferredModelsResponse
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		stored, err := authSvc.SetInstallationPreferredModels(
			c.Request.Context(),
			installation.ExternalID,
			installation.ID,
			req.Preferred,
			deployedModelSet(models),
		)
		if !respondModelSelectionError(c, err, "Failed to update preferred models.") {
			return
		}
		c.JSON(http.StatusOK, preferredModelsResponse{Preferred: stored})
	}
}

// AddExcludedModelHandler adds one model to the installation exclusion list.
func AddExcludedModelHandler(authSvc *auth.Service, models DeployedModelsSource, override ExclusionOverrideSource) gin.HandlerFunc {
	return updateExcludedModelItemHandler(authSvc, models, override, true)
}

// RemoveExcludedModelHandler removes one model from the installation exclusion list.
func RemoveExcludedModelHandler(authSvc *auth.Service, models DeployedModelsSource, override ExclusionOverrideSource) gin.HandlerFunc {
	return updateExcludedModelItemHandler(authSvc, models, override, false)
}

func updateExcludedModelItemHandler(authSvc *auth.Service, models DeployedModelsSource, override ExclusionOverrideSource, add bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		if modelExclusionOverrideActive(c, override) {
			return
		}

		var req modelSelectionItemRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		next := updateListItem(installation.ExcludedModels, req.Model, add)
		stored, err := authSvc.SetInstallationExcludedModels(
			c.Request.Context(),
			installation.ExternalID,
			installation.ID,
			next,
			deployedModelSet(models),
		)
		if !respondModelSelectionError(c, err, "Failed to update excluded models.") {
			return
		}
		sort.Strings(stored)
		c.JSON(http.StatusOK, excludedModelsResponse{
			Available: deployedModelsDTO(models),
			Excluded:  stored,
		})
	}
}

// AddPreferredModelHandler appends one model to the ordered priority ranking.
func AddPreferredModelHandler(authSvc *auth.Service, models DeployedModelsSource) gin.HandlerFunc {
	return updatePreferredModelItemHandler(authSvc, models, true)
}

// RemovePreferredModelHandler removes one model from the priority ranking.
func RemovePreferredModelHandler(authSvc *auth.Service, models DeployedModelsSource) gin.HandlerFunc {
	return updatePreferredModelItemHandler(authSvc, models, false)
}

func updatePreferredModelItemHandler(authSvc *auth.Service, models DeployedModelsSource, add bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		var req modelSelectionItemRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		stored, err := authSvc.SetInstallationPreferredModels(
			c.Request.Context(),
			installation.ExternalID,
			installation.ID,
			updateListItem(installation.PreferredModels, req.Model, add),
			deployedModelSet(models),
		)
		if !respondModelSelectionError(c, err, "Failed to update preferred models.") {
			return
		}
		c.JSON(http.StatusOK, preferredModelsResponse{Preferred: stored})
	}
}

// AddExcludedProviderHandler adds one provider to the installation exclusion list.
func AddExcludedProviderHandler(authSvc *auth.Service, models DeployedModelsSource, override ProviderExclusionOverrideSource) gin.HandlerFunc {
	return updateExcludedProviderItemHandler(authSvc, models, override, true)
}

// RemoveExcludedProviderHandler removes one provider from the installation exclusion list.
func RemoveExcludedProviderHandler(authSvc *auth.Service, models DeployedModelsSource, override ProviderExclusionOverrideSource) gin.HandlerFunc {
	return updateExcludedProviderItemHandler(authSvc, models, override, false)
}

func updateExcludedProviderItemHandler(authSvc *auth.Service, models DeployedModelsSource, override ProviderExclusionOverrideSource, add bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		if providerExclusionOverrideActive(c, override) {
			return
		}

		var req providerSelectionItemRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		available := deployedProvidersDTO(models)
		stored, err := authSvc.SetInstallationExcludedProviders(
			c.Request.Context(),
			installation.ExternalID,
			installation.ID,
			updateListItem(installation.ExcludedProviders, req.Provider, add),
			stringsSet(available),
		)
		if !respondProviderSelectionError(c, err) {
			return
		}
		sort.Strings(stored)
		c.JSON(http.StatusOK, excludedProvidersResponse{
			Available: available,
			Excluded:  stored,
		})
	}
}

func modelExclusionOverrideActive(c *gin.Context, override ExclusionOverrideSource) bool {
	if override == nil || !override.HasExcludedModelsOverride() {
		return false
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error": "Exclusion list is pinned by ROUTER_EXCLUDED_MODELS; clear the env var to edit.",
	})
	return true
}

func providerExclusionOverrideActive(c *gin.Context, override ProviderExclusionOverrideSource) bool {
	if override == nil || !override.HasExcludedProvidersOverride() {
		return false
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error": "Exclusion list is pinned by ROUTER_EXCLUDED_PROVIDERS; clear the env var to edit.",
	})
	return true
}

func deployedModelSet(models DeployedModelsSource) map[string]struct{} {
	out := make(map[string]struct{}, len(models.DefaultDeployedModels()))
	for _, model := range models.DefaultDeployedModels() {
		out[model.Model] = struct{}{}
	}
	return out
}

func stringsSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func updateListItem(values []string, value string, add bool) []string {
	out := make([]string, 0, len(values)+1)
	for _, existing := range values {
		if existing == value {
			if add {
				out = append(out, existing)
			}
			continue
		}
		out = append(out, existing)
	}
	if add {
		for _, existing := range values {
			if existing == value {
				return out
			}
		}
		out = append(out, value)
	}
	return out
}

func respondModelSelectionError(c *gin.Context, err error, failureMessage string) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, auth.ErrUnknownModel) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return false
	}
	observability.FromGin(c).Error(failureMessage, "err", err)
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": failureMessage})
	return false
}

func respondProviderSelectionError(c *gin.Context, err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, auth.ErrUnknownProvider) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return false
	}
	observability.FromGin(c).Error("Failed to update excluded providers", "err", err)
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to update excluded providers."})
	return false
}
