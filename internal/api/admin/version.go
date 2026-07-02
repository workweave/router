package admin

import (
	"net/http"

	"workweave/router/internal/config"
	"workweave/router/internal/version"

	"github.com/gin-gonic/gin"
)

type versionResponse struct {
	Commit         string `json:"commit"`
	CommitShort    string `json:"commit_short"`
	PR             string `json:"pr"`
	Display        string `json:"display"`
	BuildTime      string `json:"build_time"`
	ClusterVersion string `json:"cluster_version"`
}

// VersionHandler reports the running binary's build identity — the git commit
// it was built from, the build timestamp, and the active cluster artifact — so
// operators (and the managed-deployment badge in the README) can tell which
// router commit is live. Unauthed and mounted in both deployment modes, like
// /health; every field is public metadata with no leak risk.
func VersionHandler(c *gin.Context) {
	c.JSON(http.StatusOK, versionResponse{
		Commit:         version.Commit,
		CommitShort:    version.ShortCommit(),
		PR:             version.PR,
		Display:        version.Display(),
		BuildTime:      version.BuildTime,
		ClusterVersion: config.GetOr("ROUTER_CLUSTER_VERSION", "artifacts/latest"),
	})
}
