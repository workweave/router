package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// diffV2PromptsSHA pins the SHA-256 of testdata/diff_v2_prompts.jsonl so
// unnoticed corpus drift fails CI before TestV2MatchesV1 runs. Regenerate via
// `poetry run python regen_diff_corpus.py --n 1000 --seed 42 --routerarena-only`
// (from router-internal/scripts) and update this constant in the same commit.
const diffV2PromptsSHA = "7f72e9a4b217242e56417d2933b9b9f31c5f319ae0222115a7958b49b97aa20f"

// diffV2FixturePath is the committed fixture path; the driver script may
// override it via DIFF_V2_FIXTURE_PATH.
var diffV2FixturePath = filepath.Join("testdata", "diff_v2_prompts.jsonl")

// TestDiffV2PromptsFixtureSHA gates v2 release by catching fixture drift
// before TestV2MatchesV1 runs.
func TestDiffV2PromptsFixtureSHA(t *testing.T) {
	if env := os.Getenv("DIFF_V2_FIXTURE_PATH"); env != "" {
		// Driver-script override; go-test-only runs always check the committed fixture.
		t.Skip("DIFF_V2_FIXTURE_PATH set; skipping committed-fixture SHA check.")
	}
	raw, err := os.ReadFile(diffV2FixturePath)
	require.NoError(t, err, "fixture must be committed at %s", diffV2FixturePath)
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	assert.Equal(t, diffV2PromptsSHA, got,
		"fixture SHA drift detected. Regenerate via regen_diff_corpus.py "+
			"and update diffV2PromptsSHA in the same commit.")
}
