package cluster

import (
	"context"
	"errors"
	"fmt"
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
	version         string
	cfg             Config
	embed           Embedder
	centroids       *Centroids
	rankings        Rankings
	registry        *ModelRegistry
	candidates      []DeployedEntry
	models          []string
	metadata        *ArtifactMetadata // nil if absent; cache threshold source.
	isV2            bool
	qualityMeans    Rankings
	modelAxes       map[string]ModelAxis
	medianVerbosity float64
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

	// Validate every cluster in [0, K) has a ranking/quality_means row so a missing
	// cluster can't win top-p at request time and silently contribute zero.
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
		// Validate bundle's default routing knobs against centroids.K so a
		// misconfigured metadata.yaml fails at load time rather than HTTP 400-ing
		// every v2 request. The Alpha-override path is a scalar replacement that
		// preserves length, so once defaults are sized correctly the request-time
		// length check is unreachable from valid overrides.
		if bundle.Metadata != nil && bundle.Metadata.Training.DefaultRoutingKnobs != nil {
			dk := bundle.Metadata.Training.DefaultRoutingKnobs
			if len(dk.Alpha) != bundle.Centroids.K {
				return nil, fmt.Errorf("cluster %s: default_routing_knobs.alpha length %d must equal K=%d", bundle.Version, len(dk.Alpha), bundle.Centroids.K)
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

	return &Scorer{
		version:         bundle.Version,
		cfg:             cfg,
		embed:           embed,
		centroids:       bundle.Centroids,
		rankings:        bundle.Rankings,
		registry:        bundle.Registry,
		candidates:      candidates,
		models:          models,
		metadata:        bundle.Metadata,
		isV2:            bundle.IsV2,
		qualityMeans:    bundle.QualityMeans,
		modelAxes:       bundle.ModelAxes,
		medianVerbosity: bundle.MedianVerbosity,
	}, nil
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

// filterByProviders drops entries whose model has no ProviderBinding
// resolvable under the available set, and rewrites the entry's Provider
// to the resolved binding so downstream dispatch lands on the right
// upstream. Same semantics as a plain registry-Provider match for
// single-binding models; for multi-binding rows (e.g. fireworks primary,
// openrouter fallback) it picks the first available in catalog order.
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

	text := tailTruncate(req.PromptText, s.cfg.MaxPromptChars)
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

	// Per-request gating complements boot-time filterByProviders. For
	// multi-binding catalog rows we re-walk the binding list under the
	// per-request EnabledProviders set: a BYOK-only request may have a
	// narrower or wider provider set than the deployment, and the chosen
	// binding determines which upstream gets the dispatch. Track the
	// resolved binding per model so Decision.Provider reflects it.
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

	// Tool-use quality filter. When the inbound carries tools, drop any
	// model the catalog has marked ToolUseLow (e.g. instruct-only variants
	// that hallucinate tool calls). Soft filter: if the subtraction would
	// empty the eligible pool, fall back to the unfiltered set rather than
	// 4xx-ing the request — this is a quality preference, not a correctness
	// gate.
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

	// Image-input filter. When the inbound carries image content, drop every
	// model the catalog has marked ImageInputUnsupported (text-only OSS models
	// that 4xx on image parts). Soft filter with the same empty-pool fallback as
	// the tool-use filter: if no image-capable candidate is deployed (e.g. an
	// OSS-only self-host), keep the unfiltered pool and let the upstream report
	// the rejection rather than 503-ing here. The managed pool always carries
	// vision-capable Claude/Gemini/GPT candidates, so the drop is effective.
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

	scoreStart := time.Now()
	topClusters := topPNearest(vec, s.centroids, s.cfg.TopP)

	var scores map[string]float32
	var effectiveKnobsHash uint64

	if s.isV2 {
		// 1. Resolve Knobs and Validate
		var activeKnobs DefaultRoutingKnobs
		if s.metadata != nil && s.metadata.Training.DefaultRoutingKnobs != nil {
			activeKnobs = *s.metadata.Training.DefaultRoutingKnobs
			// Struct copy shares the Alpha slice's backing array with the
			// bundle defaults. Clone before any potential mutation so a
			// per-request override doesn't leak into subsequent requests.
			activeKnobs.Alpha = append([]float64(nil), activeKnobs.Alpha...)
		} else {
			activeKnobs = DefaultRoutingKnobs{
				Alpha:                make([]float64, s.centroids.K),
				SpeedWeight:          0.0,
				OutputCostRatio:      0.0,
				ExpectedOutputTokens: 2000,
				PerModelVerbosity:    false,
			}
			for i := range activeKnobs.Alpha {
				activeKnobs.Alpha[i] = 0.53
			}
		}

		if req.RoutingKnobs != nil {
			if req.RoutingKnobs.Alpha != nil {
				// Sledgehammer behavior: uniformly replace every alpha with the scalar
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

		// Validate effective knobs. Alpha length is sanity-checked here as a
		// defensive backstop — NewScorer validates the bundle defaults against K
		// at load time, and the override path replaces values in place without
		// resizing, so a mismatch here means a server-side bundle/registry bug
		// rather than bad client input. Map to ErrClusterUnavailable (HTTP 503)
		// to avoid misreporting a server config error as a client 400.
		if len(activeKnobs.Alpha) != s.centroids.K {
			return router.Decision{}, fmt.Errorf("%w: alpha vector length %d must equal K=%d", ErrClusterUnavailable, len(activeKnobs.Alpha), s.centroids.K)
		}
		// Validate speed_weight bounds before the per-alpha loop so an
		// out-of-range speed_weight is reported as such rather than masked by the
		// combined alpha+speed_weight constraint inside the loop.
		if activeKnobs.SpeedWeight < 0 || activeKnobs.SpeedWeight > 1 {
			return router.Decision{}, fmt.Errorf("%w: speed_weight (%f) must be in [0, 1]", ErrInvalidRoutingKnobs, activeKnobs.SpeedWeight)
		}
		for i, a := range activeKnobs.Alpha {
			if a < 0 || a > 1 {
				return router.Decision{}, fmt.Errorf("%w: alpha[%d] (%f) must be in [0, 1]", ErrInvalidRoutingKnobs, i, a)
			}
			if a+activeKnobs.SpeedWeight > 1.0+1e-9 {
				return router.Decision{}, fmt.Errorf("%w: alpha[%d] (%f) + speed_weight (%f) must be <= 1.0", ErrInvalidRoutingKnobs, i, a, activeKnobs.SpeedWeight)
			}
			if a > 0.9 {
				log.Warn("Extreme routing knob: alpha > 0.9", "cluster", i, "alpha", a)
			}
			if a+activeKnobs.SpeedWeight > 0.95 {
				log.Warn("Extreme routing knob: alpha + speed_weight > 0.95", "cluster", i, "alpha", a, "speed_weight", activeKnobs.SpeedWeight)
			}
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

		// 2. Effective per-model cost (knob-dependent)
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
		scores = make(map[string]float32, len(eligibleModels))
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
					// Mixed-timing pool: untimed peers are treated as
					// worst-case speed (sNorm=1, no wS bonus). This keeps
					// wQ/wC weighting consistent across timed and untimed
					// models — without this, the redistribution branch
					// would silently drop the cost axis when wC=0.
					var sNorm float32 = 1.0
					if sPtr != nil {
						sNorm = float32((*sPtr - sMin) / sRange)
					}
					blend := wQ*qNorm + wC*(1.0-cNorm) + wS*(1.0-sNorm)
					scores[m] += blend
				} else {
					// No timing differentiation across the entire pool
					// (all models lack AA timing, or all share the same
					// speed). Redistribute wS into wQ and wC so the
					// remaining weights still sum to 1.
					total := wQ + wC
					if total > 0 {
						blend := (wQ/total)*qNorm + (wC/total)*(1.0-cNorm)
						scores[m] += blend
					} else {
						scores[m] += qNorm
					}
				}
			}
		}
	} else {
		// Legacy v1 flow
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
	// Prefer the per-request resolved binding (which may differ from the
	// boot-time default when the request's EnabledProviders narrows or
	// widens the deployment set, e.g. self-hoster with only OPENROUTER_API_KEY
	// served by a row whose primary binding is bedrock).
	chosenProvider := chosen.Provider
	if p, ok := resolvedProvider[chosen.Model]; ok {
		chosenProvider = p
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
		},
	}
	log.Info(
		"Cluster routing decision",
		"cluster_version", s.version,
		"decision_model", decision.Model,
		"decision_provider", decision.Provider,
		"decision_reason", decision.Reason,
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

// tailTruncate keeps the last maxChars bytes, snapping to a UTF-8 boundary.
func tailTruncate(s string, maxChars int) string {
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
