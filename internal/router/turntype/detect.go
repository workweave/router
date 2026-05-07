// Package turntype classifies inbound conversation requests by role,
// independent of wire format. It is a pure, I/O-free helper used by
// the proxy service to enable role-conditioned routing (§3.3–§3.4 of
// docs/plans/AGENTIC_CODING.md).
package turntype

import (
	"strings"

	"workweave/router/internal/translate"
)

// TurnType classifies an inbound conversation turn.
type TurnType string

const (
	// MainLoop is the default — a regular user prompt scored by the cluster scorer.
	MainLoop TurnType = "main_loop"
	// ToolResult indicates the last user-side input consists entirely of
	// tool_result blocks with no text. The cluster scorer embedding is mostly
	// noise on these turns; they short-circuit to the session pin when one exists.
	ToolResult TurnType = "tool_result"
	// SubAgentDispatch indicates the request originates from a read-only
	// sub-agent (e.g. Claude Code's Explore agent). Routed to Haiku under §3.4
	// when ROUTER_HARD_PIN_EXPLORE is enabled.
	SubAgentDispatch TurnType = "sub_agent_dispatch"
	// Compaction indicates a Claude Code context-compaction turn. Always routed
	// to Haiku under §3.4 (short-out-of-long-in cost profile).
	Compaction TurnType = "compaction"
)

// DetectFromEnvelope classifies an inbound request using a parsed
// envelope and its already-computed routing features. subAgentHint is
// the optional `x-weave-subagent-type` header value: clients that
// can't pack subagent identity into Anthropic's metadata.user_id (e.g.
// OpenAI / Gemini ingress) use the header instead. Empty hint is
// ignored.
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

// isCompaction returns true when the system prompt contains Claude Code's
// context-compaction instruction markers.
func isCompaction(systemText string) bool {
	lower := strings.ToLower(systemText)
	return strings.Contains(lower, "your task is to create a detailed summary") ||
		(strings.Contains(lower, "compact") && strings.Contains(lower, "conversation"))
}

// isSubAgentDispatch returns true when the request originates from a
// sub-agent dispatch. Claude Code packs sub-agent identity into
// metadata.user_id as "subagent:<type>"; non-Anthropic clients pass it
// via the x-weave-subagent-type header (subAgentHint). The system
// prompt also carries marker phrases as a third fallback.
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
