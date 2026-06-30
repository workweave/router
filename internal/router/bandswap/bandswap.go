// Package bandswap is the per-turn large-vs-small swap head: a model-agnostic
// classifier that predicts the next engineer action from the current user
// message embedding and collapses it to a routing band (LARGE / SMALL). It lets
// an already-pinned session swap between its two band-paired models per turn
// (Stage 1 pinned the pair; this is the per-turn chooser) without re-running the
// scorer.
//
// Parity: the served head is an embedding-only multinomial-logreg trained
// offline (repo: ml_dev/router_action_classifier) on the same Jina v2 INT8
// user-message embedding the scorer already produces and stores in
// res.Fresh.Metadata.Embedding when ROUTER_EMBED_ONLY_USER_MESSAGE is on (the
// default). Inference is standardize + matmul + argmax — no ONNX/tree-model
// dependency. Pure inner-ring: no I/O beyond the compiled-in model.
package bandswap

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
)

// Band labels. LARGE = heavy/reasoning turns; SMALL = everything else.
const (
	Large = "large"
	Small = "small"
	// EmbedDim is the Jina v2 base embedding width the head expects.
	EmbedDim = 768
)

//go:embed artifacts/action_clf_logreg_embonly.json
var embeddedModel []byte

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

type modelJSON struct {
	Classes     []string    `json:"classes"`
	Coef        [][]float64 `json:"coef"`
	Intercept   []float64   `json:"intercept"`
	ScalerMean  []float64   `json:"scaler_mean"`
	ScalerScale []float64   `json:"scaler_scale"`
}

// Classifier is a loaded embedding-only action head.
type Classifier struct {
	classes   []string
	coef      [][]float64
	intercept []float64
	mean      []float64
	scale     []float64
}

// New loads the compiled-in action-classifier artifact, validating its shape so
// a malformed export fails loudly at boot rather than mis-routing silently.
func New() (clf *Classifier, err error) {
	var m modelJSON
	err = json.Unmarshal(embeddedModel, &m)
	if err != nil {
		return nil, fmt.Errorf("bandswap: parse model: %w", err)
	}
	n := len(m.Classes)
	if n == 0 || len(m.Coef) != n || len(m.Intercept) != n {
		return nil, fmt.Errorf("bandswap: model shape mismatch (classes=%d coef=%d intercept=%d)", n, len(m.Coef), len(m.Intercept))
	}
	if len(m.ScalerMean) != EmbedDim || len(m.ScalerScale) != EmbedDim {
		return nil, fmt.Errorf("bandswap: scaler width mean=%d scale=%d, want %d", len(m.ScalerMean), len(m.ScalerScale), EmbedDim)
	}
	for i, row := range m.Coef {
		if len(row) != EmbedDim {
			return nil, fmt.Errorf("bandswap: coef row %d width %d, want %d", i, len(row), EmbedDim)
		}
	}
	for _, a := range m.Classes {
		if _, ok := bandMap[a]; !ok {
			return nil, fmt.Errorf("bandswap: class %q absent from band map", a)
		}
	}
	return &Classifier{
		classes:   m.Classes,
		coef:      m.Coef,
		intercept: m.Intercept,
		mean:      m.ScalerMean,
		scale:     m.ScalerScale,
	}, nil
}

// PredictAction returns the argmax action label for a (L2-normalized) embedding.
// ok is false when the embedding width is wrong — the caller must then fall back
// to the pin's anchor model rather than guess.
func (c *Classifier) PredictAction(emb []float32) (action string, ok bool) {
	if len(emb) != EmbedDim {
		return "", false
	}
	best := 0
	bestLogit := math.Inf(-1)
	for k := range c.classes {
		logit := c.intercept[k]
		row := c.coef[k]
		for i, e := range emb {
			scale := c.scale[i]
			if scale == 0 {
				continue
			}
			logit += row[i] * (float64(e) - c.mean[i]) / scale
		}
		if logit > bestLogit {
			bestLogit = logit
			best = k
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
