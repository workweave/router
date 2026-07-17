package proxy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"workweave/router/internal/observability/apm"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/translate"
)

// TranslationCompatibilityMode controls whether broad compatibility filters
// only report (shadow) or constrain routing (enforce). Native-only safety
// requirements remain enforced outside the emergency off mode.
type TranslationCompatibilityMode string

const (
	TranslationCompatibilityOff     TranslationCompatibilityMode = "off"
	TranslationCompatibilityShadow  TranslationCompatibilityMode = "shadow"
	TranslationCompatibilityEnforce TranslationCompatibilityMode = "enforce"
)

// ErrTranslationIntrinsicallyIncompatible means the request's requirements
// have no supported preserving target at all. Retrying later cannot help.
var ErrTranslationIntrinsicallyIncompatible = errors.New("translation requirements are intrinsically incompatible")

// ErrTranslationCompatibleProviderUnavailable means a preserving target is
// defined by the compatibility matrix, but none is configured/authorized for
// this request. A configuration or credential change can make it servable.
var ErrTranslationCompatibleProviderUnavailable = errors.New("no compatible provider is available for translation requirements")

// TranslationExclusion is a stable compatibility diagnostic. Code is safe for
// metrics; provider/model are structured-log and trace dimensions only.
type TranslationExclusion struct {
	Code     string
	Provider string
	Model    string
	Enforced bool
}

// TranslationPlan is the instance-scoped result of compatibility planning.
// It intentionally reuses Request's ordinary candidate filters so every router
// implementation receives the same hard constraints.
type TranslationPlan struct {
	EnabledProviders map[string]struct{}
	ExcludedModels   map[string]struct{}
	TargetFamily     providers.TranslationFamily
	Exclusions       []TranslationExclusion
	Enforced         bool
	Intrinsic        bool
	Unavailable      bool
}

// translationConstraint is the code-owned compatibility matrix entry for one
// semantic. Families describe a wire protocol; ExactProviders is used when a
// particular endpoint (currently OpenAI Responses) is implemented by only one
// provider adapter despite sharing a broader protocol family.
type translationConstraint struct {
	Code           string
	TargetFamily   providers.TranslationFamily
	ExactProviders map[string]struct{}
}

// responsesRequirementsContextKey carries the original Responses contract
// while its Chat projection is routed through the existing turn loop.
type responsesRequirementsContextKey struct{}

type responsesTransformsContextKey struct{}

type translationPlanAppliedContextKey struct{}

func translationRequirementsFromContext(ctx context.Context) (router.TranslationRequirements, bool) {
	req, ok := ctx.Value(responsesRequirementsContextKey{}).(router.TranslationRequirements)
	return req, ok && !req.IsZero()
}

func responseTransformationsFromContext(ctx context.Context) []providers.RequestTransformation {
	transforms, _ := ctx.Value(responsesTransformsContextKey{}).([]translate.ResponseTransform)
	if len(transforms) == 0 {
		return nil
	}
	out := make([]providers.RequestTransformation, 0, len(transforms))
	for _, transform := range transforms {
		out = append(out, providers.RequestTransformation{
			Code:     transform.Code,
			Action:   providers.TransformationAction(transform.Action),
			Severity: providers.TransformationInfo,
			Path:     transform.Path,
		})
	}
	return out
}

func translationEndpointFor(env *translate.RequestEnvelope) router.TranslationEndpoint {
	switch env.SourceFormat() {
	case translate.FormatAnthropic:
		return router.EndpointAnthropicMessages
	case translate.FormatOpenAI:
		return router.EndpointOpenAIChat
	case translate.FormatGemini:
		return router.EndpointGeminiGenerate
	default:
		return ""
	}
}

// planTranslation computes candidate restrictions without performing I/O. Its
// matrix intentionally names only semantics that the existing translators can
// prove they preserve. Function tools, image inputs, and stream usage retain
// their portable paths; native-only unions, replay/signature state, provider
// cache controls, search/citation payloads, structured-output contracts, and
// non-image media require their source wire family until a translator explicitly
// adds a lossless cross-family mapping.
func (s *Service) planTranslation(req router.Request) TranslationPlan {
	plan := TranslationPlan{}
	if req.EnabledProviders != nil {
		plan.EnabledProviders = cloneStringSet(req.EnabledProviders)
	}
	plan.ExcludedModels = cloneStringSet(req.ExcludedModels)

	requirements := req.TranslationRequirements
	if requirements.IsZero() {
		return plan
	}

	plan.Enforced = s.translationCompatibilityMode == TranslationCompatibilityEnforce ||
		((requirements.NativeOnly || requirements.SourceFormat == router.WireFormatGemini) && s.translationCompatibilityMode != TranslationCompatibilityOff)
	constraints, valid := translationConstraints(requirements)
	if !valid {
		plan.Intrinsic = plan.Enforced
		return plan
	}
	if len(constraints) > 0 {
		plan.TargetFamily = constraints[0].TargetFamily
		candidates := candidateProviders(req.EnabledProviders, s.providers)
		for _, constraint := range constraints {
			plan.Exclusions = append(plan.Exclusions, constraintExclusions(candidates, constraint, plan.Enforced)...)
		}
		if plan.Enforced {
			plan.EnabledProviders = filterProvidersByConstraints(candidates, constraints)
			// A candidate must also be registered locally. This distinguishes an
			// unavailable compatible route from an intrinsic wire incompatibility
			// before the scorer can turn it into a generic no-eligible-model error.
			plan.Unavailable = len(configuredProviders(plan.EnabledProviders, s.providers)) == 0
		}
	}

	// Image capability was previously a soft quality preference. In enforce
	// mode it is semantic: sending media to a known text-only model cannot be
	// repaired by an upstream fallback after a response starts.
	if requirements.Images {
		for model := range catalog.ImageUnsupportedSet() {
			plan.Exclusions = append(plan.Exclusions, TranslationExclusion{
				Code: "image_input_unsupported", Model: model, Enforced: plan.Enforced,
			})
			if plan.Enforced {
				if plan.ExcludedModels == nil {
					plan.ExcludedModels = make(map[string]struct{})
				}
				plan.ExcludedModels[model] = struct{}{}
			}
		}
	}
	return plan
}

// translationConstraints is the compatibility matrix. Native source routing
// uses a family for Messages/Chat/GenerateContent, but Responses is exact-
// provider because only the OpenAI adapter currently implements EndpointResponses.
func translationConstraints(req router.TranslationRequirements) ([]translationConstraint, bool) {
	constraints := make([]translationConstraint, 0, 8)
	addSourceNative := func(code string) bool {
		constraint, ok := sourceNativeConstraint(req, code)
		if ok {
			constraints = append(constraints, constraint)
		}
		return ok
	}

	if req.NativeOnly && !addSourceNative("native_wire_family_required") {
		return nil, false
	}
	// Native Gemini emission is the only implemented Gemini path; do not let a
	// shadow rollout turn this into an accidental cross-format request.
	if req.SourceFormat == router.WireFormatGemini && !req.NativeOnly && !addSourceNative("native_wire_family_required") {
		return nil, false
	}

	for _, semantic := range []struct {
		present bool
		code    string
	}{
		{req.ReasoningReplay, "reasoning_replay_native_required"},
		{req.ReasoningSignature, "reasoning_signature_native_required"},
		{req.PromptCacheControl, "prompt_cache_control_native_required"},
		{req.CitationsOrSearch, "citations_or_search_native_required"},
		{req.StructuredOutput, "structured_output_native_required"},
		{req.Audio, "audio_input_native_required"},
		{req.Files, "file_input_native_required"},
	} {
		if semantic.present && !addSourceNative(semantic.code) {
			return nil, false
		}
	}
	return constraints, true
}

func sourceNativeConstraint(req router.TranslationRequirements, code string) (translationConstraint, bool) {
	switch req.SourceFormat {
	case router.WireFormatAnthropic:
		return translationConstraint{Code: code, TargetFamily: providers.FamilyAnthropic, ExactProviders: singletonProviderSet(providers.ProviderAnthropic)}, true
	case router.WireFormatOpenAI:
		constraint := translationConstraint{Code: code, TargetFamily: providers.FamilyOpenAICompat}
		if req.Endpoint == router.EndpointOpenAIResponses {
			constraint.ExactProviders = singletonProviderSet(providers.ProviderOpenAI)
		}
		return constraint, true
	case router.WireFormatGemini:
		return translationConstraint{Code: code, TargetFamily: providers.FamilyGemini, ExactProviders: singletonProviderSet(providers.ProviderGoogle)}, true
	default:
		return translationConstraint{}, false
	}
}

func singletonProviderSet(provider string) map[string]struct{} {
	return map[string]struct{}{provider: {}}
}

func candidateProviders(enabled map[string]struct{}, configured map[string]providers.Client) map[string]struct{} {
	if enabled != nil {
		return cloneStringSet(enabled)
	}
	out := make(map[string]struct{}, len(configured))
	for provider := range configured {
		out[provider] = struct{}{}
	}
	return out
}

func filterProvidersByConstraints(in map[string]struct{}, constraints []translationConstraint) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for provider := range in {
		if supportsAllConstraints(provider, constraints) {
			out[provider] = struct{}{}
		}
	}
	return out
}

func configuredProviders(candidates map[string]struct{}, configured map[string]providers.Client) map[string]struct{} {
	out := make(map[string]struct{}, len(candidates))
	for provider := range candidates {
		if _, ok := configured[provider]; ok {
			out[provider] = struct{}{}
		}
	}
	return out
}

func supportsAllConstraints(provider string, constraints []translationConstraint) bool {
	for _, constraint := range constraints {
		if len(constraint.ExactProviders) > 0 {
			if _, ok := constraint.ExactProviders[provider]; !ok {
				return false
			}
			continue
		}
		if providers.FamilyFor(provider) != constraint.TargetFamily {
			return false
		}
	}
	return true
}

func constraintExclusions(candidates map[string]struct{}, constraint translationConstraint, enforced bool) []TranslationExclusion {
	providersList := make([]string, 0, len(candidates))
	for provider := range candidates {
		providersList = append(providersList, provider)
	}
	sort.Strings(providersList)
	out := make([]TranslationExclusion, 0, len(providersList))
	for _, provider := range providersList {
		if !supportsAllConstraints(provider, []translationConstraint{constraint}) {
			out = append(out, TranslationExclusion{Code: constraint.Code, Provider: provider, Enforced: enforced})
		}
	}
	return out
}

func cloneStringSet(in map[string]struct{}) map[string]struct{} {
	if in == nil {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for value := range in {
		out[value] = struct{}{}
	}
	return out
}

// applyTranslationPlan attaches hard eligibility to req and emits compact
// structured diagnostics. It is deliberately idempotent because routeFor may
// be invoked more than once by the turn loop.
func (s *Service) applyTranslationPlan(ctx context.Context, req router.Request) (router.Request, error) {
	if applied, _ := ctx.Value(translationPlanAppliedContextKey{}).(bool); applied {
		return req, nil
	}
	plan := s.planTranslation(req)
	if plan.EnabledProviders != nil {
		req.EnabledProviders = plan.EnabledProviders
	}
	if plan.ExcludedModels != nil {
		req.ExcludedModels = plan.ExcludedModels
	}
	for _, exclusion := range plan.Exclusions {
		apm.RecordTranslationCompatibility(ctx,
			exclusion.Code,
			string(req.TranslationRequirements.SourceFormat),
			familyName(plan.TargetFamily),
			string(s.translationCompatibilityMode),
			exclusion.Enforced,
		)
		otel.RecordLog(ctx, otel.LogRecord{
			Name: "translation.compatibility",
			Time: time.Now(),
			Attrs: otel.NewAttrBuilder(7).
				String("translation.requirement", exclusion.Code).
				String("translation.source_format", string(req.TranslationRequirements.SourceFormat)).
				String("translation.target_family", familyName(plan.TargetFamily)).
				String("translation.mode", string(s.translationCompatibilityMode)).
				String("translation.provider", exclusion.Provider).
				String("translation.model", exclusion.Model).
				Bool("translation.enforced", exclusion.Enforced).
				Build(),
		})
	}
	if plan.Intrinsic {
		return req, fmt.Errorf("%w: no preserving target for source format %q", ErrTranslationIntrinsicallyIncompatible, req.TranslationRequirements.SourceFormat)
	}
	if plan.Unavailable {
		return req, fmt.Errorf("%w: target family %q", ErrTranslationCompatibleProviderUnavailable, familyName(plan.TargetFamily))
	}
	return req, nil
}

func familyName(family providers.TranslationFamily) string {
	switch family {
	case providers.FamilyAnthropic:
		return "anthropic"
	case providers.FamilyOpenAICompat:
		return "openai"
	case providers.FamilyGemini:
		return "gemini"
	default:
		return ""
	}
}
