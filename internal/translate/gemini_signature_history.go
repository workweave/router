package translate

import "github.com/tidwall/gjson"

// HasUnsignedToolCallHistory reports whether the conversation history carries at
// least one assistant tool call — Anthropic tool_use, OpenAI tool_calls, or
// Gemini functionCall — that lacks a Gemini thoughtSignature.
//
// Gemini 3.x rejects (400 INVALID_ARGUMENT) any request whose history contains
// function-call parts without the thoughtSignature it originally issued. That
// happens whenever a session is routed into Gemini after prior turns were
// served by a different model (a planner switch or a tier clamp): foreign
// history never carried a Gemini signature, and the router smuggles Gemini's
// own signatures via the tool-call id (see thought_signature_id.go), so a
// native Gemini continuation round-trips them but a cross-model handoff cannot.
//
// The router uses this to make Gemini 3.x models ineligible for such a turn,
// avoiding a guaranteed upstream 400. A single unsigned tool call is enough,
// because Gemini validates every function-call part in the history.
func (e *RequestEnvelope) HasUnsignedToolCallHistory() bool {
	if e == nil {
		return false
	}
	switch e.format {
	case FormatAnthropic:
		return anthropicHasUnsignedToolUse(e.body)
	case FormatOpenAI:
		return openAIHasUnsignedToolCalls(e.body)
	case FormatGemini:
		return geminiHasUnsignedFunctionCall(e.body)
	default:
		return false
	}
}

func anthropicHasUnsignedToolUse(body []byte) bool {
	found := false
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "assistant" {
			return true
		}
		msg.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "tool_use" && extractThoughtSignature(block) == "" {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
}

func openAIHasUnsignedToolCalls(body []byte) bool {
	found := false
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "assistant" {
			return true
		}
		msg.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			if extractThoughtSignature(tc) == "" {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
}

func geminiHasUnsignedFunctionCall(body []byte) bool {
	found := false
	gjson.GetBytes(body, "contents").ForEach(func(_, content gjson.Result) bool {
		if content.Get("role").String() != "model" {
			return true
		}
		content.Get("parts").ForEach(func(_, part gjson.Result) bool {
			if part.Get("functionCall").Exists() && part.Get("thoughtSignature").String() == "" {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
}
