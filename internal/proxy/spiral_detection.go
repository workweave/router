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

// Shadow-mode spiral detector: per-turn signals that a session is
// death-marching (error grind, same-file thrash, fuzzy action repetition,
// monologue) on the model it is routed to. Shadow mode means LOG ONLY — the
// detector records a durable event the first time each signal class fires
// for a session and changes nothing about routing. The events are joined
// offline by session_key against telemetry/session outcomes to measure fire
// rates, precision, and lead time on real traffic before any escalation
// action is armed (see docs/plans/ — the offline trajectory audit found
// ~35% of failures have no behavioral tell, so live measurement, not
// benchmarks, decides the operating point).
//
// Signal provenance (offline audit of 186 full bake-off trajectories +
// 8.5k outcome shards):
//   - trailing tool_result error streak: AUC 0.73 pooled deep-session,
//     0.86 on deepseek-v4-flash;
//   - same-file edit thrash (>=5 edits to one path): present in 9 of the 20
//     costliest death marches;
//   - recent-window action repetition (repeat fraction >=~0.3 in the last 12
//     calls at >=20 total): +8-11pp recall over a depth cap at matched FPR;
//   - monologue: industry-convergent (OpenHands stuck detector), unmeasured
//     on our traffic — shadow mode is how it gets measured.
//
// Unlike the cyclic-loop detector (exact-signature, no-edit windows), these
// signals tolerate "rhyming" spirals: the same file edited with slightly
// different args, test reruns with tiny variations.
const (
	// spiralMinToolCalls is the arming floor: no signal is evaluated before
	// this many assistant tool calls exist in the history. Early-session
	// intervention is the documented false-positive mode (the Explore
	// sub-agent lesson generalizes).
	spiralMinToolCalls = 12
	// spiralErrStreakThreshold: consecutive errored tool_results at the tail.
	spiralErrStreakThreshold = 3
	// spiralSameFileEditThreshold: edits targeting one file path.
	spiralSameFileEditThreshold = 5
	// spiralRepeatWindow / spiralRepeatMinCalls / spiralRepeatFracThreshold:
	// fraction of the last spiralRepeatWindow tool-call signatures that are
	// duplicates within that window, evaluated only once the session has
	// spiralRepeatMinCalls total calls.
	spiralRepeatWindow        = 12
	spiralRepeatMinCalls      = 20
	spiralRepeatFracThreshold = 0.34
	// spiralMonologueThreshold: consecutive assistant messages with no real
	// tool activity since the last real user input. Set above the text-only
	// nudge machinery's territory so the two don't double-report.
	spiralMonologueThreshold = 4
)

// Spiral signal-class taxonomy, recorded per event. One event per
// (session, role, reason) — a session that error-grinds at turn 20 and
// same-file-thrashes at turn 50 records two events.
const (
	spiralReasonErrStreak      = "err_streak"
	spiralReasonSameFileThrash = "same_file_thrash"
	spiralReasonRepetition     = "repetition"
	spiralReasonMonologue      = "monologue"
)

// SpiralShadowStore persists shadow-mode spiral detections durably (the
// router.spiral_shadow_events table). The count query enforces the
// once-per-(session, reason) budget across replicas.
type SpiralShadowStore interface {
	InsertSpiralShadowEvent(ctx context.Context, p SpiralShadowEvent) error
	CountSpiralShadowEvents(ctx context.Context, sessionKey []byte, role, reason string) (count int64, err error)
}

// SpiralShadowEvent mirrors one router.spiral_shadow_events row. All signal
// values are recorded on every event regardless of which reason fired, so
// thresholds can be re-tuned offline without re-running traffic.
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

	// Same-file edit thrash: max number of edit-class calls targeting one
	// path. The path itself is recorded only as a hash — enough to confirm
	// "the same file" offline without persisting customer file names.
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

	// Recent-window repetition: fraction of the last spiralRepeatWindow
	// signatures that have a duplicate within that window. Catches rhyming
	// re-grind that the exact tight-loop detector's 5-identical bar misses.
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

// spiralFiredCache de-duplicates shadow fires per (session, role, reason) on
// this replica, so the durable budget query only runs on the first crossing
// each ~hour rather than on every turn of a stuck session. Cross-replica
// duplicates are still possible in the gap before the durable count lands;
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

// handleSpiralShadow records shadow-mode spiral detections: one durable
// event + one structured log line per (session, role, reason). It takes NO
// routing action — that is the point of shadow mode. Gated upstream by
// spiralShadowEnabled and the turn-type guard (main-loop/tool-result turns
// only).
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

		// Durable once-per-(session, reason) budget, mirroring the
		// loop-escalation budget pattern. Best-effort: a lookup failure
		// proceeds (an extra row beats a lost one in shadow mode).
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
