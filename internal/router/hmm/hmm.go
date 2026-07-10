// Package hmm delegates model selection to a policy sidecar.
//
// The router builds the eligible candidate set from its catalog, delegates the
// choice, and dispatches through its normal provider machinery.
package hmm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"
)

var ErrHMMUnavailable = errors.New("hmm: policy router unavailable")

type Candidate = policy.Candidate

type Query = policy.Query
type Result = policy.Result
type Decider = policy.Decider
type OutcomeReporter = policy.OutcomeReporter
type FeedbackReporter = policy.FeedbackReporter

type Router struct {
	decider          Decider
	reporter         OutcomeReporter
	feedbackReporter FeedbackReporter
	resolver         *policy.Resolver
}

func New(decider Decider, deployed, available map[string]struct{}) *Router {
	reporter, _ := decider.(OutcomeReporter)
	feedbackReporter, _ := decider.(FeedbackReporter)
	return &Router{
		decider:          decider,
		reporter:         reporter,
		feedbackReporter: feedbackReporter,
		resolver:         policy.NewResolver(deployed, available, rosterIDFor, policy.ManagedProviderPolicy()),
	}
}

func (r *Router) ReportOutcome(ctx context.Context, payload map[string]interface{}) error {
	if r.reporter == nil {
		return nil
	}
	return r.reporter.ReportOutcome(ctx, payload)
}

func (r *Router) ReportFeedback(ctx context.Context, payload map[string]interface{}) error {
	if r.feedbackReporter == nil {
		return nil
	}
	return r.feedbackReporter.ReportFeedback(ctx, payload)
}

func (r *Router) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	log := observability.FromContext(ctx)
	resolved := r.resolver.Resolve(req)
	if len(resolved.Candidates) == 0 {
		return router.Decision{}, fmt.Errorf("hmm: no eligible candidate: %w", ErrHMMUnavailable)
	}
	requestRouteID := uuid.NewString()
	res, err := r.decider.Decide(ctx, Query{
		Strategy:             router.StrategyHMM,
		RouteID:              requestRouteID,
		OrganizationID:       req.OrganizationID,
		InstallationID:       req.InstallationID,
		ClientApp:            req.ClientApp,
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
		log.Error("HMM router: sidecar decide failed; returning ErrHMMUnavailable", "err", err)
		return router.Decision{}, fmt.Errorf("hmm: sidecar decide: %w: %w", err, ErrHMMUnavailable)
	}
	binding, ok := resolved.ByRosterID[res.Model]
	if !ok {
		return router.Decision{}, fmt.Errorf("hmm: sidecar returned unknown model %q: %w", res.Model, ErrHMMUnavailable)
	}
	if res.Provider != "" && res.Provider != binding.Provider {
		return router.Decision{}, fmt.Errorf("hmm: sidecar returned provider %q for %q, expected %q: %w", res.Provider, res.Model, binding.Provider, ErrHMMUnavailable)
	}
	propensity := float32(res.Propensity)
	if propensity <= 0 {
		propensity = 1.0
	}
	routeID := res.RouteID
	if routeID == "" {
		routeID = requestRouteID
	}
	log.Info("HMM router decided",
		"route_id", routeID,
		"model", binding.CatalogID,
		"provider", binding.Provider,
		"roster_model", res.Model,
		"score", res.Score,
		"label", res.PolicyLabel,
		"group", res.PolicyGroup,
	)
	return router.Decision{
		Provider: binding.Provider,
		Model:    binding.CatalogID,
		Reason:   reasonFor(res),
		Metadata: &router.RoutingMetadata{
			CandidateModels:      resolved.CandidateModels(),
			CandidateProviders:   resolved.CandidateProviders(),
			ChosenScore:          float32(res.Score),
			Propensity:           propensity,
			DisplayMarker:        res.DisplayMarker,
			RouteID:              routeID,
			Strategy:             string(router.StrategyHMM),
			PolicyRouteKey:       res.PolicyRouteKey,
			PolicyArtifactID:     res.PolicyArtifactID,
			PolicyArtifactSHA256: res.PolicyArtifactSHA256,
			RosterVersion:        res.RosterVersion,
			SidecarSchemaVersion: res.SchemaVersion,
			DebugRef:             res.DebugRef,
		},
	}, nil
}

func reasonFor(res Result) string {
	prefix := "hmm_policy"
	if isToolExecutionResult(res) {
		prefix = "hmm_policy:tool_execution"
	}
	if res.Reason != "" {
		return prefix + "(" + res.Reason + ")"
	}
	if res.PolicyLabel != "" {
		return prefix + "(label=" + res.PolicyLabel + ")"
	}
	return prefix
}

func isToolExecutionResult(res Result) bool {
	group := strings.TrimSpace(strings.ToLower(res.PolicyGroup))
	if group == "explore" {
		return true
	}
	label := strings.TrimSpace(strings.ToLower(res.PolicyLabel))
	return label == "spawn_explore" || strings.Contains(label, "tool_call")
}

var _ router.Router = (*Router)(nil)
var _ OutcomeReporter = (*Router)(nil)
