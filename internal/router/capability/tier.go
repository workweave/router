// Package capability assigns a coarse "tier" to each deployed model so
// the planner can overturn a cost-driven "stay" verdict when the scorer
// recommends a strictly stronger model than the session pin.
//
// The tier ladder is intentionally short (Low / Mid / High) and
// hand-maintained. We deliberately do NOT derive tiers from
// pricing.OutputUSDPer1M: output price tracks vendor pricing strategy
// more than capability (OSS models on OpenRouter are systematically
// cheap for their strength), and pricing churn would silently move
// models between tiers without any human review of whether the move
// is correct. A small explicit table is easier to reason about and
// keeps tier moves a deliberate decision.
//
// Adding a new deployed model means adding it here too. The boot-time
// Validate call against the cluster scorer's deployed set ensures a
// new model can never silently bypass the tier guard.
package capability

import (
	"fmt"
	"sort"
	"strings"
)

// Tier is the coarse capability bucket for a model. Higher is stronger.
// Comparisons use the standard integer ordering: a > b means a is in a
// strictly stronger bucket than b.
type Tier int

const (
	// TierUnknown is the zero value, returned when a model is not in the
	// tier table. Callers should treat it as "do not apply the tier
	// guard" (fail-soft per request); Validate is what makes a missing
	// entry fail loud at boot.
	TierUnknown Tier = iota
	// TierLow is small / fast / cheap models suitable for greetings,
	// short Q&A, and trivial code edits.
	TierLow
	// TierMid is capable generalists — coding, summarization, mid-depth
	// reasoning, most agentic workloads.
	TierMid
	// TierHigh is frontier models — hard design questions, long-horizon
	// reasoning, complex agentic loops.
	TierHigh
)

// String returns a snake_case label suitable for logs and OTel attrs.
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

// tiers is the source of truth. Keys must match the deployed model
// names in internal/router/cluster/artifacts/<version>/model_registry.json
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
	"claude-opus-4-7":           TierHigh,
	"gemini-3-pro-preview":      TierHigh,
	"gemini-3.1-pro-preview":    TierHigh,
	"gpt-5":                     TierHigh,
	"gpt-5.4":                   TierHigh,
	"gpt-5.5":                   TierHigh,
	"gpt-5.4-pro":               TierHigh,
	"gpt-5.5-pro":               TierHigh,
	"moonshotai/kimi-k2.5":      TierHigh,
	"deepseek/deepseek-v4-pro":  TierHigh,
}

// TierFor returns the capability tier for a model, or TierUnknown when
// the model is not in the table. TierUnknown disables the planner's
// tier guard for that decision (it never compares greater than any
// known tier).
func TierFor(model string) Tier {
	return tiers[model]
}

// Validate returns a non-nil error listing any model in deployed that
// is absent from the tier table. Intended to be called once at boot
// against the cluster scorer's deployed-models set so a newly-added
// model cannot silently bypass the tier guard.
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
