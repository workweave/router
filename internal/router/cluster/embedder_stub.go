//go:build no_onnx

package cluster

import (
	"context"
	"fmt"
)

// stubEmbedder is the no_onnx-tag implementation used by build / test
// environments without libonnxruntime available. NewEmbedder returns an
// error so the cluster.Scorer fails to construct and main.go fails open
// to the heuristic — same fail-open path taken in production when the
// embedded artifacts are missing or malformed.
type stubEmbedder struct{}

// NewEmbedder always fails under -tags=no_onnx. This is the deliberate
// dev-convenience escape hatch documented in router/CLAUDE.md.
func NewEmbedder() (*stubEmbedder, error) {
	return nil, fmt.Errorf("cluster: built with -tags=no_onnx; the ONNX-backed embedder is unavailable in this build")
}

func (*stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, fmt.Errorf("cluster: embedder unavailable (no_onnx build tag)")
}

func (*stubEmbedder) Close() error { return nil }

var _ Embedder = (*stubEmbedder)(nil)
