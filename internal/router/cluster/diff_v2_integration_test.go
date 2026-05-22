//go:build diff_v2 && onnx_integration && ORT

package cluster

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router"
)

// TestV2MatchesV1 is the release gate for the v2 routing migration.
//
// It reads a bundle directory (passed via DIFF_V2_BUNDLE_DIR) that
// contains BOTH a v1 rankings.json and v2 quality_means.json +
// model_axes.json — i.e. a bundle trained with `--write-v2`. It
// constructs one v1-shaped Scorer and one v2-shaped Scorer from the
// same files, routes the 1000-prompt fixture through both at the
// bundle's default knobs, and asserts:
//
//   - ≥99% top-1 agreement (configurable via DIFF_V2_TOLERANCE)
//   - every divergence is within score margin 1e-3
//
// Divergences are dumped to diff_v2_vs_v1_divergences.csv for review.
//
// Build tags: -tags "diff_v2 onnx_integration ORT". The onnx_integration
// + ORT tags are required for the real embedder; diff_v2 keeps the test
// out of the default `go test` matrix so contributors without an ONNX
// runtime can still iterate.
func TestV2MatchesV1(t *testing.T) {
	bundleDir := os.Getenv("DIFF_V2_BUNDLE_DIR")
	if bundleDir == "" {
		t.Skip("DIFF_V2_BUNDLE_DIR not set; release-gate diff test only runs under the diff_v2_vs_v1.py driver.")
	}
	tolerance := 0.99
	if env := os.Getenv("DIFF_V2_TOLERANCE"); env != "" {
		v, err := strconv.ParseFloat(env, 64)
		require.NoError(t, err)
		tolerance = v
	}
	fixturePath := os.Getenv("DIFF_V2_FIXTURE_PATH")
	if fixturePath == "" {
		fixturePath = filepath.Join("testdata", "diff_v2_prompts.jsonl")
	}

	v2Bundle, err := LoadBundleFromDir(bundleDir, "diff-v2")
	require.NoError(t, err)
	require.True(t, v2Bundle.IsV2, "bundle at %s must contain v2 files (quality_means.json + model_axes.json)", bundleDir)

	v1Bundle, err := LoadBundleV1Only(bundleDir, "diff-v1")
	require.NoError(t, err)
	require.False(t, v1Bundle.IsV2)

	// Embedder is the real ONNX-backed one. Both scorers share it so
	// the prompt embedding is identical input to each blend path.
	emb, err := NewEmbedder()
	require.NoError(t, err)

	cfg := DefaultConfig()
	// Single-shot decisions — both scorers see the same top-p clusters.
	available := map[string]struct{}{
		"anthropic":  {},
		"openai":     {},
		"google":     {},
		"openrouter": {},
		"fireworks":  {},
		"deepinfra":  {},
		"bedrock":    {},
	}
	v1Scorer, err := NewScorer(v1Bundle, cfg, emb, available)
	require.NoError(t, err)
	v2Scorer, err := NewScorer(v2Bundle, cfg, emb, available)
	require.NoError(t, err)

	prompts, err := loadDiffV2Fixture(fixturePath)
	require.NoError(t, err)
	t.Logf("Loaded %d fixture prompts from %s", len(prompts), fixturePath)

	var divergences []divergenceRow
	agree := 0

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, p := range prompts {
		req := router.Request{
			PromptText: p.Text,
		}
		v1Dec, err := v1Scorer.Route(ctx, req)
		require.NoError(t, err, "v1 route failed on prompt_id=%s", p.PromptID)
		v2Dec, err := v2Scorer.Route(ctx, req)
		require.NoError(t, err, "v2 route failed on prompt_id=%s", p.PromptID)

		if v1Dec.Model == v2Dec.Model {
			agree++
			continue
		}
		var v1Score, v2Score float32
		if v1Dec.Metadata != nil {
			v1Score = v1Dec.Metadata.ChosenScore
		}
		if v2Dec.Metadata != nil {
			v2Score = v2Dec.Metadata.ChosenScore
		}
		margin := float32(math.Abs(float64(v1Score - v2Score)))
		divergences = append(divergences, divergenceRow{
			PromptID: p.PromptID,
			Source:   p.Source,
			V1Top1:   v1Dec.Model,
			V2Top1:   v2Dec.Model,
			V1Score:  v1Score,
			V2Score:  v2Score,
			Margin:   margin,
		})
	}

	rate := float64(agree) / float64(len(prompts))
	t.Logf("Top-1 agreement: %d/%d (%.4f); tolerance=%.4f", agree, len(prompts), rate, tolerance)
	t.Logf("Divergences: %d", len(divergences))

	if len(divergences) > 0 {
		dumpPath := "diff_v2_vs_v1_divergences.csv"
		if err := writeDivergencesCSV(dumpPath, divergences); err != nil {
			t.Logf("Failed to write divergences CSV: %v", err)
		} else {
			t.Logf("Divergences written to %s", dumpPath)
		}
	}

	assert.GreaterOrEqual(t, rate, tolerance,
		"top-1 agreement %.4f < tolerance %.4f", rate, tolerance)
	for _, d := range divergences {
		assert.Less(t, d.Margin, float32(1e-3),
			"divergence on %s: margin %.6f exceeds 1e-3 (v1=%s score=%f, v2=%s score=%f)",
			d.PromptID, d.Margin, d.V1Top1, d.V1Score, d.V2Top1, d.V2Score)
	}
}

type diffV2Prompt struct {
	PromptID string `json:"prompt_id"`
	Text     string `json:"text"`
	Source   string `json:"source"`
}

func loadDiffV2Fixture(path string) ([]diffV2Prompt, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	var out []diffV2Prompt
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row diffV2Prompt
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse fixture line: %w", err)
		}
		out = append(out, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read fixture: %w", err)
	}
	return out, nil
}

type divergenceRow struct {
	PromptID string
	Source   string
	V1Top1   string
	V2Top1   string
	V1Score  float32
	V2Score  float32
	Margin   float32
}

func writeDivergencesCSV(path string, rows []divergenceRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"prompt_id", "source", "v1_top1", "v2_top1", "v1_score", "v2_score", "margin"}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := w.Write([]string{
			r.PromptID, r.Source, r.V1Top1, r.V2Top1,
			strconv.FormatFloat(float64(r.V1Score), 'g', -1, 32),
			strconv.FormatFloat(float64(r.V2Score), 'g', -1, 32),
			strconv.FormatFloat(float64(r.Margin), 'g', -1, 32),
		}); err != nil {
			return err
		}
	}
	return nil
}
