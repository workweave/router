package cluster

import (
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// centroids.bin file format (little-endian):
//
//	magic   [4]byte  = "CRT1"
//	version uint32   = 1
//	k       uint32   number of centroids
//	dim     uint32   embedding dimension (must equal EmbedDim)
//	data    [k][dim]float32  L2-normalized centroids, row-major
const (
	centroidsMagic   = "CRT1"
	centroidsVersion = uint32(1)
)

// LatestPointer names the file inside artifacts/ pinning the default
// served version.
const LatestPointer = "latest"

// LatestVersion is the sentinel ROUTER_CLUSTER_VERSION accepts to mean
// "read artifacts/latest".
const LatestVersion = "latest"

// embeddedArtifacts is the entire artifacts/ tree compiled into the binary.
//
//go:embed all:artifacts
var embeddedArtifacts embed.FS

// Centroids are L2-normalized at training time so runtime can use dot product.
type Centroids struct {
	K   int
	Dim int
	// Data is row-major: centroid k at Data[k*Dim : (k+1)*Dim].
	Data []float32
}

// Row returns a slice view into Data for centroid k. No copy.
func (c *Centroids) Row(k int) []float32 {
	return c.Data[k*c.Dim : (k+1)*c.Dim]
}

// Rankings is the per-(cluster, model) α-blended score table.
// Values are min-max-normalized per cluster and α-blended at training time
// (paper §3). Runtime scorer sums + argmaxes.
type Rankings map[int]map[string]float32

// rankingsFile is the on-disk JSON form; cluster ids parsed back to int.
type rankingsFile struct {
	Meta struct {
		RouterVersion   string  `json:"router_version,omitempty"`
		EmbedderModel   string  `json:"embedder_model,omitempty"`
		Alpha           float64 `json:"alpha,omitempty"`
		TopP            int     `json:"top_p,omitempty"`
		TrainingDataMix struct {
			D1 float64 `json:"d1,omitempty"`
			D2 float64 `json:"d2,omitempty"`
			D3 float64 `json:"d3,omitempty"`
		} `json:"training_data_mix,omitempty"`
	} `json:"meta,omitempty"`
	Rankings map[string]map[string]float32 `json:"rankings"`
}

// DeployedEntry is one routable target. Direct columns are 1:1
// (Model == BenchColumn); proxy entries reuse another column's scores.
type DeployedEntry struct {
	Model       string `json:"model"`
	Provider    string `json:"provider"`
	BenchColumn string `json:"bench_column"`
	Proxy       bool   `json:"proxy,omitempty"`
	ProxyNote   string `json:"proxy_note,omitempty"`
}

// ModelRegistry is the deserialized model_registry.json.
type ModelRegistry struct {
	DeployedModels []DeployedEntry `json:"deployed_models"`
}

// Models returns the deduplicated model-name set in DeployedModels order.
func (r *ModelRegistry) Models() []string {
	seen := make(map[string]struct{}, len(r.DeployedModels))
	out := make([]string, 0, len(r.DeployedModels))
	for _, e := range r.DeployedModels {
		if _, ok := seen[e.Model]; ok {
			continue
		}
		seen[e.Model] = struct{}{}
		out = append(out, e.Model)
	}
	return out
}

// ArtifactMetadata is the parsed metadata.yaml. Informational at runtime;
// routing decisions depend only on centroids + rankings + registry.
type ArtifactMetadata struct {
	FormatVersion     int                `yaml:"format_version,omitempty"`
	Version           string             `yaml:"version"`
	Parent            string             `yaml:"parent,omitempty"`
	Status            string             `yaml:"status,omitempty"`
	PromotedDate      string             `yaml:"promoted_date,omitempty"`
	FrozenDate        string             `yaml:"frozen_date,omitempty"`
	Embedder          ArtifactEmbedder   `yaml:"embedder"`
	Training          ArtifactTraining   `yaml:"training"`
	DeployedProviders []string           `yaml:"deployed_providers,omitempty"`
	DeployedModels    []string           `yaml:"deployed_models,omitempty"`
	CostPer1KInputUSD map[string]float64 `yaml:"cost_per_1k_input_usd,omitempty"`
	// TokPerS holds measured median output throughput (tokens/sec) keyed by
	// provider then model — e.g. TokPerS["google"]["gemini-3.1-flash-lite-preview"].
	// Provider-keyed because the same model's throughput varies sharply by
	// provider. Consumed by FastestModel to break the tier-clamp / hard-pin
	// out of cost-only selection; absent or partial annotation degrades to
	// cost-only via CheapestModel. Informational to the scorer (which carries
	// its own speed_weight); only the clamp/hard-pin selectors read it.
	TokPerS   map[string]map[string]float64 `yaml:"tok_per_s,omitempty"`
	Changelog string                        `yaml:"changelog,omitempty"`
	// CacheConfig carries per-version semantic-cache knobs.
	CacheConfig *ArtifactCacheConfig `yaml:"cache_config,omitempty"`
}

// ArtifactCacheConfig carries per-version semantic-cache knobs.
type ArtifactCacheConfig struct {
	DefaultThreshold    float32         `yaml:"default_threshold,omitempty"`
	PerClusterThreshold map[int]float32 `yaml:"per_cluster_threshold,omitempty"`
}

type ArtifactEmbedder struct {
	Model     string `yaml:"model"`
	EmbedDim  int    `yaml:"embed_dim"`
	MaxTokens int    `yaml:"max_tokens"`
}

type DefaultRoutingKnobs struct {
	Alpha []float64 `yaml:"alpha"`
	// AlphaFloor is an optional per-cluster minimum the QualityBias dial may not
	// pull a cluster's alpha below. It lets a bundle declare, per cluster, the
	// LOWEST quality weight it will tolerate at maximum price-sensitivity — so a
	// price-leaning dial still routes each cluster to the best model available
	// for that budget instead of collapsing the whole vector to the cheapest
	// model. Length must equal K when present; nil/empty disables flooring (the
	// dial maps to a uniform alpha, the legacy behavior). See applyDialAlpha.
	AlphaFloor           []float64 `yaml:"alpha_floor,omitempty"`
	SpeedWeight          float64   `yaml:"speed_weight"`
	OutputCostRatio      float64   `yaml:"output_cost_ratio"`
	ExpectedOutputTokens int       `yaml:"expected_output_tokens"`
	PerModelVerbosity    bool      `yaml:"per_model_verbosity"`
}

type RecommendedKnobs struct {
	Alpha           float64 `yaml:"alpha"`
	SpeedWeight     float64 `yaml:"speed_weight"`
	OutputCostRatio float64 `yaml:"output_cost_ratio"`
}

type ArtifactTraining struct {
	K                     int                          `yaml:"k"`
	TopP                  int                          `yaml:"top_p"`
	Alpha                 float64                      `yaml:"alpha"`
	Seed                  int                          `yaml:"seed"`
	NPrompts              int                          `yaml:"n_prompts"`
	TrainingDataMix       map[string]float64           `yaml:"training_data_mix,omitempty"`
	DefaultRoutingKnobs   *DefaultRoutingKnobs         `yaml:"default_routing_knobs,omitempty"`
	RecommendedUIDefaults map[string]*RecommendedKnobs `yaml:"recommended_ui_defaults,omitempty"`
}

type ModelAxis struct {
	InputPer1KUSD   *float64 `json:"input_per_1k_usd"`
	OutputPer1KUSD  *float64 `json:"output_per_1k_usd"`
	TTFTSeconds     *float64 `json:"ttft_s"`
	TPS             *float64 `json:"tps"`
	VerbosityTokens *float64 `json:"verbosity_tokens"`
}

// Bundle is one fully-loaded artifact set, held by one Scorer per
// version; the Multiversion router dispatches between them.
type Bundle struct {
	Version         string
	Centroids       *Centroids
	Rankings        Rankings
	Registry        *ModelRegistry
	Metadata        *ArtifactMetadata
	IsV2            bool
	QualityMeans    Rankings
	ModelAxes       map[string]ModelAxis
	MedianVerbosity float64 // Precomputed median verbosity for V2 bundles
}

// EmbedderID returns the embedder model recorded in metadata.yaml,
// defaulting to the Jina v2 embedder for legacy bundles without an
// embedder block.
func (b *Bundle) EmbedderID() string {
	if b.Metadata != nil && b.Metadata.Embedder.Model != "" {
		return b.Metadata.Embedder.Model
	}
	return EmbedderJinaV2
}

// EmbedDim returns the embedding dimensionality declared in
// metadata.yaml, defaulting to the Jina v2 dim for legacy bundles.
func (b *Bundle) EmbedDim() int {
	if b.Metadata != nil && b.Metadata.Embedder.EmbedDim > 0 {
		return b.Metadata.Embedder.EmbedDim
	}
	return EmbedDim
}

// ListVersions returns sorted version directories under artifacts/.
func ListVersions() ([]string, error) {
	entries, err := fs.ReadDir(embeddedArtifacts, "artifacts")
	if err != nil {
		return nil, fmt.Errorf("artifacts: read embed root: %w", err)
	}
	var versions []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == "legacy" {
			// Traverse the legacy subfolder
			legacyEntries, err := fs.ReadDir(embeddedArtifacts, "artifacts/legacy")
			if err != nil {
				return nil, fmt.Errorf("artifacts: read legacy root: %w", err)
			}
			for _, le := range legacyEntries {
				if !le.IsDir() {
					continue
				}
				versions = append(versions, le.Name())
			}
			continue
		}
		versions = append(versions, e.Name())
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("artifacts: no version directories embedded under artifacts/")
	}
	sort.Strings(versions)
	return versions, nil
}

func bundleDirForVersion(version string) string {
	p := path.Join("artifacts", version)
	if _, err := fs.Stat(embeddedArtifacts, p); err == nil {
		return p
	}
	return path.Join("artifacts", "legacy", version)
}

// ResolveVersion turns a user-supplied version string into a concrete
// directory name. Empty or "latest" reads artifacts/latest; anything
// else is returned verbatim after verifying the directory exists.
func ResolveVersion(requested string) (string, error) {
	if requested == "" || requested == LatestVersion {
		raw, err := fs.ReadFile(embeddedArtifacts, path.Join("artifacts", LatestPointer))
		if err != nil {
			return "", fmt.Errorf("artifacts: read latest pointer: %w", err)
		}
		v := strings.TrimSpace(string(raw))
		if v == "" {
			return "", fmt.Errorf("artifacts: latest pointer is empty")
		}
		if v == LatestVersion {
			return "", fmt.Errorf("artifacts: latest pointer cannot reference %q (would recurse)", LatestVersion)
		}
		return ResolveVersion(v)
	}
	dir := bundleDirForVersion(requested)
	if _, err := fs.Stat(embeddedArtifacts, dir); err != nil {
		return "", fmt.Errorf("artifacts: version %q not found: %w", requested, err)
	}
	return requested, nil
}

// LoadBundle reads centroids + rankings + registry + metadata for one
// version from the embedded artifact tree. Version must already be
// resolved (call ResolveVersion first).
func LoadBundle(version string) (*Bundle, error) {
	dir := bundleDirForVersion(version)
	return loadBundleFromPath(embeddedArtifacts, version, dir)
}

// LoadBundleFromDir reads a bundle from an arbitrary on-disk directory
// instead of the embedded tree. Used by the release-gate diff test
// (TestV2MatchesV1) and any tooling that needs to load a bundle
// produced into a temp directory before it's been committed.
//
// dir is the directory directly containing centroids.bin / metadata.yaml
// / etc. The version label is informational only.
func LoadBundleFromDir(dir string, version string) (*Bundle, error) {
	return loadBundleFromPath(os.DirFS(dir), version, ".")
}

func loadBundleFromPath(fsys fs.FS, version, dir string) (*Bundle, error) {
	rawCentroids, err := fs.ReadFile(fsys, path.Join(dir, "centroids.bin"))
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: read centroids.bin: %w", version, err)
	}
	centroids, err := loadCentroids(rawCentroids)
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: %w", version, err)
	}

	rawRegistry, err := fs.ReadFile(fsys, path.Join(dir, "model_registry.json"))
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: read model_registry.json: %w", version, err)
	}
	registry, err := loadRegistry(rawRegistry)
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: %w", version, err)
	}

	// metadata.yaml is best-effort: a bundle without one still routes.
	var meta *ArtifactMetadata
	if rawMeta, err := fs.ReadFile(fsys, path.Join(dir, "metadata.yaml")); err == nil {
		var m ArtifactMetadata
		if err := yaml.Unmarshal(rawMeta, &m); err != nil {
			return nil, fmt.Errorf("artifacts %s: parse metadata.yaml: %w", version, err)
		}
		meta = &m
	}

	if err := validateDeclaredDim(version, centroids, meta); err != nil {
		return nil, err
	}

	var isV2 bool
	var rankings Rankings
	var qualityMeans Rankings
	var modelAxes map[string]ModelAxis
	var medianVerbosity float64 = 1.0

	rawQualityMeans, err := fs.ReadFile(fsys, path.Join(dir, "quality_means.json"))
	if err == nil {
		isV2 = true
		qualityMeans, err = loadQualityMeans(rawQualityMeans)
		if err != nil {
			return nil, fmt.Errorf("artifacts %s: %w", version, err)
		}
		rawModelAxes, err := fs.ReadFile(fsys, path.Join(dir, "model_axes.json"))
		if err != nil {
			return nil, fmt.Errorf("artifacts %s: read model_axes.json: %w", version, err)
		}
		modelAxes, err = loadModelAxes(rawModelAxes)
		if err != nil {
			return nil, fmt.Errorf("artifacts %s: %w", version, err)
		}

		// Perform robust load-time validation for v2 format (Fail-Fast)
		for _, mName := range registry.Models() {
			if _, ok := modelAxes[mName]; !ok {
				return nil, fmt.Errorf("artifacts %s: load-time validation failed: model %q missing from model_axes.json", version, mName)
			}
			for k := range qualityMeans {
				if _, ok := qualityMeans[k][mName]; !ok {
					return nil, fmt.Errorf("artifacts %s: load-time validation failed: model %q missing from quality_means.json for cluster %d", version, mName, k)
				}
			}
		}

		// model_features.json is best-effort and, when present, becomes the
		// authoritative source for the quality + axes tables (the no-op
		// onboarding surface: a new model is one appended column). It is a
		// faithful repackaging of quality_means.json + model_axes.json, so
		// routing is unchanged on the current roster (asserted by
		// TestFeaturesMatchQualityMeans). A model added here without a
		// matching quality_means.json entry still routes; a deployed model
		// absent here, or a column of the wrong length, fails fast.
		if rawFeatures, ferr := fs.ReadFile(fsys, path.Join(dir, "model_features.json")); ferr == nil {
			featureQualityMeans, featureAxes, err := loadModelFeatures(rawFeatures, centroids.K)
			if err != nil {
				return nil, fmt.Errorf("artifacts %s: %w", version, err)
			}
			for _, mName := range registry.Models() {
				if _, ok := featureAxes[mName]; !ok {
					return nil, fmt.Errorf("artifacts %s: load-time validation failed: deployed model %q missing from model_features.json", version, mName)
				}
			}
			qualityMeans = featureQualityMeans
			modelAxes = featureAxes
		}

		// Precompute median of verbosity tokens over all deployed models that have data
		var verbosityVals []float64
		for _, mName := range registry.Models() {
			axis, ok := modelAxes[mName]
			if ok && axis.VerbosityTokens != nil {
				verbosityVals = append(verbosityVals, *axis.VerbosityTokens)
			}
		}
		if len(verbosityVals) > 0 {
			sort.Float64s(verbosityVals)
			medianVerbosity = verbosityVals[len(verbosityVals)/2]
		}
	} else {
		// Fallback to v1
		rawRankings, err := fs.ReadFile(fsys, path.Join(dir, "rankings.json"))
		if err != nil {
			return nil, fmt.Errorf("artifacts %s: read rankings.json: %w", version, err)
		}
		rankings, err = loadRankings(rawRankings)
		if err != nil {
			return nil, fmt.Errorf("artifacts %s: %w", version, err)
		}
	}

	return &Bundle{
		Version:         version,
		Centroids:       centroids,
		Rankings:        rankings,
		Registry:        registry,
		Metadata:        meta,
		IsV2:            isV2,
		QualityMeans:    qualityMeans,
		ModelAxes:       modelAxes,
		MedianVerbosity: medianVerbosity,
	}, nil
}

// loadBundleV1Only forces v1 (rankings.json) loading even from a
// directory that also contains quality_means.json. Used by the diff
// test to construct a v1-shaped Scorer from a dual-format bundle.
func loadBundleV1Only(fsys fs.FS, version, dir string) (*Bundle, error) {
	rawCentroids, err := fs.ReadFile(fsys, path.Join(dir, "centroids.bin"))
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: read centroids.bin: %w", version, err)
	}
	centroids, err := loadCentroids(rawCentroids)
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: %w", version, err)
	}
	rawRegistry, err := fs.ReadFile(fsys, path.Join(dir, "model_registry.json"))
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: read model_registry.json: %w", version, err)
	}
	registry, err := loadRegistry(rawRegistry)
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: %w", version, err)
	}
	rawRankings, err := fs.ReadFile(fsys, path.Join(dir, "rankings.json"))
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: read rankings.json: %w", version, err)
	}
	rankings, err := loadRankings(rawRankings)
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: %w", version, err)
	}
	var meta *ArtifactMetadata
	if rawMeta, err := fs.ReadFile(fsys, path.Join(dir, "metadata.yaml")); err == nil {
		var m ArtifactMetadata
		if err := yaml.Unmarshal(rawMeta, &m); err != nil {
			return nil, fmt.Errorf("artifacts %s: parse metadata.yaml: %w", version, err)
		}
		meta = &m
	}
	if err := validateDeclaredDim(version, centroids, meta); err != nil {
		return nil, err
	}
	return &Bundle{
		Version:   version,
		Centroids: centroids,
		Rankings:  rankings,
		Registry:  registry,
		Metadata:  meta,
		IsV2:      false,
	}, nil
}

// validateDeclaredDim cross-checks the dim recorded in metadata.yaml's
// embedder block against the centroids.bin header. Legacy bundles
// without a metadata embedder block must match the Jina default.
func validateDeclaredDim(version string, centroids *Centroids, meta *ArtifactMetadata) error {
	declared := EmbedDim
	source := "legacy default"
	if meta != nil && meta.Embedder.EmbedDim > 0 {
		declared = meta.Embedder.EmbedDim
		source = "metadata.yaml embedder.embed_dim"
	}
	if centroids.Dim != declared {
		return fmt.Errorf("artifacts %s: centroids.bin dim %d != declared %d (%s); embedder mismatch", version, centroids.Dim, declared, source)
	}
	return nil
}

// LoadBundleV1Only reads a bundle from disk forcing v1 (rankings.json)
// even if v2 files coexist. Used by the diff test driver.
func LoadBundleV1Only(dir, version string) (*Bundle, error) {
	return loadBundleV1Only(os.DirFS(dir), version, ".")
}

func loadCentroids(raw []byte) (*Centroids, error) {
	if len(raw) < 16 {
		return nil, fmt.Errorf("centroids.bin too short: %d bytes", len(raw))
	}
	if string(raw[:4]) != centroidsMagic {
		return nil, fmt.Errorf("centroids.bin bad magic: got %q want %q", raw[:4], centroidsMagic)
	}
	version := binary.LittleEndian.Uint32(raw[4:8])
	if version != centroidsVersion {
		return nil, fmt.Errorf("centroids.bin unsupported version %d (want %d)", version, centroidsVersion)
	}
	k := binary.LittleEndian.Uint32(raw[8:12])
	dim := binary.LittleEndian.Uint32(raw[12:16])
	if dim == 0 {
		return nil, fmt.Errorf("centroids.bin has dim=0")
	}
	if k == 0 {
		return nil, fmt.Errorf("centroids.bin has K=0; run router/scripts/train_cluster_router.py")
	}
	want := 16 + 4*int(k)*int(dim)
	if len(raw) != want {
		return nil, fmt.Errorf("centroids.bin size %d, expected %d for K=%d dim=%d", len(raw), want, k, dim)
	}
	data := make([]float32, int(k)*int(dim))
	for i := range data {
		off := 16 + 4*i
		bits := binary.LittleEndian.Uint32(raw[off : off+4])
		data[i] = math.Float32frombits(bits)
	}
	return &Centroids{K: int(k), Dim: int(dim), Data: data}, nil
}

// loadRankings errors on malformed JSON, non-integer cluster keys,
// empty rankings, or empty per-cluster entries.
func loadRankings(raw []byte) (Rankings, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("rankings.json is empty")
	}
	var f rankingsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("rankings.json parse: %w", err)
	}
	if len(f.Rankings) == 0 {
		return nil, fmt.Errorf("rankings.json has no clusters")
	}
	out := make(Rankings, len(f.Rankings))
	for kStr, models := range f.Rankings {
		var k int
		// Sscanf("12abc",...) succeeds with k=12; round-trip via Sprintf
		// to reject keys with trailing junk.
		_, err := fmt.Sscanf(kStr, "%d", &k)
		if err != nil || fmt.Sprintf("%d", k) != kStr {
			return nil, fmt.Errorf("rankings.json: non-integer cluster key %q", kStr)
		}
		if len(models) == 0 {
			return nil, fmt.Errorf("rankings.json: cluster %d has no models", k)
		}
		out[k] = models
	}
	return out, nil
}

type qualityMeansFile struct {
	Meta         interface{}                   `json:"meta,omitempty"`
	QualityMeans map[string]map[string]float32 `json:"quality_means"`
}

func loadQualityMeans(raw []byte) (Rankings, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("quality_means.json is empty")
	}
	var f qualityMeansFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("quality_means.json parse: %w", err)
	}
	if len(f.QualityMeans) == 0 {
		return nil, fmt.Errorf("quality_means.json has no clusters")
	}
	out := make(Rankings, len(f.QualityMeans))
	for kStr, models := range f.QualityMeans {
		var k int
		_, err := fmt.Sscanf(kStr, "%d", &k)
		if err != nil || fmt.Sprintf("%d", k) != kStr {
			return nil, fmt.Errorf("quality_means.json: non-integer cluster key %q", kStr)
		}
		if len(models) == 0 {
			return nil, fmt.Errorf("quality_means.json: cluster %d has no models", k)
		}
		out[k] = models
	}
	return out, nil
}

type modelAxesFile struct {
	Meta interface{}          `json:"meta,omitempty"`
	Axes map[string]ModelAxis `json:"axes"`
}

func loadModelAxes(raw []byte) (map[string]ModelAxis, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("model_axes.json is empty")
	}
	var f modelAxesFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("model_axes.json parse: %w", err)
	}
	return f.Axes, nil
}

// modelFeaturesFile is the on-disk form of model_features.json: a
// model-centric repackaging of the quality_means + model_axes tables. Each
// model carries its per-cluster quality column (psi_probe, length K) and its
// operational block (the same fields as ModelAxis). Routing from this artifact
// is identical to routing from quality_means.json + model_axes.json on a given
// roster; its purpose is to make onboarding a model a no-op (append one column,
// no retrain).
type modelFeaturesFile struct {
	Meta   interface{} `json:"meta,omitempty"`
	Models map[string]struct {
		PsiProbe    []float32 `json:"psi_probe"`
		Operational ModelAxis `json:"operational"`
	} `json:"models"`
}

// loadModelFeatures parses model_features.json into the runtime's existing
// shapes: a per-cluster quality table (Rankings) and the per-model axes map.
// k is the cluster count from centroids.bin; every psi_probe column must match
// it exactly (a mismatched column means the file was built against a different
// artifact version and must be rebuilt).
func loadModelFeatures(raw []byte, k int) (Rankings, map[string]ModelAxis, error) {
	if len(raw) == 0 {
		return nil, nil, fmt.Errorf("model_features.json is empty")
	}
	var f modelFeaturesFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, nil, fmt.Errorf("model_features.json parse: %w", err)
	}
	if len(f.Models) == 0 {
		return nil, nil, fmt.Errorf("model_features.json has no models")
	}
	qualityMeans := make(Rankings, k)
	for ki := 0; ki < k; ki++ {
		qualityMeans[ki] = make(map[string]float32, len(f.Models))
	}
	axes := make(map[string]ModelAxis, len(f.Models))
	for name, rec := range f.Models {
		if len(rec.PsiProbe) != k {
			return nil, nil, fmt.Errorf("model_features.json: model %q psi_probe length %d, want K=%d (rebuild against this artifact version)", name, len(rec.PsiProbe), k)
		}
		for ki := 0; ki < k; ki++ {
			qualityMeans[ki][name] = rec.PsiProbe[ki]
		}
		axes[name] = rec.Operational
	}
	return qualityMeans, axes, nil
}

// CheapestModel returns the lowest cost-per-1k-input entry among
// registry entries whose provider is in available. Returns ok=false if
// nothing matches.
func CheapestModel(meta *ArtifactMetadata, registry *ModelRegistry, available map[string]struct{}) (provider, model string, ok bool) {
	return cheapestModelFiltered(meta, registry, available, nil, nil)
}

// CheapestModelInSet is CheapestModel restricted to an allowlist and denylist.
func CheapestModelInSet(meta *ArtifactMetadata, registry *ModelRegistry, available, denySet, allowSet map[string]struct{}) (provider, model string, ok bool) {
	return cheapestModelFiltered(meta, registry, available, denySet, allowSet)
}

func cheapestModelFiltered(meta *ArtifactMetadata, registry *ModelRegistry, available, denySet, allowSet map[string]struct{}) (provider, model string, ok bool) {
	var bestCost float64 = -1
	for _, e := range registry.DeployedModels {
		resolved := resolveProviderFor(e.Model, e.Provider, available)
		if resolved == "" {
			continue
		}
		if allowSet != nil {
			if _, allowed := allowSet[e.Model]; !allowed {
				continue
			}
		}
		if _, denied := denySet[e.Model]; denied {
			continue
		}
		cost, hasCost := meta.CostPer1KInputUSD[e.Model]
		if !hasCost {
			continue
		}
		if bestCost < 0 || cost < bestCost {
			bestCost = cost
			provider = resolved
			model = e.Model
			ok = true
		}
	}
	return
}

// FastestModel returns the highest measured-throughput (tok/s) entry among
// registry entries whose provider is in available. Throughput is read from
// meta.TokPerS keyed by the resolved provider, so the same model on a slow
// provider can lose to a different model on a fast one. Falls back to
// CheapestModel when the bundle carries no usable speed annotation — it
// never returns a worse decision than the cost-only selector.
func FastestModel(meta *ArtifactMetadata, registry *ModelRegistry, available map[string]struct{}) (provider, model string, ok bool) {
	return fastestModelFiltered(meta, registry, available, nil, nil)
}

// FastestModelInSet is FastestModel restricted to an allowlist and denylist.
// Used by the tier-clamp resolver, where allowSet is the at-or-below-ceiling
// model set.
func FastestModelInSet(meta *ArtifactMetadata, registry *ModelRegistry, available, denySet, allowSet map[string]struct{}) (provider, model string, ok bool) {
	return fastestModelFiltered(meta, registry, available, denySet, allowSet)
}

func fastestModelFiltered(meta *ArtifactMetadata, registry *ModelRegistry, available, denySet, allowSet map[string]struct{}) (provider, model string, ok bool) {
	var bestSpeed float64 = -1
	for _, e := range registry.DeployedModels {
		resolved := resolveProviderFor(e.Model, e.Provider, available)
		if resolved == "" {
			continue
		}
		if allowSet != nil {
			if _, allowed := allowSet[e.Model]; !allowed {
				continue
			}
		}
		if _, denied := denySet[e.Model]; denied {
			continue
		}
		byModel, hasProvider := meta.TokPerS[resolved]
		if !hasProvider {
			continue
		}
		speed, hasSpeed := byModel[e.Model]
		if !hasSpeed {
			continue
		}
		if speed > bestSpeed {
			bestSpeed = speed
			provider = resolved
			model = e.Model
			ok = true
		}
	}
	if !ok {
		// No speed-annotated candidate resolved (un-annotated bundle, or
		// every in-ceiling model lacks a tok/s entry). Preserve the
		// established cost-only behavior rather than failing the clamp.
		return cheapestModelFiltered(meta, registry, available, denySet, allowSet)
	}
	return
}

// RoutableModelSet returns the set of model names from registry that have
// at least one catalog provider binding resolvable under available — the
// same view of "routable now" that NewScorer's filter applies. Callers in
// the composition root use this to seed the planner's available-models
// set so a pin to e.g. deepseek-v4-flash stays valid when the deploy only
// has OPENROUTER_API_KEY (catalog provides the trailing OpenRouter
// fallback binding even though the registry row reads `deepinfra`).
func RoutableModelSet(registry *ModelRegistry, available map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(registry.DeployedModels))
	for _, e := range registry.DeployedModels {
		if resolveProviderFor(e.Model, e.Provider, available) == "" {
			continue
		}
		out[e.Model] = struct{}{}
	}
	return out
}

// loadRegistry validates every entry has non-empty (model, provider,
// bench_column).
func loadRegistry(raw []byte) (*ModelRegistry, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("model_registry.json is empty")
	}
	var r ModelRegistry
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("model_registry.json parse: %w", err)
	}
	if len(r.DeployedModels) == 0 {
		return nil, fmt.Errorf("model_registry.json: deployed_models is empty")
	}
	for i, e := range r.DeployedModels {
		if e.Model == "" {
			return nil, fmt.Errorf("model_registry.json: deployed_models[%d] missing model", i)
		}
		if e.Provider == "" {
			return nil, fmt.Errorf("model_registry.json: deployed_models[%d] (%s) missing provider", i, e.Model)
		}
		if e.BenchColumn == "" {
			return nil, fmt.Errorf("model_registry.json: deployed_models[%d] (%s) missing bench_column", i, e.Model)
		}
	}
	return &r, nil
}
