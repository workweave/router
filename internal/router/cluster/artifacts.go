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

// centroidsMagic is the 4-byte header on centroids.bin so a malformed
// or truncated file is caught at load time rather than producing
// silently-wrong routing.
//
// File format (little-endian):
//
//	magic     [4]byte  = "CRT1"
//	version   uint32   = 1
//	k         uint32   number of centroids
//	dim       uint32   embedding dimension (must equal EmbedDim)
//	data      [k][dim]float32  L2-normalized centroids, row-major
const (
	centroidsMagic   = "CRT1"
	centroidsVersion = uint32(1)
)

// LatestPointer is the filename inside artifacts/ that pins the version
// served when the runtime is asked to resolve "latest". Single-line text
// holding a directory name (e.g. "v0.2"). Updating this file is what
// promotes a newly trained version to production.
const LatestPointer = "latest"

// LatestVersion is the sentinel string ROUTER_CLUSTER_VERSION accepts to
// mean "read whatever artifacts/latest currently points at". Code that
// resolves a version string should treat this value identically to an
// empty string for default-handling.
const LatestVersion = "latest"

// embeddedArtifacts is the entire artifacts/ tree compiled into the
// binary. Each subdirectory is one frozen, comparable bundle:
//
//	artifacts/
//	  latest                — single line: "v0.2"
//	  v0.1/
//	    centroids.bin
//	    rankings.json
//	    model_registry.json
//	    metadata.yaml
//	  v0.2/
//	    ...
//
// Promotion is a one-line edit to artifacts/latest plus a redeploy.
//
//go:embed all:artifacts
var embeddedArtifacts embed.FS

// Centroids is K cluster centroids in EmbedDim space, L2-normalized at
// training time so the runtime scorer can use plain dot product as a
// cosine-distance signal.
type Centroids struct {
	K   int
	Dim int
	// Data is laid out row-major: centroid k at Data[k*Dim : (k+1)*Dim].
	// Single contiguous allocation keeps the per-request distance loop
	// cache-friendly.
	Data []float32
}

// Row returns a slice view into Data for centroid k. No copy.
func (c *Centroids) Row(k int) []float32 {
	return c.Data[k*c.Dim : (k+1)*c.Dim]
}

// Rankings is the per-(cluster, model) α-blended score table. Outer key
// is cluster id (string-encoded so the JSON file is human-readable); inner
// key is the deployed model name as it appears in model_registry.json's
// deployed_models[].model field.
//
// Stored values are already min-max-normalized per cluster and α-blended
// at training time (paper §3): xⱼⁱ = α · p̃ⱼⁱ + (1 − α) · (1 − q̃ⱼⁱ).
// The runtime scorer just sums + argmaxes.
type Rankings map[int]map[string]float32

// rankingsFile is the on-disk JSON representation. Cluster ids ship as
// strings because JSON object keys must be strings; we parse them back
// into ints in loadRankings so callers get a typed map.
type rankingsFile struct {
	// Meta carries provenance. Helpful for debugging "which artifact is
	// loaded?" without redeploying.
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

// DeployedEntry is one routable target the cluster scorer may emit. Each
// entry pairs a deployed model name with the provider that should dispatch
// it and the OpenRouterBench column whose scores trained its ranking row.
// Direct columns are 1:1 (Model == BenchColumn); proxy entries reuse
// another column's scores until enough D3 traffic accumulates to rank the
// deployed model directly.
type DeployedEntry struct {
	Model       string `json:"model"`
	Provider    string `json:"provider"`
	BenchColumn string `json:"bench_column"`
	Proxy       bool   `json:"proxy,omitempty"`
	ProxyNote   string `json:"proxy_note,omitempty"`
}

// ModelRegistry is the deserialized form of model_registry.json. The
// scorer iterates DeployedModels at request time after filtering by which
// providers were registered at boot. The rankings.json table keys off
// Entry.Model — the canonical name surfaced in router.Decision.Model.
type ModelRegistry struct {
	DeployedModels []DeployedEntry `json:"deployed_models"`
}

// Models returns the deduplicated, deployed-model-name set. Order is
// preserved from DeployedModels (the on-disk artifact order is meaningful:
// it pins tie-breaking when two providers share a bench column score).
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

// ArtifactMetadata is the parsed metadata.yaml that ships alongside each
// artifact bundle. It exists so debug logs and the eval harness can read
// "what is this version?" without re-parsing rankings + registry. Every
// field is informational at runtime; the routing decision is a function
// of centroids + rankings + registry only.
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

// Bundle is one fully-loaded artifact set. The Scorer holds one of these
// per version; the Multiversion router dispatches between them.
type Bundle struct {
	Version   string
	Centroids *Centroids
	Rankings  Rankings
	Registry  *ModelRegistry
	Metadata  *ArtifactMetadata
}

// ListVersions returns the sorted version directories under artifacts/.
// Order is lexicographic on the directory name (so v0.1 < v0.2 < v0.10
// would NOT sort numerically — keep names monotonic and zero-padded if
// you ever exceed 9 minor versions). The "latest" pointer file is not
// returned.
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
// version directory name. An empty string or the literal "latest"
// reads artifacts/latest and returns its trimmed contents. Anything
// else is returned verbatim after a sanity check that the directory
// exists.
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
// version and returns them as a Bundle. The version must already be
// resolved (no "latest" handling here — call ResolveVersion first).
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
	// metadata.yaml is best-effort: a bundle without one still routes,
	// it just won't appear with full provenance in /health-style logs.
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

// loadRankings returns an error on malformed JSON, non-integer cluster
// keys, empty rankings (no clusters), or empty per-cluster entries.
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
		// fmt.Sscanf("12abc", "%d", &k) succeeds with k=12, so we round-trip
		// the parsed int through Sprintf to reject keys with trailing junk.
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

// loadRegistry validates that every entry carries a non-empty (model,
// provider, bench_column) triple. Empty fields are loud errors at boot
// rather than silent "decision routes to nothing" at request time.
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
