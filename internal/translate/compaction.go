package translate

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ClearedToolResultPlaceholder replaces the body of an old tool result during
// Tier-1 compaction cleanup. Mirrors Claude Code's own placeholder so a reader
// (human or model) recognizes that the content was elided, not lost.
const ClearedToolResultPlaceholder = "[Old tool result content cleared]"

// ClearOldToolResults replaces every tool result except the most recent
// keepRecent with ClearedToolResultPlaceholder, leaving structure intact and
// returning the number cleared. Tool results dominate agentic-session tokens;
// clearing stale ones is the cheap, model-free Tier-1 step that often avoids a
// full summarization. Pure: no I/O. keepRecent < 0 is treated as 0.
func (e *RequestEnvelope) ClearOldToolResults(keepRecent int) int {
	if e == nil {
		return 0
	}
	keepRecent = max(keepRecent, 0)
	switch e.format {
	case FormatAnthropic:
		return e.clearOldToolResultsAnthropic(keepRecent)
	case FormatOpenAI:
		return e.clearOldToolResultsOpenAI(keepRecent)
	case FormatGemini:
		return e.clearOldToolResultsGemini(keepRecent)
	default:
		return 0
	}
}

// RewriteForCompaction rewrites history to [summary + recent keepRecentTurns
// non-system messages], aligned to begin on a user turn so roles alternate.
// Orphaned tool results (whose tool_use was elided) are stripped to keep the
// request wire-valid. Unlike RewriteForHandover, a tail is kept so the model
// retains immediate working context. Returns the number of messages elided.
// Pure: no I/O. keepRecentTurns <= 0 is treated as 1.
func (e *RequestEnvelope) RewriteForCompaction(summary string, keepRecentTurns int) int {
	if e == nil {
		return 0
	}
	keepRecentTurns = max(keepRecentTurns, 1)
	switch e.format {
	case FormatAnthropic:
		return e.rewriteAnthropicForCompaction(summary, keepRecentTurns)
	case FormatOpenAI:
		return e.rewriteOpenAIForCompaction(summary, keepRecentTurns)
	case FormatGemini:
		return e.rewriteGeminiForCompaction(summary, keepRecentTurns)
	default:
		return 0
	}
}

// userAlignedStart returns the index at which to begin a recent-message window
// of size keepRecent drawn from msgs, advanced forward to the first user
// message so the window starts on a user turn. Falls back to the last user
// message's index when the window contains none.
func userAlignedStart(msgs []gjson.Result, keepRecent int) int {
	start := max(len(msgs)-keepRecent, 0)
	for start < len(msgs) && msgs[start].Get("role").String() != "user" {
		start++
	}
	if start >= len(msgs) {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Get("role").String() == "user" {
				return i
			}
		}
		return len(msgs)
	}
	return start
}

func (e *RequestEnvelope) clearOldToolResultsAnthropic(keepRecent int) int {
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return 0
	}
	all := msgs.Array()

	total := 0
	for _, m := range all {
		if m.Get("role").String() != "user" {
			continue
		}
		m.Get("content").ForEach(func(_, b gjson.Result) bool {
			if b.Get("type").String() == "tool_result" {
				total++
			}
			return true
		})
	}
	cutoff := total - keepRecent
	if cutoff <= 0 {
		return 0
	}

	seen, cleared := 0, 0
	rebuilt := make([]string, 0, len(all))
	for _, m := range all {
		content := m.Get("content")
		if m.Get("role").String() != "user" || !content.IsArray() {
			rebuilt = append(rebuilt, m.Raw)
			continue
		}
		newBlocks := make([]string, 0, len(content.Array()))
		changed := false
		content.ForEach(func(_, b gjson.Result) bool {
			if b.Get("type").String() == "tool_result" {
				seen++
				if seen <= cutoff {
					nb, err := sjson.SetBytes([]byte(b.Raw), "content", ClearedToolResultPlaceholder)
					if err == nil {
						newBlocks = append(newBlocks, string(nb))
						cleared++
						changed = true
						return true
					}
				}
			}
			newBlocks = append(newBlocks, b.Raw)
			return true
		})
		if !changed {
			rebuilt = append(rebuilt, m.Raw)
			continue
		}
		newContent := "[" + strings.Join(newBlocks, ",") + "]"
		nm, err := sjson.SetRawBytes([]byte(m.Raw), "content", []byte(newContent))
		if err != nil {
			rebuilt = append(rebuilt, m.Raw)
			continue
		}
		rebuilt = append(rebuilt, string(nm))
	}
	if cleared == 0 {
		return 0
	}
	return e.setMessages(rebuilt, cleared)
}

func (e *RequestEnvelope) clearOldToolResultsOpenAI(keepRecent int) int {
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return 0
	}
	all := msgs.Array()

	total := 0
	for _, m := range all {
		if m.Get("role").String() == "tool" {
			total++
		}
	}
	cutoff := total - keepRecent
	if cutoff <= 0 {
		return 0
	}

	seen, cleared := 0, 0
	rebuilt := make([]string, 0, len(all))
	for _, m := range all {
		if m.Get("role").String() != "tool" {
			rebuilt = append(rebuilt, m.Raw)
			continue
		}
		seen++
		if seen <= cutoff {
			nm, err := sjson.SetBytes([]byte(m.Raw), "content", ClearedToolResultPlaceholder)
			if err == nil {
				rebuilt = append(rebuilt, string(nm))
				cleared++
				continue
			}
		}
		rebuilt = append(rebuilt, m.Raw)
	}
	if cleared == 0 {
		return 0
	}
	return e.setMessages(rebuilt, cleared)
}

func (e *RequestEnvelope) clearOldToolResultsGemini(keepRecent int) int {
	contents := gjson.GetBytes(e.body, "contents")
	if !contents.IsArray() {
		return 0
	}
	all := contents.Array()

	total := 0
	for _, c := range all {
		c.Get("parts").ForEach(func(_, p gjson.Result) bool {
			if p.Get("functionResponse").Exists() {
				total++
			}
			return true
		})
	}
	cutoff := total - keepRecent
	if cutoff <= 0 {
		return 0
	}

	seen, cleared := 0, 0
	rebuilt := make([]string, 0, len(all))
	for _, c := range all {
		parts := c.Get("parts")
		if !parts.IsArray() {
			rebuilt = append(rebuilt, c.Raw)
			continue
		}
		newParts := make([]string, 0, len(parts.Array()))
		changed := false
		parts.ForEach(func(_, p gjson.Result) bool {
			if p.Get("functionResponse").Exists() {
				seen++
				if seen <= cutoff {
					np, err := sjson.SetBytes([]byte(p.Raw), "functionResponse.response", map[string]any{"result": ClearedToolResultPlaceholder})
					if err == nil {
						newParts = append(newParts, string(np))
						cleared++
						changed = true
						return true
					}
				}
			}
			newParts = append(newParts, p.Raw)
			return true
		})
		if !changed {
			rebuilt = append(rebuilt, c.Raw)
			continue
		}
		newPartsRaw := "[" + strings.Join(newParts, ",") + "]"
		nc, err := sjson.SetRawBytes([]byte(c.Raw), "parts", []byte(newPartsRaw))
		if err != nil {
			rebuilt = append(rebuilt, c.Raw)
			continue
		}
		rebuilt = append(rebuilt, string(nc))
	}
	if cleared == 0 {
		return 0
	}
	out, err := sjson.SetRawBytes(e.body, "contents", []byte("["+strings.Join(rebuilt, ",")+"]"))
	if err != nil {
		return 0
	}
	e.body = out
	return cleared
}

// setMessages writes rebuilt back to the "messages" array and returns ret on
// success, 0 on marshal failure. Shared by the Anthropic/OpenAI message-array
// rewriters.
func (e *RequestEnvelope) setMessages(rebuilt []string, ret int) int {
	out, err := sjson.SetRawBytes(e.body, "messages", []byte("["+strings.Join(rebuilt, ",")+"]"))
	if err != nil {
		return 0
	}
	e.body = out
	return ret
}

func (e *RequestEnvelope) rewriteAnthropicForCompaction(summary string, keepRecent int) int {
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return 0
	}
	all := msgs.Array()
	if len(all) == 0 {
		return 0
	}
	start := userAlignedStart(all, keepRecent)
	cleaned := stripOrphanedAnthropicToolResults(all[start:])
	rebuilt := append([]string{anthropicAssistantSummaryBlock(summary)}, cleaned...)
	elided := max(len(all)-len(cleaned), 0)
	return e.setMessages(rebuilt, elided)
}

func (e *RequestEnvelope) rewriteOpenAIForCompaction(summary string, keepRecent int) int {
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return 0
	}
	all := msgs.Array()
	if len(all) == 0 {
		return 0
	}
	systems := make([]string, 0)
	others := make([]gjson.Result, 0, len(all))
	for _, m := range all {
		if m.Get("role").String() == "system" {
			systems = append(systems, m.Raw)
			continue
		}
		others = append(others, m)
	}
	if len(others) == 0 {
		return 0
	}
	start := userAlignedStart(others, keepRecent)
	keptRaw := make([]string, 0, len(others)-start)
	for _, m := range others[start:] {
		keptRaw = append(keptRaw, m.Raw)
	}
	cleaned := stripOrphanedOpenAIToolMessages(keptRaw)
	rebuilt := make([]string, 0, len(systems)+1+len(cleaned))
	rebuilt = append(rebuilt, systems...)
	rebuilt = append(rebuilt, openAIAssistantSummaryMessage(summary))
	rebuilt = append(rebuilt, cleaned...)
	elided := max(len(others)-len(cleaned), 0)
	return e.setMessages(rebuilt, elided)
}

func (e *RequestEnvelope) rewriteGeminiForCompaction(summary string, keepRecent int) int {
	contents := gjson.GetBytes(e.body, "contents")
	if !contents.IsArray() {
		return 0
	}
	all := contents.Array()
	if len(all) == 0 {
		return 0
	}
	start := userAlignedStart(all, keepRecent)
	kept := all[start:]

	tagged := HandoverSummaryTag + summary
	summaryEntry := map[string]any{
		"role":  "model",
		"parts": []any{map[string]any{"text": tagged}},
	}
	summaryRaw, _ := json.Marshal(summaryEntry)

	rebuilt := make([]string, 0, len(kept)+1)
	rebuilt = append(rebuilt, string(summaryRaw))
	for _, m := range kept {
		rebuilt = append(rebuilt, m.Raw)
	}
	elided := max(len(all)-len(kept), 0)
	out, err := sjson.SetRawBytes(e.body, "contents", []byte("["+strings.Join(rebuilt, ",")+"]"))
	if err != nil {
		return 0
	}
	e.body = out
	return elided
}
