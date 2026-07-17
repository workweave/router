package proxy

import (
	"context"
	"errors"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
)

func compatibilityService(mode TranslationCompatibilityMode) *Service {
	return &Service{
		providers: map[string]providers.Client{
			providers.ProviderAnthropic: nil,
			providers.ProviderOpenAI:    nil,
			providers.ProviderGoogle:    nil,
		},
		translationCompatibilityMode: mode,
	}
}

func TestTranslationPlan_NativeResponsesFiltersToOpenAIFamilyInShadow(t *testing.T) {
	svc := compatibilityService(TranslationCompatibilityShadow)
	plan := svc.planTranslation(router.Request{
		EnabledProviders: map[string]struct{}{
			providers.ProviderAnthropic: {},
			providers.ProviderOpenAI:    {},
		},
		TranslationRequirements: router.TranslationRequirements{
			SourceFormat: router.WireFormatOpenAI,
			Endpoint:     router.EndpointOpenAIResponses,
			CustomTools:  true,
			NativeOnly:   true,
		},
	})

	assert.Equal(t, map[string]struct{}{providers.ProviderOpenAI: {}}, plan.EnabledProviders)
	assert.Equal(t, providers.FamilyOpenAICompat, plan.TargetFamily)
	requireExclusion(t, plan, "native_wire_family_required", providers.ProviderAnthropic, true)
}

func TestTranslationPlan_GeminiIngressNeverOffersForeignFamily(t *testing.T) {
	svc := compatibilityService(TranslationCompatibilityShadow)
	plan := svc.planTranslation(router.Request{
		EnabledProviders: map[string]struct{}{
			providers.ProviderAnthropic: {},
			providers.ProviderGoogle:    {},
		},
		TranslationRequirements: router.TranslationRequirements{
			SourceFormat: router.WireFormatGemini,
			Endpoint:     router.EndpointGeminiGenerate,
		},
	})

	assert.Equal(t, map[string]struct{}{providers.ProviderGoogle: {}}, plan.EnabledProviders)
	requireExclusion(t, plan, "native_wire_family_required", providers.ProviderAnthropic, true)
}

func TestTranslationPlan_ImageConstraintShadowsBeforeEnforcement(t *testing.T) {
	req := router.Request{TranslationRequirements: router.TranslationRequirements{Images: true}}
	shadow := compatibilityService(TranslationCompatibilityShadow).planTranslation(req)
	enforce := compatibilityService(TranslationCompatibilityEnforce).planTranslation(req)

	assert.Empty(t, shadow.ExcludedModels, "shadow mode must preserve the pre-change candidate set")
	_, shadowReported := shadow.ExcludedModels["z-ai/glm-5"]
	assert.False(t, shadowReported)
	_, enforced := enforce.ExcludedModels["z-ai/glm-5"]
	assert.True(t, enforced, "known text-only models are hard excluded in enforce mode")
}

func TestTranslationPlan_OffRestoresNativeFamilyEligibility(t *testing.T) {
	plan := compatibilityService(TranslationCompatibilityOff).planTranslation(router.Request{
		EnabledProviders: map[string]struct{}{
			providers.ProviderAnthropic: {},
			providers.ProviderOpenAI:    {},
		},
		TranslationRequirements: router.TranslationRequirements{
			SourceFormat: router.WireFormatOpenAI,
			Endpoint:     router.EndpointOpenAIResponses,
			NativeOnly:   true,
		},
	})

	assert.Equal(t, map[string]struct{}{
		providers.ProviderAnthropic: {},
		providers.ProviderOpenAI:    {},
	}, plan.EnabledProviders)
	requireExclusion(t, plan, "native_wire_family_required", providers.ProviderAnthropic, false)
}

func TestTranslationPlan_NativeResponsesRequireOpenAIResponsesAdapter(t *testing.T) {
	svc := compatibilityService(TranslationCompatibilityShadow)
	plan := svc.planTranslation(router.Request{
		EnabledProviders: map[string]struct{}{
			providers.ProviderOpenAI:     {},
			providers.ProviderOpenRouter: {},
		},
		TranslationRequirements: router.TranslationRequirements{
			SourceFormat: router.WireFormatOpenAI,
			Endpoint:     router.EndpointOpenAIResponses,
			NativeOnly:   true,
		},
	})

	assert.Equal(t, map[string]struct{}{providers.ProviderOpenAI: {}}, plan.EnabledProviders)
	requireExclusion(t, plan, "native_wire_family_required", providers.ProviderOpenRouter, true)
}

func TestTranslationPlan_BroadSemanticRequirementOnlyFiltersInEnforce(t *testing.T) {
	req := router.Request{
		EnabledProviders: map[string]struct{}{
			providers.ProviderAnthropic: {},
			providers.ProviderOpenAI:    {},
		},
		TranslationRequirements: router.TranslationRequirements{
			SourceFormat:       router.WireFormatAnthropic,
			Endpoint:           router.EndpointAnthropicMessages,
			PromptCacheControl: true,
		},
	}
	shadow := compatibilityService(TranslationCompatibilityShadow).planTranslation(req)
	enforce := compatibilityService(TranslationCompatibilityEnforce).planTranslation(req)

	assert.Equal(t, req.EnabledProviders, shadow.EnabledProviders)
	requireExclusion(t, shadow, "prompt_cache_control_native_required", providers.ProviderOpenAI, false)
	assert.Equal(t, map[string]struct{}{providers.ProviderAnthropic: {}}, enforce.EnabledProviders)
	requireExclusion(t, enforce, "prompt_cache_control_native_required", providers.ProviderOpenAI, true)
}

func TestApplyTranslationPlan_CompatibleButUnavailable(t *testing.T) {
	svc := &Service{
		providers:                    map[string]providers.Client{providers.ProviderAnthropic: nil},
		translationCompatibilityMode: TranslationCompatibilityShadow,
	}
	_, err := svc.applyTranslationPlan(context.Background(), router.Request{
		EnabledProviders: map[string]struct{}{providers.ProviderAnthropic: {}},
		TranslationRequirements: router.TranslationRequirements{
			SourceFormat: router.WireFormatOpenAI,
			Endpoint:     router.EndpointOpenAIResponses,
			NativeOnly:   true,
		},
	})

	assert.ErrorIs(t, err, ErrTranslationCompatibleProviderUnavailable)
}

func TestApplyTranslationPlan_IntrinsicallyIncompatible(t *testing.T) {
	svc := compatibilityService(TranslationCompatibilityEnforce)
	_, err := svc.applyTranslationPlan(context.Background(), router.Request{
		TranslationRequirements: router.TranslationRequirements{NativeOnly: true},
	})

	assert.True(t, errors.Is(err, ErrTranslationIntrinsicallyIncompatible))
}

func requireExclusion(t *testing.T, plan TranslationPlan, code, provider string, enforced bool) {
	t.Helper()
	for _, exclusion := range plan.Exclusions {
		if exclusion.Code == code && exclusion.Provider == provider {
			assert.Equal(t, enforced, exclusion.Enforced)
			return
		}
	}
	t.Fatalf("missing exclusion code=%q provider=%q in %#v", code, provider, plan.Exclusions)
}
