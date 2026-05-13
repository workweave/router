// Package capability assigns a coarse Low/Mid/High tier to each
// deployed model so the planner can overturn a cost-driven "stay" when
// the scorer recommends a strictly stronger model. Hand-maintained on
// purpose — deriving tiers from price would silently move models on
// every pricing change.
package capability

import (
	"fmt"
	"sort"
	"strings"
)

// Tier is the coarse capability bucket. Higher is stronger; integer
// ordering is load-bearing (planner compares freshTier > pinTier).
type Tier int

const (
	// TierUnknown is the zero value for models absent from the table.
	// Per-request it disables the tier guard; Validate fails loud at
	// boot so a missing entry can't silently bypass it.
	TierUnknown Tier = iota
	TierLow
	TierMid
	TierHigh
)

// String returns a snake_case label for logs and OTel attrs.
func (t Tier) String() string {
	switch t {
	case TierLow:
		return "low"
	case TierMid:
		return "mid"
	case TierHigh:
		return "high"
	default:
		return "unknown"
	}
}

// tiers keys must match deployed model names in
// internal/router/cluster/artifacts/<version>/model_registry.json
// verbatim; Validate enforces this at boot.
var tiers = map[string]Tier{
	// --- Low ---
	"claude-haiku-4-5":                 TierLow,
	"gemini-3.1-flash-lite-preview":    TierLow,
	"gemini-2.5-flash":                 TierLow,
	"gpt-4.1-nano":                     TierLow,
	"gpt-4.1-mini":                     TierLow,
	"qwen/qwen3-30b-a3b-instruct-2507": TierLow,
	"qwen/qwen3.5-flash-02-23":         TierLow,
	"deepseek/deepseek-v4-flash":       TierLow,
	"mistralai/mistral-small-2603":     TierLow,

	// --- Mid ---
	"claude-sonnet-4-5":                TierMid,
	"gemini-3-flash-preview":           TierMid,
	"gpt-4.1":                          TierMid,
	"gpt-5.5-nano":                     TierMid,
	"gpt-5.5-mini":                     TierMid,
	"gpt-5.4-nano":                     TierMid,
	"gpt-5.4-mini":                     TierMid,
	"qwen/qwen3-235b-a22b-2507":        TierMid,
	"qwen/qwen3-coder":                 TierMid,
	"qwen/qwen3-coder-next":            TierMid,
	"qwen/qwen3-next-80b-a3b-instruct": TierMid,

	// --- High ---
	"claude-opus-4-7":          TierHigh,
	"gemini-3-pro-preview":     TierHigh,
	"gemini-3.1-pro-preview":   TierHigh,
	"gpt-5":                    TierHigh,
	"gpt-5.4":                  TierHigh,
	"gpt-5.5":                  TierHigh,
	"gpt-5.4-pro":              TierHigh,
	"gpt-5.5-pro":              TierHigh,
	"moonshotai/kimi-k2.5":     TierHigh,
	"deepseek/deepseek-v4-pro": TierHigh,
}

// TierFor returns the model's tier, or TierUnknown if absent.
func TierFor(model string) Tier {
	return tiers[model]
}

// IsAtOrBelow reports whether the given model's tier is known and at or
// below the ceiling. Unknown-tier models return false so the tier-ceiling
// guard fails closed — Validate already catches missing entries at boot,
// so any TierUnknown at request time means the table is out of sync.
func IsAtOrBelow(model string, ceiling Tier) bool {
	t := TierFor(model)
	if t == TierUnknown {
		return false
	}
	return t <= ceiling
}

// AllowedAtOrBelow returns the set of known models whose tier is at or
// below the ceiling. Used by the cluster bundle when picking a clamp
// target that respects the requested-model ceiling.
func AllowedAtOrBelow(ceiling Tier) map[string]struct{} {
	out := make(map[string]struct{}, len(tiers))
	for m, t := range tiers {
		if t != TierUnknown && t <= ceiling {
			out[m] = struct{}{}
		}
	}
	return out
}

// Validate returns an error naming any deployed model missing from the
// tier table. Called once at boot against the scorer's deployed set.
func Validate(deployed []string) error {
	var missing []string
	for _, m := range deployed {
		if _, ok := tiers[m]; !ok {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("capability: deployed models missing from tier table — add them to internal/router/capability/tier.go: %s", strings.Join(missing, ", "))
}
