// Package turntype classifies inbound Anthropic-format request bodies by
// conversation role. It is a pure, I/O-free helper used by the proxy service
// to enable role-conditioned routing (§3.3–§3.4 of docs/plans/AGENTIC_CODING.md).
package turntype

import (
	"strings"

	"github.com/tidwall/gjson"
)

// TurnType classifies an inbound conversation turn.
type TurnType string

const (
	// MainLoop is the default — a regular user prompt scored by the cluster scorer.
	MainLoop TurnType = "main_loop"
	// ToolResult indicates the last user message consists entirely of
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

// Detect classifies the inbound Anthropic-format request body.
// Conservative: false negatives (returning MainLoop) are safe — the cluster
// scorer runs normally. False positives are the real risk; each heuristic is
// intentionally tight.
func Detect(body []byte) TurnType {
	if isCompaction(body) {
		return Compaction
	}
	if isSubAgentDispatch(body) {
		return SubAgentDispatch
	}
	if isToolResult(body) {
		return ToolResult
	}
	return MainLoop
}

// isCompaction returns true when the system prompt contains Claude Code's
// context-compaction instruction markers.
func isCompaction(body []byte) bool {
	text := systemText(body)
	lower := strings.ToLower(text)
	return strings.Contains(lower, "your task is to create a detailed summary") ||
		(strings.Contains(lower, "compact") && strings.Contains(lower, "conversation"))
}

// isSubAgentDispatch returns true when the request originates from a Claude
// Code sub-agent. Claude Code encodes sub-agent identity in metadata.user_id
// as "subagent:<type>", or marks it in the system prompt.
func isSubAgentDispatch(body []byte) bool {
	if strings.HasPrefix(gjson.GetBytes(body, "metadata.user_id").String(), "subagent:") {
		return true
	}
	lower := strings.ToLower(systemText(body))
	return strings.Contains(lower, "subagent_type") || strings.Contains(lower, "sub-agent")
}

// isToolResult returns true when the last user message contains only
// tool_result blocks and no text blocks.
func isToolResult(body []byte) bool {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return false
	}
	var lastMsg gjson.Result
	msgs.ForEach(func(_, msg gjson.Result) bool {
		lastMsg = msg
		return true
	})
	if !lastMsg.Exists() || lastMsg.Get("role").String() != "user" {
		return false
	}
	content := lastMsg.Get("content")
	if !content.IsArray() {
		return false
	}
	hasToolResult := false
	hasText := false
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "tool_result":
			hasToolResult = true
		case "text":
			hasText = true
		}
		return true
	})
	return hasToolResult && !hasText
}

// systemText extracts plain text from the system field, which may be a string
// or an array of content blocks.
func systemText(body []byte) string {
	sys := gjson.GetBytes(body, "system")
	if sys.Type == gjson.String {
		return sys.String()
	}
	if !sys.IsArray() {
		return ""
	}
	var b strings.Builder
	sys.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "text" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(block.Get("text").String())
		}
		return true
	})
	return b.String()
}
