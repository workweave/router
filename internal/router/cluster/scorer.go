package cluster

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
)

// ErrClusterUnavailable is returned when the cluster scorer cannot produce
// a routing decision. Callers map to HTTP 503.
var ErrClusterUnavailable = errors.New("cluster: routing unavailable")

// ErrNoEligibleProvider is returned when req.EnabledProviders has no
// overlap with boot-time candidates. Callers map to HTTP 4xx.
var ErrNoEligibleProvider = errors.New("cluster: no eligible provider for request")

// ErrInvalidRoutingKnobs is returned when effective routing knobs fail validation.
var ErrInvalidRoutingKnobs = errors.New("cluster: invalid routing knobs")

// Config carries the scorer's runtime knobs.
type Config struct {
	TopP           int
	MaxPromptChars int
	EmbedTimeout   time.Duration
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		TopP:           4,
		MaxPromptChars: 1024,
		EmbedTimeout:   1500 * time.Millisecond,
	}
}

// Scorer is the cluster router for one frozen artifact version.
type Scorer struct {
	version    string
	cfg        Config
	embed      Embedder
	centroids  *Centroids
	rankings   Rankings
	registry   *ModelRegistry
	candidates []DeployedEntry
	// availableProviders is the deployment's wired provider set (the same set
	// candidates were boot-filtered against). Retained so the dial preview can
	// reproduce Route's provider gate when projecting under an exclusion set.
	availableProviders map[string]struct{}
	models             []string
	metadata           *ArtifactMetadata // nil if absent; cache threshold source.
	isV2               bool
	qualityMeans       Rankings
	modelAxes          map[string]ModelAxis
	medianVerbosity    float64
	// dialAlphaBreakpoints holds ascending uniform-alpha values where the routed
	// mix changes (first=0, last=1). dialToAlpha interpolates across them so
	// equal dial travel = equal mix changes, killing dead zones where a wide
	// alpha range routes an identical mix. nil for v1 bundles.
	dialAlphaBreakpoints []float64
}

// Version returns the artifact version (e.g. "v0.2").
func (s *Scorer) Version() string { return s.version }

// DeployedModels returns a copy of the provider-filtered candidate list.
func (s *Scorer) DeployedModels() []DeployedEntry {
	out := make([]DeployedEntry, len(s.candidates))
	copy(out, s.candidates)
	return out
}

// CacheThresholds returns per-version semantic-cache thresholds from the
// bundle's metadata.yaml. defaultThreshold is 0 when unset; callers
// substitute their own runtime default.
func (s *Scorer) CacheThresholds() (perCluster map[int]float32, defaultThreshold float32) {
	if s.metadata == nil || s.metadata.CacheConfig == nil {
		return nil, 0
	}
	cfg := s.metadata.CacheConfig
	if len(cfg.PerClusterThreshold) > 0 {
		perCluster = make(map[int]float32, len(cfg.PerClusterThreshold))
		for k, v := range cfg.PerClusterThreshold {
			perCluster[k] = v
		}
	}
	return perCluster, cfg.DefaultThreshold
}

// NewScorer wires a Scorer from a pre-loaded Bundle. Entries whose
// provider is not in availableProviders are filtered out.
func NewScorer(bundle *Bundle, cfg Config, embed Embedder, availableProviders map[string]struct{}) (*Scorer, error) {
	if bundle == nil {
		return nil, fmt.Errorf("cluster: bundle must not be nil")
	}
	if embed == nil {
		return nil, fmt.Errorf("cluster: embedder must not be nil")
	}
	if cfg.TopP <= 0 {
		return nil, fmt.Errorf("cluster: TopP must be positive, got %d", cfg.TopP)
	}
	if cfg.MaxPromptChars <= 0 {
		return nil, fmt.Errorf("cluster: MaxPromptChars must be positive, got %d", cfg.MaxPromptChars)
	}
	if len(availableProviders) == 0 {
		return nil, fmt.Errorf("cluster: availableProviders must not be empty")
	}

	// A bundle trained in one embedding space must never be scored with a
	// different embedder; dim alone doesn't guard this since dims can collide.
	if embed.ID() != bundle.EmbedderID() {
		return nil, fmt.Errorf("cluster %s: bundle declares embedder %q but runtime embedder is %q", bundle.Version, bundle.EmbedderID(), embed.ID())
	}
	if embed.Dim() != bundle.Centroids.Dim {
		return nil, fmt.Errorf("cluster %s: embedder dim %d != centroids dim %d", bundle.Version, embed.Dim(), bundle.Centroids.Dim)
	}

	if bundle.Centroids.K < cfg.TopP {
		return nil, fmt.Errorf("cluster %s: K=%d < TopP=%d", bundle.Version, bundle.Centroids.K, cfg.TopP)
	}

	candidates := filterByProviders(bundle.Registry.DeployedModels, availableProviders)
	if len(candidates) == 0 {
		return nil, fmt.Errorf(
			"cluster %s: no deployed entry matches the registered providers %v; "+
				"add a provider key (ANTHROPIC_API_KEY / OPENAI_API_KEY / "+
				"OPENROUTER_API_KEY / GOOGLE_API_KEY) or re-run train_cluster_router.py to "+
				"include a model from a registered provider",
			bundle.Version, sortedKeys(availableProviders),
		)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Model < candidates[j].Model
	})

	deduped := candidates[:0]
	for i := range candidates {
		if i == 0 || candidates[i].Model != candidates[i-1].Model {
			deduped = append(deduped, candidates[i])
		}
	}
	candidates = deduped

	models := make([]string, len(candidates))
	for i, c := range candidates {
		models[i] = c.Model
	}

	// Every cluster in [0, K) must have a ranking/quality_means row, or a
	// missing cluster could win top-p at request time and contribute zero.
	if bundle.IsV2 {
		for k := 0; k < bundle.Centroids.K; k++ {
			row, ok := bundle.QualityMeans[k]
			if !ok {
				return nil, fmt.Errorf("cluster %s: quality_means missing cluster %d (centroids has K=%d)", bundle.Version, k, bundle.Centroids.K)
			}
			for _, m := range models {
				if _, ok := row[m]; !ok {
					return nil, fmt.Errorf("cluster %s: quality_means cluster %d missing model %q", bundle.Version, k, m)
				}
			}
		}
		// Validate default routing knobs against centroids.K at load time so a
		// misconfigured metadata.yaml fails here instead of HTTP 400-ing every
		// v2 request.
		if bundle.Metadata != nil && bundle.Metadata.Training.DefaultRoutingKnobs != nil {
			dk := bundle.Metadata.Training.DefaultRoutingKnobs
			if len(dk.Alpha) != bundle.Centroids.K {
				return nil, fmt.Errorf("cluster %s: default_routing_knobs.alpha length %d must equal K=%d", bundle.Version, len(dk.Alpha), bundle.Centroids.K)
			}
			// alpha_floor, if present, must be a full per-cluster vector of valid
			// weights: applyDialAlpha writes floor[i] straight into the alpha slot.
			if dk.AlphaFloor != nil {
				if len(dk.AlphaFloor) != bundle.Centroids.K {
					return nil, fmt.Errorf("cluster %s: default_routing_knobs.alpha_floor length %d must equal K=%d", bundle.Version, len(dk.AlphaFloor), bundle.Centroids.K)
				}
				for i, f := range dk.AlphaFloor {
					if f < 0 || f > 1 {
						return nil, fmt.Errorf("cluster %s: default_routing_knobs.alpha_floor[%d] (%f) must be in [0, 1]", bundle.Version, i, f)
					}
				}
			}
		}
	} else {
		for k := 0; k < bundle.Centroids.K; k++ {
			row, ok := bundle.Rankings[k]
			if !ok {
				return nil, fmt.Errorf("cluster %s: rankings missing cluster %d (centroids has K=%d)", bundle.Version, k, bundle.Centroids.K)
			}
			for _, m := range models {
				if _, ok := row[m]; !ok {
					return nil, fmt.Errorf("cluster %s: rankings cluster %d missing model %q", bundle.Version, k, m)
				}
			}
		}
	}

	s := &Scorer{
		version:            bundle.Version,
		cfg:                cfg,
		embed:              embed,
		centroids:          bundle.Centroids,
		rankings:           bundle.Rankings,
		registry:           bundle.Registry,
		candidates:         candidates,
		availableProviders: availableProviders,
		models:             models,
		metadata:           bundle.Metadata,
		isV2:               bundle.IsV2,
		qualityMeans:       bundle.QualityMeans,
		modelAxes:          bundle.ModelAxes,
		medianVerbosity:    bundle.MedianVerbosity,
	}
	// Must run after qualityMeans/modelAxes/models/centroids are populated,
	// since it replays the scorer across the alpha range. v1 bundles keep a
	// nil calibration (dialToAlpha falls back to identity).
	if bundle.IsV2 {
		s.dialAlphaBreakpoints = s.computeDialCalibration()
	}
	return s, nil
}

// qualityBiasCalibrationGrid is the uniform-alpha resolution swept at build
// time to find mix-change points: 401 points = steps of 0.0025.
const qualityBiasCalibrationGrid = 401

// computeDialCalibration walks a uniform alpha from 0 to 1, scoring every
// cluster centroid through the live blend, and records each alpha where the
// routed model mix first changes (ascending, first forced to 0, last to 1) —
// the dial breakpoints.
//
// These exist because cheap models are far cheaper while quality_means are
// tightly bunched, so a linear dial spends most of its travel on flat
// all-cheapest/all-best/stable-mid regions (the "50% looks like 20%" bug).
// dialToAlpha interpolates across the breakpoints instead, so equal dial
// travel crosses an equal number of mix changes.
//
// Returns nil when fewer than two distinct mixes exist, so dialToAlpha falls
// back to the identity.
func (s *Scorer) computeDialCalibration() []float64 {
	k := s.centroids.K
	centroidTopClusters := make([][]int, k)
	for c := 0; c < k; c++ {
		centroidTopClusters[c] = topPNearest(s.centroids.Row(c), s.centroids, s.cfg.TopP)
	}

	base := s.defaultActiveKnobs()
	breakpoints := make([]float64, 0, 32)
	prevSig := ""
	for g := 0; g < qualityBiasCalibrationGrid; g++ {
		a := float64(g) / float64(qualityBiasCalibrationGrid-1)
		knobs := base
		knobs.Alpha = make([]float64, k)
		for i := range knobs.Alpha {
			knobs.Alpha[i] = a
		}
		counts := make(map[string]int, len(s.models))
		for c := 0; c < k; c++ {
			scores := s.blendScoresV2(centroidTopClusters[c], knobs, s.models, nil, nil)
			winner, _ := argmax(scores, s.models)
			// Skip empty winners (matches RoutingDistribution) so a cluster
			// flipping between "" and a real model can't fake a breakpoint.
			if winner != "" {
				counts[winner]++
			}
		}
		sig := mixSignature(counts)
		if sig != prevSig {
			breakpoints = append(breakpoints, a)
			prevSig = sig
		}
	}

	if len(breakpoints) < 2 {
		return nil
	}
	breakpoints[0] = 0
	breakpoints[len(breakpoints)-1] = 1
	return breakpoints
}

// mixSignature renders a winner-count map as a stable, order-independent key so
// two dial positions that route the same model mix compare equal.
func mixSignature(counts map[string]int) string {
	models := make([]string, 0, len(counts))
	for m := range counts {
		models = append(models, m)
	}
	sort.Strings(models)
	var b strings.Builder
	for _, m := range models {
		fmt.Fprintf(&b, "%s:%d,", m, counts[m])
	}
	return b.String()
}

// dialToAlpha maps the QualityBias dial t in [0, 1] to the uniform per-cluster
// alpha, placing the bundle's mix breakpoints at equal dial spacing so travel
// isn't wasted on a dead zone. Identity when uncalibrated. t is clamped to
// [0, 1].
func (s *Scorer) dialToAlpha(t float64) float64 {
	if t <= 0 {
		return 0
	}
	if t >= 1 {
		return 1
	}
	bp := s.dialAlphaBreakpoints
	if len(bp) < 2 {
		return t
	}
	x := t * float64(len(bp)-1)
	i := int(x)
	if i >= len(bp)-1 {
		return bp[len(bp)-1]
	}
	frac := x - float64(i)
	return bp[i] + frac*(bp[i+1]-bp[i])
}

// applyDialAlpha resolves dial position t to per-cluster alpha in place:
// alpha[i] = max(dialToAlpha(t), floor[i]). floor is the lowest quality
// weight the bundle tolerates per cluster at max price-sensitivity, so a
// price-leaning dial can't collapse the whole vector onto the cheapest model
// (which stranded agentic turns on models that can't drive the harness).
// floor==nil disables flooring. Single source of truth shared by Route and
// RoutingDistribution; caller guarantees len(floor)==len(alpha) when non-nil.
func (s *Scorer) applyDialAlpha(t float64, alpha, floor []float64) {
	a := s.dialToAlpha(t)
	for i := range alpha {
		if floor != nil && floor[i] > a {
			alpha[i] = floor[i]
		} else {
			alpha[i] = a
		}
	}
}

// resolveProviderFor walks the catalog's ordered ProviderBinding list for
// modelID and returns the first binding whose Provider name is in
// available. Falls back to the registry's recorded provider for models
// absent from the catalog (defense in depth). Returns "" when no binding
// resolves under the available set.
func resolveProviderFor(modelID, registryProvider string, available map[string]struct{}) string {
	m, ok := catalog.ByID(modelID)
	if !ok {
		if _, ok := available[registryProvider]; ok {
			return registryProvider
		}
		return ""
	}
	for _, b := range m.Providers {
		if _, ok := available[b.Provider]; ok {
			return b.Provider
		}
	}
	return ""
}

// filterByProviders drops entries with no ProviderBinding resolvable under
// available, and rewrites each surviving entry's Provider to the resolved
// binding. For multi-binding rows (e.g. fireworks primary, openrouter
// fallback) it picks the first available in catalog order.
func filterByProviders(entries []DeployedEntry, available map[string]struct{}) []DeployedEntry {
	out := make([]DeployedEntry, 0, len(entries))
	for _, e := range entries {
		resolved := resolveProviderFor(e.Model, e.Provider, available)
		if resolved == "" {
			continue
		}
		e.Provider = resolved
		out = append(out, e)
	}
	return out
}

// eligibleForDistribution returns deployed model ids surviving the caller's
// exclusions, mirroring Route's eligibility: dropped if named in
// excludedModels, or if no binding resolves against wired providers minus
// excluded ones. Empty exclusions return the full roster. Iterates
// s.candidates so order matches Route.
func (s *Scorer) eligibleForDistribution(excludedModels, excludedProviders map[string]struct{}) []string {
	if len(excludedModels) == 0 && len(excludedProviders) == 0 {
		return s.models
	}

	// Gate against wired providers minus excluded, not the full catalog
	// binding list — else a model whose only WIRED binding is excluded would
	// wrongly survive on an undeployed catalog binding.
	effective := s.availableProviders
	if len(excludedProviders) > 0 {
		effective = make(map[string]struct{}, len(s.availableProviders))
		for p := range s.availableProviders {
			if _, excluded := excludedProviders[p]; !excluded {
				effective[p] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(s.models))
	for _, c := range s.candidates {
		if _, drop := excludedModels[c.Model]; drop {
			continue
		}
		if resolveProviderFor(c.Model, c.Provider, effective) == "" {
			continue
		}
		out = append(out, c.Model)
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Route embeds the prompt, scores clusters, returns the argmax decision.
func (s *Scorer) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	start := time.Now()
	log := observability.Get()

	text := TailTruncate(req.PromptText, s.cfg.MaxPromptChars)
	truncated := len(req.PromptText) > s.cfg.MaxPromptChars

	embedCtx, cancel := context.WithTimeout(ctx, s.cfg.EmbedTimeout)
	defer cancel()

	embedStart := time.Now()
	// Race Embed against EmbedTimeout since hugot can't be canceled.
	type embedResult struct {
		vec []float32
		err error
	}
	resCh := make(chan embedResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("Cluster scorer: embed panic; returning ErrClusterUnavailable", "panic", fmt.Sprint(r))
				resCh <- embedResult{err: fmt.Errorf("embed panic: %v", r)}
			}
		}()
		v, e := s.embed.Embed(embedCtx, text)
		resCh <- embedResult{vec: v, err: e}
	}()
	var vec []float32
	var err error
	select {
	case r := <-resCh:
		vec, err = r.vec, r.err
	case <-embedCtx.Done():
		err = embedCtx.Err()
	}
	embedMs := time.Since(embedStart).Milliseconds()
	if err != nil {
		log.Error(
			"Cluster scorer: embed failed; returning ErrClusterUnavailable",
			"err", err,
			"embed_ms", embedMs,
			"prompt_chars", len(text),
			"prompt_truncated", truncated,
			"requested_model", req.RequestedModel,
		)
		return router.Decision{}, fmt.Errorf("embed failed after %dms: %w (cause: %v)", embedMs, ErrClusterUnavailable, err)
	}
	if len(vec) != s.centroids.Dim {
		log.Error(
			"Cluster scorer: embedding dim mismatch; returning ErrClusterUnavailable",
			"got_dim", len(vec),
			"want_dim", s.centroids.Dim,
			"embed_ms", embedMs,
		)
		return router.Decision{}, fmt.Errorf("embedding dim %d != expected %d: %w", len(vec), s.centroids.Dim, ErrClusterUnavailable)
	}

	// Re-walk multi-binding rows under the per-request EnabledProviders set: a
	// BYOK-only request may narrow/widen the provider set vs. the deployment.
	// Track the resolved binding per model so Decision.Provider reflects it.
	eligibleModels := s.models
	resolvedProvider := make(map[string]string, len(s.candidates))
	if req.EnabledProviders != nil {
		eligibleModels = eligibleModels[:0:0]
		for _, c := range s.candidates {
			r := resolveProviderFor(c.Model, c.Provider, req.EnabledProviders)
			if r == "" {
				continue
			}
			resolvedProvider[c.Model] = r
			eligibleModels = append(eligibleModels, c.Model)
		}
		if len(eligibleModels) == 0 {
			log.Warn(
				"Cluster scorer: no eligible provider for request; returning ErrNoEligibleProvider",
				"enabled_providers", sortedKeys(req.EnabledProviders),
				"requested_model", req.RequestedModel,
			)
			return router.Decision{}, fmt.Errorf("enabled providers %v have no overlap with deployed candidates: %w", sortedKeys(req.EnabledProviders), ErrNoEligibleProvider)
		}
	}

	// Model exclusion composes with provider gating.
	if len(req.ExcludedModels) > 0 {
		filtered := eligibleModels[:0:0]
		for _, m := range eligibleModels {
			if _, drop := req.ExcludedModels[m]; !drop {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) == 0 {
			log.Warn(
				"Cluster scorer: exclusion list empties eligible pool; returning ErrNoEligibleProvider",
				"excluded_models", sortedKeys(req.ExcludedModels),
				"requested_model", req.RequestedModel,
			)
			return router.Decision{}, fmt.Errorf("excluded models %v leave no eligible candidates: %w", sortedKeys(req.ExcludedModels), ErrNoEligibleProvider)
		}
		eligibleModels = filtered
	}

	// Drop ToolUseLow models (e.g. instruct-only variants that hallucinate
	// tool calls) when the request carries tools. Soft filter: falls back to
	// the unfiltered set if it would empty the pool — a preference, not a gate.
	if req.HasTools {
		if blacklist := catalog.ToolUseLowSet(); len(blacklist) > 0 {
			filtered := eligibleModels[:0:0]
			var dropped []string
			for _, m := range eligibleModels {
				if _, drop := blacklist[m]; drop {
					dropped = append(dropped, m)
					continue
				}
				filtered = append(filtered, m)
			}
			if len(filtered) > 0 && len(dropped) > 0 {
				log.Debug(
					"Cluster scorer: tool-use blacklist applied",
					"dropped_models", dropped,
					"requested_model", req.RequestedModel,
				)
				eligibleModels = filtered
			}
		}
	}

	// Drop AgenticLow models on has_tools turns — models that emit valid tool
	// calls but can't sustain the skill/tool loop (e.g. minimax-m3 grepped the
	// filesystem for a skill instead of invoking it). This is what lets the
	// price dial demote Opus to a cheaper harness-capable model (Sonnet, GLM,
	// DeepSeek-Pro) instead of stranding the turn on the pool's cheapest model.
	// Same soft empty-pool fallback as the tool-use filter.
	if req.HasTools {
		if blacklist := catalog.AgenticLowSet(); len(blacklist) > 0 {
			filtered := eligibleModels[:0:0]
			var dropped []string
			for _, m := range eligibleModels {
				if _, drop := blacklist[m]; drop {
					dropped = append(dropped, m)
					continue
				}
				filtered = append(filtered, m)
			}
			if len(filtered) > 0 && len(dropped) > 0 {
				log.Debug(
					"Cluster scorer: agentic-harness blacklist applied",
					"dropped_models", dropped,
					"requested_model", req.RequestedModel,
				)
				eligibleModels = filtered
			}
		}
	}

	// Drop ImageInputUnsupported models (text-only OSS that 4xx on image parts)
	// on image-bearing requests. Same soft fallback: if no image-capable
	// candidate is deployed, keep the unfiltered pool and let the upstream
	// report the rejection rather than 503-ing here.
	if req.HasImages {
		if textOnly := catalog.ImageUnsupportedSet(); len(textOnly) > 0 {
			filtered := eligibleModels[:0:0]
			var dropped []string
			for _, m := range eligibleModels {
				if _, drop := textOnly[m]; drop {
					dropped = append(dropped, m)
					continue
				}
				filtered = append(filtered, m)
			}
			switch {
			case len(filtered) == 0 && len(dropped) > 0:
				log.Warn(
					"Cluster scorer: image-bearing request but no image-capable candidate; keeping text-only pool",
					"dropped_models", dropped,
					"requested_model", req.RequestedModel,
				)
			case len(dropped) > 0:
				log.Debug(
					"Cluster scorer: image-input filter applied",
					"dropped_models", dropped,
					"requested_model", req.RequestedModel,
				)
				eligibleModels = filtered
			}
		}
	}

	// Turn the ranked preference list into a per-model additive bonus. Rank is
	// compacted over eligible entries, so an excluded/undeployed/filtered
	// preference is skipped (stale preference = no-op). Applied per top-P
	// cluster inside blendScoresV2.
	var priorityBonus map[string]float32
	if len(req.PreferredModels) > 0 {
		eligible := make(map[string]struct{}, len(eligibleModels))
		for _, m := range eligibleModels {
			eligible[m] = struct{}{}
		}
		priorityBonus = make(map[string]float32, len(req.PreferredModels))
		rank := 0
		for _, m := range req.PreferredModels {
			if _, ok := eligible[m]; !ok {
				continue
			}
			if _, dup := priorityBonus[m]; dup {
				continue
			}
			priorityBonus[m] = priorityBonusFor(rank)
			rank++
		}
	}

	scoreStart := time.Now()
	topClusters := topPNearest(vec, s.centroids, s.cfg.TopP)

	var scores map[string]float32
	var effectiveKnobsHash uint64

	if s.isV2 {
		// 1. Resolve Knobs and Validate
		//
		// defaultKnobs is kept alongside activeKnobs (both fresh clones from
		// defaultActiveKnobs) so the extreme-value check below can tell a
		// request-level override apart from the bundle's own baked-in
		// defaults -- a bundle is allowed to ship an alpha > 0.9 on some
		// clusters by design, and that's not something an operator can act
		// on, so it must never WARN.
		defaultKnobs := s.defaultActiveKnobs()
		activeKnobs := s.defaultActiveKnobs()

		if req.RoutingKnobs != nil {
			switch {
			case req.RoutingKnobs.QualityBias != nil:
				// Map the dial through the mix-change calibration (no dead
				// zone), then floor each cluster so the dial cheapens
				// conversational turns without ever stranding agentic on a
				// cheap model. Takes precedence over a co-set Alpha.
				t := *req.RoutingKnobs.QualityBias
				if math.IsNaN(t) || math.IsInf(t, 0) || t < 0 || t > 1 {
					return router.Decision{}, fmt.Errorf("%w: quality_bias (%f) must be a finite value in [0, 1]", ErrInvalidRoutingKnobs, t)
				}
				s.applyDialAlpha(t, activeKnobs.Alpha, activeKnobs.AlphaFloor)
			case req.RoutingKnobs.Alpha != nil:
				// Eval/debug lever: replace every alpha with the scalar,
				// ignoring per-cluster dispersion.
				for i := range activeKnobs.Alpha {
					activeKnobs.Alpha[i] = *req.RoutingKnobs.Alpha
				}
			}
			if req.RoutingKnobs.SpeedWeight != nil {
				activeKnobs.SpeedWeight = *req.RoutingKnobs.SpeedWeight
			}
			if req.RoutingKnobs.OutputCostRatio != nil {
				activeKnobs.OutputCostRatio = *req.RoutingKnobs.OutputCostRatio
			}
			if req.RoutingKnobs.ExpectedOutputTokens != nil {
				activeKnobs.ExpectedOutputTokens = *req.RoutingKnobs.ExpectedOutputTokens
			}
			if req.RoutingKnobs.PerModelVerbosity != nil {
				activeKnobs.PerModelVerbosity = *req.RoutingKnobs.PerModelVerbosity
			}
		}

		// Alpha length mismatch here means a server-side bundle/registry bug, not
		// bad client input (NewScorer validates defaults against K; overrides
		// replace in place without resizing) — map to 503, not 400.
		if len(activeKnobs.Alpha) != s.centroids.K {
			return router.Decision{}, fmt.Errorf("%w: alpha vector length %d must equal K=%d", ErrClusterUnavailable, len(activeKnobs.Alpha), s.centroids.K)
		}
		// Check speed_weight bounds before the per-alpha loop so it's reported
		// distinctly, not masked by the combined alpha+speed_weight check below.
		if activeKnobs.SpeedWeight < 0 || activeKnobs.SpeedWeight > 1 {
			return router.Decision{}, fmt.Errorf("%w: speed_weight (%f) must be in [0, 1]", ErrInvalidRoutingKnobs, activeKnobs.SpeedWeight)
		}
		// speedWeightOverridden is request-scoped (SpeedWeight is a single
		// scalar, not per-cluster), so it's resolved once outside the loop.
		speedWeightOverridden := activeKnobs.SpeedWeight != defaultKnobs.SpeedWeight

		var extremeAlphaClusters []int
		var extremeAlphaValues []float64
		var extremeAlphaSpeedClusters []int
		var extremeAlphaSpeedValues []float64
		for i, a := range activeKnobs.Alpha {
			if a < 0 || a > 1 {
				return router.Decision{}, fmt.Errorf("%w: alpha[%d] (%f) must be in [0, 1]", ErrInvalidRoutingKnobs, i, a)
			}
			if a+activeKnobs.SpeedWeight > 1.0+1e-9 {
				return router.Decision{}, fmt.Errorf("%w: alpha[%d] (%f) + speed_weight (%f) must be <= 1.0", ErrInvalidRoutingKnobs, i, a, activeKnobs.SpeedWeight)
			}
			// Only flag a cluster as "extreme" if the request actually moved
			// it away from the bundle's own default -- otherwise this fires
			// on every request for a bundle that simply ships a high alpha
			// on some clusters by design, drowning out genuinely actionable
			// WARNs. Aggregated below into a single log line per bundle.
			alphaOverridden := a != defaultKnobs.Alpha[i]
			if alphaOverridden && a > 0.9 {
				extremeAlphaClusters = append(extremeAlphaClusters, i)
				extremeAlphaValues = append(extremeAlphaValues, a)
			}
			if (alphaOverridden || speedWeightOverridden) && a+activeKnobs.SpeedWeight > 0.95 {
				extremeAlphaSpeedClusters = append(extremeAlphaSpeedClusters, i)
				extremeAlphaSpeedValues = append(extremeAlphaSpeedValues, a)
			}
		}
		if len(extremeAlphaClusters) > 0 {
			log.Warn(
				"Extreme routing knob override: alpha > 0.9",
				"clusters", extremeAlphaClusters,
				"alphas", extremeAlphaValues,
				"count", len(extremeAlphaClusters),
			)
		}
		if len(extremeAlphaSpeedClusters) > 0 {
			log.Warn(
				"Extreme routing knob override: alpha + speed_weight > 0.95",
				"clusters", extremeAlphaSpeedClusters,
				"alphas", extremeAlphaSpeedValues,
				"speed_weight", activeKnobs.SpeedWeight,
				"count", len(extremeAlphaSpeedClusters),
			)
		}
		if activeKnobs.OutputCostRatio < 0 || activeKnobs.OutputCostRatio > 10 {
			return router.Decision{}, fmt.Errorf("%w: output_cost_ratio (%f) must be in [0, 10]", ErrInvalidRoutingKnobs, activeKnobs.OutputCostRatio)
		}
		if activeKnobs.ExpectedOutputTokens < 0 || activeKnobs.ExpectedOutputTokens > 100000 {
			return router.Decision{}, fmt.Errorf("%w: expected_output_tokens (%d) must be in [0, 100000]", ErrInvalidRoutingKnobs, activeKnobs.ExpectedOutputTokens)
		}

		effectiveKnobsHash = ComputeKnobsHash(
			activeKnobs.Alpha,
			activeKnobs.SpeedWeight,
			activeKnobs.OutputCostRatio,
			activeKnobs.ExpectedOutputTokens,
			activeKnobs.PerModelVerbosity,
		)

		scores = s.blendScoresV2(topClusters, activeKnobs, eligibleModels, req.SubsidizedModelCostFactor, priorityBonus)
	} else {
		// Legacy v1: static cluster rankings, no cost axis, so
		// SubsidizedModelCostFactor doesn't apply. All deployed bundles run V2;
		// this is a fallback.
		scores = make(map[string]float32, len(eligibleModels))
		for _, k := range topClusters {
			row := s.rankings[k]
			for _, m := range eligibleModels {
				scores[m] += row[m]
			}
		}
	}

	chosenModel, chosenScore := argmax(scores, eligibleModels)
	scoreUs := time.Since(scoreStart).Microseconds()

	if chosenModel == "" {
		// Defensive: only reachable if rankings.json contains NaN scores.
		log.Error(
			"Cluster scorer: argmax produced empty model; returning ErrClusterUnavailable",
			"requested_model", req.RequestedModel,
		)
		return router.Decision{}, fmt.Errorf("argmax produced empty model (likely NaN in rankings.json): %w", ErrClusterUnavailable)
	}
	chosen := s.lookupCandidate(chosenModel)
	if chosen == nil {
		// Unreachable: argmax picks from s.models, built from s.candidates.
		log.Error(
			"Cluster scorer: argmax model not found in candidates; returning ErrClusterUnavailable",
			"chosen_model", chosenModel,
		)
		return router.Decision{}, fmt.Errorf("argmax model %q not found in candidates: %w", chosenModel, ErrClusterUnavailable)
	}

	// Copy slices for downstream (semantic cache) reuse.
	embedCopy := make([]float32, len(vec))
	copy(embedCopy, vec)
	clustersCopy := make([]int, len(topClusters))
	copy(clustersCopy, topClusters)
	candidatesCopy := make([]string, len(eligibleModels))
	copy(candidatesCopy, eligibleModels)
	// Copy the pre-argmax score vector for off-policy logging, restricted to
	// eligible models so it matches the candidate set.
	scoresCopy := make(map[string]float32, len(eligibleModels))
	for _, m := range eligibleModels {
		if v, ok := scores[m]; ok {
			scoresCopy[m] = v
		}
	}
	// Per-request provider binding per eligible model, for a wrapping explorer.
	// Falls back to each candidate's default when EnabledProviders wasn't set.
	providersCopy := make(map[string]string, len(eligibleModels))
	for _, m := range eligibleModels {
		if p, ok := resolvedProvider[m]; ok {
			providersCopy[m] = p
		}
	}
	if req.EnabledProviders == nil {
		for _, c := range s.candidates {
			if _, seen := providersCopy[c.Model]; !seen {
				providersCopy[c.Model] = c.Provider
			}
		}
	}
	// Per-request resolved binding may differ from the boot-time default
	// (e.g. a self-hoster with only OPENROUTER_API_KEY served by a row whose
	// primary binding is bedrock).
	chosenProvider := chosen.Provider
	if p, ok := resolvedProvider[chosen.Model]; ok {
		chosenProvider = p
	}
	// Runner-up: second-best eligible model, the other half of the band pair
	// frozen into the session pin so a later per-turn policy can swap without
	// re-scoring. Empty when the pool has a single model.
	pairedModel, pairedScore := runnerUp(scores, eligibleModels, chosen.Model)
	pairedProvider := ""
	if pairedModel != "" {
		if pc := s.lookupCandidate(pairedModel); pc != nil {
			pairedProvider = pc.Provider
		}
		if p, ok := resolvedProvider[pairedModel]; ok {
			pairedProvider = p
		}
	}
	decision := router.Decision{
		Provider: chosenProvider,
		Model:    chosen.Model,
		Reason: fmt.Sprintf(
			"cluster:%s top_p=%s model=%s provider=%s",
			s.version, clusterIDsString(topClusters), chosen.Model, chosenProvider,
		),
		Metadata: &router.RoutingMetadata{
			Embedding:            embedCopy,
			ClusterIDs:           clustersCopy,
			CandidateModels:      candidatesCopy,
			ChosenScore:          chosenScore,
			ClusterRouterVersion: s.version,
			EffectiveKnobsHash:   effectiveKnobsHash,
			CandidateScores:      scoresCopy,
			CandidateProviders:   providersCopy,
			// Deterministic argmax; an exploration policy wrapping this scorer
			// overwrites Propensity with its sampling probability.
			Propensity:     1.0,
			PairedModel:    pairedModel,
			PairedProvider: pairedProvider,
			PairedScore:    pairedScore,
		},
	}
	log.Info(
		"Cluster routing decision",
		"cluster_version", s.version,
		"decision_model", decision.Model,
		"decision_provider", decision.Provider,
		"decision_reason", decision.Reason,
		"paired_model", pairedModel,
		"paired_provider", pairedProvider,
		"paired_score", pairedScore,
		"top_clusters", topClusters,
		"chosen_score", chosenScore,
		"embed_ms", embedMs,
		"score_us", scoreUs,
		"total_ms", time.Since(start).Milliseconds(),
		"prompt_chars", len(text),
		"embedded_tokens", len(text)/4,
		"prompt_truncated", truncated,
		"requested_model", req.RequestedModel,
		"total_input_tokens", req.EstimatedInputTokens,
		"has_tools", req.HasTools,
		"has_images", req.HasImages,
	)
	return decision, nil
}

func (s *Scorer) lookupCandidate(model string) *DeployedEntry {
	for i := range s.candidates {
		if s.candidates[i].Model == model {
			return &s.candidates[i]
		}
	}
	return nil
}

// topPNearest returns indices of the p centroids closest to vec by
// cosine similarity (dot product on L2-normed vectors).
func topPNearest(vec []float32, c *Centroids, p int) []int {
	if p > c.K {
		p = c.K
	}
	type scoredCluster struct {
		idx int
		sim float32
	}
	scored := make([]scoredCluster, c.K)
	for k := 0; k < c.K; k++ {
		row := c.Row(k)
		var sum float32
		for i, v := range row {
			sum += v * vec[i]
		}
		scored[k] = scoredCluster{idx: k, sim: sum}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].sim != scored[j].sim {
			return scored[i].sim > scored[j].sim
		}
		return scored[i].idx < scored[j].idx
	})
	out := make([]int, p)
	for i := 0; i < p; i++ {
		out[i] = scored[i].idx
	}
	sort.Ints(out)
	return out
}

// argmax returns the model with the highest score, tie-breaking by order.
func argmax(scores map[string]float32, order []string) (string, float32) {
	var bestModel string
	var bestScore float32
	first := true
	for _, m := range order {
		s, ok := scores[m]
		if !ok {
			continue
		}
		if first || s > bestScore {
			bestModel = m
			bestScore = s
			first = false
		}
	}
	return bestModel, bestScore
}

// runnerUp returns the highest-scoring model other than exclude, tie-breaking
// by order to match argmax. Returns ("", 0) when no eligible peer exists (a
// single-model pool), so the caller surfaces an empty PairedModel rather than a
// duplicate of the chosen model.
func runnerUp(scores map[string]float32, order []string, exclude string) (string, float32) {
	var bestModel string
	var bestScore float32
	first := true
	for _, m := range order {
		if m == exclude {
			continue
		}
		s, ok := scores[m]
		if !ok {
			continue
		}
		if first || s > bestScore {
			bestModel = m
			bestScore = s
			first = false
		}
	}
	return bestModel, bestScore
}

// TailTruncate keeps the last maxChars bytes, snapping to a UTF-8 boundary.
func TailTruncate(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	cut := len(s) - maxChars
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return s[cut:]
}

func clusterIDsString(ks []int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, k := range ks {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", k)
	}
	b.WriteByte(']')
	return b.String()
}

var _ router.Router = (*Scorer)(nil)

// subsidyMaxBonus is the max per-cluster score lift for a subscription-covered
// model, scaling by (1−f) where f is plan-window headroom (≈epsilon when
// slack, →1 as it binds) so cash/OSS re-enter on merit as the plan saturates.
// Set to the blend ceiling (1.0): prefer a fully-slack plan over cash unless
// the covered model is near-worst for the task. Additive, so it never
// disturbs the quality/cost/speed blend weights.
const subsidyMaxBonus float32 = 1.0

// preferredBonusBase is the rank-1 score bonus (≈[0,1] blend units) a
// per-installation preferred model gets — a soft finger on the scale that
// tilts close argmax calls without overriding a clearly-better model.
// Additive, like the subsidy bonus above.
const preferredBonusBase float32 = 0.15

// preferredBonusDecay shrinks the bonus by rank so lower preferences press the
// scale less hard: bonus(rank) = preferredBonusBase * decay^rank (rank 0-based).
// 0.55 yields rank 0 ≈ 0.15, rank 1 ≈ 0.08, rank 2 ≈ 0.045.
const preferredBonusDecay float64 = 0.55

// priorityBonusFor returns the additive per-cluster score bonus for a model at
// the given zero-based preference rank (0 = first preference). Later ranks decay
// toward zero; a negative rank yields no bonus.
func priorityBonusFor(rank int) float32 {
	if rank < 0 {
		return 0
	}
	return preferredBonusBase * float32(math.Pow(preferredBonusDecay, float64(rank)))
}

// blendScoresV2 computes v2 per-model blended scores for the top-P clusters
// under the effective knobs. Extracted from Route so the distribution preview
// scores identically to live routing — single source of truth for the
// cost/quality/speed blend. Caller owns knob validation and QualityBias->Alpha.
func (s *Scorer) blendScoresV2(topClusters []int, activeKnobs DefaultRoutingKnobs, eligibleModels []string, subsidyFactors map[string]float64, priorityBonus map[string]float32) map[string]float32 {
	// 2. Effective per-model cost. Kept at FULL catalog scale even for
	// subscription-covered models: the catalog ratio tracks plan-quota burn
	// (Anthropic's unified rate limit weights Opus far above Haiku), which is
	// the intra-family signal the blend needs — compressing it would wash
	// Haiku and Opus together. The subscription PREFERENCE (use the prepaid
	// plan over cash) is a separate uniform per-family bonus below.
	costs := make(map[string]float64, len(s.models))
	for _, m := range s.models {
		axis := s.modelAxes[m]
		vFactor := 1.0
		if activeKnobs.PerModelVerbosity && axis.VerbosityTokens != nil && s.medianVerbosity > 0 {
			vFactor = *axis.VerbosityTokens / s.medianVerbosity
		}
		inputPer1K := 0.0
		if axis.InputPer1KUSD != nil {
			inputPer1K = *axis.InputPer1KUSD
		}
		outputPer1K := 0.0
		if axis.OutputPer1KUSD != nil {
			outputPer1K = *axis.OutputPer1KUSD
		}
		costs[m] = inputPer1K + activeKnobs.OutputCostRatio*outputPer1K*vFactor
	}

	// 3. Effective per-model speed
	speeds := make(map[string]*float64, len(s.models))
	for _, m := range s.models {
		axis := s.modelAxes[m]
		if axis.TTFTSeconds != nil && axis.TPS != nil && *axis.TPS > 0 {
			val := *axis.TTFTSeconds + float64(activeKnobs.ExpectedOutputTokens) / *axis.TPS
			speeds[m] = &val
		} else {
			speeds[m] = nil
		}
	}

	// 4. Normalize over DEPLOYED model set
	qMin := make(map[int]float32)
	qMax := make(map[int]float32)
	for _, k := range topClusters {
		row := s.qualityMeans[k]
		first := true
		for _, m := range s.models {
			qVal := row[m]
			if first {
				qMin[k] = qVal
				qMax[k] = qVal
				first = false
			} else {
				if qVal < qMin[k] {
					qMin[k] = qVal
				}
				if qVal > qMax[k] {
					qMax[k] = qVal
				}
			}
		}
	}

	var cMin, cMax float64
	firstC := true
	for _, m := range s.models {
		cVal := costs[m]
		if firstC {
			cMin = cVal
			cMax = cVal
			firstC = false
		} else {
			if cVal < cMin {
				cMin = cVal
			}
			if cVal > cMax {
				cMax = cVal
			}
		}
	}
	cRange := cMax - cMin

	useSpeed := activeKnobs.SpeedWeight > 0
	var sMin, sMax float64
	firstS := true
	for _, m := range s.models {
		if !useSpeed {
			break
		}
		sPtr := speeds[m]
		if sPtr == nil {
			continue
		}
		sVal := *sPtr
		if firstS {
			sMin = sVal
			sMax = sVal
			firstS = false
		} else {
			if sVal < sMin {
				sMin = sVal
			}
			if sVal > sMax {
				sMax = sVal
			}
		}
	}
	sRange := 0.0
	if useSpeed && !firstS {
		sRange = sMax - sMin
	}

	// 6. Blend per top-P cluster
	scores := make(map[string]float32, len(eligibleModels))
	for _, k := range topClusters {
		row := s.qualityMeans[k]
		wQ := float32(activeKnobs.Alpha[k])
		wS := float32(0.0)
		if useSpeed {
			wS = float32(activeKnobs.SpeedWeight)
		}
		wC := float32(1.0) - wQ - wS

		qRange := qMax[k] - qMin[k]

		for _, m := range eligibleModels {
			qVal := row[m]
			qNorm := float32(0.0)
			if qRange > 0 {
				qNorm = (qVal - qMin[k]) / qRange
			}

			cVal := costs[m]
			cNorm := float32(0.0)
			if cRange > 0 {
				cNorm = float32((cVal - cMin) / cRange)
			}

			sPtr := speeds[m]
			if sRange > 0 {
				// Untimed peers count as worst-case speed (sNorm=1, no wS
				// bonus) so wQ/wC weighting stays consistent across timed and
				// untimed models.
				var sNorm float32 = 1.0
				if sPtr != nil {
					sNorm = float32((*sPtr - sMin) / sRange)
				}
				blend := wQ*qNorm + wC*(1.0-cNorm) + wS*(1.0-sNorm)
				scores[m] += blend
			} else {
				// No timing differentiation across the pool: redistribute wS
				// into wQ/wC so weights still sum to 1.
				total := wQ + wC
				if total > 0 {
					blend := (wQ/total)*qNorm + (wC/total)*(1.0-cNorm)
					scores[m] += blend
				} else {
					scores[m] += qNorm
				}
			}

			// Subscription preference: lift a covered model by
			// subsidyMaxBonus·(1−f), f = per-credential headroom (≈epsilon
			// slack, →1 as it binds). Uniform across the covered family, so it
			// only decides plan-vs-cash and never reorders within the family.
			if f, ok := subsidyFactors[m]; ok {
				scores[m] += subsidyMaxBonus * float32(1.0-f)
			}

			// Per-installation preference: lift by its rank-decaying bonus.
			// Additive, so it only decides close calls. Absent = no preference.
			if b, ok := priorityBonus[m]; ok {
				scores[m] += b
			}
		}
	}
	return scores
}
