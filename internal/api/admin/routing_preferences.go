package admin

import (
	"net/http"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"

	"github.com/gin-gonic/gin"
)

// Neutral starting point shown when no preference is stored. Mirrors the
// managed control plane's defaults so both surfaces present the same dials.
const (
	defaultQualityPct = 50.0
	defaultSpeedPct   = 25.0
	defaultPricePct   = 25.0
)

type routingPreferencesResponse struct {
	Quality   float64 `json:"quality"`
	Speed     float64 `json:"speed"`
	Price     float64 `json:"price"`
	IsDefault bool    `json:"is_default"`
}

// updateRoutingPreferencesRequest carries the raw slider values. They need not
// sum to 100 -- the server normalizes them into weights. reset=true clears the
// preference so the scorer reverts to its tuned defaults.
type updateRoutingPreferencesRequest struct {
	Reset   bool    `json:"reset"`
	Quality float64 `json:"quality"`
	Speed   float64 `json:"speed"`
	Price   float64 `json:"price"`
}

func defaultRoutingPreferences() routingPreferencesResponse {
	return routingPreferencesResponse{
		Quality:   defaultQualityPct,
		Speed:     defaultSpeedPct,
		Price:     defaultPricePct,
		IsDefault: true,
	}
}

// routingPreferencesFor renders the stored weights as percentages, or the
// neutral default when no preference is set. The two weights are written and
// cleared as a pair; a half-set row is treated as unset.
func routingPreferencesFor(installation *auth.Installation) routingPreferencesResponse {
	if installation.RoutingQualityWeight == nil || installation.RoutingSpeedWeight == nil {
		return defaultRoutingPreferences()
	}
	quality := *installation.RoutingQualityWeight
	speed := *installation.RoutingSpeedWeight
	price := 1.0 - quality - speed
	if price < 0 {
		price = 0
	}
	return routingPreferencesResponse{
		Quality:   quality * 100,
		Speed:     speed * 100,
		Price:     price * 100,
		IsDefault: false,
	}
}

// normalizeRoutingWeights converts raw slider values into quality/speed
// weights summing to <= 1 (price is the implied remainder). ok is false when
// any value is negative or the total is non-positive, so the weights can't be
// normalized.
func normalizeRoutingWeights(quality, speed, price float64) (qualityWeight, speedWeight float64, ok bool) {
	if quality < 0 || speed < 0 || price < 0 {
		return 0, 0, false
	}
	total := quality + speed + price
	if total <= 0 {
		return 0, 0, false
	}
	return quality / total, speed / total, true
}

// GetRoutingPreferencesHandler returns the installation's routing dials as
// percentages, or the neutral default when none are stored.
func GetRoutingPreferencesHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, routingPreferencesFor(installation))
	}
}

// UpdateRoutingPreferencesHandler normalizes the raw slider values into weights
// summing to 1 and persists them. reset=true clears the preference. Rejects a
// non-positive total with 400.
func UpdateRoutingPreferencesHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		var req updateRoutingPreferencesRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if req.Reset {
			if err := authSvc.SetInstallationRoutingPreferences(c.Request.Context(), installation.ExternalID, installation.ID, nil, nil); err != nil {
				log.Error("Failed to clear routing preferences", "err", err, "installation_id", installation.ID)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to update routing preferences"})
				return
			}
			c.JSON(http.StatusOK, defaultRoutingPreferences())
			return
		}

		qualityWeight, speedWeight, ok := normalizeRoutingWeights(req.Quality, req.Speed, req.Price)
		if !ok {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "at least one routing preference must be greater than zero"})
			return
		}

		if err := authSvc.SetInstallationRoutingPreferences(c.Request.Context(), installation.ExternalID, installation.ID, &qualityWeight, &speedWeight); err != nil {
			log.Error("Failed to update routing preferences", "err", err, "installation_id", installation.ID)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to update routing preferences"})
			return
		}

		priceWeight := 1.0 - qualityWeight - speedWeight
		if priceWeight < 0 {
			priceWeight = 0
		}
		c.JSON(http.StatusOK, routingPreferencesResponse{
			Quality:   qualityWeight * 100,
			Speed:     speedWeight * 100,
			Price:     priceWeight * 100,
			IsDefault: false,
		})
	}
}
