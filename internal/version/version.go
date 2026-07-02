// Package version carries the router binary's build identity, stamped in at
// link time via -ldflags. It imports nothing so any layer may read it, and its
// defaults make an un-stamped build (e.g. `go run`, a local `go build`) report
// "unknown" rather than an empty string.
package version

// shortLen is the git short-SHA length GitHub and the README badge display.
const shortLen = 7

var (
	// Commit is the git SHA the binary was built from. Overridden at build
	// time with `-ldflags "-X workweave/router/internal/version.Commit=<sha>"`.
	Commit = "unknown"
	// BuildTime is the RFC3339 UTC build timestamp, stamped the same way.
	BuildTime = "unknown"
)

// ShortCommit returns the 7-character prefix of Commit, or Commit unchanged
// when it is already shorter (e.g. the "unknown" default).
func ShortCommit() string {
	if len(Commit) <= shortLen {
		return Commit
	}
	return Commit[:shortLen]
}
