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
)

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
