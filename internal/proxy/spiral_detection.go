package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

// Shadow-mode spiral detector: flags sessions death-marching (error grind,
// same-file thrash, fuzzy repetition, monologue) on their routed model.
// LOG ONLY — records one durable event per signal class per session, no
// routing change. Events are joined offline against outcomes to pick fire
// rate/precision/lead-time before any escalation is armed (~35% of failures
// have no behavioral tell per the offline trajectory audit, so live
// measurement decides the operating point, not benchmarks).
//
// Thresholds from an offline audit of 186 bake-off trajectories + 8.5k
// outcome shards: err streak AUC 0.73 pooled (0.86 on deepseek-v4-flash);
// same-file thrash (>=5 edits) present in 9/20 costliest death marches;
// repeat-window fraction >=~0.3 over last 12 calls: +8-11pp recall over a
// depth cap at matched FPR; monologue is industry-convergent (OpenHands)
// but unmeasured here — shadow mode measures it.
//
// Unlike the cyclic-loop detector (exact-signature, no-edit windows), these
// tolerate "rhyming" spirals — same file, slightly different args.
const (
	// Arming floor: no signal evaluated before this many tool calls exist.
	// Early-session intervention is the documented false-positive mode.
	spiralMinToolCalls = 12
	// spiralErrStreakThreshold: consecutive errored tool_results at the tail.
	spiralErrStreakThreshold = 3
	// spiralSameFileEditThreshold: edits targeting one file path.
	spiralSameFileEditThreshold = 5
	// Repetition: fraction of the last spiralRepeatWindow signatures that
	// are duplicates, evaluated once spiralRepeatMinCalls is reached.
	spiralRepeatWindow        = 12
	spiralRepeatMinCalls      = 20
	spiralRepeatFracThreshold = 0.34
	// Consecutive assistant messages with no tool activity since the last
	// real user input. Set above the text-only nudge threshold to avoid
	// double-reporting.
	spiralMonologueThreshold = 4
)

// Spiral signal-class taxonomy. One event per (session, role, reason).
const (
	spiralReasonErrStreak      = "err_streak"
	spiralReasonSameFileThrash = "same_file_thrash"
	spiralReasonRepetition     = "repetition"
	spiralReasonMonologue      = "monologue"
)

// SpiralShadowStore persists shadow spiral detections (router.spiral_shadow_events).
// CountSpiralShadowEvents enforces the once-per-(session, reason) budget across replicas.
type SpiralShadowStore interface {
	InsertSpiralShadowEvent(ctx context.Context, p SpiralShadowEvent) error
	CountSpiralShadowEvents(ctx context.Context, sessionKey []byte, role, reason string) (count int64, err error)
}

// SpiralShadowEvent mirrors one router.spiral_shadow_events row. All signal
// values are recorded regardless of which reason fired, so thresholds can
// be re-tuned offline without re-running traffic.
type SpiralShadowEvent struct {
	InstallationID   string
	SessionKey       []byte
	Role             string
	RoutedModel      string
	TurnType         string
	Reason           string
	ErrStreak        int32
	ErroredResults   int32
	ToolResults      int32
	MaxSameFileEdits int32
	SameFilePathHash string
	RepeatFrac       float64
	MonologueLen     int32
	ToolCallCount    int32
	MessageCount     int32
}

// spiralSignals is the per-turn signal snapshot, a pure function of the
// request body (computeSpiralSignals).
type spiralSignals struct {
	errStats         translate.ToolResultErrorStats
	maxSameFileEdits int
	sameFilePathHash string
	repeatFrac       float64
	monologueLen     int
	toolCallCount    int
	messageCount     int
}

// computeSpiralSignals derives the spiral signal snapshot from the inbound
// envelope. Pure and stateless — the full history arrives on every turn.
func computeSpiralSignals(env *translate.RequestEnvelope, messageCount int) spiralSignals {
	sigs := env.AssistantToolCallSignatures()
	s := spiralSignals{
		errStats:      env.ToolResultErrors(),
		monologueLen:  env.TrailingAssistantMonologue(),
		toolCallCount: len(sigs),
		messageCount:  messageCount,
	}

	// Path recorded as a hash only — confirms "same file" offline without
	// persisting customer file names.
	pathCounts := make(map[string]int)
	for _, fp := range env.AssistantToolCallFilePaths() {
		if _, isEdit := editToolNames[fp.Name]; !isEdit {
			continue
		}
		pathCounts[fp.Path]++
		if pathCounts[fp.Path] > s.maxSameFileEdits {
			s.maxSameFileEdits = pathCounts[fp.Path]
			h := sha256.Sum256([]byte(fp.Path))
			s.sameFilePathHash = hex.EncodeToString(h[:8])
		}
	}

	// Catches rhyming re-grind that the exact tight-loop detector's
	// 5-identical bar misses.
	if len(sigs) >= spiralRepeatWindow {
		window := sigs[len(sigs)-spiralRepeatWindow:]
		counts := make(map[string]int, len(window))
		for _, sig := range window {
			counts[sig.Name+"\x00"+sig.InputHash]++
		}
		repeated := 0
		for _, c := range counts {
			if c > 1 {
				repeated += c
			}
		}
		s.repeatFrac = float64(repeated) / float64(len(window))
	}
	return s
}

// spiralReasons returns the signal classes whose thresholds the snapshot
// crosses. Empty below the arming floor.
func spiralReasons(s spiralSignals) []string {
	if s.toolCallCount < spiralMinToolCalls {
		return nil
	}
	var reasons []string
	if s.errStats.TrailingErrStreak >= spiralErrStreakThreshold {
		reasons = append(reasons, spiralReasonErrStreak)
	}
	if s.maxSameFileEdits >= spiralSameFileEditThreshold {
		reasons = append(reasons, spiralReasonSameFileThrash)
	}
	if s.toolCallCount >= spiralRepeatMinCalls && s.repeatFrac >= spiralRepeatFracThreshold {
		reasons = append(reasons, spiralReasonRepetition)
	}
	if s.monologueLen >= spiralMonologueThreshold {
		reasons = append(reasons, spiralReasonMonologue)
	}
	return reasons
}

// spiralFiredCache de-dupes shadow fires per (session, role, reason) per
// replica so the durable budget query runs once per ~hour, not every turn.
// Cross-replica dupes are still possible before the durable count lands;
// offline analysis de-dupes by (session_key, reason).
const (
	spiralFiredCacheSize = 8192
	spiralFiredCacheTTL  = time.Hour
)

type spiralTracker struct {
	fired *lru.LRU[string, struct{}]
}

func newSpiralTracker() *spiralTracker {
	return &spiralTracker{
		fired: lru.NewLRU[string, struct{}](spiralFiredCacheSize, nil, spiralFiredCacheTTL),
	}
}

func spiralFiredKey(sessionKey [sessionpin.SessionKeyLen]byte, role, reason string) string {
	return string(sessionKey[:]) + "\x00" + role + "\x00" + reason
}

// handleSpiralShadow records one durable event + one log line per
// (session, role, reason). Takes no routing action. Gated upstream by
// spiralShadowEnabled and the turn-type guard.
func (s *Service) handleSpiralShadow(
	ctx context.Context,
	sig spiralSignals,
	reasons []string,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
	role string,
	routedModel string,
	turnType string,
) {
	log := observability.FromContext(ctx)
	for _, reason := range reasons {
		key := spiralFiredKey(sessionKey, role, reason)
		if _, seen := s.spiralTracker.fired.Get(key); seen {
			continue
		}

		// Best-effort: a lookup failure proceeds — an extra row beats a
		// lost one in shadow mode.
		if s.spiralShadowStore != nil && installationID != uuid.Nil {
			count, err := s.spiralShadowStore.CountSpiralShadowEvents(ctx, sessionKey[:], role, reason)
			if err != nil {
				log.Error("spiral-shadow: budget lookup failed", "err", err)
			} else if count > 0 {
				s.spiralTracker.fired.Add(key, struct{}{})
				continue
			}
		}

		log.Info("router.spiral_shadow",
			"reason", reason,
			"routed_model", routedModel,
			"turn_type", turnType,
			"err_streak", sig.errStats.TrailingErrStreak,
			"errored_results", sig.errStats.Errored,
			"tool_results", sig.errStats.Total,
			"max_same_file_edits", sig.maxSameFileEdits,
			"same_file_path_hash", sig.sameFilePathHash,
			"repeat_frac", sig.repeatFrac,
			"monologue_len", sig.monologueLen,
			"tool_call_count", sig.toolCallCount,
			"message_count", sig.messageCount,
			"session_key_prefix", shortSessionKey(sessionKey),
			"role", role,
		)

		if s.spiralShadowStore != nil && installationID != uuid.Nil {
			event := SpiralShadowEvent{
				InstallationID:   installationID.String(),
				SessionKey:       sessionKey[:],
				Role:             role,
				RoutedModel:      routedModel,
				TurnType:         turnType,
				Reason:           reason,
				ErrStreak:        int32(sig.errStats.TrailingErrStreak),
				ErroredResults:   int32(sig.errStats.Errored),
				ToolResults:      int32(sig.errStats.Total),
				MaxSameFileEdits: int32(sig.maxSameFileEdits),
				SameFilePathHash: sig.sameFilePathHash,
				RepeatFrac:       sig.repeatFrac,
				MonologueLen:     int32(sig.monologueLen),
				ToolCallCount:    int32(sig.toolCallCount),
				MessageCount:     int32(sig.messageCount),
			}
			// context.Background(): the request ctx may already be canceled;
			// losing the row would skew the shadow fire-rate corpus.
			if err := s.spiralShadowStore.InsertSpiralShadowEvent(context.Background(), event); err != nil {
				log.Error("spiral-shadow: event insert failed", "err", err)
				continue // leave the LRU unset so the next turn retries
			}
		}
		s.spiralTracker.fired.Add(key, struct{}{})
	}
}
