package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
)

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

// Scorer is the cluster router for one frozen artifact version. Errors
// delegate to the fallback router so the request stays serviceable.
type Scorer struct {
	version    string
	cfg        Config
	embed      Embedder
	centroids  *Centroids
	rankings   Rankings
	registry   *ModelRegistry
	candidates []DeployedEntry
	models     []string
	fallback   router.Router
}

// Version returns the artifact version (e.g. "v0.2") for logging and
// /health-style endpoints.
func (s *Scorer) Version() string { return s.version }

// NewScorer wires a Scorer from a pre-loaded artifact Bundle. Entries whose
// provider is not in availableProviders are filtered out of the candidate set.
func NewScorer(bundle *Bundle, cfg Config, embed Embedder, fallback router.Router, availableProviders map[string]struct{}) (*Scorer, error) {
	if bundle == nil {
		return nil, fmt.Errorf("cluster: bundle must not be nil")
	}
	if embed == nil {
		return nil, fmt.Errorf("cluster: embedder must not be nil")
	}
	if fallback == nil {
		return nil, fmt.Errorf("cluster: fallback must not be nil")
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
		// TopP > K means top-p selection collapses to "all clusters",
		// which trivially defeats the routing premise. Fail fast.
		return nil, fmt.Errorf("cluster %s: K=%d < TopP=%d", bundle.Version, bundle.Centroids.K, cfg.TopP)
	}

	candidates := filterByProviders(bundle.Registry.DeployedModels, availableProviders)
	if len(candidates) == 0 {
		return nil, fmt.Errorf(
			"cluster %s: no deployed entry matches the registered providers %v; "+
				"add a provider key (ANTHROPIC_API_KEY / OPENAI_PROVIDER_API_KEY / "+
				"GOOGLE_PROVIDER_API_KEY) or re-run train_cluster_router.py to "+
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

	// Every centroid id must have a ranking row, and every candidate must
	// have a score in every row. Iterating over [0, K) (not just rankings
	// keys) catches the case where rankings.json is missing a cluster
	// entirely — that cluster could win top-p selection at request time
	// and silently contribute zero scores to argmax.
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
		fallback:   fallback,
	}, nil
}

// filterByProviders returns entries whose Provider is in the available set.
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

// Route embeds the prompt, scores clusters, and returns the argmax decision.
func (s *Scorer) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	start := time.Now()
	log := observability.Get()

	if len(req.PromptText) < s.cfg.MinPromptChars {
		log.Debug(
			"Cluster scorer: prompt too short, falling through to heuristic",
			"prompt_chars", len(req.PromptText),
			"min_prompt_chars", s.cfg.MinPromptChars,
			"requested_model", req.RequestedModel,
		)
		return s.fallback.Route(ctx, req)
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
		log.Warn(
			"Cluster scorer: embed failed; falling back to heuristic",
			"err", err,
			"embed_ms", embedMs,
			"prompt_chars", len(text),
			"prompt_truncated", truncated,
			"requested_model", req.RequestedModel,
		)
		return s.fallback.Route(ctx, req)
	}
	if len(vec) != s.centroids.Dim {
		log.Warn(
			"Cluster scorer: embedding dim mismatch; falling back to heuristic",
			"got_dim", len(vec),
			"want_dim", s.centroids.Dim,
			"embed_ms", embedMs,
		)
		return s.fallback.Route(ctx, req)
	}

	scoreStart := time.Now()
	topClusters := topPNearest(vec, s.centroids, s.cfg.TopP)
	scores := make(map[string]float32, len(s.models))
	for _, k := range topClusters {
		row := s.rankings[k]
		for _, m := range s.models {
			scores[m] += row[m]
		}
	}
	chosenModel, chosenScore := argmax(scores, s.models)
	scoreUs := time.Since(scoreStart).Microseconds()

	if chosenModel == "" {
		// Defensive: only reachable if rankings.json contains NaN scores.
		// Treat as a fail-open path so we don't return a zero-Decision.
		log.Warn(
			"Cluster scorer: argmax produced empty model; falling back to heuristic",
			"requested_model", req.RequestedModel,
		)
		return s.fallback.Route(ctx, req)
	}
	chosen := s.lookupCandidate(chosenModel)
	if chosen == nil {
		// Unreachable in practice: argmax only picks from s.models which
		// is built directly from s.candidates. Guard so a future refactor
		// that decouples them still fail-opens cleanly.
		log.Warn(
			"Cluster scorer: argmax model not found in candidates; falling back to heuristic",
			"chosen_model", chosenModel,
		)
		return s.fallback.Route(ctx, req)
	}

	decision := router.Decision{
		Provider: chosen.Provider,
		Model:    chosen.Model,
		Reason: fmt.Sprintf(
			"cluster:%s top_p=%s model=%s provider=%s",
			s.version, clusterIDsString(topClusters), chosen.Model, chosen.Provider,
		),
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

// lookupCandidate returns nil when no candidate matches; callers
// fail-open to the heuristic rather than returning a zero Decision.
func (s *Scorer) lookupCandidate(model string) *DeployedEntry {
	for i := range s.candidates {
		if s.candidates[i].Model == model {
			return &s.candidates[i]
		}
	}
	return nil
}

// topPNearest returns the indices of the p centroids closest to vec by
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
