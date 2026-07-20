package policy

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"

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
	capabilitiesMu   sync.RWMutex
	capabilities     Capabilities
	capabilitiesSet  bool
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

// WithCapabilities gates optional outcome/feedback callbacks based on the sidecar's negotiated support.
func (r *SidecarRouter) WithCapabilities(capabilities Capabilities) *SidecarRouter {
	r.capabilitiesMu.Lock()
	defer r.capabilitiesMu.Unlock()

	r.capabilities = capabilities
	r.capabilitiesSet = true
	return r
}

// CurrentCapabilities returns the currently applied capability set.
func (r *SidecarRouter) CurrentCapabilities() Capabilities {
	r.capabilitiesMu.RLock()
	defer r.capabilitiesMu.RUnlock()
	return r.capabilities
}

func (r *SidecarRouter) ReportOutcome(ctx context.Context, payload map[string]interface{}) error {
	r.capabilitiesMu.RLock()
	enabled := !r.capabilitiesSet || r.capabilities.ReportsOutcomes
	r.capabilitiesMu.RUnlock()
	if !enabled || r.reporter == nil {
		return nil
	}
	return r.reporter.ReportOutcome(ctx, payload)
}

func (r *SidecarRouter) ReportFeedback(ctx context.Context, payload map[string]interface{}) error {
	r.capabilitiesMu.RLock()
	enabled := !r.capabilitiesSet || r.capabilities.ReportsFeedback
	r.capabilitiesMu.RUnlock()
	if !enabled || r.feedbackReporter == nil {
		return nil
	}
	return r.feedbackReporter.ReportFeedback(ctx, payload)
}

// PreviewRoute resolves candidates and returns all arms chosen by the sidecar's
// first nonempty ranked group without dispatching or lifecycle callbacks.
func (r *SidecarRouter) PreviewRoute(ctx context.Context, req router.Request) (PreviewResult, error) {
	strategy := r.config.Strategy
	r.capabilitiesMu.RLock()
	previewUnsupported := r.capabilitiesSet && !r.capabilities.SupportsPreview
	r.capabilitiesMu.RUnlock()
	if previewUnsupported {
		return PreviewResult{}, fmt.Errorf("%s: sidecar does not support preview: %w", strategy, r.config.Unavailable)
	}
	previewer, ok := r.decider.(PreviewDecider)
	if !ok {
		return PreviewResult{}, fmt.Errorf("%s: sidecar client has no preview contract: %w", strategy, r.config.Unavailable)
	}

	resolved := r.resolver.Resolve(req)
	requestRouteID := uuid.NewString()
	result, err := previewer.Preview(ctx, Query{
		SchemaVersion:        r.resolver.SchemaVersion(),
		Strategy:             strategy,
		ExecutionMode:        ExecutionModePreview,
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
		ClientSessionID:      req.ClientSessionID,
		TurnContext:          req.PolicyTurnContext,
		EstimatedInputTokens: req.EstimatedInputTokens,
		HasTools:             req.HasTools,
		HasImages:            req.HasImages,
		RoutingIntent:        req.RoutingIntent,
		PreferredModels:      req.PreferredModels,
		RoutingKnobs:         req.RoutingKnobs,
		TrainingAllowed:      false,
		CaptureMode:          req.CaptureMode,
		DebugEnabled:         true,
		Candidates:           resolved.Candidates,
	})
	if err != nil {
		return PreviewResult{}, fmt.Errorf("%s: sidecar preview: %w: %w", strategy, err, r.config.Unavailable)
	}
	if result.RouteID != "" && result.RouteID != requestRouteID {
		return PreviewResult{}, fmt.Errorf("%s: preview route id mismatch: %w", strategy, r.config.Unavailable)
	}
	if err := validatePreviewResult(result, r.resolver.SchemaVersion()); err != nil {
		return PreviewResult{}, fmt.Errorf("%s: invalid preview result: %v: %w", strategy, err, r.config.Unavailable)
	}

	eligibleCandidates := resolved.ByRosterID
	if r.resolver.SchemaVersion() == SchemaVersionV2 {
		eligibleCandidates = resolved.ByArmID
	}
	seen := make(map[string]struct{}, len(result.EligibleRosterIDs))
	for _, rosterID := range result.EligibleRosterIDs {
		if _, duplicate := seen[rosterID]; duplicate {
			return PreviewResult{}, fmt.Errorf("%s: preview returned duplicate roster id %q: %w", strategy, rosterID, r.config.Unavailable)
		}
		seen[rosterID] = struct{}{}
		if _, offered := eligibleCandidates[rosterID]; !offered {
			return PreviewResult{}, fmt.Errorf("%s: preview returned unknown roster id %q: %w", strategy, rosterID, r.config.Unavailable)
		}
	}
	if (len(result.EligibleRosterIDs) > 0) != (result.SelectedGroup != "") {
		return PreviewResult{}, fmt.Errorf("%s: preview selected group/arms mismatch: %w", strategy, r.config.Unavailable)
	}

	result.RouteID = requestRouteID
	result.Strategy = strategy
	result.ResolverCandidates = resolved.Candidates
	result.ResolverExclusions = resolved.Diagnostics
	return result, nil
}

func validatePreviewResult(result PreviewResult, expectedSchemaVersion string) error {
	if result.SchemaVersion != expectedSchemaVersion {
		return fmt.Errorf("unsupported schema %q", result.SchemaVersion)
	}
	if result.PolicyArtifactID == "" || result.PolicyArtifactSHA256 == "" || result.RosterSHA256 == "" {
		return fmt.Errorf("missing frozen artifact identity")
	}
	if len(result.HMMStatePath) == 0 || len(result.HMMStateProbabilities) == 0 || len(result.ClassOrder) == 0 {
		return fmt.Errorf("missing state or class order")
	}
	if result.HMMStateID < 0 || result.HMMStateID >= len(result.HMMStateProbabilities) {
		return fmt.Errorf("HMM state id is outside the posterior vector")
	}
	hmmTotal := 0.0
	for index, probability := range result.HMMStateProbabilities {
		if math.IsNaN(probability) || probability < 0 || probability > 1 {
			return fmt.Errorf("invalid HMM state probability at index %d", index)
		}
		hmmTotal += probability
	}
	if math.Abs(hmmTotal-1) > 1e-6 {
		return fmt.Errorf("HMM state probabilities sum to %.9f", hmmTotal)
	}
	for index, stateID := range result.HMMStatePath {
		if stateID < 0 || stateID >= len(result.HMMStateProbabilities) {
			return fmt.Errorf("HMM state path value at index %d is outside the posterior vector", index)
		}
	}
	if len(result.ClassProbabilities) != len(result.ClassOrder) || len(result.RankedFallback) != len(result.ClassOrder) {
		return fmt.Errorf("class probability/fallback cardinality mismatch")
	}
	seenClasses := make(map[string]struct{}, len(result.ClassOrder))
	total := 0.0
	for index, className := range result.ClassOrder {
		if className == "" {
			return fmt.Errorf("empty class at index %d", index)
		}
		if _, duplicate := seenClasses[className]; duplicate {
			return fmt.Errorf("duplicate class %q", className)
		}
		seenClasses[className] = struct{}{}
		probability, ok := result.ClassProbabilities[className]
		if !ok || math.IsNaN(probability) || probability < 0 || probability > 1 {
			return fmt.Errorf("invalid probability for class %q", className)
		}
		total += probability
	}
	if math.Abs(total-1) > 1e-6 {
		return fmt.Errorf("class probabilities sum to %.9f", total)
	}
	classRank := make(map[string]int, len(result.ClassOrder))
	for index, className := range result.ClassOrder {
		classRank[className] = index
	}
	seenFallback := make(map[string]struct{}, len(result.RankedFallback))
	for index, fallback := range result.RankedFallback {
		probability, ok := result.ClassProbabilities[fallback.Group]
		if !ok || math.Abs(fallback.Probability-probability) > 1e-9 {
			return fmt.Errorf("fallback probability mismatch for class %q", fallback.Group)
		}
		if _, duplicate := seenFallback[fallback.Group]; duplicate {
			return fmt.Errorf("duplicate fallback class %q", fallback.Group)
		}
		seenFallback[fallback.Group] = struct{}{}
		if index > 0 {
			previous := result.RankedFallback[index-1]
			if previous.Probability < fallback.Probability ||
				(previous.Probability == fallback.Probability && classRank[previous.Group] > classRank[fallback.Group]) {
				return fmt.Errorf("fallback groups are not deterministically ranked")
			}
		}
	}

	var selectedArms []string
	for _, fallback := range result.RankedFallback {
		if fallback.Group == result.SelectedGroup {
			selectedArms = fallback.EligibleArms
			break
		}
	}
	if !slices.Equal(selectedArms, result.EligibleRosterIDs) {
		return fmt.Errorf("selected fallback arms do not match eligible roster ids")
	}
	return nil
}

func (r *SidecarRouter) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	strategy := r.config.Strategy
	r.capabilitiesMu.RLock()
	capabilities := r.capabilities
	shadowUnsupported := r.capabilitiesSet && !capabilities.SupportsShadow
	r.capabilitiesMu.RUnlock()
	if req.ShadowMode && shadowUnsupported {
		return router.Decision{}, fmt.Errorf("%s: sidecar does not support shadow routing: %w", strategy, r.config.Unavailable)
	}
	executionMode := ExecutionModeServing
	if req.ShadowMode {
		executionMode = ExecutionModeShadow
		// Shadow decisions are operational comparisons, never learning events.
		req.TrainingAllowed = false
		req.DebugEnabled = false
	}
	resolved := r.resolver.Resolve(req)
	if len(resolved.Candidates) == 0 {
		return router.Decision{}, fmt.Errorf("%s: no eligible candidate: %w", strategy, r.config.Unavailable)
	}
	requestRouteID := uuid.NewString()
	res, err := r.decider.Decide(ctx, Query{
		SchemaVersion:        r.resolver.SchemaVersion(),
		Strategy:             strategy,
		ExecutionMode:        executionMode,
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
		ClientSessionID:      req.ClientSessionID,
		TurnContext:          req.PolicyTurnContext,
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
	binding, ok := resolved.BindingForSelection(res.ArmID, res.Model)
	if !ok {
		return router.Decision{}, fmt.Errorf("%s: sidecar returned unknown arm %q or model %q: %w", strategy, res.ArmID, res.Model, r.config.Unavailable)
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
		"execution_mode", executionMode,
		"route_id", routeID,
		"model", binding.CatalogID,
		"provider", binding.Provider,
		"arm_id", res.ArmID,
		"roster_model", res.Model,
		"score", res.Score,
	)
	return router.Decision{
		Provider: binding.Provider,
		Model:    binding.CatalogID,
		Reason:   reason,
		Metadata: &router.RoutingMetadata{
			CandidateModels:               resolved.CandidateModels(),
			CandidateProviders:            resolved.CandidateProviders(),
			CandidateScores:               resolved.CatalogCandidateScores(res.CandidateScores),
			CandidateArmProviders:         resolved.CandidateArmProviders(),
			CandidateArmScores:            resolved.ArmCandidateScores(res.CandidateScores),
			ChosenScore:                   float32(res.Score),
			Propensity:                    propensity,
			DisplayMarker:                 res.DisplayMarker,
			RouteID:                       routeID,
			Strategy:                      string(strategy),
			PolicyRouteKey:                res.PolicyRouteKey,
			PolicyArtifactID:              res.PolicyArtifactID,
			PolicyArtifactSHA256:          res.PolicyArtifactSHA256,
			RosterVersion:                 res.RosterVersion,
			SelectedArmID:                 res.ArmID,
			SidecarSchemaVersion:          res.SchemaVersion,
			DebugRef:                      debugRef,
			AuthoritativePerTurnSelection: capabilities.AuthoritativePerTurnSelection,
			SelectedUpstreamID:            binding.UpstreamID,
			BindingIndex:                  binding.BindingIndex,
			CandidateArmIDs:               resolved.CandidateArmIDs(),
		},
	}, nil
}

var _ RoutePreviewer = (*SidecarRouter)(nil)

var _ router.Router = (*SidecarRouter)(nil)
var _ OutcomeReporter = (*SidecarRouter)(nil)
var _ FeedbackReporter = (*SidecarRouter)(nil)
