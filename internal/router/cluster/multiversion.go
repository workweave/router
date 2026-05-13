package cluster

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"

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

// VersionFromContext returns the per-request version override or "".
func VersionFromContext(ctx context.Context) string {
	v, _ := ctx.Value(versionContextKey{}).(string)
	return v
}

// alphaSuffixPattern captures the trailing -a<NN> form used to mark
// alpha-blend variants of the same training stem (e.g. v0.38-a05 → stem
// "v0.38", alpha 5). Two-digit zero-padded so lexicographic listing matches
// numeric order.
var alphaSuffixPattern = regexp.MustCompile(`^(.*)-a(\d{2})$`)

// parseAlphaSuffix returns (stem, alphaValue, true) when name has a valid
// -a<NN> suffix with NN in 0..10. Anything else returns ok=false.
func parseAlphaSuffix(name string) (stem string, alpha int, ok bool) {
	matches := alphaSuffixPattern.FindStringSubmatch(name)
	if matches == nil {
		return "", 0, false
	}
	alpha, err := strconv.Atoi(matches[2])
	if err != nil || alpha < 0 || alpha > 10 {
		return "", 0, false
	}
	return matches[1], alpha, true
}

// Multiversion holds one Scorer per artifact version and dispatches
// per-request based on a context override or an installation's routing alpha.
type Multiversion struct {
	Default  string
	Versions map[string]*Scorer
	// alphaToVersion maps an integer alpha (0..10) to the bundle name that
	// corresponds to it within the default version's stem. Empty when the
	// default version is a legacy bundle without an -a<NN> suffix, in which
	// case routing-alpha overrides degrade gracefully to the default scorer.
	alphaToVersion map[int]string
}

// NewMultiversion requires defaultVersion to be a key in versions.
func NewMultiversion(defaultVersion string, versions map[string]*Scorer) (*Multiversion, error) {
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
	alphaToVersion := map[int]string{}
	if stem, _, ok := parseAlphaSuffix(defaultVersion); ok {
		for name := range versions {
			candidateStem, alpha, ok := parseAlphaSuffix(name)
			if !ok || candidateStem != stem {
				continue
			}
			alphaToVersion[alpha] = name
		}
	}
	return &Multiversion{
		Default:        defaultVersion,
		Versions:       versions,
		alphaToVersion: alphaToVersion,
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

// DefaultDeployedModels returns the deployed candidates from the default
// version's Scorer. The admin model-selection UI uses this as the universe
// of valid model IDs; per-version differences are intentionally hidden.
func (m *Multiversion) DefaultDeployedModels() []DeployedEntry {
	s, ok := m.Versions[m.Default]
	if !ok {
		return nil
	}
	return s.DeployedModels()
}

// Route dispatches based on (in priority order): the per-request version
// header override; the per-installation routing alpha (when alpha bundles for
// the default's stem are built); otherwise the default version. Missing
// overrides degrade silently to the default so a misconfigured installation
// never errors a request — the cluster scorer's "fail loud" sentinels are
// reserved for unavailability, not for routing-preference fallbacks.
func (m *Multiversion) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	chosen := m.Default
	if requested := VersionFromContext(ctx); requested != "" {
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
	} else if req.AlphaSet {
		if alphaVersion, ok := m.alphaToVersion[req.Alpha]; ok {
			chosen = alphaVersion
		} else {
			// Bundle for this alpha isn't built (legacy default, or alpha
			// outside the built sweep). Log Debug rather than Warn so the
			// expected legacy case doesn't flood production logs.
			observability.Get().Debug(
				"Cluster scorer: routing alpha bundle not built; serving default",
				"alpha", req.Alpha,
				"default_version", m.Default,
			)
		}
	}
	scorer, ok := m.Versions[chosen]
	if !ok {
		// Defensive: NewMultiversion enforces Default ∈ Versions. Surface
		// the bug as ErrClusterUnavailable rather than silently degrading.
		observability.Get().Error(
			"Cluster scorer: chosen version missing; returning ErrClusterUnavailable",
			"chosen_version", chosen,
		)
		return router.Decision{}, fmt.Errorf("cluster multiversion: chosen version %q not built: %w", chosen, ErrClusterUnavailable)
	}
	return scorer.Route(ctx, req)
}
