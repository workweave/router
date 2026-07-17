package router

import (
	"context"
	"errors"
)

// ErrStrategyUnavailable signals that a selected strategy has no configured
// implementation. Strategy-specific sentinels may wrap this error.
var ErrStrategyUnavailable = errors.New("router: strategy unavailable")

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
	// StrategyHMMEmbedding uses the HMM sidecar with the embedding-quality-seeded bandit prior.
	StrategyHMMEmbedding Strategy = "hmm_embedding"
	// StrategyBandit routes via Thompson sampling over a frozen
	// ts_posterior.json (cluster×model reward posterior). Opt-in only; wired
	// when ROUTER_BANDIT_POSTERIOR_FILE is set at boot.
	StrategyBandit Strategy = "bandit"
)

// IsHMMStrategy reports whether strategy uses the HMM policy contract and
// lifecycle semantics.
func IsHMMStrategy(strategy Strategy) bool {
	return strategy == StrategyHMM || strategy == StrategyHMMEmbedding
}

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
