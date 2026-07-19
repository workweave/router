package proxy

import (
	"context"
	"fmt"
	"strings"

	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/router/turntype"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

const (
	AgentShadowModelHeader   = "x-weave-agent-shadow-model"
	AgentShadowRolloutHeader = "x-weave-agent-shadow-rollout-id"
	AgentShadowStateHeader   = "x-weave-agent-shadow-state-id"
	ReasonAgentShadowEval    = "agent_shadow_eval_force"
)

type AgentShadowEvaluation struct {
	Model     string
	RolloutID string
	StateID   string
}

type AgentShadowEvalContextKey struct{}

func AgentShadowEvalFromContext(ctx context.Context) (AgentShadowEvaluation, bool) {
	value, ok := ctx.Value(AgentShadowEvalContextKey{}).(AgentShadowEvaluation)
	return value, ok && value.Model != "" && value.RolloutID != "" && value.StateID != ""
}

// runAgentShadowEvaluationRoute forces the preplanned model without touching pins, planners, feedback, or policy state.
func (s *Service) runAgentShadowEvaluationRoute(
	ctx context.Context,
	env *translate.RequestEnvelope,
	feats translate.RoutingFeatures,
	installationID uuid.UUID,
	req router.Request,
	evaluation AgentShadowEvaluation,
) (turnLoopResult, error) {
	req.TranslationRequirements = env.TranslationRequirements(router.EndpointAnthropicMessages)
	var err error
	req, err = s.applyTranslationPlan(ctx, req)
	if err != nil {
		return turnLoopResult{}, err
	}
	rawModel := strings.TrimSpace(evaluation.Model)
	model, provider, known, _ := resolveForceModelWithEffort(rawModel)
	if !known || model != rawModel {
		return turnLoopResult{}, fmt.Errorf("agent-shadow model must be a canonical catalog id: %q", rawModel)
	}
	if _, excluded := req.ExcludedModels[model]; excluded {
		return turnLoopResult{}, fmt.Errorf("agent-shadow planned model %q is no longer eligible: %w", model, cluster.ErrNoEligibleProvider)
	}
	if req.EnabledProviders != nil {
		if _, enabled := req.EnabledProviders[provider]; !enabled {
			// Planned provider may be disabled; fall back to the first available catalog binding.
			provider = ""
			for _, binding := range catalog.AvailableBindings(model, req.EnabledProviders) {
				if _, excludedProvider := s.excludedProvidersForRequest(ctx)[binding.Provider]; !excludedProvider {
					provider = binding.Provider
					break
				}
			}
			if provider == "" {
				return turnLoopResult{}, fmt.Errorf("agent-shadow model %q has no enabled provider: %w", model, cluster.ErrNoEligibleProvider)
			}
		}
	}
	if _, err := s.provider(provider); err != nil {
		return turnLoopResult{}, fmt.Errorf("agent-shadow provider %q is unavailable: %w", provider, err)
	}
	decision := router.Decision{Model: model, Provider: provider, Reason: ReasonAgentShadowEval}
	requestedTier := catalog.TierFor(feats.Model)
	return turnLoopResult{
		InstallationID: installationID,
		TurnType:       turntype.DetectFromEnvelope(env, feats, ""),
		PinTier:        "agent_shadow_eval",
		PinRole:        roleForTier(requestedTier),
		RequestedTier:  requestedTier,
		Decision:       decision,
		Fresh:          decision,
		// Eval routing does not read pins, so request evidence must guard stale signatures.
		StripThinkingBlocks: feats.Model != model || env.SignatureTokenSavings() > 0,
	}, nil
}

func isAgentShadowEvaluation(ctx context.Context) bool {
	_, ok := AgentShadowEvalFromContext(ctx)
	return ok
}
