package router

import "context"

// Strategy names a routing strategy a request can opt into via the
// x-weave-router-strategy header. The zero value ("") means the deployment
// default (cluster).
type Strategy string

const (
	// StrategyCluster is the default cluster-scorer strategy (AvengersPro).
	StrategyCluster Strategy = "cluster"
	// StrategyRL routes via the trained RL/DPO policy served by the
	// out-of-process policy sidecar. Opt-in only; never the silent default.
	StrategyRL Strategy = "rl"
	// StrategyHMM routes via an out-of-process policy sidecar. Opt-in only;
	// never the silent default.
	StrategyHMM Strategy = "hmm"
	// StrategyBandit routes via Thompson sampling over a frozen
	// ts_posterior.json (cluster×model reward posterior). Opt-in only; wired
	// when ROUTER_BANDIT_POSTERIOR_FILE is set at boot.
	StrategyBandit Strategy = "bandit"
)

type strategyContextKey struct{}

// WithStrategy stashes a routing-strategy override on ctx. An empty strategy
// leaves ctx unchanged so StrategyFromContext falls back to the default.
func WithStrategy(ctx context.Context, s Strategy) context.Context {
	if s == "" {
		return ctx
	}
	return context.WithValue(ctx, strategyContextKey{}, s)
}

// StrategyFromContext returns the per-request strategy override, or
// StrategyCluster when none was set.
func StrategyFromContext(ctx context.Context) Strategy {
	s, ok := ctx.Value(strategyContextKey{}).(Strategy)
	if !ok || s == "" {
		return StrategyCluster
	}
	return s
}
