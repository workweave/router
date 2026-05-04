package cluster

import (
	"context"
	"fmt"
	"sort"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
)

type versionContextKey struct{}

// WithVersion stashes a cluster artifact version override on ctx.
func WithVersion(ctx context.Context, version string) context.Context {
	if version == "" {
		return ctx
	}
	return context.WithValue(ctx, versionContextKey{}, version)
}

// VersionFromContext reads the per-request version override, or "".
func VersionFromContext(ctx context.Context) string {
	v, _ := ctx.Value(versionContextKey{}).(string)
	return v
}

// Multiversion holds one Scorer per artifact version and dispatches
// per-request based on a context override. Defaults to the configured version.
type Multiversion struct {
	Default  string
	Versions map[string]*Scorer
	Fallback router.Router
}

// NewMultiversion requires defaultVersion to be a key in versions.
func NewMultiversion(defaultVersion string, versions map[string]*Scorer, fallback router.Router) (*Multiversion, error) {
	if defaultVersion == "" {
		return nil, fmt.Errorf("cluster multiversion: default version must not be empty")
	}
	if _, ok := versions[defaultVersion]; !ok {
		built := make([]string, 0, len(versions))
		for v := range versions {
			built = append(built, v)
		}
		sort.Strings(built)
		return nil, fmt.Errorf("cluster multiversion: default %q not in built versions %v", defaultVersion, built)
	}
	if fallback == nil {
		return nil, fmt.Errorf("cluster multiversion: fallback must not be nil")
	}
	return &Multiversion{
		Default:  defaultVersion,
		Versions: versions,
		Fallback: fallback,
	}, nil
}

// Built returns the sorted set of built version names.
func (m *Multiversion) Built() []string {
	out := make([]string, 0, len(m.Versions))
	for v := range m.Versions {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Route dispatches to the per-request version override if built, otherwise Default.
func (m *Multiversion) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	requested := VersionFromContext(ctx)
	chosen := m.Default
	if requested != "" {
		if _, ok := m.Versions[requested]; ok {
			chosen = requested
		} else {
			observability.Get().Warn(
				"Cluster scorer: requested version not built; serving default",
				"requested_version", requested,
				"default_version", m.Default,
				"built_versions", m.Built(),
			)
		}
	}
	scorer, ok := m.Versions[chosen]
	if !ok {
		// Defensive: NewMultiversion enforces that Default is in Versions
		// at construction time. Log and fail-open to the package fallback
		// rather than panic if a future refactor breaks that invariant.
		observability.Get().Warn(
			"Cluster scorer: chosen version missing; falling back to heuristic",
			"chosen_version", chosen,
		)
		return m.Fallback.Route(ctx, req)
	}
	return scorer.Route(ctx, req)
}
