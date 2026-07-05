// Command routing-report routes a labeled probe corpus
// (testdata/register_probes.jsonl) through the REAL cluster Scorer for a
// given artifact and reports, per register, which models get chosen — plus
// an auto-derived label per cluster and a diff vs the deployed `latest`
// artifact. Lets a PR reviewer see where conversational/trivial turns route
// before shipping a clustering change, instead of finding out post-deploy
// that everything went to Opus.
//
// Runs under `-tags no_onnx`: probe embeddings are precomputed
// (testdata/register_probes.emb) and fed to the Scorer via a static
// embedder, so the blend/normalization/eligibility path is exact production
// code. Regenerate the cache with scripts/embed_register_probes.py when the
// corpus or embedder changes — a hash mismatch fails loudly.
//
// Usage:
//
//	go run -tags no_onnx ./cmd/routing-report --target v0.68
//	go run -tags no_onnx ./cmd/routing-report --target v0.68 --baseline v0.67
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"workweave/router/internal/router"
	"workweave/router/internal/router/cluster"
)

// Registers in display order. Must match the `register` values in the corpus.
var registers = []string{
	"conversational", "trivial_nl", "knowledge_qa",
	"easy_code", "hard_code", "agentic_tool",
}

// opusModels and premiumModels classify the deployed pool for the summary
// percentages; premium = tiers a chit-chat turn has no business reaching.
var opusModels = map[string]struct{}{
	"claude-opus-4-8": {}, "claude-opus-4-7": {},
}
var premiumModels = map[string]struct{}{
	"claude-opus-4-8": {}, "claude-opus-4-7": {},
	"gpt-5.5": {}, "gemini-3.1-pro-preview": {}, "claude-sonnet-4-6": {},
}

type probe struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Register string `json:"register"`
	Note     string `json:"note"`
}

// embHeader mirrors the JSON header written by scripts/embed_register_probes.py.
type embHeader struct {
	EmbedderID   string `json:"embedder_id"`
	EmbedDim     int    `json:"embed_dim"`
	N            int    `json:"n"`
	CorpusSHA256 string `json:"corpus_sha256"`
	Comment      string `json:"comment"`
}

// maxPromptChars mirrors Scorer's Config.MaxPromptChars; the cache must be
// keyed by the same tail-truncation the Scorer applies before Embed.
const maxPromptChars = 1024

// staticEmbedder satisfies cluster.Embedder from vectors keyed by the
// tail-truncated probe text, matching what the Scorer passes to Embed.
type staticEmbedder struct {
	id     string
	dim    int
	byText map[string][]float32
}

// buildEmbedder keys vectors by the truncated text the Scorer will request.
func buildEmbedder(hdr embHeader, probes []probe, vecs [][]float32) *staticEmbedder {
	byText := make(map[string][]float32, len(probes))
	for i, p := range probes {
		byText[cluster.TailTruncate(p.Text, maxPromptChars)] = vecs[i]
	}
	return &staticEmbedder{id: hdr.EmbedderID, dim: hdr.EmbedDim, byText: byText}
}

func (s *staticEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v, ok := s.byText[text]
	if !ok {
		return nil, fmt.Errorf("static embedder: no cached embedding for text %q", truncForErr(text))
	}
	return v, nil
}
func (s *staticEmbedder) ID() string { return s.id }
func (s *staticEmbedder) Dim() int   { return s.dim }

func truncForErr(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

func main() {
	// Scorer warns on every alpha>0.9 cluster (~20 lines/probe under deployed
	// config); quiet unless caller overrides. Must precede first observability.Get().
	if os.Getenv("LOG_LEVEL") == "" {
		os.Setenv("LOG_LEVEL", "error")
	}
	var (
		artifactsDir = flag.String("artifacts-dir", "internal/router/cluster/artifacts", "path to the artifacts tree")
		target       = flag.String("target", "", "target artifact version (e.g. v0.68); default = current `latest`")
		baseline     = flag.String("baseline", "", "baseline artifact version to diff against; default = current `latest`")
		corpusPath   = flag.String("corpus", "internal/router/cluster/testdata/register_probes.jsonl", "labeled probe corpus")
		embPath      = flag.String("emb", "internal/router/cluster/testdata/register_probes.emb", "precomputed probe embeddings")
		topP         = flag.Int("top-p", 2, "top-P clusters blended per decision (prod runs 2)")
		qualityBias  = flag.Float64("quality-bias", -1, "QualityBias dial position in [0,1] to route at; <0 uses the bundle's default knobs (no dial)")
		outPath      = flag.String("out", "", "write markdown here instead of stdout")
		perPromptOut = flag.String("per-prompt-json", "", "also dump [{id,model}] per probe (target chosen model, corpus order) here")
	)
	flag.Parse()

	latest, err := readLatest(*artifactsDir)
	if err != nil {
		fatal("read latest pointer: %v", err)
	}
	if *target == "" {
		*target = latest
	}
	if *baseline == "" {
		*baseline = latest
	}

	probes, corpusSHA, err := loadCorpus(*corpusPath)
	if err != nil {
		fatal("load corpus: %v", err)
	}
	hdr, vecs, err := loadEmb(*embPath)
	if err != nil {
		fatal("load embeddings: %v", err)
	}
	if hdr.CorpusSHA256 != corpusSHA {
		fatal("embedding cache is STALE: %s was built for corpus sha %s but the corpus is now %s.\n"+
			"Regenerate it: python scripts/embed_register_probes.py", *embPath, hdr.CorpusSHA256[:12], corpusSHA[:12])
	}
	if hdr.N != len(probes) {
		fatal("embedding cache has %d rows but corpus has %d", hdr.N, len(probes))
	}
	embedder := buildEmbedder(hdr, probes, vecs)

	tgt, err := routeCorpus(*artifactsDir, *target, probes, embedder, *topP, *qualityBias)
	if err != nil {
		fatal("route target %s: %v", *target, err)
	}
	if *perPromptOut != "" {
		type pp struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		}
		rows := make([]pp, len(probes))
		for i, p := range probes {
			rows[i] = pp{ID: p.ID, Model: tgt.chosen[i]}
		}
		buf, mErr := json.MarshalIndent(rows, "", "  ")
		if mErr != nil {
			fatal("marshal per-prompt: %v", mErr)
		}
		if wErr := os.WriteFile(*perPromptOut, buf, 0o644); wErr != nil {
			fatal("write per-prompt-json: %v", wErr)
		}
	}
	var base *routeResult
	if *baseline != *target {
		base, err = routeCorpus(*artifactsDir, *baseline, probes, embedder, *topP, *qualityBias)
		if err != nil {
			fatal("route baseline %s: %v", *baseline, err)
		}
	}

	// A change is deploy-affecting only when the target IS the deployed
	// `latest`; a candidate diff (target != latest) is informational.
	deployChange := base != nil && *target == latest
	md := render(*target, *baseline, *topP, hdr.EmbedderID, deployChange, probes, tgt, base)
	if *outPath != "" {
		if err := os.WriteFile(*outPath, []byte(md), 0o644); err != nil {
			fatal("write out: %v", err)
		}
	} else {
		fmt.Print(md)
	}
}

type routeResult struct {
	version     string
	chosen      []string // per probe, in corpus order
	clusterSets [][]int  // per probe top-P set (from Scorer)
	nearest     []int    // per probe single closest cluster (for auto-labels)
}

func routeCorpus(artifactsDir, version string, probes []probe, embedder *staticEmbedder, topP int, qualityBias float64) (*routeResult, error) {
	dir := filepath.Join(artifactsDir, version)
	bundle, err := cluster.LoadBundleFromDir(dir, version)
	if err != nil {
		return nil, fmt.Errorf("load bundle: %w", err)
	}
	if bundle.EmbedderID() != embedder.id {
		return nil, fmt.Errorf("embedder mismatch: bundle declares %q, cache is %q — regenerate the cache for this embedder",
			bundle.EmbedderID(), embedder.id)
	}
	providers := availableProviders(bundle)
	cfg := cluster.DefaultConfig()
	cfg.TopP = topP
	scorer, err := cluster.NewScorer(bundle, cfg, embedder, providers)
	if err != nil {
		return nil, fmt.Errorf("new scorer: %w", err)
	}

	res := &routeResult{
		version:     version,
		chosen:      make([]string, len(probes)),
		clusterSets: make([][]int, len(probes)),
		nearest:     make([]int, len(probes)),
	}
	ctx := context.Background()
	var knobs *router.Overrides
	if qualityBias >= 0 {
		qb := qualityBias
		knobs = &router.Overrides{QualityBias: &qb}
	}
	for i, p := range probes {
		dec, err := scorer.Route(ctx, router.Request{PromptText: p.Text, RoutingKnobs: knobs})
		if err != nil {
			return nil, fmt.Errorf("route probe %s: %w", p.ID, err)
		}
		res.chosen[i] = dec.Model
		if dec.Metadata != nil {
			res.clusterSets[i] = dec.Metadata.ClusterIDs
		}
		res.nearest[i] = nearestCluster(embedder.byText[p.Text], bundle.Centroids)
	}
	return res, nil
}

// nearestCluster returns the single closest centroid index by cosine (both
// vectors are L2-normalized, so dot product suffices).
func nearestCluster(vec []float32, c *cluster.Centroids) int {
	best, bestSim := 0, float32(-2)
	for k := 0; k < c.K; k++ {
		row := c.Row(k)
		var dot float32
		for d := 0; d < c.Dim; d++ {
			dot += vec[d] * row[d]
		}
		if dot > bestSim {
			best, bestSim = k, dot
		}
	}
	return best
}

func availableProviders(b *cluster.Bundle) map[string]struct{} {
	out := make(map[string]struct{})
	if b.Metadata != nil && len(b.Metadata.DeployedProviders) > 0 {
		for _, p := range b.Metadata.DeployedProviders {
			out[p] = struct{}{}
		}
		return out
	}
	for _, e := range b.Registry.DeployedModels {
		out[e.Provider] = struct{}{}
	}
	return out
}

// ---- rendering ----

func render(target, baseline string, topP int, embedderID string, deployChange bool, probes []probe, tgt, base *routeResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Cluster routing report — `%s`\n\n", target)
	switch {
	case deployChange:
		fmt.Fprintf(&b, "⚠️ Deployed model changes **`%s` → `%s`**. ", baseline, target)
	case base != nil:
		fmt.Fprintf(&b, "Candidate **`%s`** vs deployed **`%s`** (`latest` unchanged — not deploying). ", target, baseline)
	}
	fmt.Fprintf(&b, "Routed %d labeled probes through the real Scorer at `top_p=%d`, embedder `%s`. "+
		"Premium = opus-4-7/4-8, gpt-5.5, gemini-3.1-pro, sonnet-4-6.\n\n", len(probes), topP, embedderID)

	// Section 1: register -> model for the target.
	tgtReg := byRegister(probes, tgt)
	b.WriteString("### Register → model (this artifact)\n\n")
	b.WriteString("| register | Opus % | premium % | top models |\n|---|--:|--:|---|\n")
	for _, reg := range registers {
		s := tgtReg[reg]
		fmt.Fprintf(&b, "| %s | %.0f%% | %.0f%% | %s |\n",
			reg, 100*s.opusFrac(), 100*s.premiumFrac(), s.topMix(4))
	}
	b.WriteString("\n")

	// Section 2: diff vs baseline.
	if base != nil {
		baseReg := byRegister(probes, base)
		b.WriteString("### Change vs deployed (`" + baseline + "`)\n\n")
		b.WriteString("| register | Opus % | premium % |\n|---|--:|--:|\n")
		for _, reg := range registers {
			bs, ts := baseReg[reg], tgtReg[reg]
			fmt.Fprintf(&b, "| %s | %.0f%% → %.0f%% (%+.0f) | %.0f%% → %.0f%% (%+.0f) |\n",
				reg,
				100*bs.opusFrac(), 100*ts.opusFrac(), 100*(ts.opusFrac()-bs.opusFrac()),
				100*bs.premiumFrac(), 100*ts.premiumFrac(), 100*(ts.premiumFrac()-bs.premiumFrac()))
		}
		b.WriteString("\n")
	}

	// Section 3: auto cluster labels.
	b.WriteString("### What each cluster represents (probe register mix by nearest centroid)\n\n")
	b.WriteString("| cluster | probes | dominant | register breakdown |\n|--:|--:|---|---|\n")
	type clRow struct {
		k     int
		total int
		mix   map[string]int
	}
	byCl := map[int]*clRow{}
	for i, p := range probes {
		k := tgt.nearest[i]
		r := byCl[k]
		if r == nil {
			r = &clRow{k: k, mix: map[string]int{}}
			byCl[k] = r
		}
		r.total++
		r.mix[p.Register]++
	}
	keys := make([]int, 0, len(byCl))
	for k := range byCl {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		r := byCl[k]
		fmt.Fprintf(&b, "| %d | %d | %s | %s |\n", k, r.total, dominant(r.mix), mixStr(r.mix))
	}
	b.WriteString("\n")
	return b.String()
}

type regStats struct {
	n   int
	mix map[string]int
}

func (s regStats) frac(set map[string]struct{}) float64 {
	if s.n == 0 {
		return 0
	}
	c := 0
	for m, cnt := range s.mix {
		if _, ok := set[m]; ok {
			c += cnt
		}
	}
	return float64(c) / float64(s.n)
}
func (s regStats) opusFrac() float64    { return s.frac(opusModels) }
func (s regStats) premiumFrac() float64 { return s.frac(premiumModels) }

func (s regStats) topMix(n int) string { return mixStrN(s.mix, n) }

func byRegister(probes []probe, r *routeResult) map[string]regStats {
	out := map[string]regStats{}
	for _, reg := range registers {
		out[reg] = regStats{mix: map[string]int{}}
	}
	for i, p := range probes {
		s := out[p.Register]
		s.n++
		s.mix[r.chosen[i]]++
		out[p.Register] = s
	}
	return out
}

func dominant(mix map[string]int) string {
	pairs := sortedPairs(mix)
	if len(pairs) == 0 {
		return ""
	}
	return pairs[0].k
}

type kv struct {
	k string
	n int
}

func sortedPairs(mix map[string]int) []kv {
	out := make([]kv, 0, len(mix))
	for k, v := range mix {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].k < out[j].k
	})
	return out
}

func mixStr(mix map[string]int) string { return mixStrN(mix, 0) }
func mixStrN(mix map[string]int, n int) string {
	pairs := sortedPairs(mix)
	if n > 0 && len(pairs) > n {
		pairs = pairs[:n]
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf("%s:%d", short(p.k), p.n))
	}
	return strings.Join(parts, " ")
}

func short(model string) string {
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		return model[i+1:]
	}
	return model
}

// ---- loaders ----

func readLatest(artifactsDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(artifactsDir, cluster.LatestPointer))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func loadCorpus(path string) ([]probe, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	var probes []probe
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var p probe
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, "", fmt.Errorf("parse corpus line: %w", err)
		}
		probes = append(probes, p)
	}
	if err := sc.Err(); err != nil {
		return nil, "", err
	}
	return probes, fmt.Sprintf("%x", sum), nil
}

func loadEmb(path string) (embHeader, [][]float32, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return embHeader{}, nil, err
	}
	if len(raw) < 8 || string(raw[:4]) != "PEM1" {
		return embHeader{}, nil, fmt.Errorf("bad magic in %s", path)
	}
	hlen := binary.LittleEndian.Uint32(raw[4:8])
	off := 8 + int(hlen)
	if off > len(raw) {
		return embHeader{}, nil, fmt.Errorf("truncated header in %s", path)
	}
	var hdr embHeader
	if err := json.Unmarshal(raw[8:off], &hdr); err != nil {
		return embHeader{}, nil, fmt.Errorf("parse emb header: %w", err)
	}
	need := off + hdr.N*hdr.EmbedDim*4
	if need != len(raw) {
		return embHeader{}, nil, fmt.Errorf("emb body size mismatch: header implies %d bytes, file has %d", need, len(raw))
	}
	vecs := make([][]float32, hdr.N)
	p := off
	for i := 0; i < hdr.N; i++ {
		v := make([]float32, hdr.EmbedDim)
		for d := 0; d < hdr.EmbedDim; d++ {
			v[d] = math.Float32frombits(binary.LittleEndian.Uint32(raw[p : p+4]))
			p += 4
		}
		vecs[i] = v
	}
	return hdr, vecs, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "routing-report: "+format+"\n", args...)
	os.Exit(1)
}
