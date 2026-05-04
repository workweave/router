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

// model.onnx and tokenizer.json are both hosted on HuggingFace Hub
// (see scripts/upload_to_hf.py), pulled into the runtime image by the
// Dockerfile, and into local dev via scripts/download_from_hf.py.
// They're loaded together because they're paired: a tokenizer must
// match the model it was trained against, so versioning them as one
// HF revision is the correct unit. Override the directory via
// ROUTER_ONNX_ASSETS_DIR for local dev (the export/download scripts
// write to assets/ under the repo root by default).
const defaultAssetsDir = "/opt/router/assets"

// minModelSizeBytes is a sanity floor that catches an unpopulated
// HF download or a stray pointer/placeholder. The real INT8-quantized
// jina-v2-base-code is ~160 MB; even a smaller drop-in embedder is
// going to be tens of MB. If the file is under this threshold
// something has gone wrong and we want to fail loudly at boot rather
// than silently miscalibrating embeddings.
const minModelSizeBytes = 1 << 20 // 1 MiB

// onnxEmbedder is the production Embedder implementation. It owns one
// hugot session and one feature-extraction pipeline; both are
// goroutine-safe so a single instance is shared across all requests.
//
// The hugot pipeline already handles:
//   - tokenization (BERT WordPiece via the embedded tokenizer.json)
//   - ONNX inference through onnxruntime_go (CGO; needs
//     libonnxruntime.so at link time)
//   - mean pooling over the token axis
//   - L2 normalization (via WithNormalization)
//
// so Embed reduces to a one-call wrapper plus type-shape sanity checks.
type onnxEmbedder struct {
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline

	// closeOnce guards Close so a double-close from tests or shutdown
	// hooks doesn't try to destroy the underlying ORT session twice.
	closeOnce sync.Once
}

// resolveAssetsDir returns the directory containing model.onnx and
// tokenizer.json. ROUTER_ONNX_ASSETS_DIR wins; otherwise defaultAssetsDir.
func resolveAssetsDir() string {
	if d := os.Getenv("ROUTER_ONNX_ASSETS_DIR"); d != "" {
		return d
	}
	return defaultAssetsDir
}

// NewEmbedder reads model.onnx and tokenizer.json from disk and
// constructs the shared session + pipeline. Hugot loads from the
// directory directly, so no temp-dir copy is needed. Callers are
// expected to call Close on shutdown so the underlying ORT session is
// released.
//
// Returns an error on any of: missing/undersized model.onnx, missing
// tokenizer.json (the HF download or local export hasn't run; main.go
// fail-opens to the heuristic on these paths), ONNX session creation
// failure (libonnxruntime missing or wrong version), or pipeline
// construction failure.
func NewEmbedder() (*onnxEmbedder, error) {
	assetsDir := resolveAssetsDir()
	modelPath := filepath.Join(assetsDir, "model.onnx")
	tokenizerPath := filepath.Join(assetsDir, "tokenizer.json")

	info, err := os.Stat(modelPath)
	if err != nil {
		return nil, fmt.Errorf("cluster: stat model.onnx at %s: %w (run scripts/download_from_hf.py or set ROUTER_ONNX_ASSETS_DIR)", modelPath, err)
	}
	if info.Size() < minModelSizeBytes {
		return nil, fmt.Errorf("cluster: model.onnx at %s is %d bytes (< %d); likely a placeholder or interrupted download", modelPath, info.Size(), minModelSizeBytes)
	}
	if _, err := os.Stat(tokenizerPath); err != nil {
		return nil, fmt.Errorf("cluster: stat tokenizer.json at %s: %w", tokenizerPath, err)
	}

	// hugot defaults to looking for libonnxruntime at fixed OS-specific
	// paths (/usr/lib/libonnxruntime.so on Linux,
	// /usr/local/lib/libonnxruntime.dylib on macOS). The Dockerfile
	// arranges the Linux default; on macOS dev boxes brew installs to
	// /opt/homebrew/lib which is *not* the default. ROUTER_ONNX_LIBRARY_DIR
	// is an opt-in dev escape hatch that overrides the lookup directory
	// (per hugot v0.7.0 the option takes a *directory*, not a file).
	var sessOpts []options.WithOption
	if dir := os.Getenv("ROUTER_ONNX_LIBRARY_DIR"); dir != "" {
		sessOpts = append(sessOpts, options.WithOnnxLibraryPath(dir))
	}
	session, err := hugot.NewORTSession(sessOpts...)
	if err != nil {
		return nil, fmt.Errorf("cluster: ort session: %w", err)
	}

	pipelineCfg := hugot.FeatureExtractionConfig{
		ModelPath:    assetsDir,
		Name:         "weave-router-jina-v2",
		OnnxFilename: "model.onnx",
		Options: []hugot.FeatureExtractionOption{
			// Mean pooling + L2 normalization is the standard
			// sentence-transformers contract; jina-embeddings-v2-base-code
			// is trained against that pooling so we hard-pin it here.
			pipelines.WithNormalization(),
		},
	}
	pipeline, err := hugot.NewPipeline(session, pipelineCfg)
	if err != nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("cluster: feature-extraction pipeline: %w", err)
	}

	return &onnxEmbedder{
		session:  session,
		pipeline: pipeline,
	}, nil
}

// Embed runs the pipeline on a single text. Returns a [EmbedDim]float32
// L2-normalized vector or an error. ctx is ignored here — hugot v0.7.0's
// RunPipeline doesn't accept a context. The request-path timeout is
// enforced by scorer.Route, which races this call against
// context.WithTimeout in a goroutine; a slow inference still runs to
// completion in the background but the request returns on timeout.
func (e *onnxEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	out, err := e.pipeline.RunPipeline([]string{text})
	if err != nil {
		return nil, fmt.Errorf("pipeline: %w", err)
	}
	if len(out.Embeddings) != 1 {
		return nil, fmt.Errorf("pipeline returned %d embeddings, want 1", len(out.Embeddings))
	}
	vec := out.Embeddings[0]
	if len(vec) != EmbedDim {
		return nil, fmt.Errorf("embedding dim %d, want %d", len(vec), EmbedDim)
	}
	return vec, nil
}

// Close releases the ORT session. Idempotent.
func (e *onnxEmbedder) Close() error {
	var firstErr error
	e.closeOnce.Do(func() {
		if e.session != nil {
			if err := e.session.Destroy(); err != nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

var _ Embedder = (*onnxEmbedder)(nil)
