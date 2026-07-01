package percallband

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"

	"github.com/dmitryikh/leaves"
)

// artifacts holds the two frozen binary band boosters + metadata, exported by
// ml_dev/router_action_classifier/percall/export.py and relabeled to the v3 header
// the pure-Go leaves reader accepts. Compiled into the binary — no runtime I/O.
//
//go:embed artifacts/percall_band_markov.txt artifacts/percall_band_markov_emb.txt artifacts/percall_band_metadata.json
var artifacts embed.FS

const (
	markovFile    = "artifacts/percall_band_markov.txt"
	markovEmbFile = "artifacts/percall_band_markov_emb.txt"
	metadataFile  = "artifacts/percall_band_metadata.json"
)

// headSpec is one head's frozen config from metadata.json.
type headSpec struct {
	File             string  `json:"file"`
	NFeatures        int     `json:"n_features"`
	UsesEmbedding    bool    `json:"uses_embedding"`
	OOFOrgAUC        float64 `json:"oof_org_auc"`
	OOFSessionAUC    float64 `json:"oof_session_auc"`
	DefaultThreshold float64 `json:"default_threshold"`
}

type metadata struct {
	DecisionRule       string   `json:"decision_rule"`
	DefaultMissCap     float64  `json:"default_miss_cap"`
	MarkovFeatureNames []string `json:"markov_feature_names"`
	Embedding          struct {
		Dim int `json:"dim"`
	} `json:"embedding"`
	ActionTypes []string            `json:"action_types"`
	BandMap     map[string]string   `json:"band_map"`
	Heads       map[string]headSpec `json:"heads"`
}

// Head is the loaded per-call band predictor: two boosters (markov-only for
// ToolResult turns where the embedding is not free; markov+emb for MainLoop turns
// where the router already computed the prompt vector) + their frozen thresholds.
type Head struct {
	markov     *leaves.Ensemble
	markovEmb  *leaves.Ensemble
	markovT    float64
	markovEmbT float64
	embDim     int
	meta       metadata
}

// New loads the embedded artifacts into a ready-to-serve Head. Fails fast if an
// artifact is missing/corrupt or the embedded feature layout disagrees with the Go
// feature builder (a train/serve-skew guard).
func New() (*Head, error) {
	rawMeta, err := artifacts.ReadFile(metadataFile)
	if err != nil {
		return nil, fmt.Errorf("percallband: read metadata: %w", err)
	}
	var m metadata
	if err := json.Unmarshal(rawMeta, &m); err != nil {
		return nil, fmt.Errorf("percallband: parse metadata: %w", err)
	}
	if err := checkFeatureLayout(m.MarkovFeatureNames); err != nil {
		return nil, err
	}
	markov, err := loadEnsemble(markovFile)
	if err != nil {
		return nil, err
	}
	markovEmb, err := loadEnsemble(markovEmbFile)
	if err != nil {
		return nil, err
	}
	h := &Head{
		markov:     markov,
		markovEmb:  markovEmb,
		markovT:    m.Heads["markov"].DefaultThreshold,
		markovEmbT: m.Heads["markov_emb"].DefaultThreshold,
		embDim:     m.Embedding.Dim,
		meta:       m,
	}
	return h, nil
}

// PredictBand returns the routing band, P(LARGE), and which head fired. When emb is
// non-nil and matches the trained dimension, the markov+emb head is used (MainLoop);
// otherwise the markov-only head (ToolResult / cold path). Decision rule (frozen in
// metadata): serve SMALL iff P(LARGE) < threshold.
func (h *Head) PredictBand(markov []float32, emb []float32) (band Band, pLarge float64, usedEmb bool) {
	if emb != nil && len(emb) == h.embDim {
		fvals := make([]float64, 0, len(markov)+len(emb))
		fvals = appendF32(fvals, markov)
		fvals = appendF32(fvals, emb)
		pLarge = h.markovEmb.PredictSingle(fvals, 0)
		return bandFor(pLarge, h.markovEmbT), pLarge, true
	}
	pLarge = h.markov.PredictSingle(f32to64(markov), 0)
	return bandFor(pLarge, h.markovT), pLarge, false
}

// EmbedDim is the embedding dimension the markov+emb head expects.
func (h *Head) EmbedDim() int { return h.embDim }

func bandFor(pLarge, threshold float64) Band {
	if pLarge < threshold {
		return SmallBand
	}
	return LargeBand
}

func loadEnsemble(name string) (*leaves.Ensemble, error) {
	raw, err := artifacts.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("percallband: read %s: %w", name, err)
	}
	// loadTransformation=true so PredictSingle returns the sigmoid'd P(LARGE),
	// matching the Python booster_.predict the parity fixture was frozen against.
	ens, err := leaves.LGEnsembleFromReader(bufio.NewReader(bytes.NewReader(raw)), true)
	if err != nil {
		return nil, fmt.Errorf("percallband: parse %s: %w", name, err)
	}
	return ens, nil
}

// checkFeatureLayout guards against train/serve skew: the embedded markov feature
// names (from Python export) must equal the Go builder's column order exactly.
func checkFeatureLayout(embedded []string) error {
	got := FeatureNames()
	if len(embedded) != len(got) {
		return fmt.Errorf("percallband: markov feature count mismatch: metadata=%d go=%d", len(embedded), len(got))
	}
	for i := range got {
		if embedded[i] != got[i] {
			return fmt.Errorf("percallband: markov feature[%d] mismatch: metadata=%q go=%q", i, embedded[i], got[i])
		}
	}
	return nil
}

func appendF32(dst []float64, src []float32) []float64 {
	for _, v := range src {
		dst = append(dst, float64(v))
	}
	return dst
}

func f32to64(src []float32) []float64 {
	return appendF32(make([]float64, 0, len(src)), src)
}
