package middleware

import (
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/router/cluster"

	"github.com/gin-gonic/gin"
)

// ClusterVersionOverrideHeader pins a request to a specific cluster artifact
// version. Only honored for allow-listed installations.
const ClusterVersionOverrideHeader = "x-weave-cluster-version"

// WithClusterVersionOverride stashes the requested cluster version on the
// request context when the installation is allow-listed and the header is set.
func WithClusterVersionOverride() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader(ClusterVersionOverrideHeader))
		if raw == "" {
			c.Next()
			return
		}
		installation := InstallationFrom(c)
		if installation == nil {
			c.Next()
			return
		}
		if !installation.IsEvalAllowlisted {
			observability.FromGin(c).Debug(
				"Ignored cluster-version override from non-allow-listed installation",
				"installation_id", installation.ID,
			)
			c.Next()
			return
		}
		ctx := cluster.WithVersion(c.Request.Context(), raw)
		c.Request = c.Request.WithContext(ctx)
		observability.FromGin(c).Info(
			"Cluster-version override applied",
			"installation_id", installation.ID,
			"requested_version", raw,
		)
		c.Next()
	}
}
