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
	// TitleGen: Claude Code's sidebar-title generation call. Fires once
	// per user turn alongside the real conversation request, carries no
	// tools and a JSON-schema response format, and always asks for the
	// same fixed-shape output ({"title": "..."}). Hard-pinned to the
	// cheap model AND skips session-pin creation: routing the real
	// conversation through the cluster scorer is the whole point of the
	// router, but the title-gen call has no signal worth scoring and a
	// pin written here would anchor the real-conv turn that lands ~25ms
	// later (same session key) before its own scorer even runs.
	TitleGen TurnType = "title_gen"
	// Classifier: a short-form classification call — no tools, small
	// max_tokens, short message list. Anthropic's Claude Code security
	// monitor (allow/block-with-reason on the agent's latest action) is
	// the canonical case, but the pattern is general: any caller asking
	// the model to emit a tiny verdict from a fixed-shape context. Same
	// routing treatment as Probe and TitleGen — hard-pinned to the cheap
	// model and skips session-pin creation so the real-conv pin doesn't
	// inherit a classifier's decision.
	Classifier TurnType = "classifier"
)

// probeMaxTokensThreshold is the inclusive upper bound on max_tokens for
// classifying a request as a probe. The Anthropic SDK quota check uses
// max_tokens=1; allow up to 4 to absorb minor variations across SDK
// versions without false-positiving real "give me a short answer" calls
// (which start around 64+).
const probeMaxTokensThreshold = 4

// classifier{Max,Msg}Threshold bound the Classifier turn type to short-form
// classification calls. Claude Code's security monitor uses max_tokens=64
// and message_count=2 (system + a single transcript-bundled user turn);
// the bounds give headroom for similar yes/no/verdict classifiers without
// catching real "short reply" main-loop turns (which start at ~512 output
// tokens and carry the full 128-tool Claude Code registry).
const (
	classifierMaxTokensThreshold = 256
	classifierMaxMessageCount    = 3
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
	// Probe is checked first: it's the most specific signal (a numeric
	// cap on the caller's request, not a heuristic over text) and the
	// consequence is biggest — a probe that anchors a session pin
	// poisons every subsequent turn in that session.
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

// isProbe reports whether a request is a liveness/quota check that should
// be hard-pinned and excluded from session-pin creation. The Anthropic
// SDK's `client.messages.create(..., max_tokens=1)` quota probe is the
// canonical case; OpenAI's `max_completion_tokens` and Gemini's
// `generationConfig.maxOutputTokens` produce the same RoutingFeatures.
func isProbe(feats translate.RoutingFeatures) bool {
	return feats.MaxTokens > 0 && feats.MaxTokens <= probeMaxTokensThreshold
}

// isClassifier reports whether a request is a short-form classifier call
// (Anthropic's security monitor, or any similar caller asking for a tiny
// verdict from a fixed-shape context). Three structural signals, all
// required:
//
//  1. No tools — a real Claude Code main-loop turn always carries the
//     full 128-tool registry, even when the user asks for a short reply.
//  2. max_tokens within classifierMaxTokensThreshold — classifier outputs
//     are tiny (security monitor uses 64; title-gen and friends similar).
//  3. message_count within classifierMaxMessageCount — classifiers are
//     stateless and bundle their context into a single user turn; real
//     conversations grow the message list past 3 quickly.
//
// Probe (max_tokens<=4) is checked first and wins; TitleGen and Compaction
// have more specific fingerprints and also win when they match. This is a
// general fallback for the broader classifier shape. False negatives
// (returning MainLoop) are safe; false positives would route a real
// conversation to the cheap hard-pin, hence the tight conjunction.
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
// generation call. Detection uses two structural signals (per the
// project's classifier-must-not-string-match-content rule), both
// required:
//
//  1. The request declares a JSON-schema response format whose top-level
//     schema has a string "title" property — i.e. it is asking the model
//     to emit `{"title": "..."}` and nothing else. This is the structural
//     fingerprint of Claude Code's title generator; a general-purpose
//     structured-output call with a different schema does not match.
//  2. tools is empty. A real conversation that wants a title-shaped
//     structured output still carries the full Claude Code tool
//     registry, so this guard prevents hard-pinning conversations.
//
// Per this file's invariant, false negatives (returning MainLoop) are
// safe — the cluster scorer runs normally; only false positives are
// dangerous.
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
// sub-agent dispatch. Three independent signals:
//
//  1. x-weave-subagent-type header — set by non-Anthropic clients or
//     middleware that knows it dispatched a sub-agent.
//  2. metadata.user_id starting with "subagent:" — older Anthropic SDK
//     convention.
//  3. First user message wrapped in Claude Code's "<transcript>...</transcript>"
//     envelope — what Claude Code's Agent tool actually emits today for
//     dispatches like Explore. metadata.user_id is unset in this case
//     (Claude Code reuses the parent session_id), so without this signal
//     real Explore agents would never classify as SubAgentDispatch.
//
// "<transcript>" lives in the user-message body, not the system prompt.
// That's deliberate: an earlier attempt matched "subagent_type" in the
// system text and false-positived on every main-loop turn that exposed
// the Agent tool description (since the tool description contains that
// parameter name verbatim). The "<transcript>" envelope is specific to
// dispatched sub-agent prompts and does not appear in main-loop traffic.
//
// Per this file's invariant, false negatives (returning MainLoop) are
// safe — the cluster scorer runs normally; only false positives are
// dangerous.
func isSubAgentDispatch(metadataUserID, firstUserText, subAgentHint string) bool {
	if subAgentHint != "" {
		return true
	}
	if strings.HasPrefix(metadataUserID, "subagent:") {
		return true
	}
	// Claude Code's Agent tool wraps the dispatched prompt as:
	//   <transcript>
	//   User: <body>
	//   </transcript>
	// Look at a bounded prefix so a stray "<transcript>" string embedded
	// deep in user content can't trigger SubAgentDispatch on a long
	// main-loop turn.
	const sniffLen = 64
	prefix := firstUserText
	if len(prefix) > sniffLen {
		prefix = prefix[:sniffLen]
	}
	return strings.Contains(prefix, "<transcript>")
}
