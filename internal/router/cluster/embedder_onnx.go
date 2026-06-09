//go:build !no_onnx

package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/options"
	"github.com/knights-analytics/hugot/pipelines"
)

// Each embedder's model.onnx and tokenizer.json live under
// <assetsRoot>/<EmbedderSpec.AssetSubdir>/. Override the root with
// ROUTER_ONNX_ASSETS_DIR for local dev.
const defaultAssetsDir = "/opt/router/assets"

// minModelSizeBytes catches unpopulated HF downloads or placeholders.
// Real INT8-quantized embedders are >100 MB.
const minModelSizeBytes = 1 << 20

// onnxEmbedder is the production Embedder for one EmbedderSpec. The
// pipeline is goroutine-safe so one instance serves all requests; the
// hugot session is owned by the EmbedderSet, not the embedder.
type onnxEmbedder struct {
	spec     EmbedderSpec
	pipeline *pipelines.FeatureExtractionPipeline
}

// resolveAssetsDir returns ROUTER_ONNX_ASSETS_DIR if set, else defaultAssetsDir.
func resolveAssetsDir() string {
	if d := os.Getenv("ROUTER_ONNX_ASSETS_DIR"); d != "" {
		return d
	}
	return defaultAssetsDir
}

// assetsDirForSpec resolves the model directory for a spec. The Jina
// embedder falls back to the flat legacy layout (<root>/model.onnx)
// when its subdir is absent, so existing local-dev setups keep working.
func assetsDirForSpec(spec EmbedderSpec) string {
	root := resolveAssetsDir()
	dir := filepath.Join(root, spec.AssetSubdir)
	if _, err := os.Stat(filepath.Join(dir, "model.onnx")); err == nil {
		return dir
	}
	if spec.ID == EmbedderJinaV2 {
		if _, err := os.Stat(filepath.Join(root, "model.onnx")); err == nil {
			return root
		}
	}
	return dir
}

// newONNXEmbedder builds the feature-extraction pipeline for one spec on
// a shared hugot session.
//
// Pooling contract: the Jina export emits token-level (3D) outputs which
// hugot mean-pools; the Qwen3 export bakes last-token pooling into the
// graph and emits a pooled (2D) output which hugot returns as-is. Both
// are L2-normalized via WithNormalization.
func newONNXEmbedder(session *hugot.Session, spec EmbedderSpec) (*onnxEmbedder, error) {
	assetsDir := assetsDirForSpec(spec)
	modelPath := filepath.Join(assetsDir, "model.onnx")
	tokenizerPath := filepath.Join(assetsDir, "tokenizer.json")

	info, err := os.Stat(modelPath)
	if err != nil {
		return nil, fmt.Errorf("cluster: stat model.onnx for embedder %s at %s: %w (set ROUTER_ONNX_ASSETS_DIR to a directory containing %s/{model.onnx,tokenizer.json})", spec.ID, modelPath, err, spec.AssetSubdir)
	}
	if info.Size() < minModelSizeBytes {
		return nil, fmt.Errorf("cluster: model.onnx for embedder %s at %s is %d bytes (< %d); likely a placeholder or interrupted download", spec.ID, modelPath, info.Size(), minModelSizeBytes)
	}
	if _, err := os.Stat(tokenizerPath); err != nil {
		return nil, fmt.Errorf("cluster: stat tokenizer.json for embedder %s at %s: %w", spec.ID, tokenizerPath, err)
	}

	pipelineCfg := hugot.FeatureExtractionConfig{
		ModelPath:    assetsDir,
		Name:         "weave-router-" + spec.ID,
		OnnxFilename: "model.onnx",
		Options:      []hugot.FeatureExtractionOption{pipelines.WithNormalization()},
	}
	pipeline, err := hugot.NewPipeline(session, pipelineCfg)
	if err != nil {
		return nil, fmt.Errorf("cluster: feature-extraction pipeline for embedder %s: %w", spec.ID, err)
	}

	return &onnxEmbedder{spec: spec, pipeline: pipeline}, nil
}

// Embed runs the pipeline on a single text. ctx is ignored because hugot
// v0.7.0's RunPipeline doesn't accept one; scorer.Route races this call
// against context.WithTimeout in a goroutine instead.
func (e *onnxEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	out, err := e.pipeline.RunPipeline([]string{text})
	if err != nil {
		return nil, fmt.Errorf("pipeline: %w", err)
	}
	if len(out.Embeddings) != 1 {
		return nil, fmt.Errorf("pipeline returned %d embeddings, want 1", len(out.Embeddings))
	}
	vec := out.Embeddings[0]
	if len(vec) != e.spec.Dim {
		return nil, fmt.Errorf("embedding dim %d, want %d", len(vec), e.spec.Dim)
	}
	return vec, nil
}

// ID returns the embedder model identity.
func (e *onnxEmbedder) ID() string { return e.spec.ID }

// Dim returns the output dimensionality.
func (e *onnxEmbedder) Dim() int { return e.spec.Dim }

var _ Embedder = (*onnxEmbedder)(nil)

// EmbedderSet lazily constructs and caches one Embedder per registered
// spec, all sharing a single ORT session. Construct in the composition
// root; Get only the IDs the built artifact versions require so prod
// (single default version) loads exactly one model into memory.
type EmbedderSet struct {
	mu        sync.Mutex
	session   *hugot.Session
	embedders map[string]*onnxEmbedder
	closeOnce sync.Once
}

// NewEmbedderSet creates the shared ORT session. No model is loaded
// until Get is called.
func NewEmbedderSet() (*EmbedderSet, error) {
	// hugot defaults to /usr/lib (Linux) / /usr/local/lib (macOS);
	// ROUTER_ONNX_LIBRARY_DIR overrides (macOS brew → /opt/homebrew/lib).
	var sessOpts []options.WithOption
	if dir := os.Getenv("ROUTER_ONNX_LIBRARY_DIR"); dir != "" {
		sessOpts = append(sessOpts, options.WithOnnxLibraryPath(dir))
	}
	session, err := hugot.NewORTSession(sessOpts...)
	if err != nil {
		return nil, fmt.Errorf("cluster: ort session: %w", err)
	}
	return &EmbedderSet{
		session:   session,
		embedders: make(map[string]*onnxEmbedder),
	}, nil
}

// Get returns the cached Embedder for id, constructing it on first use.
func (s *EmbedderSet) Get(id string) (Embedder, error) {
	spec, ok := SpecForEmbedder(id)
	if !ok {
		return nil, fmt.Errorf("cluster: unknown embedder %q (known: %s, %s)", id, EmbedderJinaV2, EmbedderQwen3)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.embedders[id]; ok {
		return e, nil
	}
	e, err := newONNXEmbedder(s.session, spec)
	if err != nil {
		return nil, err
	}
	s.embedders[id] = e
	return e, nil
}

// Built returns the IDs of embedders constructed so far.
func (s *EmbedderSet) Built() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.embedders))
	for id := range s.embedders {
		out = append(out, id)
	}
	return out
}

// Close releases the shared ORT session (and with it every pipeline).
// Idempotent.
func (s *EmbedderSet) Close() error {
	var firstErr error
	s.closeOnce.Do(func() {
		if s.session != nil {
			if err := s.session.Destroy(); err != nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

// StandaloneEmbedder couples one Embedder with its owning EmbedderSet
// so single-embedder callers (integration tests) can Close the session.
type StandaloneEmbedder struct {
	Embedder
	set *EmbedderSet
}

// Close releases the underlying ORT session. Idempotent.
func (s *StandaloneEmbedder) Close() error { return s.set.Close() }

// NewEmbedder constructs a standalone Jina v2 embedder with its own
// session. Retained for the parity integration tests; the composition
// root uses NewEmbedderSet directly.
func NewEmbedder() (*StandaloneEmbedder, error) {
	set, err := NewEmbedderSet()
	if err != nil {
		return nil, err
	}
	e, err := set.Get(EmbedderJinaV2)
	if err != nil {
		_ = set.Close()
		return nil, err
	}
	return &StandaloneEmbedder{Embedder: e, set: set}, nil
}
