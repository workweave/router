package cluster

import (
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// centroids.bin file format (little-endian):
//
//	magic     [4]byte  = "CRT1"
//	version   uint32   = 1
//	k         uint32   number of centroids
//	dim       uint32   embedding dimension (must equal EmbedDim)
//	data      [k][dim]float32  L2-normalized centroids, row-major
//
// Magic + version header catches malformed/truncated files at load time
// rather than producing silently-wrong routing.
const (
	centroidsMagic   = "CRT1"
	centroidsVersion = uint32(1)
)

// LatestPointer names the file inside artifacts/ pinning the default
// served version. Promoting a trained version is a one-line edit.
const LatestPointer = "latest"

// LatestVersion is the sentinel ROUTER_CLUSTER_VERSION accepts to mean
// "read artifacts/latest". Treat identically to empty string.
const LatestVersion = "latest"

// embeddedArtifacts is the entire artifacts/ tree compiled into the
// binary; each subdir is one frozen, comparable bundle.
//
//go:embed all:artifacts
var embeddedArtifacts embed.FS

// Centroids are L2-normalized at training time so the runtime can use
// dot product as a cosine-distance signal.
type Centroids struct {
	K   int
	Dim int
	// Data is row-major: centroid k at Data[k*Dim : (k+1)*Dim]. Single
	// contiguous allocation keeps the distance loop cache-friendly.
	Data []float32
}

// Row returns a slice view into Data for centroid k. No copy.
func (c *Centroids) Row(k int) []float32 {
	return c.Data[k*c.Dim : (k+1)*c.Dim]
}

// Rankings is the per-(cluster, model) α-blended score table.
// Values are min-max-normalized per cluster and α-blended at training
// time (paper §3): xⱼⁱ = α · p̃ⱼⁱ + (1 − α) · (1 − q̃ⱼⁱ).
// Runtime scorer just sums + argmaxes.
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
// (Model == BenchColumn); proxy entries reuse another column's scores
// until enough D3 traffic accumulates to rank directly.
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
// Order is meaningful: it pins tie-breaking when providers share a
// bench column score.
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

// ArtifactMetadata is the parsed metadata.yaml. Informational at
// runtime; routing decisions depend only on centroids + rankings + registry.
type ArtifactMetadata struct {
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
	Changelog         string             `yaml:"changelog,omitempty"`
	// CacheConfig is optional. Each version may tune thresholds
	// independently — looser geometry can ship lower thresholds.
	CacheConfig *ArtifactCacheConfig `yaml:"cache_config,omitempty"`
}

// ArtifactCacheConfig carries per-version semantic-cache knobs.
type ArtifactCacheConfig struct {
	// DefaultThreshold is the cosine floor applied to clusters without
	// override; 0 means fall back to the runtime's compiled-in default.
	DefaultThreshold float32 `yaml:"default_threshold,omitempty"`
	// PerClusterThreshold is sparse; only list outliers. Values in [0, 1].
	PerClusterThreshold map[int]float32 `yaml:"per_cluster_threshold,omitempty"`
}

type ArtifactEmbedder struct {
	Model     string `yaml:"model"`
	EmbedDim  int    `yaml:"embed_dim"`
	MaxTokens int    `yaml:"max_tokens"`
}

type ArtifactTraining struct {
	K               int                `yaml:"k"`
	TopP            int                `yaml:"top_p"`
	Alpha           float64            `yaml:"alpha"`
	Seed            int                `yaml:"seed"`
	NPrompts        int                `yaml:"n_prompts"`
	TrainingDataMix map[string]float64 `yaml:"training_data_mix,omitempty"`
}

// Bundle is one fully-loaded artifact set, held by one Scorer per
// version; the Multiversion router dispatches between them.
type Bundle struct {
	Version   string
	Centroids *Centroids
	Rankings  Rankings
	Registry  *ModelRegistry
	Metadata  *ArtifactMetadata
}

// ListVersions returns sorted version directories under artifacts/.
// Order is lexicographic — keep names monotonic and zero-padded past
// 9 minor versions. The "latest" pointer file is not returned.
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
		versions = append(versions, e.Name())
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("artifacts: no version directories embedded under artifacts/")
	}
	sort.Strings(versions)
	return versions, nil
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
	if _, err := fs.Stat(embeddedArtifacts, path.Join("artifacts", requested)); err != nil {
		return "", fmt.Errorf("artifacts: version %q not found: %w", requested, err)
	}
	return requested, nil
}

// LoadBundle reads centroids + rankings + registry + metadata for one
// version. Version must already be resolved (call ResolveVersion first).
func LoadBundle(version string) (*Bundle, error) {
	dir := path.Join("artifacts", version)
	rawCentroids, err := fs.ReadFile(embeddedArtifacts, path.Join(dir, "centroids.bin"))
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: read centroids.bin: %w", version, err)
	}
	rawRankings, err := fs.ReadFile(embeddedArtifacts, path.Join(dir, "rankings.json"))
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: read rankings.json: %w", version, err)
	}
	rawRegistry, err := fs.ReadFile(embeddedArtifacts, path.Join(dir, "model_registry.json"))
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: read model_registry.json: %w", version, err)
	}
	centroids, err := loadCentroids(rawCentroids)
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: %w", version, err)
	}
	rankings, err := loadRankings(rawRankings)
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: %w", version, err)
	}
	registry, err := loadRegistry(rawRegistry)
	if err != nil {
		return nil, fmt.Errorf("artifacts %s: %w", version, err)
	}
	// metadata.yaml is best-effort: a bundle without one still routes.
	var meta *ArtifactMetadata
	if rawMeta, err := fs.ReadFile(embeddedArtifacts, path.Join(dir, "metadata.yaml")); err == nil {
		var m ArtifactMetadata
		if err := yaml.Unmarshal(rawMeta, &m); err != nil {
			return nil, fmt.Errorf("artifacts %s: parse metadata.yaml: %w", version, err)
		}
		meta = &m
	}
	return &Bundle{
		Version:   version,
		Centroids: centroids,
		Rankings:  rankings,
		Registry:  registry,
		Metadata:  meta,
	}, nil
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
	if dim != EmbedDim {
		return nil, fmt.Errorf("centroids.bin dim %d, expected %d (embedder mismatch)", dim, EmbedDim)
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

// CheapestModel returns the lowest cost-per-1k-input entry among
// registry entries whose provider is in available. Entries missing a
// cost are skipped. Returns ok=false if nothing matches.
func CheapestModel(meta *ArtifactMetadata, registry *ModelRegistry, available map[string]struct{}) (provider, model string, ok bool) {
	var bestCost float64 = -1
	for _, e := range registry.DeployedModels {
		if _, inSet := available[e.Provider]; !inSet {
			continue
		}
		cost, hasCost := meta.CostPer1KInputUSD[e.Model]
		if !hasCost {
			continue
		}
		if bestCost < 0 || cost < bestCost {
			bestCost = cost
			provider = e.Provider
			model = e.Model
			ok = true
		}
	}
	return
}

// loadRegistry validates every entry has a non-empty (model, provider,
// bench_column) triple — loud at boot beats silent "routes to nothing".
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
