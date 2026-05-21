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

// IsAtOrBelow reports whether the model has a known tier at or below the
// ceiling. Unknown-tier models return false.
func IsAtOrBelow(id string, ceiling Tier) bool {
	t := TierFor(id)
	if t == TierUnknown {
		return false
	}
	return t <= ceiling
}

// Per-tier conservative input-context fallback used by FitsContext when
// a Model.MaxInputTokens is unset (zero). Sized to the smallest credible
// context window observed in each tier's family so the filter excludes
// flash/low-tier models that demonstrably can't synthesize coherent
// responses to large bodies (observed: qwen3.5-flash returning empty
// end_turn for an 80k-token post-tool follow-up). Per-model overrides
// in catalog.go should be added whenever a verified number is available.
const (
	defaultMaxInputTokensTierLow  = 64_000
	defaultMaxInputTokensTierMid  = 128_000
	defaultMaxInputTokensTierHigh = 200_000
)

// FitsContext reports whether the model's input context window can hold
// the estimated input tokens for this turn. Returns true when:
//   - tokens <= 0 (no estimate yet — don't filter)
//   - model is absent from the catalog (passthrough — caller decides)
//   - model has an explicit MaxInputTokens >= tokens
//   - model has MaxInputTokens == 0 and the per-tier fallback >= tokens
//
// Used by the cluster scorer to exclude candidates that demonstrably
// can't synthesize a response at the requested size.
func FitsContext(modelID string, tokens int) bool {
	if tokens <= 0 {
		return true
	}
	m, ok := ByID(modelID)
	if !ok {
		return true
	}
	limit := m.MaxInputTokens
	if limit == 0 {
		switch m.Tier {
		case TierLow:
			limit = defaultMaxInputTokensTierLow
		case TierMid:
			limit = defaultMaxInputTokensTierMid
		case TierHigh:
			limit = defaultMaxInputTokensTierHigh
		default:
			return true // TierUnknown: passthrough-only, don't filter.
		}
	}
	return tokens <= limit
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
