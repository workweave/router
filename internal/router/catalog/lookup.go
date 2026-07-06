package catalog

import (
	"fmt"
	"sort"
	"strings"

	"workweave/router/internal/router"
)

// byID is built once at init from Models so accessors are O(1).
var byID map[string]Model

func init() {
	byID = make(map[string]Model, len(Models))
	for _, m := range Models {
		byID[m.ID] = m
	}
}

// ByID returns the model with the given ID. If the exact ID isn't found,
// retries after stripping a trailing Anthropic (-20251001) or OpenAI (-2024-08-06) date suffix.
func ByID(id string) (Model, bool) {
	if m, ok := byID[id]; ok {
		return m, true
	}
	if base := router.StripDateSuffix(id); base != id {
		if m, ok := byID[base]; ok {
			return m, true
		}
	}
	return Model{}, false
}

// ResolveBinding returns the first ProviderBinding whose Provider is in
// `available`. Used at boot to pick each routable model's upstream.
func ResolveBinding(id string, available map[string]struct{}) (ProviderBinding, bool) {
	m, ok := ByID(id)
	if !ok {
		return ProviderBinding{}, false
	}
	for _, b := range m.Providers {
		if _, ok := available[b.Provider]; ok {
			return b, true
		}
	}
	return ProviderBinding{}, false
}

// AvailableBindings returns every ProviderBinding for the model whose Provider
// is in `available`, in catalog order. Used by the proxy's failover loop:
// index 0 is primary, indexes >0 are ordered fallbacks.
func AvailableBindings(id string, available map[string]struct{}) []ProviderBinding {
	m, ok := ByID(id)
	if !ok {
		return nil
	}
	out := make([]ProviderBinding, 0, len(m.Providers))
	for _, b := range m.Providers {
		if _, ok := available[b.Provider]; ok {
			out = append(out, b)
		}
	}
	return out
}

// PriceFor returns the per-(provider, model) pricing.
func PriceFor(provider, id string) (Pricing, bool) {
	m, ok := ByID(id)
	if !ok {
		return Pricing{}, false
	}
	for _, b := range m.Providers {
		if b.Provider == provider {
			return b.Price, true
		}
	}
	return Pricing{}, false
}

// PrimaryPriceFor returns the pricing of the model's first (primary) binding,
// for call sites that don't thread a specific provider through.
func PrimaryPriceFor(id string) (Pricing, bool) {
	m, ok := ByID(id)
	if !ok {
		return Pricing{}, false
	}
	if len(m.Providers) == 0 {
		return Pricing{}, false
	}
	return m.Providers[0].Price, true
}

// TierFor returns the tier of the model, or TierUnknown if absent.
func TierFor(id string) Tier {
	m, ok := ByID(id)
	if !ok {
		return TierUnknown
	}
	return m.Tier
}

// ThinkTagReasoningFor reports whether the model streams chain-of-thought as
// inline <think>…</think> in content (the Anthropic translator reroutes it
// into thinking). Unknown models return false.
func ThinkTagReasoningFor(id string) bool {
	m, ok := ByID(id)
	if !ok {
		return false
	}
	return m.ThinkTagReasoning
}

// IsAtOrBelow reports whether the model has a known tier at or below the
// ceiling. Unknown-tier models return false.
func IsAtOrBelow(id string, ceiling Tier) bool {
	t := TierFor(id)
	if t == TierUnknown {
		return false
	}
	return t <= ceiling
}

// AllowedAtOrBelow returns the set of known model IDs whose tier is at or
// below the ceiling.
func AllowedAtOrBelow(ceiling Tier) map[string]struct{} {
	out := make(map[string]struct{}, len(Models))
	for _, m := range Models {
		if m.Tier != TierUnknown && m.Tier <= ceiling {
			out[m.ID] = struct{}{}
		}
	}
	return out
}

// DefaultContextWindow is the fallback context window in tokens for models
// with no ContextWindow set in the catalog.
const DefaultContextWindow = 128_000

// ContextWindowFor returns the context window in tokens for the given model.
// Returns DefaultContextWindow when the model is absent or has no ContextWindow set.
func ContextWindowFor(id string) int {
	m, ok := ByID(id)
	if !ok || m.ContextWindow <= 0 {
		return DefaultContextWindow
	}
	return m.ContextWindow
}

// ToolUseLowSet returns model IDs with ToolUseLow quality. The cluster scorer
// excludes these when req.HasTools, falling back to the unfiltered pool if
// that would empty the eligible set.
func ToolUseLowSet() map[string]struct{} {
	out := make(map[string]struct{}, len(Models))
	for _, m := range Models {
		if m.ToolUseQuality == ToolUseLow {
			out[m.ID] = struct{}{}
		}
	}
	return out
}

// AgenticLowSet returns model IDs whose AgenticUse is AgenticLow — models that
// emit valid tool calls but can't sustain an agentic harness loop. Excluded
// alongside ToolUseLowSet so a cost demotion lands on a harness-capable model,
// not just the cheapest one. Mirrors ToolUseLowSet's fallback behavior.
func AgenticLowSet() map[string]struct{} {
	out := make(map[string]struct{}, len(Models))
	for _, m := range Models {
		if m.AgenticUse == AgenticLow {
			out[m.ID] = struct{}{}
		}
	}
	return out
}

// ImageUnsupportedSet returns model IDs flagged ImageInputUnsupported,
// excluded when the request carries image content. Mirrors ToolUseLowSet's
// fallback behavior.
func ImageUnsupportedSet() map[string]struct{} {
	out := make(map[string]struct{}, len(Models))
	for _, m := range Models {
		if m.ImageInput == ImageInputUnsupported {
			out[m.ID] = struct{}{}
		}
	}
	return out
}

// AcceptsImages reports whether the model accepts image content. Unknown
// models default to true; only an explicit ImageInputUnsupported flag
// returns false.
func AcceptsImages(id string) bool {
	m, ok := ByID(id)
	if !ok {
		return true
	}
	return m.ImageInput != ImageInputUnsupported
}

// AllPrimaryPricing returns primary-binding pricing for every known model,
// keyed by model ID.
func AllPrimaryPricing() map[string]Pricing {
	out := make(map[string]Pricing, len(Models))
	for _, m := range Models {
		if len(m.Providers) == 0 {
			continue
		}
		out[m.ID] = m.Providers[0].Price
	}
	return out
}

// ValidateDeployed returns an error naming any deployed model missing from
// the catalog or lacking a tier.
func ValidateDeployed(deployed []string) error {
	var missing []string
	for _, id := range deployed {
		m, ok := ByID(id)
		if !ok {
			missing = append(missing, id+" (not in catalog)")
			continue
		}
		if m.Tier == TierUnknown {
			missing = append(missing, id+" (no tier set)")
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("catalog: deployed models missing or unconfigured — add or fix them in internal/router/catalog/catalog.go: %s", strings.Join(missing, ", "))
}
