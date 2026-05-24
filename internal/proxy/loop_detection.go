package proxy

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

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

// handleToolCallLoopBreak short-circuits a runaway tool-call loop. It writes a
// synthetic end_turn response in the inbound wire format and expires the
// session pin so the next user turn re-routes to a fresh decision rather than
// re-anchoring on the model that produced the loop.
//
// Pin expiry is best-effort: a Postgres write failure logs but does not block
// the response, because the client is already stuck and getting a clean break
// out is more important than guaranteeing the pin row reflects the new state.
func (s *Service) handleToolCallLoopBreak(
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
	log := observability.Get()

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
		"session_key_hex", fmt.Sprintf("%x", sessionKey),
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
	if s.pinCache != nil {
		s.pinCache.Remove(sessionPinCacheKey(sessionKey, role))
	}

	switch env.SourceFormat() {
	case translate.FormatOpenAI:
		return writeSyntheticOpenAIResponse(w, env, msg)
	default:
		return writeSyntheticAnthropicResponse(w, env, msg)
	}
}
