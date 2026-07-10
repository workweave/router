package proxy

import (
	"context"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
)

// PolicyShadowStore persists comparison-only policy decisions; if nil,
// shadow routing still logs results without durable storage.
type PolicyShadowStore interface {
	InsertPolicyShadowDecision(ctx context.Context, event PolicyShadowDecision) error
}

// PolicyShadowDecision is one immutable serving-vs-shadow comparison. It does
// not contain prompt or response content.
type PolicyShadowDecision struct {
	InstallationID  string
	OrganizationID  string
	RolloutID       string
	ClientApp       string
	TrainingAllowed bool

	ServingStrategy             string
	ServingModel                string
	ServingProvider             string
	ServingRouteID              string
	ServingPolicyRouteKey       string
	ServingPolicyArtifactID     string
	ServingPolicyArtifactSHA256 string

	ShadowStrategy             string
	ShadowModel                string
	ShadowProvider             string
	ShadowRouteID              string
	ShadowPolicyRouteKey       string
	ShadowPolicyArtifactID     string
	ShadowPolicyArtifactSHA256 string
	ShadowLatencyMs            int64
	ShadowError                string
	ModelsAgree                bool
}

func (s *Service) firePolicyShadowForServingDecision(ctx context.Context, serving router.Decision, req router.Request) {
	req = s.withPolicyRequestContext(ctx, req)
	s.firePolicyShadowDecision(ctx, router.StrategyFromContext(ctx), serving, req)
}

func (s *Service) firePolicyShadowDecision(ctx context.Context, servingStrategy router.Strategy, serving router.Decision, req router.Request) {
	shadowStrategy, _ := ctx.Value(PolicyShadowStrategyContextKey{}).(router.Strategy)
	if shadowStrategy == "" || shadowStrategy == servingStrategy {
		return
	}

	shadowReq := req
	shadowReq.ShadowMode = true
	shadowReq.TrainingAllowed = false
	shadowReq.DebugEnabled = false
	store, _ := s.telemetry.(PolicyShadowStore)
	base := PolicyShadowDecision{
		InstallationID:  s.installationID(ctx),
		OrganizationID:  req.OrganizationID,
		RolloutID:       req.RolloutID,
		ClientApp:       req.ClientApp,
		TrainingAllowed: req.TrainingAllowed,
		ServingStrategy: string(servingStrategy),
		ServingModel:    serving.Model,
		ServingProvider: serving.Provider,
		ShadowStrategy:  string(shadowStrategy),
	}
	applyServingPolicyMetadata(&base, serving.Metadata)

	log := observability.FromContext(ctx).With(
		"installation_id", base.InstallationID,
		"serving_strategy", servingStrategy,
		"shadow_strategy", shadowStrategy,
	)
	observability.SafeGo(log, policyShadowDecisionTimeout, "policyShadowDecision", func(shadowCtx context.Context) {
		started := time.Now()
		shadow, err := s.routeWithStrategy(shadowCtx, shadowStrategy, shadowReq)
		event := base
		event.ShadowLatencyMs = time.Since(started).Milliseconds()
		if err != nil {
			event.ShadowError = truncatePolicyShadowError(err.Error())
		} else {
			event.ShadowModel = shadow.Model
			event.ShadowProvider = shadow.Provider
			event.ModelsAgree = shadow.Model == serving.Model
			applyShadowPolicyMetadata(&event, shadow.Metadata)
		}

		log.Info("router.policy_shadow",
			"serving_model", event.ServingModel,
			"shadow_model", event.ShadowModel,
			"models_agree", event.ModelsAgree,
			"shadow_latency_ms", event.ShadowLatencyMs,
			"shadow_error", event.ShadowError,
		)
		if store != nil && event.InstallationID != "" {
			if insertErr := store.InsertPolicyShadowDecision(shadowCtx, event); insertErr != nil {
				log.Warn("Policy shadow decision insert failed", "err", insertErr)
			}
		}
	})
}

func (s *Service) installationID(ctx context.Context) string {
	id, _ := ctx.Value(InstallationIDContextKey{}).(string)
	return id
}

func applyServingPolicyMetadata(event *PolicyShadowDecision, metadata *router.RoutingMetadata) {
	if metadata == nil {
		return
	}
	event.ServingRouteID = metadata.RouteID
	event.ServingPolicyRouteKey = metadata.PolicyRouteKey
	event.ServingPolicyArtifactID = metadata.PolicyArtifactID
	event.ServingPolicyArtifactSHA256 = metadata.PolicyArtifactSHA256
}

func applyShadowPolicyMetadata(event *PolicyShadowDecision, metadata *router.RoutingMetadata) {
	if metadata == nil {
		return
	}
	event.ShadowRouteID = metadata.RouteID
	event.ShadowPolicyRouteKey = metadata.PolicyRouteKey
	event.ShadowPolicyArtifactID = metadata.PolicyArtifactID
	event.ShadowPolicyArtifactSHA256 = metadata.PolicyArtifactSHA256
}

func truncatePolicyShadowError(value string) string {
	const maxBytes = 1000
	if len(value) <= maxBytes {
		return value
	}
	return value[:maxBytes]
}
