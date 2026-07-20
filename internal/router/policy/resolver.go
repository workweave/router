// Package policy provides shared, I/O-free building blocks for router
// implementations that delegate a decision to an external policy.
package policy

import (
	"crypto/sha256"
	"fmt"
	"sort"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
)

// RosterMapper maps a catalog model to the identifier understood by a policy
// artifact. An empty identifier intentionally excludes the model.
type RosterMapper func(catalog.Model) string

// ProviderPolicy limits which dispatch providers a policy router may offer.
type ProviderPolicy struct {
	Denied map[string]struct{}
}

// ManagedProviderPolicy excludes OpenRouter from managed policy candidates.
func ManagedProviderPolicy() ProviderPolicy {
	return ProviderPolicy{Denied: map[string]struct{}{providers.ProviderOpenRouter: {}}}
}

// Allows reports whether provider may be offered to the policy.
func (p ProviderPolicy) Allows(provider string) bool {
	_, denied := p.Denied[provider]
	return !denied
}

// ExclusionReason identifies why a deployed catalog model was not offered.
type ExclusionReason string

const (
	// ExclusionRequested means the installation or request excluded the model.
	ExclusionRequested ExclusionReason = "requested_exclusion"
	// ExclusionUnknownCatalogModel means the deployed set named no catalog row.
	ExclusionUnknownCatalogModel ExclusionReason = "unknown_catalog_model"
	// ExclusionUnmappedRoster means the strategy intentionally has no roster ID.
	ExclusionUnmappedRoster ExclusionReason = "unmapped_roster"
	// ExclusionNoProvider means no request-enabled provider can dispatch the model.
	ExclusionNoProvider ExclusionReason = "no_enabled_provider"
	// ExclusionProviderPolicy means all resolvable providers were policy-denied.
	ExclusionProviderPolicy ExclusionReason = "provider_policy"
	// ExclusionImageCapability means a capable peer replaced this text-only model.
	ExclusionImageCapability ExclusionReason = "image_capability"
	// ExclusionToolCapability means a capable peer replaced this weak tool model.
	ExclusionToolCapability ExclusionReason = "tool_capability"
	// ExclusionAmbiguousRoster means multiple catalog models mapped to one roster ID.
	ExclusionAmbiguousRoster ExclusionReason = "ambiguous_roster_id"
	// ExclusionContextWindow means the estimated input cannot fit the model.
	ExclusionContextWindow ExclusionReason = "context_window"
)

// Diagnostic describes one candidate exclusion for conformance checks and
// debug-mode inspection. It contains no request content.
type Diagnostic struct {
	CatalogID string          `json:"catalog_id"`
	RosterID  string          `json:"roster_id,omitempty"`
	Reason    ExclusionReason `json:"reason"`
}

// Candidate is one catalog-backed model offered to a policy sidecar.
type Candidate struct {
	ArmID                     string                `json:"arm_id"`
	RosterID                  string                `json:"roster_id"`
	CatalogID                 string                `json:"catalog_id"`
	Provider                  string                `json:"provider"`
	UpstreamID                string                `json:"upstream_id"`
	PreferenceRank            *int                  `json:"preference_rank,omitempty"`
	InputUSDPer1M             float64               `json:"input_usd_per_1m"`
	OutputUSDPer1M            float64               `json:"output_usd_per_1m"`
	EstimatedCostUSD          float64               `json:"estimated_cost_usd"`
	CacheReadMultiplier       float64               `json:"cache_read_multiplier"`
	MarginalCostFactor        float64               `json:"marginal_cost_factor"`
	EffectiveInputUSDPer1M    float64               `json:"effective_input_usd_per_1m"`
	EffectiveOutputUSDPer1M   float64               `json:"effective_output_usd_per_1m"`
	EffectiveEstimatedCostUSD float64               `json:"effective_estimated_cost_usd"`
	Capabilities              CandidateCapabilities `json:"capabilities"`
}

// CandidateCapabilities describes only dispatch-relevant catalog facts. It is
// deliberately compact and versioned by the enclosing policy contract.
type CandidateCapabilities struct {
	ContextWindow  int    `json:"context_window"`
	Tier           string `json:"tier"`
	SupportsTools  bool   `json:"supports_tools"`
	SupportsImages bool   `json:"supports_images"`
}

// Binding is the authoritative dispatch binding for an offered roster ID.
type Binding struct {
	CatalogID  string
	Provider   string
	UpstreamID string
}

// ResolvedCandidates is the complete result of candidate resolution.
type ResolvedCandidates struct {
	Candidates  []Candidate
	ByArmID     map[string]Binding
	ByRosterID  map[string]Binding
	Diagnostics []Diagnostic
}

// CandidateModels returns catalog IDs in deterministic candidate order.
func (r ResolvedCandidates) CandidateModels() []string {
	models := make([]string, 0, len(r.Candidates))
	for _, candidate := range r.Candidates {
		models = append(models, candidate.CatalogID)
	}
	return models
}

// CandidateProviders returns the resolved provider for each catalog model.
func (r ResolvedCandidates) CandidateProviders() map[string]string {
	result := make(map[string]string, len(r.Candidates))
	for _, candidate := range r.Candidates {
		result[candidate.CatalogID] = candidate.Provider
	}
	return result
}

// CatalogCandidateScores translates sidecar roster IDs to telemetry catalog IDs.
func (r ResolvedCandidates) CatalogCandidateScores(scores map[string]float32) map[string]float32 {
	result := make(map[string]float32, len(scores))
	for selectionID, score := range scores {
		if binding, ok := r.ByArmID[selectionID]; ok {
			result[binding.CatalogID] = score
			continue
		}
		if binding, ok := r.ByRosterID[selectionID]; ok {
			result[binding.CatalogID] = score
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// Resolver builds the eligible catalog-backed candidate set for a policy.
type Resolver struct {
	deployed          map[string]struct{}
	available         map[string]struct{}
	mapper            RosterMapper
	providerPolicy    ProviderPolicy
	enumerateBindings bool
	toolLow           map[string]struct{}
	imageLow          map[string]struct{}
}

// NewResolver constructs a reusable policy candidate resolver.
func NewResolver(deployed, available map[string]struct{}, mapper RosterMapper, providerPolicy ProviderPolicy) *Resolver {
	return &Resolver{
		deployed:       deployed,
		available:      available,
		mapper:         mapper,
		providerPolicy: providerPolicy,
		toolLow:        catalog.ToolUseLowSet(),
		imageLow:       catalog.ImageUnsupportedSet(),
	}
}

// NewArmResolver returns every enabled catalog provider binding as a distinct
// policy candidate. It is for policies that score dispatch arms rather than
// logical catalog models; existing policy artifacts should keep NewResolver.
func NewArmResolver(deployed, available map[string]struct{}, mapper RosterMapper, providerPolicy ProviderPolicy) *Resolver {
	resolver := NewResolver(deployed, available, mapper, providerPolicy)
	resolver.enumerateBindings = true
	return resolver
}

type eligibleCandidate struct {
	Candidate
}

// Resolve applies request filters, provider policy, capability soft filters,
// and roster mapping. The returned candidate order is deterministic.
func (r *Resolver) Resolve(req router.Request) ResolvedCandidates {
	diagnostics := make([]Diagnostic, 0)
	base := make([]eligibleCandidate, 0, len(r.deployed))
	preferenceRanks := preferenceRanks(req.PreferredModels)

	deployedIDs := make([]string, 0, len(r.deployed))
	for id := range r.deployed {
		deployedIDs = append(deployedIDs, id)
	}
	sort.Strings(deployedIDs)

	for _, id := range deployedIDs {
		if _, excluded := req.ExcludedModels[id]; excluded {
			diagnostics = append(diagnostics, Diagnostic{CatalogID: id, Reason: ExclusionRequested})
			continue
		}
		model, ok := catalog.ByID(id)
		if !ok {
			diagnostics = append(diagnostics, Diagnostic{CatalogID: id, Reason: ExclusionUnknownCatalogModel})
			continue
		}
		rosterID := r.mapper(model)
		if rosterID == "" {
			diagnostics = append(diagnostics, Diagnostic{CatalogID: id, Reason: ExclusionUnmappedRoster})
			continue
		}
		contextWindow := catalog.ContextWindowFor(id)
		if requiredContextTokens(req) > contextWindow {
			diagnostics = append(diagnostics, Diagnostic{CatalogID: id, RosterID: rosterID, Reason: ExclusionContextWindow})
			continue
		}

		providerSet := req.EnabledProviders
		if providerSet == nil {
			providerSet = r.available
		}
		allowedBindings := catalog.AvailableBindings(
			id,
			r.allowedProviders(providerSet),
		)
		if len(allowedBindings) == 0 {
			reason := ExclusionNoProvider
			if unrestrictedBindings := catalog.AvailableBindings(id, providerSet); len(unrestrictedBindings) > 0 {
				reason = ExclusionProviderPolicy
			}
			diagnostics = append(diagnostics, Diagnostic{CatalogID: id, RosterID: rosterID, Reason: reason})
			continue
		}

		if !r.enumerateBindings {
			allowedBindings = allowedBindings[:1]
		}
		for _, binding := range allowedBindings {
			upstreamID := upstreamID(id, binding.UpstreamID)
			armID := rosterID
			if r.enumerateBindings {
				armID = makeArmID(rosterID, id, binding.Provider, upstreamID)
			}
			marginalCostFactor := 1.0
			if factor, found := req.SubsidizedModelCostFactor[id]; found && factor > 0 {
				marginalCostFactor = factor
			}
			base = append(base, eligibleCandidate{Candidate: Candidate{
				ArmID:                     armID,
				RosterID:                  rosterID,
				CatalogID:                 id,
				Provider:                  binding.Provider,
				UpstreamID:                upstreamID,
				PreferenceRank:            preferenceRanks[id],
				InputUSDPer1M:             binding.Price.InputUSDPer1M,
				OutputUSDPer1M:            binding.Price.OutputUSDPer1M,
				EstimatedCostUSD:          estimatedCostUSD(req, binding.Price),
				CacheReadMultiplier:       binding.Price.EffectiveCacheReadMultiplier(),
				MarginalCostFactor:        marginalCostFactor,
				EffectiveInputUSDPer1M:    binding.Price.InputUSDPer1M * marginalCostFactor,
				EffectiveOutputUSDPer1M:   binding.Price.OutputUSDPer1M * marginalCostFactor,
				EffectiveEstimatedCostUSD: estimatedCostUSD(req, binding.Price) * marginalCostFactor,
				Capabilities: CandidateCapabilities{
					ContextWindow:  contextWindow,
					Tier:           model.Tier.String(),
					SupportsTools:  model.ToolUseQuality != catalog.ToolUseLow && model.AgenticUse != catalog.AgenticLow,
					SupportsImages: model.ImageInput != catalog.ImageInputUnsupported,
				},
			}})
		}
	}

	base, diagnostics = softFilter(base, req.HasImages, r.imageLow, ExclusionImageCapability, diagnostics)
	base, diagnostics = softFilter(base, req.HasTools, r.toolLow, ExclusionToolCapability, diagnostics)

	selectionCounts := make(map[string]int, len(base))
	for _, candidate := range base {
		selectionCounts[candidate.ArmID]++
	}

	resolved := ResolvedCandidates{
		Candidates:  make([]Candidate, 0, len(base)),
		ByArmID:     make(map[string]Binding, len(base)),
		ByRosterID:  make(map[string]Binding, len(base)),
		Diagnostics: diagnostics,
	}
	for _, candidate := range base {
		if selectionCounts[candidate.ArmID] > 1 {
			resolved.Diagnostics = append(resolved.Diagnostics, Diagnostic{
				CatalogID: candidate.CatalogID,
				RosterID:  candidate.RosterID,
				Reason:    ExclusionAmbiguousRoster,
			})
			continue
		}
		resolved.Candidates = append(resolved.Candidates, candidate.Candidate)
		binding := Binding{
			CatalogID:  candidate.CatalogID,
			Provider:   candidate.Provider,
			UpstreamID: candidate.UpstreamID,
		}
		resolved.ByArmID[candidate.ArmID] = binding
		if _, exists := resolved.ByRosterID[candidate.RosterID]; !exists {
			resolved.ByRosterID[candidate.RosterID] = binding
		} else {
			delete(resolved.ByRosterID, candidate.RosterID)
		}
	}
	return resolved
}

// BindingForSelection resolves a sidecar selection by arm ID first, then
// preserves legacy roster-ID selection for existing policy artifacts.
func (r ResolvedCandidates) BindingForSelection(armID, rosterID string) (Binding, bool) {
	if armID != "" {
		binding, ok := r.ByArmID[armID]
		return binding, ok
	}
	binding, ok := r.ByRosterID[rosterID]
	return binding, ok
}

func upstreamID(catalogID, bindingID string) string {
	if bindingID != "" {
		return bindingID
	}
	return catalogID
}

func makeArmID(rosterID, catalogID, provider, upstreamID string) string {
	sum := sha256.Sum256([]byte(
		rosterID + "\x00" + catalogID + "\x00" + provider + "\x00" + upstreamID,
	))
	return fmt.Sprintf("arm_%x", sum)
}

func estimatedCostUSD(req router.Request, pricing catalog.Pricing) float64 {
	outputTokens := expectedOutputTokens(req)
	return (float64(req.EstimatedInputTokens)*pricing.InputUSDPer1M +
		float64(outputTokens)*pricing.OutputUSDPer1M) / 1_000_000
}

func requiredContextTokens(req router.Request) int {
	return max(req.EstimatedInputTokens, 0) + expectedOutputTokens(req)
}

func expectedOutputTokens(req router.Request) int {
	if req.RoutingKnobs == nil || req.RoutingKnobs.ExpectedOutputTokens == nil {
		return 0
	}
	return max(*req.RoutingKnobs.ExpectedOutputTokens, 0)
}

func (r *Resolver) allowedProviders(in map[string]struct{}) map[string]struct{} {
	allowed := make(map[string]struct{}, len(in))
	for provider := range in {
		if r.providerPolicy.Allows(provider) {
			allowed[provider] = struct{}{}
		}
	}
	return allowed
}

func preferenceRanks(models []string) map[string]*int {
	result := make(map[string]*int, len(models))
	for rank, model := range models {
		if _, exists := result[model]; exists {
			continue
		}
		rankCopy := rank
		result[model] = &rankCopy
	}
	return result
}

func softFilter(in []eligibleCandidate, active bool, drop map[string]struct{}, reason ExclusionReason, diagnostics []Diagnostic) ([]eligibleCandidate, []Diagnostic) {
	if !active || len(drop) == 0 {
		return in, diagnostics
	}
	kept := make([]eligibleCandidate, 0, len(in))
	dropped := make([]eligibleCandidate, 0)
	for _, candidate := range in {
		if _, shouldDrop := drop[candidate.CatalogID]; shouldDrop {
			dropped = append(dropped, candidate)
			continue
		}
		kept = append(kept, candidate)
	}
	if len(kept) == 0 {
		return in, diagnostics
	}
	for _, candidate := range dropped {
		diagnostics = append(diagnostics, Diagnostic{CatalogID: candidate.CatalogID, RosterID: candidate.RosterID, Reason: reason})
	}
	return kept, diagnostics
}
