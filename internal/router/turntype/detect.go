// Package turntype classifies inbound conversation requests by role,
// independent of wire format. Pure, I/O-free helper used by the proxy
// service for role-conditioned routing.
package turntype

import (
	"strings"

	"workweave/router/internal/translate"
)

// TurnType classifies an inbound conversation turn.
type TurnType string

const (
	MainLoop TurnType = "main_loop"
	// ToolResult: last user-side input is all tool_result blocks with no
	// text. Embedding is mostly noise on these turns; short-circuits to
	// the session pin when one exists.
	ToolResult TurnType = "tool_result"
	// SubAgentDispatch: request originates from a read-only sub-agent
	// (e.g. Claude Code's Explore agent). Routed to Haiku when
	// ROUTER_HARD_PIN_EXPLORE is enabled.
	SubAgentDispatch TurnType = "sub_agent_dispatch"
	// Compaction: Claude Code context-compaction turn. Always routed to
	// Haiku (short-out-of-long-in cost profile).
	Compaction TurnType = "compaction"
	// Probe: a quota / liveness check the caller issued before any real
	// conversation (Anthropic SDK + Claude Code do this on init with
	// max_tokens=1). Always hard-pinned to the cheap model AND skips
	// session-pin creation so a probe never anchors subsequent routing.
	Probe TurnType = "probe"
)

// probeMaxTokensThreshold is the inclusive upper bound on max_tokens for
// classifying a request as a probe. The Anthropic SDK quota check uses
// max_tokens=1; allow up to 4 to absorb minor variations across SDK
// versions without false-positiving real "give me a short answer" calls
// (which start around 64+).
const probeMaxTokensThreshold = 4

// DetectFromEnvelope classifies an inbound request. subAgentHint is the
// optional x-weave-subagent-type header value; empty is ignored.
//
// Conservative: false negatives (returning MainLoop) are safe — the
// cluster scorer runs normally. False positives are the real risk; each
// heuristic is intentionally tight.
func DetectFromEnvelope(env *translate.RequestEnvelope, feats translate.RoutingFeatures, subAgentHint string) TurnType {
	if env == nil {
		return MainLoop
	}
	// Probe is checked first: it's the most specific signal (a numeric
	// cap on the caller's request, not a heuristic over text) and the
	// consequence is biggest — a probe that anchors a session pin
	// poisons every subsequent turn in that session.
	if isProbe(feats) {
		return Probe
	}
	systemText := env.SystemText()
	if isCompaction(systemText) {
		return Compaction
	}
	if isSubAgentDispatch(env.MetadataUserID(), systemText, subAgentHint) {
		return SubAgentDispatch
	}
	if feats.LastKind == "tool_result" {
		return ToolResult
	}
	return MainLoop
}

// isProbe reports whether a request is a liveness/quota check that should
// be hard-pinned and excluded from session-pin creation. The Anthropic
// SDK's `client.messages.create(..., max_tokens=1)` quota probe is the
// canonical case; OpenAI's `max_completion_tokens` and Gemini's
// `generationConfig.maxOutputTokens` produce the same RoutingFeatures.
func isProbe(feats translate.RoutingFeatures) bool {
	return feats.MaxTokens > 0 && feats.MaxTokens <= probeMaxTokensThreshold
}

// isCompaction reports whether the system prompt contains Claude Code's
// context-compaction instruction markers.
func isCompaction(systemText string) bool {
	lower := strings.ToLower(systemText)
	return strings.Contains(lower, "your task is to create a detailed summary") ||
		(strings.Contains(lower, "compact") && strings.Contains(lower, "conversation"))
}

// isSubAgentDispatch reports whether the request originates from a
// sub-agent dispatch. Claude Code packs sub-agent identity into
// metadata.user_id as "subagent:<type>"; non-Anthropic clients pass it
// via the x-weave-subagent-type header. System-prompt marker phrases
// are a third fallback.
func isSubAgentDispatch(metadataUserID, systemText, subAgentHint string) bool {
	if subAgentHint != "" {
		return true
	}
	if strings.HasPrefix(metadataUserID, "subagent:") {
		return true
	}
	lower := strings.ToLower(systemText)
	return strings.Contains(lower, "subagent_type") || strings.Contains(lower, "sub-agent")
}
