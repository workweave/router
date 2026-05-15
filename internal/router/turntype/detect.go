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

// classifier thresholds bound the Classifier type to short-form calls.
// Claude Code's security monitor uses max_tokens=64, message_count=2;
// headroom for similar classifiers without catching real main-loop turns.
const (
	classifierMaxTokensThreshold = 256
	classifierMaxMessageCount    = 3
)

// DetectFromEnvelope classifies an inbound request. subAgentHint is the
// optional x-weave-subagent-type header value.
//
// Conservative: false negatives (returning MainLoop) are safe — the
// cluster scorer runs normally. False positives are the real risk; each
// heuristic is intentionally tight.
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
	if isCompaction(systemText) {
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

// isClassifier reports whether a request is a short-form classifier call.
// Three structural signals, all required:
//  1. No tools — real Claude Code turns always carry the tool registry.
//  2. max_tokens within classifierMaxTokensThreshold.
//  3. message_count within classifierMaxMessageCount.
//
// Probe (max_tokens<=4) is checked first and wins. False negatives are
// safe; false positives would hard-pin a real conversation — hence tight.
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

// isTitleGen reports whether a request is Claude Code's sidebar-title
// generation call. Two structural signals:
//  1. Declares a JSON-schema response format with top-level "title" string
//     property — asking the model to emit {"title": "..."}.
//  2. tools is empty — a real conversation with structured output still
//     carries the tool registry.
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
	return strings.Contains(lower, "your task is to create a detailed summary") ||
		(strings.Contains(lower, "compact") && strings.Contains(lower, "conversation"))
}

// isSubAgentDispatch reports whether the request originates from a
// sub-agent. Three independent signals:
//  1. x-weave-subagent-type header.
//  2. metadata.user_id starting with "subagent:".
//  3. First user message wrapped in "<transcript>...</transcript>"
//     (Claude Code's Agent tool convention).
//
// "<transcript>" lives in the user-message body, not the system prompt —
// earlier attempt matching "subagent_type" in system text false-positived
// on every main-loop turn exposing the Agent tool description.
//
// Sniff bounded prefix so a stray "<transcript>" deep in content can't
// trigger on long main-loop turns.
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
