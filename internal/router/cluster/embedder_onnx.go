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

// model.onnx and tokenizer.json are versioned together on HF Hub.
// Override with ROUTER_ONNX_ASSETS_DIR for local dev.
const defaultAssetsDir = "/opt/router/assets"

// minModelSizeBytes catches unpopulated HF downloads or placeholders.
// Real INT8-quantized jina-v2-base-code is ~160 MB.
const minModelSizeBytes = 1 << 20

// onnxEmbedder is the production Embedder. Owns one hugot session and
// pipeline; both goroutine-safe so one instance serves all requests.
type onnxEmbedder struct {
	session   *hugot.Session
	pipeline  *pipelines.FeatureExtractionPipeline
	closeOnce sync.Once
}

// resolveAssetsDir returns ROUTER_ONNX_ASSETS_DIR if set, else defaultAssetsDir.
func resolveAssetsDir() string {
	if d := os.Getenv("ROUTER_ONNX_ASSETS_DIR"); d != "" {
		return d
	}
	return defaultAssetsDir
}

// NewEmbedder reads model.onnx and tokenizer.json from disk and
// constructs the shared session + pipeline.
func NewEmbedder() (*onnxEmbedder, error) {
	assetsDir := resolveAssetsDir()
	modelPath := filepath.Join(assetsDir, "model.onnx")
	tokenizerPath := filepath.Join(assetsDir, "tokenizer.json")

	info, err := os.Stat(modelPath)
	if err != nil {
		return nil, fmt.Errorf("cluster: stat model.onnx at %s: %w (set ROUTER_ONNX_ASSETS_DIR to a directory containing model.onnx and tokenizer.json)", modelPath, err)
	}
	if info.Size() < minModelSizeBytes {
		return nil, fmt.Errorf("cluster: model.onnx at %s is %d bytes (< %d); likely a placeholder or interrupted download", modelPath, info.Size(), minModelSizeBytes)
	}
	if _, err := os.Stat(tokenizerPath); err != nil {
		return nil, fmt.Errorf("cluster: stat tokenizer.json at %s: %w", tokenizerPath, err)
	}

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

	pipelineCfg := hugot.FeatureExtractionConfig{
		ModelPath:    assetsDir,
		Name:         "weave-router-jina-v2",
		OnnxFilename: "model.onnx",
		Options:      []hugot.FeatureExtractionOption{pipelines.WithNormalization()},
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
