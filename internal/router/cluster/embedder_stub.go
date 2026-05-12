//go:build no_onnx

package cluster

import (
	"context"
	"fmt"
)

// stubEmbedder is the no_onnx-tag impl for environments without
// libonnxruntime. NewEmbedder errors so cluster.Scorer fails to
// construct — the documented dev-convenience escape hatch.
type stubEmbedder struct{}

// NewEmbedder always fails under -tags=no_onnx.
func NewEmbedder() (*stubEmbedder, error) {
	return nil, fmt.Errorf("cluster: built with -tags=no_onnx; the ONNX-backed embedder is unavailable in this build")
}

func (*stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, fmt.Errorf("cluster: embedder unavailable (no_onnx build tag)")
}

func (*stubEmbedder) Close() error { return nil }

var _ Embedder = (*stubEmbedder)(nil)
