package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

// escalateModel is the strong model a looping cheap/mid session is rescued onto.
const escalateModel = "claude-opus-4-8"

// LoopEscalationStore persists cyclic-loop detections (one row per
// session+role); CountLoopEscalationEvents enforces the once-per-session budget.
type LoopEscalationStore interface {
	InsertLoopEscalationEvent(ctx context.Context, p LoopEscalationEvent) error
	CountLoopEscalationEvents(ctx context.Context, sessionKey []byte, role string) (count int64, err error)
}

// LoopEscalationEvent mirrors one router.loop_escalation_events row.
type LoopEscalationEvent struct {
	InstallationID   string
	SessionKey       []byte
	Role             string
	LoopingModel     string
	Action           string
	EscalationTarget string
	LoopTool         string
	LoopInputHash    string
	RepeatCount      int32
	DistinctRatio    float64
	WindowSize       int32
}

// Loop-escalation action taxonomy, recorded per event. Exactly one applies.
const (
	// loopActionEscalated: the session was pinned to escalateModel.
	loopActionEscalated = "escalated"
	// loopActionHoldout: log-not-act bucket — loop detected but not escalated,
	// so the ~43% self-recovery rate can be subtracted from rescue claims.
	loopActionHoldout = "holdout"
	// loopActionAlreadyStrong: the looping model IS the escalation target — a
	// genuinely hard task, not a misroute. Record-only training signal.
	loopActionAlreadyStrong = "already_strong"
	// loopActionUserForced: a /force-model (or x-weave-force-model) pin
	// outranks auto-escalation; the forced pin is left in place.
	loopActionUserForced = "user_forced"
	// loopActionDisabled: the ROUTER_LOOP_ESCALATION_ENABLED kill switch is
	// off. Detection and telemetry continue; the pin write does not.
	loopActionDisabled = "disabled"
)

// inLoopEscalationHoldout deterministically buckets a session using the
// session key (already uniform sha256), so the bucket is stable across replicas/retries.
func inLoopEscalationHoldout(sessionKey [sessionpin.SessionKeyLen]byte, pct int) bool {
	if pct <= 0 {
		return false
	}
	if pct >= 100 {
		return true
	}
	return int(binary.BigEndian.Uint32(sessionKey[0:4])%100) < pct
}

// Loop-detection knobs: MaxRepeats identical (name+args) calls within the
// last Window calls trips the break. Mirrors charmbracelet/crush's detector —
// tolerates legit retries but catches qwen3's repeat-call failure mode fast.
const (
	loopDetectionWindowSize = 10
	loopDetectionMaxRepeats = 5
)

// detectToolCallLoop reports whether the same (tool_name, args) signature
// repeats loopDetectionMaxRepeats+ times within the last loopDetectionWindowSize
// tool calls, returning the signature and count for logs/the stop message.
func detectToolCallLoop(env *translate.RequestEnvelope) (looped bool, sig translate.ToolCallSig, count int) {
	sigs := env.AssistantToolCallSignatures()
	if len(sigs) < loopDetectionMaxRepeats {
		return false, translate.ToolCallSig{}, 0
	}
	start := 0
	if len(sigs) > loopDetectionWindowSize {
		start = len(sigs) - loopDetectionWindowSize
	}
	window := sigs[start:]
	counts := make(map[string]int, len(window))
	keys := make(map[string]translate.ToolCallSig, len(window))
	for _, s := range window {
		key := s.Name + "\x00" + s.InputHash
		counts[key]++
		keys[key] = s
		if counts[key] >= loopDetectionMaxRepeats {
			// Dump the ordered window to distinguish a real loop (identical
			// args) from a false positive (distinct args that canonicalize-collide).
			log := observability.Get()
			args := env.AssistantToolCallArgsPreview(start, 200)
			log.Info("loop detector window dump",
				"tool_name", s.Name,
				"input_hash", s.InputHash,
				"window_args", args,
			)
			return true, s, counts[key]
		}
	}
	return false, translate.ToolCallSig{}, 0
}

// Cyclic-loop-detection knobs. detectToolCallLoop catches a TIGHT loop (one
// call >=5x in 10); this catches a WIDER cycle — re-reading a few files for
// dozens of turns (seen post-#332: gpt-5.5/haiku re-read x45+ over 400 turns).
const (
	cyclicLoopWindowSize       = 30
	cyclicLoopMinCalls         = 24
	cyclicLoopMaxDistinctRatio = 0.4
)

// editToolNames are tool calls that constitute real progress; their presence in
// the window means the agent is changing the repo, not stuck re-reading.
var editToolNames = map[string]struct{}{
	"Edit": {}, "Write": {}, "MultiEdit": {}, "NotebookEdit": {},
}

// detectCyclicToolCallLoop reports a wide re-read cycle: cyclicLoopMinCalls+
// calls with distinct-signature ratio below cyclicLoopMaxDistinctRatio and no
// edit/write call in the window (the #271 false-positive guard — a healthy
// Explore reads many distinct files; a stuck agent re-reads the same few).
func detectCyclicToolCallLoop(env *translate.RequestEnvelope) (looped bool, top translate.ToolCallSig, topCount int, distinctRatio float64, total int) {
	sigs := env.AssistantToolCallSignatures()
	if len(sigs) < cyclicLoopMinCalls {
		return false, translate.ToolCallSig{}, 0, 0, 0
	}
	start := 0
	if len(sigs) > cyclicLoopWindowSize {
		start = len(sigs) - cyclicLoopWindowSize
	}
	window := sigs[start:]
	counts := make(map[string]int, len(window))
	keys := make(map[string]translate.ToolCallSig, len(window))
	for _, s := range window {
		if _, isEdit := editToolNames[s.Name]; isEdit {
			// Real progress in the window — not a stuck loop.
			return false, translate.ToolCallSig{}, 0, 0, len(window)
		}
		key := s.Name + "\x00" + s.InputHash
		counts[key]++
		keys[key] = s
	}
	distinctRatio = float64(len(counts)) / float64(len(window))
	if distinctRatio >= cyclicLoopMaxDistinctRatio {
		return false, translate.ToolCallSig{}, 0, distinctRatio, len(window)
	}
	for k, c := range counts {
		if c > topCount {
			topCount, top = c, keys[k]
		}
	}
	return true, top, topCount, distinctRatio, len(window)
}

// handleLoopEscalation pins a session stuck in a wide tool-call cycle to opus
// and records a telemetry event; it writes no response, so normal routing
// picks up the pin and dispatches this turn. Idempotent via the pin check plus
// a durable once-per-session budget. The pin write is further gated by the
// kill switch, the log-not-act holdout, a user-forced pin, or the looping
// model already being the target — those cases still record the event
// (action column says which) but withhold the rescue.
func (s *Service) handleLoopEscalation(
	ctx context.Context,
	top translate.ToolCallSig,
	topCount int,
	distinctRatio float64,
	window int,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
	role string,
	routedModel string,
) {
	log := observability.FromContext(ctx)

	loopingModel := routedModel
	userForced := false
	if s.pinStore != nil && installationID != uuid.Nil {
		existing, found, err := s.pinStore.Get(ctx, sessionKey, role)
		if err != nil {
			log.Error("loop-escalation: prior pin lookup failed", "err", err)
		} else if found {
			if existing.Reason == translate.ReasonLoopEscalation {
				return // already rescued this session; don't re-pin or double-log
			}
			// A user's explicit /force-model choice outranks auto-escalation —
			// record the loop for telemetry but leave the forced pin in place.
			if existing.Reason == translate.ReasonUserForceModel {
				userForced = true
			}
			if existing.Model != "" {
				loopingModel = existing.Model
			}
		}
	}

	// Once-per-session budget, durable past pin TTL expiry: covers sessions
	// outliving their pin, and non-escalating actions that never write a pin
	// (else they'd emit one event per turn). Lookup failure proceeds (best-effort).
	if s.loopEscalationStore != nil && installationID != uuid.Nil {
		count, err := s.loopEscalationStore.CountLoopEscalationEvents(ctx, sessionKey[:], role)
		if err != nil {
			log.Error("loop-escalation: budget lookup failed", "err", err)
		} else if count > 0 {
			return // this session already fired its one escalation event
		}
	}

	// Holdout only applies when the event can be recorded (wired store + real
	// installation id) — otherwise withholding the rescue is pure loss, not measurement.
	holdout := s.loopEscalationStore != nil && installationID != uuid.Nil &&
		inLoopEscalationHoldout(sessionKey, s.loopEscalationHoldoutPct)

	action := loopActionEscalated
	switch {
	case !s.loopEscalationEnabled:
		action = loopActionDisabled
	case userForced:
		action = loopActionUserForced
	case loopingModel == escalateModel:
		action = loopActionAlreadyStrong
	case holdout:
		action = loopActionHoldout
	}
	willEscalate := action == loopActionEscalated

	// This (session, looping_model) event is a training label for the
	// difficulty/routing model; joined offline by session_key against the final shard result.
	log.Info("router.loop_escalation",
		"looping_model", loopingModel,
		"action", action,
		"escalated", willEscalate,
		"user_forced", userForced,
		"escalation_target", escalateModel,
		"loop_tool", top.Name,
		"loop_input_hash", top.InputHash,
		"repeat_count", topCount,
		"distinct_ratio", distinctRatio,
		"window_size", window,
		"session_key_prefix", shortSessionKey(sessionKey),
		"role", role,
	)

	// Rescue first, record second: the durable row backs the once-per-session
	// budget, so recording before the pin lands would permanently block retry
	// on a failed rescue. On upsert failure, return without a row so the loop re-detects next turn.
	if willEscalate {
		// Pin opus for the rest of the session (immutable sticky via
		// ReasonLoopEscalation).
		if s.pinStore == nil || installationID == uuid.Nil {
			return
		}
		var lastServed string
		if existing, found, err := s.pinStore.Get(ctx, sessionKey, role); err == nil && found {
			lastServed = existing.LastServedModel
		}
		pin := sessionpin.Pin{
			SessionKey:      sessionKey,
			Role:            role,
			InstallationID:  installationID,
			Provider:        providers.ProviderAnthropic,
			Model:           escalateModel,
			Reason:          translate.ReasonLoopEscalation,
			TurnCount:       1,
			PinnedUntil:     time.Now().Add(pinSessionTTL),
			LastServedModel: lastServed,
		}
		// context.Background(): the request ctx may already be canceled by the
		// time this runs; the pin write must still land or the next turn re-loops.
		if err := s.pinStore.Upsert(context.Background(), pin); err != nil {
			log.Error("loop-escalation: pin upsert failed", "err", err)
			return
		}
	}

	// Durable row for fire-rate/opus-share metrics and the training corpus.
	// context.Background(): request ctx may be canceled; losing the row would
	// skew the corpus and break the once-per-session budget (pin check still dedupes re-fires meanwhile).
	if s.loopEscalationStore != nil && installationID != uuid.Nil {
		event := LoopEscalationEvent{
			InstallationID:   installationID.String(),
			SessionKey:       sessionKey[:],
			Role:             role,
			LoopingModel:     loopingModel,
			Action:           action,
			EscalationTarget: escalateModel,
			LoopTool:         top.Name,
			LoopInputHash:    top.InputHash,
			RepeatCount:      int32(topCount),
			DistinctRatio:    distinctRatio,
			WindowSize:       int32(window),
		}
		if err := s.loopEscalationStore.InsertLoopEscalationEvent(context.Background(), event); err != nil {
			log.Error("loop-escalation: event insert failed", "err", err)
		}
	}
}

// handleToolCallLoopBreak short-circuits a runaway tool-call loop: writes a
// synthetic end_turn response and expires the session pin so the next turn
// re-routes instead of re-anchoring on the looping model. Pin expiry is
// best-effort — a write failure logs but doesn't block the response.
func (s *Service) handleToolCallLoopBreak(
	ctx context.Context,
	w http.ResponseWriter,
	env *translate.RequestEnvelope,
	sig translate.ToolCallSig,
	count int,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
	role string,
	decisionModel string,
	decisionProvider string,
	inputTokens int,
) error {
	log := observability.FromContext(ctx)

	msg := fmt.Sprintf(
		"✦ **Weave Router** → Tool-call loop detected: `%s` was called %d times in the last %d turns with identical arguments. Stopping this turn and clearing the session pin so the next message re-routes to a fresh model.\n\nIf the task is genuinely incomplete, send a follow-up message describing what's still needed; the router will pick a different model.\n\n",
		sig.Name, count, loopDetectionWindowSize,
	)
	if env.SourceFormat() == translate.FormatOpenAI {
		msg = fmt.Sprintf(
			"Weave Router: tool-call loop detected (%s called %d times with identical args). Stopping and clearing the session pin. Send a follow-up message to resume; the router will pick a different model.",
			sig.Name, count,
		)
	}

	log.Info("Tool-call loop detected; breaking turn",
		"tool_name", sig.Name,
		"repeat_count", count,
		"window_size", loopDetectionWindowSize,
		"decision_model", decisionModel,
		"decision_provider", decisionProvider,
		"session_key_prefix", shortSessionKey(sessionKey),
		"role", role,
	)

	// Expire the pin in Postgres (not just the in-proc cache) so a racing
	// reader on another pod can't repopulate the LRU from the stale row.
	if s.pinStore != nil && installationID != uuid.Nil {
		if err := s.expireSessionPin(ctx, installationID, sessionKey, role, "tool_call_loop_break"); err != nil {
			log.Error("loop-break: pin store upsert failed", "err", err)
		}
	}

	switch env.SourceFormat() {
	case translate.FormatOpenAI:
		return writeSyntheticOpenAIResponse(w, env, msg, inputTokens)
	default:
		return writeSyntheticAnthropicResponse(w, env, msg, inputTokens)
	}
}
