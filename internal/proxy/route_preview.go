package proxy

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"workweave/router/internal/billing"
	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"
	"workweave/router/internal/translate"
)

func (s *Service) anthropicRoutingRequest(ctx context.Context, body []byte, headers http.Header) (router.Request, error) {
	log := observability.FromContext(ctx)
	cleanBody, err := stripRoutingMarkerFromMessages(body)
	if err != nil {
		return router.Request{}, fmt.Errorf("strip routing marker: %w", err)
	}
	if withoutFooter, footerErr := translate.StripFeedbackFooterFromMessages(cleanBody); footerErr != nil {
		log.Error("Failed to strip feedback footer from route preview", "err", footerErr)
	} else {
		cleanBody = withoutFooter
	}
	if canonical, _, modelErr := translate.CanonicalizeModelInBody(cleanBody); modelErr != nil {
		log.Error("Failed to canonicalize model for route preview", "err", modelErr)
	} else {
		cleanBody = canonical
	}

	env, err := translate.ParseAnthropic(cleanBody)
	if err != nil {
		return router.Request{}, fmt.Errorf("parse request: %w", err)
	}
	embedOnlyUser := s.ResolveEmbedOnlyUserMessage(ctx)
	features := env.RoutingFeatures(embedOnlyUser)
	promptText := features.PromptText
	if embedOnlyUser && features.OnlyUserMessageText != "" {
		promptText = features.OnlyUserMessageText
	}

	enabledProviders := s.enabledProvidersForRequest(ctx, providers.ProviderAnthropic, headers)
	if billing.SubscriptionOnlyFromContext(ctx) {
		enabledProviders = restrictToSubscriptionProviders(ctx, headers, enabledProviders)
	}
	outputReserve := contextWindowOutputReserve
	if features.MaxTokens > outputReserve {
		outputReserve = features.MaxTokens
	}
	excluded := s.excludedModelsForRequest(ctx)
	excluded, _ = excludeContextOverflowModels(
		env.ContextOverflowTokenEstimate(),
		env.SignatureTokenSavings(),
		outputReserve,
		excluded,
		s.availableModels,
	)
	excluded, _ = excludeGemini3xOnUnsignedHistory(env, excluded, s.availableModels)

	organizationID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	installationID := ""
	if id := installationIDFromContext(ctx); id != uuid.Nil {
		installationID = id.String()
	}
	return router.Request{
		RequestedModel:               features.Model,
		EstimatedInputTokens:         features.Tokens,
		HasTools:                     features.HasTools,
		HasImages:                    features.HasImages,
		TranslationRequirements:      env.TranslationRequirements(router.EndpointAnthropicMessages),
		ReasoningConfigurationSHA256: env.ReasoningConfigurationSHA256(),
		ToolConfigurationSHA256:      env.ToolConfigurationSHA256(),
		PromptText:                   promptText,
		ConversationMessages:         conversationMessagesForRouting(env),
		AvailableTools:               availableToolsForRouting(env),
		OrganizationID:               organizationID,
		InstallationID:               installationID,
		ClientSessionID:              env.ClientSessionID(),
		EnabledProviders:             enabledProviders,
		ExcludedModels:               excluded,
		PreferredModels:              s.preferredModelsForRequest(ctx),
		RoutingKnobs:                 routingKnobsForRequest(ctx),
	}, nil
}

// PreviewAnthropicRoute evaluates an Anthropic request with the registered
// policy preview contract without dispatching or invoking serving lifecycle state.
func (s *Service) PreviewAnthropicRoute(ctx context.Context, body []byte, headers http.Header) (policy.PreviewResult, error) {
	req, err := s.anthropicRoutingRequest(ctx, body, headers)
	if err != nil {
		return policy.PreviewResult{}, err
	}
	req, err = s.applyTranslationPlan(ctx, req)
	if err != nil {
		return policy.PreviewResult{}, err
	}
	req = s.withPolicyRequestContext(ctx, req)
	strategy := router.StrategyFromContext(ctx)
	registered, ok := s.strategies[strategy]
	if !ok || registered.router == nil {
		return policy.PreviewResult{}, fmt.Errorf("strategy %q requested but no router configured: %w", strategy, defaultStrategyUnavailable(strategy))
	}
	previewer, ok := registered.router.(policy.RoutePreviewer)
	if !ok {
		return policy.PreviewResult{}, fmt.Errorf("strategy %q has no route preview contract: %w", strategy, registered.unavailable)
	}
	return previewer.PreviewRoute(ctx, req)
}
