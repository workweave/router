// Package otel provides the OTel span emitter and model pricing.
//
//go:generate go run ../../../cmd/genprices
package otel

import "workweave/router/internal/router/pricing"

// Pricing holds the per-1M-token USD costs for a single model.
//
// Re-exported from internal/router/pricing so existing callers in the
// observability adapter (and the genprices command) keep working. The
// source-of-truth table lives in the inner-ring pricing package.
type Pricing = pricing.Pricing

// AllPricing returns a copy of the full pricing table keyed by model name.
func AllPricing() map[string]Pricing {
	return pricing.All()
}

// Lookup returns pricing for the given model. If the exact name is not found,
// it retries after stripping a trailing 8-digit suffix (e.g. "-20251001") so
// dated model variants resolve to their canonical pricing. Returns zero-value
// for unknown models.
func Lookup(model string) Pricing {
	p, _ := pricing.For(model)
	return p
}
