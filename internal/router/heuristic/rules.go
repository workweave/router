// Package heuristic implements a deterministic Router that picks between two
// models based on a token-count threshold.
package heuristic

import (
	"context"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
)

type Config struct {
	Provider        string
	SmallModel      string
	LargeModel      string
	ThresholdTokens int
}

type Rules struct {
	cfg Config
}

func NewRules(cfg Config) *Rules {
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	return &Rules{cfg: cfg}
}

func (r *Rules) Route(_ context.Context, req router.Request) (router.Decision, error) {
	var decision router.Decision
	if req.EstimatedInputTokens >= r.cfg.ThresholdTokens {
		decision = router.Decision{
			Provider: r.cfg.Provider,
			Model:    r.cfg.LargeModel,
			Reason:   "heuristic:long_prompt",
		}
	} else {
		decision = router.Decision{
			Provider: r.cfg.Provider,
			Model:    r.cfg.SmallModel,
			Reason:   "heuristic:short_prompt",
		}
	}
	observability.Get().Info(
		"Heuristic routing decision",
		"decision_model", decision.Model,
		"decision_reason", decision.Reason,
		"requested_model", req.RequestedModel,
		"estimated_input_tokens", req.EstimatedInputTokens,
		"threshold_tokens", r.cfg.ThresholdTokens,
		"has_tools", req.HasTools,
	)
	return decision, nil
}

var _ router.Router = (*Rules)(nil)
