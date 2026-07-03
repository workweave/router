// Package turntype classifies inbound conversation turns by role, independent
// of wire format, for role-conditioned routing.
package turntype

import (
	"strings"

	"workweave/router/internal/translate"
)

// TurnType classifies an inbound conversation turn.
type TurnType string

const (
	MainLoop         TurnType = "main_loop"
	ToolResult       TurnType = "tool_result"
	SubAgentDispatch TurnType = "sub_agent_dispatch"
	// Compaction: Claude Code context-compaction turn. Always Haiku.
	Compaction TurnType = "compaction"
	// Probe: quota/liveness check (max_tokens=1..4). Hard-pinned to cheap
	// model AND skips session-pin creation.
	Probe TurnType = "probe"
	// TitleGen: Claude Code sidebar-title generation. Hard-pinned AND
	// skips session-pin creation.
	TitleGen TurnType = "title_gen"
	// Classifier: short-form classification call (security monitor, etc.).
	// Hard-pinned AND skips session-pin creation.
	Classifier TurnType = "classifier"
)

const probeMaxTokensThreshold = 4

// Bounds for short-form classifier calls (e.g. Claude Code's security monitor:
// max_tokens=64, message_count=2). Headroom for similar calls without catching main-loop turns.
const (
	classifierMaxTokensThreshold = 256
	classifierMaxMessageCount    = 3
)

// DetectFromEnvelope classifies an inbound request. subAgentHint is the
// optional x-weave-subagent-type header value.
//
// Conservative by design: false negatives (MainLoop) are safe, false
// positives aren't, so each heuristic below is intentionally tight.
func DetectFromEnvelope(env *translate.RequestEnvelope, feats translate.RoutingFeatures, subAgentHint string) TurnType {
	if env == nil {
		return MainLoop
	}
	// Probe first: most specific signal with biggest consequence.
	if isProbe(feats) {
		return Probe
	}
	if isTitleGen(env, feats.HasTools) {
		return TitleGen
	}
	systemText := env.SystemText()
	// Compaction is Claude-Code-only, and Claude Code always talks Anthropic
	// format. Gating on format keeps Codex/OpenAI clients — whose prompts can
	// incidentally mention "compact" — out of the hard pin.
	if env.SourceFormat() == translate.FormatAnthropic && isCompaction(systemText) {
		return Compaction
	}
	if isSubAgentDispatch(env.MetadataUserID(), env.FirstUserMessageText(), subAgentHint) {
		return SubAgentDispatch
	}
	if isClassifier(feats) {
		return Classifier
	}
	if feats.LastKind == "tool_result" {
		return ToolResult
	}
	return MainLoop
}

func isProbe(feats translate.RoutingFeatures) bool {
	return feats.MaxTokens > 0 && feats.MaxTokens <= probeMaxTokensThreshold
}

// isClassifier reports whether a request is a short-form classifier call:
// no tools (real Claude Code turns always carry the tool registry), plus
// max_tokens/message_count within their thresholds. Checked after Probe.
// Tight on purpose — a false positive would hard-pin a real conversation.
func isClassifier(feats translate.RoutingFeatures) bool {
	if feats.HasTools {
		return false
	}
	if feats.MaxTokens <= 0 || feats.MaxTokens > classifierMaxTokensThreshold {
		return false
	}
	if feats.MessageCount <= 0 || feats.MessageCount > classifierMaxMessageCount {
		return false
	}
	return true
}

// isTitleGen reports whether a request is Claude Code's sidebar-title call:
// no tools, plus a JSON-schema response format asking for {"title": "..."}.
func isTitleGen(env *translate.RequestEnvelope, hasTools bool) bool {
	if hasTools {
		return false
	}
	return env.RequestsTitleSchema()
}

// isCompaction reports whether the system prompt contains Claude Code's
// context-compaction instruction markers.
func isCompaction(systemText string) bool {
	lower := strings.ToLower(systemText)
	return strings.Contains(lower, "your task is to create a detailed summary")
}

// isSubAgentDispatch reports whether the request originates from a sub-agent:
// the x-weave-subagent-type header, a "subagent:" metadata.user_id prefix, or
// a "<transcript>" tag (Claude Code's Agent tool convention) near the start
// of the first user message. Matching in the user-message body rather than
// the system prompt avoids false-positiving on the Agent tool's own
// description, which appears in every main-loop turn's system prompt. The
// prefix is bounded so a stray "<transcript>" deep in a long turn can't trigger.
func isSubAgentDispatch(metadataUserID, firstUserText, subAgentHint string) bool {
	if subAgentHint != "" {
		return true
	}
	if strings.HasPrefix(metadataUserID, "subagent:") {
		return true
	}
	const sniffLen = 64
	prefix := firstUserText
	if len(prefix) > sniffLen {
		prefix = prefix[:sniffLen]
	}
	return strings.Contains(prefix, "<transcript>")
}
