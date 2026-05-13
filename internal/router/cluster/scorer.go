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
)

// ErrClusterUnavailable is returned when the cluster scorer cannot
// produce a routing decision. Callers map to HTTP 503: silent fallback
// masks real regressions in eval and lets quality silently degrade.
var ErrClusterUnavailable = errors.New("cluster: routing unavailable")

// ErrNoEligibleProvider is returned when req.EnabledProviders has no
// overlap with boot-time candidates. Callers map to HTTP 4xx; silently
// routing to an unavailable provider would 401 upstream.
var ErrNoEligibleProvider = errors.New("cluster: no eligible provider for request")

// Config carries the scorer's runtime knobs.
type Config struct {
	TopP           int
	MinPromptChars int
	MaxPromptChars int
	EmbedTimeout   time.Duration
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		TopP:           4,
		MinPromptChars: 20,
		MaxPromptChars: 1024,
		EmbedTimeout:   1500 * time.Millisecond,
	}
}

// Scorer is the cluster router for one frozen artifact version.
// Failure modes return ErrClusterUnavailable rather than silently
// falling back to a default model.
type Scorer struct {
	version    string
	cfg        Config
	embed      Embedder
	centroids  *Centroids
	rankings   Rankings
	registry   *ModelRegistry
	candidates []DeployedEntry
	models     []string
	// metadata is the parsed metadata.yaml; nil if absent. Used only by
	// the semantic cache (CacheThresholds).
	metadata *ArtifactMetadata
}

// Version returns the artifact version (e.g. "v0.2").
func (s *Scorer) Version() string { return s.version }

// DeployedModels returns the static, provider-filtered candidate list this
// Scorer was built with. The slice is a copy; mutating it does not affect
// routing. Used by the admin API to render the model-selection checklist
// (we surface the universe of choices, not just the currently-eligible
// subset for a given installation).
func (s *Scorer) DeployedModels() []DeployedEntry {
	out := make([]DeployedEntry, len(s.candidates))
	copy(out, s.candidates)
	return out
}

// CacheThresholds returns per-version semantic-cache thresholds from the
// bundle's metadata.yaml cache_config block. defaultThreshold is 0 when
// unset; callers substitute their own runtime default.
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
		// TopP > K collapses top-p to "all clusters", defeating routing.
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

	// Iterate [0, K) (not rankings keys) so a missing cluster fails fast:
	// it could win top-p at request time and silently contribute zero.
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

	return &Scorer{
		version:    bundle.Version,
		cfg:        cfg,
		embed:      embed,
		centroids:  bundle.Centroids,
		rankings:   bundle.Rankings,
		registry:   bundle.Registry,
		candidates: candidates,
		models:     models,
		metadata:   bundle.Metadata,
	}, nil
}

func filterByProviders(entries []DeployedEntry, available map[string]struct{}) []DeployedEntry {
	out := make([]DeployedEntry, 0, len(entries))
	for _, e := range entries {
		if _, ok := available[e.Provider]; ok {
			out = append(out, e)
		}
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

	if len(req.PromptText) < s.cfg.MinPromptChars {
		log.Warn(
			"Cluster scorer: prompt too short; returning ErrClusterUnavailable",
			"prompt_chars", len(req.PromptText),
			"min_prompt_chars", s.cfg.MinPromptChars,
			"requested_model", req.RequestedModel,
		)
		return router.Decision{}, fmt.Errorf("prompt has %d chars, minimum is %d: %w", len(req.PromptText), s.cfg.MinPromptChars, ErrClusterUnavailable)
	}

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

	// Per-request gating complements boot-time filterByProviders: boot
	// excludes providers without env keys; request excludes providers
	// without BYOK / client keys for this installation.
	eligibleModels := s.models
	if req.EnabledProviders != nil {
		eligibleModels = eligibleModels[:0:0]
		for _, c := range s.candidates {
			if _, ok := req.EnabledProviders[c.Provider]; ok {
				eligibleModels = append(eligibleModels, c.Model)
			}
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

	// Per-installation (or env-var-driven) model exclusion. Applied after
	// EnabledProviders so the two filters compose: provider-eligible AND
	// not-excluded. Empties → ErrNoEligibleProvider (no silent fallback;
	// the operator deliberately narrowed the pool).
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

	scoreStart := time.Now()
	topClusters := topPNearest(vec, s.centroids, s.cfg.TopP)
	scores := make(map[string]float32, len(eligibleModels))
	for _, k := range topClusters {
		row := s.rankings[k]
		for _, m := range eligibleModels {
			scores[m] += row[m]
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
		// Guard for future refactor that decouples them.
		log.Error(
			"Cluster scorer: argmax model not found in candidates; returning ErrClusterUnavailable",
			"chosen_model", chosenModel,
		)
		return router.Decision{}, fmt.Errorf("argmax model %q not found in candidates: %w", chosenModel, ErrClusterUnavailable)
	}

	// Copy slices so downstream consumers (semantic cache) can reuse the
	// embedding + top-p clusters without re-embedding; originals are
	// short-lived within this call.
	embedCopy := make([]float32, len(vec))
	copy(embedCopy, vec)
	clustersCopy := make([]int, len(topClusters))
	copy(clustersCopy, topClusters)
	candidatesCopy := make([]string, len(eligibleModels))
	copy(candidatesCopy, eligibleModels)
	decision := router.Decision{
		Provider: chosen.Provider,
		Model:    chosen.Model,
		Reason: fmt.Sprintf(
			"cluster:%s top_p=%s model=%s provider=%s",
			s.version, clusterIDsString(topClusters), chosen.Model, chosen.Provider,
		),
		Metadata: &router.RoutingMetadata{
			Embedding:            embedCopy,
			ClusterIDs:           clustersCopy,
			CandidateModels:      candidatesCopy,
			ChosenScore:          chosenScore,
			ClusterRouterVersion: s.version,
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
		"prompt_truncated", truncated,
		"requested_model", req.RequestedModel,
		"estimated_input_tokens", req.EstimatedInputTokens,
		"has_tools", req.HasTools,
	)
	return decision, nil
}

// lookupCandidate returns nil when no candidate matches.
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
