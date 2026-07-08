package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

// Enforcing assistant-text repetition detector.
//
// Existing detectors key on stagnation evidence (identical tool signatures,
// errored results, no tool activity). Fresh tool calls each turn advance the
// no-progress fingerprint while the assistant narrates the same intent
// verbatim — the repeated text is the only durable tell.
//
// Scans narration since the last real user turn; fires when a normalized text
// of ≥textRepetitionMinLen chars recurs ≥textRepetitionThreshold times within
// textRepetitionWindow messages. Gated by ROUTER_TEXT_REPETITION_BREAK_ENABLED.
const (
	// textRepetitionWindow bounds the scan to recent messages so an early
	// duplicate the agent recovered from doesn't count against it.
	textRepetitionWindow = 12
	// textRepetitionThreshold: verbatim recurrences of one line that trip the
	// break. Three identical substantive turns is a loop, not a coincidence.
	textRepetitionThreshold = 3
	// textRepetitionMinLen: ignore short lines ("Done.", "OK") whose repetition
	// is benign; only count narration long enough to be a real intent restatement.
	textRepetitionMinLen = 40
)

// normalizeAssistantText lowercases and collapses whitespace so cosmetic
// drift (re-wrap, trailing newline) doesn't defeat the exact-match count.
func normalizeAssistantText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// detectTextRepetition reports whether a normalized assistant text recurs
// textRepetitionThreshold+ times within textRepetitionWindow messages. Returns
// count and an 8-byte hash of the offending text (safe for logs).
func detectTextRepetition(env *translate.RequestEnvelope) (looped bool, count int, sampleHash string) {
	texts := env.TrailingAssistantTexts()
	if len(texts) < textRepetitionThreshold {
		return false, 0, ""
	}
	start := 0
	if len(texts) > textRepetitionWindow {
		start = len(texts) - textRepetitionWindow
	}
	window := texts[start:]

	counts := make(map[string]int, len(window))
	topKey := ""
	for _, t := range window {
		n := normalizeAssistantText(t)
		if len(n) < textRepetitionMinLen {
			continue
		}
		counts[n]++
		if counts[n] > count {
			count = counts[n]
			topKey = n
		}
	}
	if count < textRepetitionThreshold {
		return false, count, ""
	}
	h := sha256.Sum256([]byte(topKey))
	return true, count, hex.EncodeToString(h[:8])
}

// handleTextRepetitionBreak writes a synthetic end_turn response and expires
// the session pin so the next turn re-routes off the stuck model instead of
// re-anchoring on it. Mirrors handleNoProgressBreak's mechanics; pin expiry is
// best-effort.
func (s *Service) handleTextRepetitionBreak(
	ctx context.Context,
	w http.ResponseWriter,
	env *translate.RequestEnvelope,
	count int,
	sampleHash string,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
	role string,
	decisionModel string,
	decisionProvider string,
	inputTokens int,
) error {
	log := observability.FromContext(ctx)

	msg := fmt.Sprintf(
		"✦ **Weave Router** → Repetition loop detected: `%s` (`%s`) repeated the same response %d times without making progress. Stopping this turn and clearing the session pin so the next message re-routes to a fresh model.\n\nIf the task is genuinely incomplete, send a follow-up message describing what's still needed; the router will pick a different model.\n\n",
		decisionModel, decisionProvider, count,
	)
	if env.SourceFormat() == translate.FormatOpenAI {
		msg = fmt.Sprintf(
			"Weave Router: repetition loop detected (%s/%s repeated the same response %d times). Stopping and clearing the session pin; send a follow-up to resume on a different model.",
			decisionModel, decisionProvider, count,
		)
	}

	log.Info("Text-repetition loop detected; breaking turn",
		"repeat_count", count,
		"window_size", textRepetitionWindow,
		"text_sample_hash", sampleHash,
		"decision_model", decisionModel,
		"decision_provider", decisionProvider,
		"session_key_prefix", shortSessionKey(sessionKey),
		"role", role,
	)

	// Expire the pin in Postgres (not just the in-proc cache) so a racing
	// reader on another pod can't repopulate the LRU from the stale row.
	// Skip a zero session key: a zero-keyed pin row is a zombie shared by every
	// zero-keyed session.
	if s.pinStore != nil && installationID != uuid.Nil && sessionKey != ([sessionpin.SessionKeyLen]byte{}) {
		if err := s.expireSessionPinAndHMMHistory(ctx, installationID, sessionKey, role, "text_repetition_break"); err != nil {
			log.Error("text-repetition-break: pin store upsert failed", "err", err)
		}
	}

	switch env.SourceFormat() {
	case translate.FormatOpenAI:
		return writeSyntheticOpenAIResponse(w, env, msg, inputTokens)
	default:
		return writeSyntheticAnthropicResponse(w, env, msg, inputTokens)
	}
}
