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

// LoopEscalationStore persists cyclic-loop detections durably (the
// router.loop_escalation_events table). One row per (session, role) detection;
// the count query enforces the once-per-session budget across pin TTL expiry.
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
	// loopActionHoldout: log-not-act measurement bucket — the loop was
	// detected but deliberately NOT escalated, so the self-recovery rate
	// (sessions that un-stick on their own; measured at 43% in the agent-flow
	// session-dynamics analysis) can be subtracted from any rescue claim.
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

// inLoopEscalationHoldout deterministically buckets a session into the
// log-not-act holdout. Keyed on the session key (already a sha256-derived
// digest, so uniformly distributed) rather than a random draw, so the same
// session always lands in the same bucket across replicas and retries.
func inLoopEscalationHoldout(sessionKey [sessionpin.SessionKeyLen]byte, pct int) bool {
	if pct <= 0 {
		return false
	}
	if pct >= 100 {
		return true
	}
	return int(binary.BigEndian.Uint32(sessionKey[0:4])%100) < pct
}

// Loop-detection knobs. Window is the number of recent tool calls inspected;
// MaxRepeats is the count of identical (name+args) calls within the window
// that trips the break. Values mirror charmbracelet/crush's loop detector:
// large enough to absorb legitimate retries of a tool that returns the same
// result twice, small enough to catch the qwen3 "same call N times in a row"
// failure mode within a few seconds rather than minutes.
const (
	loopDetectionWindowSize = 10
	loopDetectionMaxRepeats = 5
)

// detectToolCallLoop scans the trailing tool-call signatures in the assistant's
// message history and reports whether the same (tool_name, args) signature
// appears at least loopDetectionMaxRepeats times within the most recent
// loopDetectionWindowSize entries.
//
// Returns the looping signature and its count so the caller can surface them
// in logs and the synthetic stop message.
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
			// Dump the full ordered window so we can tell a real loop
			// (5× identical args) from a false positive (5 distinct
			// args that canonicalize-collide). On a true loop every
			// printed entry will match; on a false positive they'll
			// look different in this log even though they share a hash.
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

// Cyclic-loop-detection knobs. Where detectToolCallLoop catches a TIGHT loop
// (the same single call >=5x in the last 10), this catches a WIDER cycle: an
// agent re-reading the same small set of files over and over across dozens of
// turns (observed in the post-#332 re-bake — gpt-5.5/haiku reading defaults.ini
// x45, package.json x51 across 400 turns, never editing, ending error_max_turns).
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

// detectCyclicToolCallLoop reports whether the trailing tool-call window is a
// wide re-read cycle: at least cyclicLoopMinCalls calls whose distinct
// (name, args) fraction is below cyclicLoopMaxDistinctRatio, with no edit/write
// call in the window (the no-progress guard). A healthy Explore sub-agent reads
// MANY DISTINCT files (high diversity → no trip); a stuck agent re-reads the
// SAME few (low diversity → trip) — the #271 false-positive guard applied to the
// wide-cycle case. Returns the most-repeated signature + count, the distinct
// ratio, and the window size, for telemetry.
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

// handleLoopEscalation rescues a session stuck in a wide tool-call cycle by
// pinning it to opus for the remainder of its life, and records a structured
// telemetry event. It does NOT write a synthetic response — the caller falls
// through to normal routing, which loads the just-written escalation pin (an
// immutable sticky, like /force-model) and dispatches this turn to opus.
//
// Idempotent: a session already on a loop_escalation pin is left alone, and a
// durable once-per-session budget (CountLoopEscalationEvents) backstops the pin
// check across TTL expiry, so the telemetry fires once per session. The pin
// write is further gated by the kill switch (loopEscalationEnabled), the
// log-not-act holdout (loopEscalationHoldoutPct), a user-forced pin, and the
// looping model already being the escalation target — in every one of those
// cases the event is still recorded (action column says which), only the
// rescue is withheld.
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
			// A user's explicit /force-model (or x-weave-force-model) choice
			// outranks auto-escalation — never silently overwrite it. Record the
			// loop for telemetry, but leave the forced pin in place.
			if existing.Reason == translate.ReasonUserForceModel {
				userForced = true
			}
			if existing.Model != "" {
				loopingModel = existing.Model
			}
		}
	}

	// Once-per-session budget, durable across pin TTL expiry. The
	// ReasonLoopEscalation pin check above covers the common case, but a
	// session outliving its pin (1h sliding TTL) would otherwise re-fire — and
	// the non-escalating actions (holdout/disabled/already-strong) never write
	// a pin at all, so without this check they would emit one event per turn.
	// Best-effort: a lookup failure proceeds rather than suppressing a rescue.
	if s.loopEscalationStore != nil && installationID != uuid.Nil {
		count, err := s.loopEscalationStore.CountLoopEscalationEvents(ctx, sessionKey[:], role)
		if err != nil {
			log.Error("loop-escalation: budget lookup failed", "err", err)
		} else if count > 0 {
			return // this session already fired its one escalation event
		}
	}

	// Holdout only applies when the event can be recorded — withholding the
	// rescue without a durable row would be pure loss, not a measurement.
	holdout := s.loopEscalationStore != nil && inLoopEscalationHoldout(sessionKey, s.loopEscalationHoldoutPct)

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

	// First-class telemetry. This (session, looping_model) → looped event is the
	// exact misroute the embedder cannot predict up front, so it is a training
	// label for the difficulty/routing model. Emit for prod AND eval traffic; the
	// post-escalation outcome is joined offline by session_key against the final
	// shard result.
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

	// Durable row in router.loop_escalation_events — the queryable fire-rate /
	// opus-share / rescue-rate source and the training corpus.
	// context.Background(): the request ctx may already be canceled; losing the
	// row would both skew the corpus and break the once-per-session budget.
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

	if !willEscalate {
		return // disabled / user-forced / already-strong / holdout — record-only
	}

	// Pin opus for the rest of the session (immutable sticky via ReasonLoopEscalation).
	// context.Background(): the request ctx may already be canceled by the time
	// this runs; the pin write must still land or the next turn re-loops.
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
	if err := s.pinStore.Upsert(context.Background(), pin); err != nil {
		log.Error("loop-escalation: pin upsert failed", "err", err)
	}
}

// handleToolCallLoopBreak short-circuits a runaway tool-call loop. It writes a
// synthetic end_turn response in the inbound wire format and expires the
// session pin so the next user turn re-routes to a fresh decision rather than
// re-anchoring on the model that produced the loop.
//
// Pin expiry is best-effort: a Postgres write failure logs but does not block
// the response, because the client is already stuck and getting a clean break
// out is more important than guaranteeing the pin row reflects the new state.
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
) error {
	log := observability.FromContext(ctx)

	msg := fmt.Sprintf(
		"✦ **Weave Router** → tool-call loop detected: `%s` was called %d times in the last %d turns with identical arguments. Stopping this turn and clearing the session pin so the next message re-routes to a fresh model.\n\nIf the task is genuinely incomplete, send a follow-up message describing what's still needed; the router will pick a different model.\n\n",
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

	// Expire the session pin so the NEXT user turn re-routes. We cannot
	// simply Remove() the in-proc cache without also pushing an expired row
	// to Postgres — a racing reader on another pod would repopulate the LRU
	// from the stale row and re-anchor on the bad model.
	if s.pinStore != nil && installationID != uuid.Nil {
		expired := sessionpin.Pin{
			SessionKey:     sessionKey,
			Role:           role,
			InstallationID: installationID,
			Provider:       "",
			Model:          "",
			Reason:         "tool_call_loop_break",
			TurnCount:      1,
			PinnedUntil:    time.Now().Add(-time.Second),
		}
		// context.Background() — the request ctx may already be canceled by
		// the time we get here (Claude Code drops slow turns). Upserting on
		// a canceled context would silently fail and leave the bad pin.
		if err := s.pinStore.Upsert(context.Background(), expired); err != nil {
			log.Error("loop-break: pin store upsert failed", "err", err)
		}
	}

	switch env.SourceFormat() {
	case translate.FormatOpenAI:
		return writeSyntheticOpenAIResponse(w, env, msg)
	default:
		return writeSyntheticAnthropicResponse(w, env, msg)
	}
}
