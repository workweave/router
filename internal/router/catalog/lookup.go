package catalog

import (
	"fmt"
	"sort"
	"strings"
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
// retries after stripping a trailing date suffix (e.g. "-20251001").
func ByID(id string) (Model, bool) {
	if m, ok := byID[id]; ok {
		return m, true
	}
	if base := stripDateSuffix(id); base != id {
		if m, ok := byID[base]; ok {
			return m, true
		}
	}
	return Model{}, false
}

// ResolveBinding returns the first ProviderBinding whose Provider name is
// in `available`. Used at boot by the cluster scorer to pick which
// upstream serves each routable model.
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

// AvailableBindings returns every ProviderBinding for the model whose
// Provider name is in `available`, in catalog order. Used by the proxy's
// per-request failover loop: index 0 is the primary, indexes >0 are
// ordered fallbacks. Empty result means the model has no binding under
// the available set.
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

// PriceFor returns the per-(provider, model) pricing. Used by the planner
// when both pin and fresh have a resolved provider.
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

// PrimaryPriceFor returns the pricing of the first (primary) binding for
// the given model. Used by call sites (OTel emitter, billing debit hook,
// install-script generation) that don't yet thread provider through.
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
// inline <think>…</think> in the content channel (so the Anthropic translator
// should reroute it into thinking). Unknown models return false.
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

// ToolUseLowSet returns the set of model IDs whose ToolUseQuality is
// ToolUseLow. The cluster scorer subtracts this set from the eligible pool
// when req.HasTools is true; falls back to the unfiltered pool when the
// subtraction would empty the eligible set so a routing decision is always
// returned.
func ToolUseLowSet() map[string]struct{} {
	out := make(map[string]struct{}, len(Models))
	for _, m := range Models {
		if m.ToolUseQuality == ToolUseLow {
			out[m.ID] = struct{}{}
		}
	}
	return out
}

// AgenticLowSet returns the set of model IDs whose AgenticUse is AgenticLow —
// models that emit valid tool calls but can't sustain an agentic harness loop.
// The cluster scorer subtracts this set from the eligible pool when req.HasTools
// is true (on top of the ToolUseLow subtraction), so the price/quality dial
// demotes Opus to a cheaper harness-capable model rather than the cheapest model
// in the pool. Falls back to the unfiltered pool when the subtraction would empty
// the eligible set so a routing decision is always returned. Mirrors ToolUseLowSet.
func AgenticLowSet() map[string]struct{} {
	out := make(map[string]struct{}, len(Models))
	for _, m := range Models {
		if m.AgenticUse == AgenticLow {
			out[m.ID] = struct{}{}
		}
	}
	return out
}

// ImageUnsupportedSet returns the set of model IDs flagged
// ImageInputUnsupported. The cluster scorer subtracts this set from the
// eligible pool when the inbound request carries image content, falling back
// to the unfiltered pool when the subtraction would empty the eligible set so
// a routing decision is always returned. Mirrors ToolUseLowSet.
func ImageUnsupportedSet() map[string]struct{} {
	out := make(map[string]struct{}, len(Models))
	for _, m := range Models {
		if m.ImageInput == ImageInputUnsupported {
			out[m.ID] = struct{}{}
		}
	}
	return out
}

// AcceptsImages reports whether the model accepts image content parts. Unknown
// models and models with no ImageInput flag are treated as image-capable — only
// an explicit ImageInputUnsupported flag returns false. Used by the turn loop to
// evict a text-only session pin when the current turn carries an image.
func AcceptsImages(id string) bool {
	m, ok := ByID(id)
	if !ok {
		return true
	}
	return m.ImageInput != ImageInputUnsupported
}

// AllPrimaryPricing returns a copy of the primary-binding pricing for
// every known model, keyed by model ID. Used by the OTel emitter and the
// genprices install-script generator.
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

// ValidateDeployed returns an error naming any deployed model missing
// from the catalog or lacking a tier. Called once at boot against the
// cluster scorer's deployed-model set.
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

// stripDateSuffix removes a trailing "-XXXXXXXX" (hyphen + 8 digits).
func stripDateSuffix(model string) string {
	if len(model) < 10 {
		return model
	}
	tail := model[len(model)-9:]
	if tail[0] != '-' {
		return model
	}
	for _, c := range tail[1:] {
		if c < '0' || c > '9' {
			return model
		}
	}
	return model[:len(model)-9]
}
