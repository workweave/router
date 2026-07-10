package policy

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
)

// ReasonRenderer converts policy metadata into the compact internal reason
// consumed by the existing pin/planner layer.
type ReasonRenderer func(Result) string

// SidecarRouterConfig is the small strategy-specific registration required to
// plug a versioned policy sidecar into the shared routing harness.
type SidecarRouterConfig struct {
	Strategy    router.Strategy
	Unavailable error
	Reason      ReasonRenderer
}

// SidecarRouter is a shared adapter for out-of-process policy routers.
type SidecarRouter struct {
	config           SidecarRouterConfig
	decider          Decider
	reporter         OutcomeReporter
	feedbackReporter FeedbackReporter
	resolver         *Resolver
}

// NewSidecarRouter constructs a reusable policy adapter. Strategy packages
// provide only a roster mapper/resolver and optional reason renderer.
func NewSidecarRouter(config SidecarRouterConfig, decider Decider, resolver *Resolver) *SidecarRouter {
	reporter, _ := decider.(OutcomeReporter)
	feedbackReporter, _ := decider.(FeedbackReporter)
	if config.Unavailable == nil {
		config.Unavailable = router.ErrStrategyUnavailable
	}
	return &SidecarRouter{
		config:           config,
		decider:          decider,
		reporter:         reporter,
		feedbackReporter: feedbackReporter,
		resolver:         resolver,
	}
}

func (r *SidecarRouter) ReportOutcome(ctx context.Context, payload map[string]interface{}) error {
	if r.reporter == nil {
		return nil
	}
	return r.reporter.ReportOutcome(ctx, payload)
}

func (r *SidecarRouter) ReportFeedback(ctx context.Context, payload map[string]interface{}) error {
	if r.feedbackReporter == nil {
		return nil
	}
	return r.feedbackReporter.ReportFeedback(ctx, payload)
}

func (r *SidecarRouter) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	strategy := r.config.Strategy
	resolved := r.resolver.Resolve(req)
	if len(resolved.Candidates) == 0 {
		return router.Decision{}, fmt.Errorf("%s: no eligible candidate: %w", strategy, r.config.Unavailable)
	}
	requestRouteID := uuid.NewString()
	res, err := r.decider.Decide(ctx, Query{
		Strategy:             strategy,
		RouteID:              requestRouteID,
		OrganizationID:       req.OrganizationID,
		InstallationID:       req.InstallationID,
		ClientApp:            req.ClientApp,
		RolloutID:            req.RolloutID,
		RequestedModel:       req.RequestedModel,
		PromptText:           req.PromptText,
		ConversationMessages: req.ConversationMessages,
		AvailableTools:       req.AvailableTools,
		FeedbackKey:          req.FeedbackKey,
		FeedbackRole:         req.FeedbackRole,
		EstimatedInputTokens: req.EstimatedInputTokens,
		HasTools:             req.HasTools,
		HasImages:            req.HasImages,
		RoutingIntent:        req.RoutingIntent,
		PreferredModels:      req.PreferredModels,
		RoutingKnobs:         req.RoutingKnobs,
		TrainingAllowed:      req.TrainingAllowed,
		CaptureMode:          req.CaptureMode,
		DebugEnabled:         req.DebugEnabled,
		Candidates:           resolved.Candidates,
	})
	if err != nil {
		observability.FromContext(ctx).Error("Policy router sidecar decision failed", "strategy", strategy, "err", err)
		return router.Decision{}, fmt.Errorf("%s: sidecar decide: %w: %w", strategy, err, r.config.Unavailable)
	}
	binding, ok := resolved.ByRosterID[res.Model]
	if !ok {
		return router.Decision{}, fmt.Errorf("%s: sidecar returned unknown model %q: %w", strategy, res.Model, r.config.Unavailable)
	}
	if res.Provider != "" && res.Provider != binding.Provider {
		return router.Decision{}, fmt.Errorf("%s: sidecar returned provider %q for %q, expected %q: %w", strategy, res.Provider, res.Model, binding.Provider, r.config.Unavailable)
	}

	propensity := float32(res.Propensity)
	if propensity <= 0 {
		propensity = 1
	}
	routeID := res.RouteID
	if routeID == "" {
		routeID = requestRouteID
	}
	debugRef := ""
	if req.DebugEnabled {
		debugRef = res.DebugRef
	}
	reason := string(strategy) + "_policy"
	if r.config.Reason != nil {
		reason = r.config.Reason(res)
	} else if res.Reason != "" {
		reason += "(" + res.Reason + ")"
	}

	observability.FromContext(ctx).Info("Policy router decided",
		"strategy", strategy,
		"route_id", routeID,
		"model", binding.CatalogID,
		"provider", binding.Provider,
		"roster_model", res.Model,
		"score", res.Score,
	)
	return router.Decision{
		Provider: binding.Provider,
		Model:    binding.CatalogID,
		Reason:   reason,
		Metadata: &router.RoutingMetadata{
			CandidateModels:      resolved.CandidateModels(),
			CandidateProviders:   resolved.CandidateProviders(),
			CandidateScores:      resolved.CatalogCandidateScores(res.CandidateScores),
			ChosenScore:          float32(res.Score),
			Propensity:           propensity,
			DisplayMarker:        res.DisplayMarker,
			RouteID:              routeID,
			Strategy:             string(strategy),
			PolicyRouteKey:       res.PolicyRouteKey,
			PolicyArtifactID:     res.PolicyArtifactID,
			PolicyArtifactSHA256: res.PolicyArtifactSHA256,
			RosterVersion:        res.RosterVersion,
			SidecarSchemaVersion: res.SchemaVersion,
			DebugRef:             debugRef,
		},
	}, nil
}

var _ router.Router = (*SidecarRouter)(nil)
var _ OutcomeReporter = (*SidecarRouter)(nil)
var _ FeedbackReporter = (*SidecarRouter)(nil)
