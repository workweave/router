// Package bandswap is the per-turn large-vs-small swap head: a model-agnostic
// classifier that predicts the next engineer action from the current user
// message embedding and collapses it to a routing band (LARGE / SMALL). It lets
// an already-pinned session swap between its two band-paired models per turn
// (Stage 1 pinned the pair; this is the per-turn chooser) without re-running the
// scorer.
//
// Parity: the served head is an embedding-only LightGBM multiclass booster
// trained offline (repo: ml_dev/router_action_classifier) on the same Jina v2
// INT8 user-message embedding the scorer already produces and stores in
// res.Fresh.Metadata.Embedding when ROUTER_EMBED_ONLY_USER_MESSAGE is on (the
// default). Inference runs through the pure-Go `leaves` reader over the booster
// text dump — no Python, ONNX, or cgo. Pure inner-ring: no I/O beyond the
// compiled-in model.
package bandswap

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"

	"github.com/dmitryikh/leaves"
)

// Band labels. LARGE = heavy/reasoning turns; SMALL = everything else.
const (
	Large = "large"
	Small = "small"
	// EmbedDim is the Jina v2 base embedding width the head expects.
	EmbedDim = 768
)

//go:embed artifacts/action_clf_lgbm_embonly.txt
var embeddedModel []byte

//go:embed artifacts/action_clf_classes.json
var embeddedClasses []byte

// bandMap mirrors ml_dev/router_action_classifier/bands.py BAND_MAP verbatim.
// Keep the two in sync — divergence is a routing bug.
var bandMap = map[string]string{
	"planning":       Large,
	"learning":       Large,
	"providing_info": Small,
	"tool_call":      Small,
	"confirmation":   Small,
	"refinement":     Small,
}

// Classifier is a loaded embedding-only LightGBM action head. The booster stores
// integer classes 0..k-1; classes maps each output-group index to its action
// label (the sklearn classes_ order persisted at export time).
type Classifier struct {
	model   *leaves.Ensemble
	classes []string
}

// New loads the compiled-in LightGBM action head, validating its shape so a
// malformed export fails loudly at boot rather than mis-routing silently.
func New() (clf *Classifier, err error) {
	var cj struct {
		Classes []string `json:"classes"`
	}
	err = json.Unmarshal(embeddedClasses, &cj)
	if err != nil {
		return nil, fmt.Errorf("bandswap: parse classes: %w", err)
	}
	model, err := leaves.LGEnsembleFromReader(bufio.NewReader(bytes.NewReader(embeddedModel)), true)
	if err != nil {
		return nil, fmt.Errorf("bandswap: load lgbm booster: %w", err)
	}
	if model.NFeatures() != EmbedDim {
		return nil, fmt.Errorf("bandswap: booster features %d, want %d", model.NFeatures(), EmbedDim)
	}
	if model.NOutputGroups() != len(cj.Classes) {
		return nil, fmt.Errorf("bandswap: booster output groups %d != %d class labels", model.NOutputGroups(), len(cj.Classes))
	}
	for _, a := range cj.Classes {
		if _, ok := bandMap[a]; !ok {
			return nil, fmt.Errorf("bandswap: class %q absent from band map", a)
		}
	}
	return &Classifier{model: model, classes: cj.Classes}, nil
}

// PredictAction returns the argmax action label for a (L2-normalized) embedding.
// ok is false when the embedding width is wrong or inference fails — the caller
// must then fall back to the pin's anchor model rather than guess. Safe for
// concurrent use: each call allocates its own buffers and the booster is
// read-only.
func (c *Classifier) PredictAction(emb []float32) (action string, ok bool) {
	if len(emb) != EmbedDim {
		return "", false
	}
	fvals := make([]float64, EmbedDim)
	for i, e := range emb {
		fvals[i] = float64(e)
	}
	preds := make([]float64, len(c.classes))
	if err := c.model.Predict(fvals, 0, preds); err != nil {
		return "", false
	}
	best := 0
	bestP := math.Inf(-1)
	for i, p := range preds {
		if p > bestP {
			bestP = p
			best = i
		}
	}
	return c.classes[best], true
}

// PredictBand returns the predicted action and its routing band.
func (c *Classifier) PredictBand(emb []float32) (action, band string, ok bool) {
	action, ok = c.PredictAction(emb)
	if !ok {
		return "", "", false
	}
	return action, Band(action), true
}

// Band collapses an action label to its routing band. An unknown label routes
// LARGE (the safe, capability-preserving direction).
func Band(action string) string {
	if b, ok := bandMap[action]; ok {
		return b
	}
	return Large
}
