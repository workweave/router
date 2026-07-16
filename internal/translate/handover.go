package translate

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// HandoverSummaryTag prefixes the synthesized summary message inserted by
// RewriteForHandover so it's distinguishable from real assistant output.
const HandoverSummaryTag = "[handover summary] "

// RewriteForHandover replaces all non-system messages with [assistantSummary,
// latestUserMessage] to bound input-token cost on mid-session model switches.
// Returns the count of elided messages; no-ops if there are none.
func (e *RequestEnvelope) RewriteForHandover(summary string) int {
	if e == nil {
		return 0
	}
	switch e.format {
	case FormatAnthropic:
		return e.rewriteAnthropicForHandover(summary)
	case FormatOpenAI:
		return e.rewriteOpenAIForHandover(summary)
	case FormatGemini:
		return e.rewriteGeminiForHandover(summary)
	default:
		return 0
	}
}

// TrimLastNMessages keeps the most recent n non-system messages plus system
// blocks. Falls back to n=3 when n <= 0. Returns the number elided.
func (e *RequestEnvelope) TrimLastNMessages(n int) int {
	if e == nil {
		return 0
	}
	if n <= 0 {
		n = 3
	}
	switch e.format {
	case FormatAnthropic:
		return e.trimAnthropicLastN(n)
	case FormatOpenAI:
		return e.trimOpenAILastN(n)
	case FormatGemini:
		return e.trimGeminiLastN(n)
	default:
		return 0
	}
}

// rewriteAnthropicForHandover rewrites the "messages" array for Anthropic format.
func (e *RequestEnvelope) rewriteAnthropicForHandover(summary string) int {
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return 0
	}
	all := msgs.Array()
	if len(all) == 0 {
		return 0
	}

	// Find the last user message (walking from the end).
	var latestUser gjson.Result
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Get("role").String() == "user" {
			latestUser = all[i]
			break
		}
	}

	summaryBlock := anthropicAssistantSummaryBlock(summary)
	rebuilt := []string{summaryBlock}
	preserved := 0
	if latestUser.Exists() {
		// Strip tool_result blocks: the summary has no tool_use blocks,
		// so any tool_results would be orphaned.
		cleaned := stripAnthropicToolResultMsg(latestUser, nil)
		if cleaned != "" {
			rebuilt = append(rebuilt, cleaned)
			preserved = 1
		}
	}

	// elided counts original conversation messages no longer present;
	// the synthesized summary is not part of the original conversation.
	elided := max(len(all)-preserved, 0)

	newMessages := "[" + strings.Join(rebuilt, ",") + "]"
	out, err := sjson.SetRawBytes(e.body, "messages", []byte(newMessages))
	if err != nil {
		return 0
	}
	e.body = out
	return elided
}

// anthropicAssistantSummaryBlock builds a synthesized assistant entry with a
// single text block containing the tagged summary.
func anthropicAssistantSummaryBlock(summary string) string {
	tagged := HandoverSummaryTag + summary
	msg := map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": tagged},
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		// json.Marshal can only fail on unsupported values; both keys
		// are strings, so this is defensive.
		escaped, _ := json.Marshal(tagged)
		return `{"role":"assistant","content":[{"type":"text","text":` + string(escaped) + `}]}`
	}
	return string(raw)
}

func (e *RequestEnvelope) trimAnthropicLastN(n int) int {
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return 0
	}
	all := msgs.Array()
	if len(all) <= n {
		return 0
	}
	keep := all[len(all)-n:]
	rebuilt := stripOrphanedAnthropicToolResults(keep)
	newMessages := "[" + strings.Join(rebuilt, ",") + "]"
	out, err := sjson.SetRawBytes(e.body, "messages", []byte(newMessages))
	if err != nil {
		return 0
	}
	e.body = out
	return len(all) - n
}

// rewriteOpenAIForHandover preserves role=="system" messages and replaces
// every other message with [assistantSummary, latestUser].
func (e *RequestEnvelope) rewriteOpenAIForHandover(summary string) int {
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return 0
	}
	all := msgs.Array()
	if len(all) == 0 {
		return 0
	}

	systems := make([]string, 0)
	others := make([]string, 0, len(all))
	for _, m := range all {
		if m.Get("role").String() == "system" {
			systems = append(systems, m.Raw)
			continue
		}
		others = append(others, m.Raw)
	}

	if len(others) == 0 {
		return 0
	}

	// Walk the non-system entries backwards for the latest user message.
	var latestUserRaw string
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Get("role").String() == "user" {
			latestUserRaw = all[i].Raw
			break
		}
	}

	summaryMsg := openAIAssistantSummaryMessage(summary)
	rebuilt := make([]string, 0, len(systems)+2)
	rebuilt = append(rebuilt, systems...)
	rebuilt = append(rebuilt, summaryMsg)
	preserved := 0
	if latestUserRaw != "" {
		rebuilt = append(rebuilt, latestUserRaw)
		preserved = 1
	}

	// elided counts original conversation messages no longer present;
	// preserved systems and the (optional) latestUser are not counted.
	elided := max(len(others)-preserved, 0)

	newMessages := "[" + strings.Join(rebuilt, ",") + "]"
	out, err := sjson.SetRawBytes(e.body, "messages", []byte(newMessages))
	if err != nil {
		return 0
	}
	e.body = out
	return elided
}

// openAIAssistantSummaryMessage builds an OpenAI assistant message with the
// tagged summary as a single string content.
func openAIAssistantSummaryMessage(summary string) string {
	tagged := HandoverSummaryTag + summary
	msg := map[string]any{
		"role":    "assistant",
		"content": tagged,
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		escaped, _ := json.Marshal(tagged)
		return `{"role":"assistant","content":` + string(escaped) + `}`
	}
	return string(raw)
}

func (e *RequestEnvelope) trimOpenAILastN(n int) int {
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return 0
	}
	all := msgs.Array()
	if len(all) == 0 {
		return 0
	}
	systems := make([]string, 0)
	others := make([]string, 0, len(all))
	for _, m := range all {
		if m.Get("role").String() == "system" {
			systems = append(systems, m.Raw)
			continue
		}
		others = append(others, m.Raw)
	}
	if len(others) <= n {
		return 0
	}
	keep := others[len(others)-n:]
	cleaned := stripOrphanedOpenAIToolMessages(keep)
	rebuilt := make([]string, 0, len(systems)+len(cleaned))
	rebuilt = append(rebuilt, systems...)
	rebuilt = append(rebuilt, cleaned...)
	newMessages := "[" + strings.Join(rebuilt, ",") + "]"
	out, err := sjson.SetRawBytes(e.body, "messages", []byte(newMessages))
	if err != nil {
		return 0
	}
	e.body = out
	return len(others) - n
}

// rewriteGeminiForHandover mirrors the Anthropic path against Gemini's
// `contents` array. systemInstruction is untouched.
func (e *RequestEnvelope) rewriteGeminiForHandover(summary string) int {
	contents := gjson.GetBytes(e.body, "contents")
	if !contents.IsArray() {
		return 0
	}
	all := contents.Array()
	if len(all) == 0 {
		return 0
	}

	var latestUser gjson.Result
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Get("role").String() == "user" {
			latestUser = all[i]
			break
		}
	}

	tagged := HandoverSummaryTag + summary
	summaryEntry := map[string]any{
		"role":  "model",
		"parts": []any{map[string]any{"text": tagged}},
	}
	summaryRaw, _ := json.Marshal(summaryEntry)

	rebuilt := make([]string, 0, 2)
	rebuilt = append(rebuilt, string(summaryRaw))
	preserved := 0
	if latestUser.Exists() {
		// Strip functionResponse parts: the summary has no functionCall parts,
		// so any functionResponse would be orphaned.
		cleaned := stripGeminiFunctionResponseEntry(latestUser, nil)
		if cleaned != "" {
			rebuilt = append(rebuilt, cleaned)
			preserved = 1
		}
	}

	elided := max(len(all)-preserved, 0)

	newContents := "[" + strings.Join(rebuilt, ",") + "]"
	out, err := sjson.SetRawBytes(e.body, "contents", []byte(newContents))
	if err != nil {
		return 0
	}
	e.body = out
	return elided
}

// stripGeminiFunctionResponseEntry removes functionResponse parts whose name
// is not in knownNames (nil knownNames = strip all). Returns "" if nothing
// remains — matching stripAnthropicToolResultMsg.
func stripGeminiFunctionResponseEntry(entry gjson.Result, knownNames map[string]struct{}) string {
	parts := entry.Get("parts")
	if !parts.IsArray() {
		return entry.Raw
	}
	hasOrphans := false
	parts.ForEach(func(_, part gjson.Result) bool {
		resp := part.Get("functionResponse")
		if !resp.Exists() {
			resp = part.Get("function_response")
		}
		if !resp.Exists() {
			return true
		}
		name := resp.Get("name").String()
		if _, ok := knownNames[name]; !ok {
			hasOrphans = true
			return false
		}
		return true
	})
	if !hasOrphans {
		return entry.Raw
	}
	var kept []string
	parts.ForEach(func(_, part gjson.Result) bool {
		resp := part.Get("functionResponse")
		if !resp.Exists() {
			resp = part.Get("function_response")
		}
		if resp.Exists() {
			name := resp.Get("name").String()
			if _, ok := knownNames[name]; !ok {
				return true
			}
		}
		kept = append(kept, part.Raw)
		return true
	})
	if len(kept) == 0 {
		return ""
	}
	newParts := "[" + strings.Join(kept, ",") + "]"
	out, err := sjson.SetRawBytes([]byte(entry.Raw), "parts", []byte(newParts))
	if err != nil {
		return entry.Raw
	}
	return string(out)
}

func (e *RequestEnvelope) trimGeminiLastN(n int) int {
	contents := gjson.GetBytes(e.body, "contents")
	if !contents.IsArray() {
		return 0
	}
	all := contents.Array()
	if len(all) <= n {
		return 0
	}
	keep := all[len(all)-n:]
	rebuilt := stripOrphanedGeminiFunctionResponses(keep)
	newContents := "[" + strings.Join(rebuilt, ",") + "]"
	out, err := sjson.SetRawBytes(e.body, "contents", []byte(newContents))
	if err != nil {
		return 0
	}
	e.body = out
	return len(all) - n
}

// stripOrphanedGeminiFunctionResponses walks entries in order and drops
// functionResponse parts that lack a preceding unmatched functionCall of the
// same name in the kept window. A later same-name call must not resurrect a
// stale response from a trimmed turn. User entries left empty are omitted.
func stripOrphanedGeminiFunctionResponses(entries []gjson.Result) []string {
	pending := make(map[string]int)
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Get("role").String() == "model" {
			addGeminiPendingFunctionCalls(pending, entry)
			result = append(result, entry.Raw)
			continue
		}
		cleaned := stripGeminiFunctionResponseEntryConsuming(entry, pending)
		if cleaned != "" {
			result = append(result, cleaned)
		}
	}
	return result
}

// addGeminiPendingFunctionCalls increments the unmatched functionCall count
// for each named call in a model entry.
func addGeminiPendingFunctionCalls(pending map[string]int, entry gjson.Result) {
	entry.Get("parts").ForEach(func(_, part gjson.Result) bool {
		call := part.Get("functionCall")
		if !call.Exists() {
			call = part.Get("function_call")
		}
		if name := call.Get("name").String(); name != "" {
			pending[name]++
		}
		return true
	})
}

// stripGeminiFunctionResponseEntryConsuming keeps functionResponse parts that
// can be paired with a preceding unmatched functionCall (decrementing the
// pending count). Unmatched responses are dropped. Returns "" if nothing remains.
func stripGeminiFunctionResponseEntryConsuming(entry gjson.Result, pending map[string]int) string {
	parts := entry.Get("parts")
	if !parts.IsArray() {
		return entry.Raw
	}
	var kept []string
	changed := false
	parts.ForEach(func(_, part gjson.Result) bool {
		resp := part.Get("functionResponse")
		if !resp.Exists() {
			resp = part.Get("function_response")
		}
		if !resp.Exists() {
			kept = append(kept, part.Raw)
			return true
		}
		name := resp.Get("name").String()
		if pending[name] > 0 {
			pending[name]--
			kept = append(kept, part.Raw)
			return true
		}
		changed = true
		return true
	})
	if !changed {
		return entry.Raw
	}
	if len(kept) == 0 {
		return ""
	}
	newParts := "[" + strings.Join(kept, ",") + "]"
	out, err := sjson.SetRawBytes([]byte(entry.Raw), "parts", []byte(newParts))
	if err != nil {
		return entry.Raw
	}
	return string(out)
}

// stripOrphanedAnthropicToolResults drops tool_result blocks whose tool_use_id
// has no matching tool_use among the set's assistant messages; user messages
// left empty afterward are omitted entirely.
func stripOrphanedAnthropicToolResults(msgs []gjson.Result) []string {
	knownIDs := collectAnthropicToolUseIDs(msgs)
	result := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Get("role").String() != "user" {
			result = append(result, m.Raw)
			continue
		}
		cleaned := stripAnthropicToolResultMsg(m, knownIDs)
		if cleaned != "" {
			result = append(result, cleaned)
		}
	}
	return result
}

// collectAnthropicToolUseIDs returns the set of tool_use IDs present in
// assistant messages.
func collectAnthropicToolUseIDs(msgs []gjson.Result) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, m := range msgs {
		if m.Get("role").String() != "assistant" {
			continue
		}
		m.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "tool_use" {
				if id := block.Get("id").String(); id != "" {
					ids[id] = struct{}{}
				}
			}
			return true
		})
	}
	return ids
}

// stripAnthropicToolResultMsg removes tool_result blocks not in knownIDs (nil
// strips all). Returns "" if the message is left with no content.
func stripAnthropicToolResultMsg(msg gjson.Result, knownIDs map[string]struct{}) string {
	content := msg.Get("content")
	if !content.IsArray() {
		return msg.Raw
	}

	hasOrphans := false
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "tool_result" {
			id := block.Get("tool_use_id").String()
			if _, ok := knownIDs[id]; !ok {
				hasOrphans = true
				return false
			}
		}
		return true
	})
	if !hasOrphans {
		return msg.Raw
	}

	var kept []string
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "tool_result" {
			id := block.Get("tool_use_id").String()
			if _, ok := knownIDs[id]; !ok {
				return true
			}
		}
		kept = append(kept, block.Raw)
		return true
	})
	if len(kept) == 0 {
		return ""
	}
	newContent := "[" + strings.Join(kept, ",") + "]"
	out, err := sjson.SetRawBytes([]byte(msg.Raw), "content", []byte(newContent))
	if err != nil {
		return msg.Raw
	}
	return string(out)
}

// stripOrphanedOpenAIToolMessages removes role:"tool" messages whose
// tool_call_id doesn't match any assistant tool_calls[].id in the set.
func stripOrphanedOpenAIToolMessages(msgs []string) []string {
	knownIDs := make(map[string]struct{})
	for _, raw := range msgs {
		parsed := gjson.Parse(raw)
		if parsed.Get("role").String() != "assistant" {
			continue
		}
		parsed.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			if id := tc.Get("id").String(); id != "" {
				knownIDs[id] = struct{}{}
			}
			return true
		})
	}
	result := make([]string, 0, len(msgs))
	for _, raw := range msgs {
		parsed := gjson.Parse(raw)
		if parsed.Get("role").String() == "tool" {
			tcID := parsed.Get("tool_call_id").String()
			if _, ok := knownIDs[tcID]; !ok {
				continue
			}
		}
		result = append(result, raw)
	}
	return result
}
