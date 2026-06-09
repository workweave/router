//go:build no_onnx

package cluster

import (
	"context"
	"fmt"
)

// stubEmbedder is the no_onnx-tag impl for environments without
// libonnxruntime. NewEmbedderSet errors so cluster.Scorer fails to
// construct — the documented dev-convenience escape hatch.
type stubEmbedder struct{}

func (*stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, fmt.Errorf("cluster: embedder unavailable (no_onnx build tag)")
}

func (*stubEmbedder) ID() string { return "" }

func (*stubEmbedder) Dim() int { return 0 }

func (*stubEmbedder) Close() error { return nil }

var _ Embedder = (*stubEmbedder)(nil)

// EmbedderSet is the no_onnx stub; NewEmbedderSet always fails.
type EmbedderSet struct{}

// NewEmbedderSet always fails under -tags=no_onnx.
func NewEmbedderSet() (*EmbedderSet, error) {
	return nil, fmt.Errorf("cluster: built with -tags=no_onnx; the ONNX-backed embedder is unavailable in this build")
}

// Get always fails under -tags=no_onnx.
func (*EmbedderSet) Get(_ string) (Embedder, error) {
	return nil, fmt.Errorf("cluster: embedder unavailable (no_onnx build tag)")
}

// Built returns no embedders under -tags=no_onnx.
func (*EmbedderSet) Built() []string { return nil }

// Close is a no-op under -tags=no_onnx.
func (*EmbedderSet) Close() error { return nil }

// NewEmbedder always fails under -tags=no_onnx.
func NewEmbedder() (*stubEmbedder, error) {
	return nil, fmt.Errorf("cluster: built with -tags=no_onnx; the ONNX-backed embedder is unavailable in this build")
}
