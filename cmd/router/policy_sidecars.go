package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"workweave/router/internal/policyclient"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/policy"
)

const maxConfiguredPolicySidecars = 16

var configuredPolicyStrategyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

var reservedPolicyStrategies = map[router.Strategy]struct{}{
	router.StrategyCluster:      {},
	router.StrategyRL:           {},
	router.StrategyHMM:          {},
	router.StrategyHMMEmbedding: {},
	router.StrategyBandit:       {},
}

// buildConfiguredPolicySidecars turns a JSON strategy-to-URL map into policy.StrategySpec registrations; sidecars own model logic, the router owns candidate resolution and lifecycle.
func buildConfiguredPolicySidecars(
	ctx context.Context,
	raw string,
	timeout time.Duration,
	deployed, availableProviders map[string]struct{},
	httpClient *http.Client,
	logger *slog.Logger,
) ([]policy.StrategySpec, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var configured map[string]string
	if err := json.Unmarshal([]byte(raw), &configured); err != nil {
		return nil, fmt.Errorf("parse ROUTER_POLICY_SIDECARS: %w", err)
	}
	if len(configured) > maxConfiguredPolicySidecars {
		return nil, fmt.Errorf("ROUTER_POLICY_SIDECARS has %d entries; maximum is %d", len(configured), maxConfiguredPolicySidecars)
	}

	strategies := make([]string, 0, len(configured))
	for name := range configured {
		strategies = append(strategies, name)
	}
	sort.Strings(strategies)
	seen := make(map[router.Strategy]struct{}, len(strategies))
	registrations := make([]policy.StrategySpec, 0, len(strategies))
	for _, configuredName := range strategies {
		strategy := router.Strategy(strings.ToLower(strings.TrimSpace(configuredName)))
		if strategy == "" {
			return nil, fmt.Errorf("ROUTER_POLICY_SIDECARS contains an empty strategy")
		}
		if !configuredPolicyStrategyPattern.MatchString(string(strategy)) {
			return nil, fmt.Errorf("ROUTER_POLICY_SIDECARS strategy %q must match %s", strategy, configuredPolicyStrategyPattern)
		}
		if _, reserved := reservedPolicyStrategies[strategy]; reserved {
			return nil, fmt.Errorf("ROUTER_POLICY_SIDECARS strategy %q is reserved", strategy)
		}
		if _, duplicate := seen[strategy]; duplicate {
			return nil, fmt.Errorf("ROUTER_POLICY_SIDECARS strategy %q is duplicated after normalization", strategy)
		}
		seen[strategy] = struct{}{}

		sidecarURL := strings.TrimSpace(configured[configuredName])
		parsed, err := url.Parse(sidecarURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("ROUTER_POLICY_SIDECARS strategy %q has invalid URL %q", strategy, sidecarURL)
		}

		client := policyclient.New(sidecarURL, httpClient, timeout)
		capabilityCtx, cancel := context.WithTimeout(ctx, timeout)
		capabilities, capabilityErr := client.Capabilities(capabilityCtx)
		cancel()
		if capabilityErr != nil {
			// Serving remains wired and fail-closed. Optional behavior stays
			// conservative until the next restart can discover capabilities.
			logger.Warn("Policy sidecar capabilities unavailable at boot",
				"strategy", strategy,
				"sidecar_url", sidecarURL,
				"err", capabilityErr,
			)
		}
		resolver := policy.NewResolver(
			deployed,
			availableProviders,
			func(model catalog.Model) string { return model.ID },
			policy.ManagedProviderPolicy(),
		)
		unavailable := fmt.Errorf("%s policy router unavailable: %w", strategy, router.ErrStrategyUnavailable)
		adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
			Strategy:    strategy,
			Unavailable: unavailable,
		}, client, resolver).WithCapabilities(capabilities)
		registrations = append(registrations, policy.StrategySpec{
			Strategy:     strategy,
			Router:       adapter,
			Unavailable:  unavailable,
			Capabilities: capabilities,
		})
	}
	return registrations, nil
}
