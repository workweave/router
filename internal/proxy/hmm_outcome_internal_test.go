package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router"
)

type captureHMMOutcomeReporter struct {
	ch chan map[string]interface{}
}

func (r *captureHMMOutcomeReporter) ReportOutcome(ctx context.Context, payload map[string]interface{}) error {
	select {
	case r.ch <- payload:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestReportHMMOutcome_UsesFreshMetadataForStickyServedDecision(t *testing.T) {
	reporter := &captureHMMOutcomeReporter{ch: make(chan map[string]interface{}, 1)}
	s := &Service{hmmOutcomeReporter: reporter}

	routeRes := turnLoopResult{
		StickyHit: true,
		Fresh: router.Decision{
			Model:    "moonshotai/kimi-k2.7",
			Provider: "fireworks",
			Metadata: &router.RoutingMetadata{
				RouteID:  "route-fresh",
				Strategy: string(router.StrategyHMM),
			},
		},
	}
	served := router.Decision{
		Model:    "claude-haiku-4-5",
		Provider: "anthropic",
	}

	s.reportHMMOutcome(context.Background(), routeRes, served, "anthropic", 100, 90, 10, 0, 0, 12, 34, nil, &hmmOutcomeResponse{
		Body:      []byte(`{"content":[{"type":"text","text":"done"}]}`),
		Truncated: false,
	})

	select {
	case payload := <-reporter.ch:
		require.Equal(t, "route-fresh", payload["route_id"])
		assert.Equal(t, "claude-haiku-4-5", payload["served_model"])
		assert.Equal(t, "anthropic", payload["served_provider"])
		assert.Equal(t, "moonshotai/kimi-k2.7", payload["decision_model"])
		assert.Equal(t, "fireworks", payload["decision_provider"])
		assert.Equal(t, true, payload["sticky_hit"])
		assert.Equal(t, `{"content":[{"type":"text","text":"done"}]}`, payload["response_body"])
		assert.Equal(t, "client_anthropic", payload["response_body_format"])
		assert.Equal(t, false, payload["response_body_truncated"])
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for HMM outcome payload")
	}
}
