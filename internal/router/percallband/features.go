package percallband

import (
	"fmt"
	"math"
)

// Causal engineered features for per-call action prediction — the Go mirror of
// features.py. Every feature comes from PAST calls (the ring State) + the CURRENT
// request only (never the current response), so it reproduces at routing time and
// is leakage-free. The column order here MUST match features.py `_feature_names()`
// exactly; features_test.go asserts it against a frozen fixture.

const (
	lastK       = 5
	lastKWide   = 12
	sinceCap    = 50  // cap on "calls since action last occurred"
	longGapSecs = 300 // a >5min gap before a call ~ a new human turn
)

// lags are the 1..5 previous-action one-hots.
var lags = [...]int{1, 2, 3, 4, 5}

// extBuckets is the file-extension bucket order (mirror features.py EXT_BUCKETS).
var extBuckets = [...]string{"code", "config", "docs", "data", "other", "none"}

var (
	extCode   = set("go", "ts", "tsx", "js", "jsx", "py", "rs", "java", "kt", "c", "cc", "cpp", "h", "hpp", "rb", "php", "cs", "swift", "scala", "sql", "vue", "svelte")
	extConfig = set("json", "yaml", "yml", "toml", "ini", "env", "cfg", "conf", "lock", "xml")
	extDocs   = set("md", "mdx", "txt", "rst", "adoc")
	extData   = set("csv", "tsv", "parquet", "db", "sqlite")
)

// FeatureCount is the markov feature-vector length (must equal len(FeatureNames())).
const FeatureCount = 80

// Call is the current request's routing-time signals the head reads at decision
// time (the response — hence the derived action — is not yet known).
type Call struct {
	TurnType             string // "main_loop" | "tool_result" | ...
	StepIdx              int
	Ts                   int64 // unix seconds
	EstimatedInputTokens int
	LastIsError          bool
	LastFileExt          string // extension without dot, e.g. "go"; "" -> "none" bucket
}

// State is the per-session action ring buffer: the compact trajectory memory the
// Markov features need. Persist one per session pin; update via Advance after each
// served call (from the response-derived action), and read via Features at the
// next call's decision time. The zero value is a valid empty session.
type State struct {
	hist        []Action // last lastK actions, oldest..newest
	histWide    []Action // last lastKWide actions
	cum         map[Action]int
	lastPos     map[Action]int // 1-indexed by prior-call count of each action's last use
	nPrior      int
	nToolResult int
	prevAction  Action
	streak      int
	outHist     []int // last 2 output-token counts
	outSum      float64
	firstTs     int64
	prevTs      int64
	hasPrevTs   bool
}

// NewState returns an empty session ring.
func NewState() *State {
	return &State{cum: map[Action]int{}, lastPos: map[Action]int{}}
}

// Advance folds a completed call's derived action + accounting into the ring so
// the next call's Features see it. Call this post-turn (the action is response-
// derived), mirroring features.py's per-row state advance.
func (s *State) Advance(action Action, ts int64, outputTokens int, isToolResult bool) {
	if s.cum == nil {
		s.cum = map[Action]int{}
	}
	if s.lastPos == nil {
		s.lastPos = map[Action]int{}
	}
	if s.nPrior == 0 {
		s.firstTs = ts
	}
	if s.nPrior > 0 && action == s.prevAction {
		s.streak++
	} else {
		s.streak = 1
	}
	s.prevAction = action
	s.hist = appendRing(s.hist, action, lastK)
	s.histWide = appendRing(s.histWide, action, lastKWide)
	s.cum[action]++
	s.nPrior++
	s.lastPos[action] = s.nPrior
	if isToolResult {
		s.nToolResult++
	}
	s.outHist = appendIntRing(s.outHist, outputTokens, 2)
	s.outSum += float64(outputTokens)
	s.prevTs = ts
	s.hasPrevTs = true
}

// Features builds the markov feature vector for the CURRENT call from the ring's
// prior state + the current request. Pure: does not mutate the ring.
func (s *State) Features(c Call) []float32 {
	out := make([]float32, 0, FeatureCount)
	put := func(v float64) { out = append(out, float32(v)) }

	// lag actions one-hot (1..5); recent[-lag] or the "none" slot.
	for _, lag := range lags {
		idx := noneIndex
		if len(s.hist) >= lag {
			idx = actionIndex[s.hist[len(s.hist)-lag]]
		}
		for k := 0; k <= noneIndex; k++ {
			put(b2f(k == idx))
		}
	}
	// last-k and wide-window histograms.
	for _, a := range ActionTypes {
		put(float64(countAction(s.hist, a)))
	}
	for _, a := range ActionTypes {
		put(float64(countAction(s.histWide, a)))
	}
	// cumulative fraction.
	for _, a := range ActionTypes {
		put(cumFrac(s.cum[a], s.nPrior))
	}
	// calls since each action last occurred (recency; capped, log1p).
	for _, a := range ActionTypes {
		dist := sinceCap
		if p, ok := s.lastPos[a]; ok {
			dist = s.nPrior + 1 - p
		}
		put(log1p(float64(minInt(dist, sinceCap))))
	}
	// streak / diversity / entropy / tool-heaviness.
	put(float64(s.streak))
	// distinct_actions: quirk-for-quirk parity with features.py. There `cum` is a
	// defaultdict, and the cumulative-fraction loop above reads cum[a] for every
	// action once n_prior>0, inserting all 6 keys — so len(cum) is 0 on the first
	// call and a constant 6 thereafter (an effectively-dead feature baked into the
	// trained model). Reproduced exactly so serving matches training.
	if s.nPrior == 0 {
		put(0.0)
	} else {
		put(float64(len(ActionTypes)))
	}
	put(s.entropy())
	put(cumFrac(s.nToolResult, s.nPrior))
	// turn type / position / time / context / prev sizes.
	isTR := c.TurnType == "tool_result"
	put(b2f(isTR))
	put(b2f(c.TurnType == "main_loop"))
	put(float64(c.StepIdx))
	// gap since prior call: -1 sentinel on the first call of a session.
	if s.hasPrevTs {
		gap := float64(c.Ts - s.prevTs)
		put(log1p(gap))
	} else {
		put(-1.0)
	}
	firstTs := s.firstTs
	if s.nPrior == 0 {
		firstTs = c.Ts // first call: session starts now -> time_since_start = 0
	}
	put(log1p(float64(c.Ts - firstTs)))
	longGap := s.hasPrevTs && float64(c.Ts-s.prevTs) > longGapSecs
	put(b2f(longGap))
	put(log1p(float64(c.EstimatedInputTokens)))
	put(prevOut(s.outHist, 1))
	put(prevOut(s.outHist, 2))
	if s.nPrior > 0 {
		put(log1p(s.outSum / float64(s.nPrior)))
	} else {
		put(0.0)
	}
	put(b2f(c.LastIsError))
	// file-extension one-hot.
	bucket := extBucket(c.LastFileExt)
	for _, b := range extBuckets {
		put(b2f(b == bucket))
	}
	return out
}

func (s *State) entropy() float64 {
	if s.nPrior == 0 {
		return 0.0
	}
	var e float64
	for _, a := range ActionTypes {
		n := s.cum[a]
		if n == 0 {
			continue
		}
		p := float64(n) / float64(s.nPrior)
		e -= p * math.Log(p)
	}
	return e
}

func extBucket(ext string) string {
	if ext == "" {
		return "none"
	}
	e := lower(ext)
	switch {
	case has(extCode, e):
		return "code"
	case has(extConfig, e):
		return "config"
	case has(extDocs, e):
		return "docs"
	case has(extData, e):
		return "data"
	default:
		return "other"
	}
}

// FeatureNames returns the feature column names in build order — used to
// cross-check the embedded metadata's markov_feature_names.
func FeatureNames() []string {
	names := make([]string, 0, FeatureCount)
	for _, lag := range lags {
		for k := 0; k <= noneIndex; k++ {
			names = append(names, fmt.Sprintf("lag%d_%d", lag, k))
		}
	}
	for _, a := range ActionTypes {
		names = append(names, "lastk_"+string(a))
	}
	for _, a := range ActionTypes {
		names = append(names, "lastk12_"+string(a))
	}
	for _, a := range ActionTypes {
		names = append(names, "cum_"+string(a))
	}
	for _, a := range ActionTypes {
		names = append(names, "since_"+string(a))
	}
	names = append(names,
		"streak", "distinct_actions", "action_entropy", "frac_tool_result",
		"is_tool_result", "is_main_loop", "step_idx", "time_since_last",
		"time_since_start", "is_long_gap", "log_ctx_tokens", "prev_out",
		"prev2_out", "mean_out", "last_is_error",
	)
	for _, b := range extBuckets {
		names = append(names, "ext_"+b)
	}
	return names
}

// --- small helpers ---

func appendRing(r []Action, v Action, cap int) []Action {
	r = append(r, v)
	if len(r) > cap {
		r = r[len(r)-cap:]
	}
	return r
}

func appendIntRing(r []int, v, cap int) []int {
	r = append(r, v)
	if len(r) > cap {
		r = r[len(r)-cap:]
	}
	return r
}

func countAction(r []Action, a Action) int {
	n := 0
	for _, x := range r {
		if x == a {
			n++
		}
	}
	return n
}

// prevOut returns log1p of the k-th-from-last output size (k=1 newest), or 0 when
// fewer than k prior calls exist.
func prevOut(r []int, k int) float64 {
	if len(r) < k {
		return 0.0
	}
	return log1p(float64(r[len(r)-k]))
}

func cumFrac(n, total int) float64 {
	if total <= 0 {
		return 0.0
	}
	return float64(n) / float64(total)
}

func log1p(x float64) float64 {
	if x < 0 {
		x = 0
	}
	return math.Log1p(x)
}

func b2f(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
