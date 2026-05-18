package translate

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// GeminiToOpenAIResponse converts a non-streaming Gemini response to OpenAI format.
func GeminiToOpenAIResponse(body []byte, requestModel string) ([]byte, error) {
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("id")
	jw.Str(generateChatCmplID())
	jw.Key("object")
	jw.Str("chat.completion")
	jw.Key("created")
	jw.Int(time.Now().Unix())
	jw.Key("model")
	jw.Str(requestModel)
	jw.Key("choices")
	jw.Arr()
	jw.Obj()
	jw.Key("index")
	jw.Int(0)
	jw.Key("message")
	hasToolCalls, _ := writeOpenAIMessageFromGemini(jw, gjson.GetBytes(body, "candidates.0"))
	finishReason := mapGeminiFinishReason(gjson.GetBytes(body, "candidates.0.finishReason").String(), hasToolCalls)
	jw.Key("finish_reason")
	jw.Str(finishReason)
	jw.EndObj()
	jw.EndArr()
	jw.Key("usage")
	writeOpenAIUsageFromGemini(jw, gjson.GetBytes(body, "usageMetadata"))
	jw.EndObj()
	return jw.Bytes(), nil
}

// writeOpenAIMessageFromGemini writes the "message" object for a single Gemini candidate.
// Returns hasToolCalls and leadingSig (non-empty when a leading text part carries a thoughtSignature
// and there are no function calls — callers may propagate it as an off-spec field).
func writeOpenAIMessageFromGemini(jw *jsonWriter, candidate gjson.Result) (hasToolCalls bool, leadingSig string) {
	parts := candidate.Get("content.parts")

	var texts []string
	var firstTextSig string
	type toolCallEntry struct {
		id        string
		name      string
		args      string
		signature string
	}
	var toolCalls []toolCallEntry

	parts.ForEach(func(_, part gjson.Result) bool {
		if fc := part.Get("functionCall"); fc.Exists() {
			name := fc.Get("name").String()
			args := fc.Get("args").Raw
			if args == "" {
				args = "{}"
			}
			sig := part.Get("thoughtSignature").String()
			id := embedSignatureInID(generateToolCallID(), sig)
			toolCalls = append(toolCalls, toolCallEntry{id: id, name: name, args: args, signature: sig})
			return true
		}
		if t := part.Get("text").String(); t != "" {
			texts = append(texts, t)
			if firstTextSig == "" {
				if sig := part.Get("thoughtSignature").String(); sig != "" {
					firstTextSig = sig
				}
			}
		}
		return true
	})

	hasToolCalls = len(toolCalls) > 0
	if !hasToolCalls {
		leadingSig = firstTextSig
	}

	jw.Obj()
	jw.Key("role")
	jw.Str("assistant")
	jw.Key("content")
	text := strings.Join(texts, "")
	if text != "" {
		jw.Str(text)
	} else {
		jw.Null()
	}
	if hasToolCalls {
		jw.Key("tool_calls")
		jw.Arr()
		for _, tc := range toolCalls {
			jw.Obj()
			jw.Key("id")
			jw.Str(tc.id)
			jw.Key("type")
			jw.Str("function")
			jw.Key("function")
			jw.Obj()
			jw.Key("name")
			jw.Str(tc.name)
			jw.Key("arguments")
			jw.Str(tc.args)
			if tc.signature != "" {
				jw.Key("thought_signature")
				jw.Str(tc.signature)
			}
			jw.EndObj()
			if tc.signature != "" {
				jw.Key("thought_signature")
				jw.Str(tc.signature)
			}
			jw.EndObj()
		}
		jw.EndArr()
	} else if leadingSig != "" {
		// Off-spec; litellm/openai-go pass through unknown fields.
		jw.Key("thought_signature")
		jw.Str(leadingSig)
	}
	jw.EndObj()

	return hasToolCalls, leadingSig
}

// writeOpenAIUsageFromGemini writes the "usage" object from Gemini usageMetadata.
func writeOpenAIUsageFromGemini(jw *jsonWriter, meta gjson.Result) {
	prompt := meta.Get("promptTokenCount").Int()
	completion := meta.Get("candidatesTokenCount").Int()
	total := meta.Get("totalTokenCount").Int()
	if total == 0 {
		total = prompt + completion
	}
	jw.Obj()
	jw.Key("prompt_tokens")
	jw.Int(prompt)
	jw.Key("completion_tokens")
	jw.Int(completion)
	jw.Key("total_tokens")
	jw.Int(total)
	jw.EndObj()
}

// GeminiToAnthropicResponse converts a non-streaming Gemini response to
// Anthropic Messages format.
func GeminiToAnthropicResponse(body []byte, requestModel string) ([]byte, error) {
	hasToolUse, content := buildAnthropicContent(gjson.GetBytes(body, "candidates.0"))
	stopReason := mapGeminiFinishReasonToAnthropic(
		gjson.GetBytes(body, "candidates.0.finishReason").String(),
		hasToolUse,
	)

	jw := newJSONWriter()
	jw.Obj()
	jw.Key("id")
	jw.Str(generateAnthropicMsgID())
	jw.Key("type")
	jw.Str("message")
	jw.Key("role")
	jw.Str("assistant")
	jw.Key("model")
	jw.Str(requestModel)
	jw.Key("content")
	jw.RawBytes(content)
	jw.Key("stop_reason")
	jw.Str(stopReason)
	jw.Key("stop_sequence")
	jw.Null()
	jw.Key("usage")
	writeAnthropicUsageFromGemini(jw, gjson.GetBytes(body, "usageMetadata"))
	jw.EndObj()
	return jw.Bytes(), nil
}

// buildAnthropicContent walks candidate parts and returns the serialised content
// array plus whether any tool_use block was emitted.
func buildAnthropicContent(candidate gjson.Result) (hasToolUse bool, content []byte) {
	jw := newJSONWriter()
	jw.Arr()
	candidate.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
		if fc := part.Get("functionCall"); fc.Exists() {
			sig := part.Get("thoughtSignature").String()
			args := fc.Get("args").Raw
			if args == "" {
				args = "{}"
			}
			jw.Obj()
			jw.Key("type")
			jw.Str("tool_use")
			jw.Key("id")
			jw.Str(embedSignatureInID(generateToolUseID(), sig))
			jw.Key("name")
			jw.Str(fc.Get("name").String())
			jw.Key("input")
			jw.Raw(args)
			if sig != "" {
				jw.Key("thought_signature")
				jw.Str(sig)
			}
			jw.EndObj()
			hasToolUse = true
			return true
		}
		if t := part.Get("text").String(); t != "" {
			jw.Obj()
			jw.Key("type")
			jw.Str("text")
			jw.Key("text")
			jw.Str(t)
			if sig := part.Get("thoughtSignature").String(); sig != "" {
				jw.Key("thought_signature")
				jw.Str(sig)
			}
			jw.EndObj()
		}
		return true
	})
	jw.EndArr()
	return hasToolUse, jw.Bytes()
}

// writeAnthropicUsageFromGemini writes the "usage" object in Anthropic format.
func writeAnthropicUsageFromGemini(jw *jsonWriter, meta gjson.Result) {
	jw.Obj()
	jw.Key("input_tokens")
	jw.Int(meta.Get("promptTokenCount").Int())
	jw.Key("output_tokens")
	jw.Int(meta.Get("candidatesTokenCount").Int())
	jw.EndObj()
}

// GeminiToOpenAIError re-wraps a Gemini error envelope as an OpenAI error.
func GeminiToOpenAIError(body []byte) []byte {
	code := gjson.GetBytes(body, "error.code")
	msg := gjson.GetBytes(body, "error.message").String()
	status := gjson.GetBytes(body, "error.status").String()
	if msg == "" && status == "" {
		return body
	}
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("error")
	jw.Obj()
	jw.Key("message")
	jw.Str(msg)
	jw.Key("type")
	jw.Str(strings.ToLower(status))
	jw.Key("param")
	jw.Null()
	jw.Key("code")
	jw.Int(code.Int())
	jw.EndObj()
	jw.EndObj()
	return jw.Bytes()
}

// mapGeminiFinishReason converts Gemini finishReason to OpenAI finish_reason.
func mapGeminiFinishReason(reason string, hasToolCalls bool) string {
	switch reason {
	case "STOP":
		if hasToolCalls {
			return "tool_calls"
		}
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "OTHER", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	case "":
		return "stop"
	default:
		return "stop"
	}
}

func mapGeminiFinishReasonToAnthropic(reason string, hasToolUse bool) string {
	switch reason {
	case "STOP":
		if hasToolUse {
			return "tool_use"
		}
		return "end_turn"
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "OTHER", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

func generateChatCmplID() string {
	return "chatcmpl-" + randomHex(8)
}

func generateToolCallID() string {
	return "call_" + randomHex(4)
}

func generateAnthropicMsgID() string {
	return "msg_" + randomHex(8)
}

func generateToolUseID() string {
	return "toolu_" + randomHex(8)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}
