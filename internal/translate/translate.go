// Package translate converts request/response bodies between OpenAI and
// Anthropic wire formats. This file handles non-streaming responses.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"workweave/router/internal/translate/toolcheck"

	"github.com/tidwall/gjson"
)

// AnthropicToOpenAIResponse converts a non-streaming Anthropic response to OpenAI format.
func AnthropicToOpenAIResponse(body []byte, requestModel string) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("unmarshal anthropic response: invalid JSON")
	}
	msgID := gjson.GetBytes(body, "id").String()
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		model = requestModel
	}
	stopReason := mapStopReason(gjson.GetBytes(body, "stop_reason").String())

	jw := newJSONWriter()
	jw.Obj()
	jw.Key("id")
	jw.Str(msgID)
	jw.Key("object")
	jw.Str("chat.completion")
	jw.Key("created")
	jw.Int(time.Now().Unix())
	jw.Key("model")
	jw.Str(model)
	jw.Key("choices")
	jw.Arr()
	jw.Obj()
	jw.Key("index")
	jw.Int(0)
	jw.Key("message")
	writeOpenAIMessageFromAnthropic(jw, gjson.GetBytes(body, "content"))
	jw.Key("finish_reason")
	jw.Str(stopReason)
	jw.EndObj()
	jw.EndArr()
	jw.Key("usage")
	writeOpenAIUsageFromAnthropic(jw, gjson.GetBytes(body, "usage"))
	jw.EndObj()
	return jw.Bytes(), nil
}

func writeOpenAIMessageFromAnthropic(jw *jsonWriter, content gjson.Result) {
	jw.Obj()
	jw.Key("role")
	jw.Str("assistant")

	var textParts []string
	var toolCallBlocks []gjson.Result

	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "text":
			if t := block.Get("text").String(); t != "" {
				textParts = append(textParts, t)
			}
		case "tool_use":
			toolCallBlocks = append(toolCallBlocks, block)
		}
		return true
	})

	jw.Key("content")
	if len(textParts) > 0 {
		jw.Str(strings.Join(textParts, "\n"))
	} else {
		jw.Null()
	}

	if len(toolCallBlocks) > 0 {
		jw.Key("tool_calls")
		jw.Arr()
		for _, block := range toolCallBlocks {
			jw.Obj()
			jw.Key("id")
			jw.Str(block.Get("id").String())
			jw.Key("type")
			jw.Str("function")
			jw.Key("function")
			jw.Obj()
			jw.Key("name")
			jw.Str(block.Get("name").String())
			// Anthropic input is a JSON object; OpenAI arguments is that object serialized as a string.
			jw.Key("arguments")
			jw.Str(block.Get("input").Raw)
			jw.EndObj()
			jw.EndObj()
		}
		jw.EndArr()
	}

	jw.EndObj()
}

func writeOpenAIUsageFromAnthropic(jw *jsonWriter, usage gjson.Result) {
	input := usage.Get("input_tokens").Int()
	output := usage.Get("output_tokens").Int()
	jw.Obj()
	jw.Key("prompt_tokens")
	jw.Int(input)
	jw.Key("completion_tokens")
	jw.Int(output)
	jw.Key("total_tokens")
	jw.Int(input + output)
	jw.EndObj()
}

// mapStopReason converts an Anthropic stop_reason to an OpenAI finish_reason.
func mapStopReason(reason any) string {
	s, _ := reason.(string)
	switch s {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		// claude-fable-5 safety classifiers decline with stop_reason
		// "refusal" on HTTP 200; content_filter is the OpenAI equivalent.
		return "content_filter"
	default:
		return "stop"
	}
}

// AnthropicToOpenAIError re-wraps an Anthropic error as OpenAI format.
func AnthropicToOpenAIError(body []byte) []byte {
	errType := gjson.GetBytes(body, "error.type").String()
	errMsg := gjson.GetBytes(body, "error.message").String()
	if errType == "" && errMsg == "" {
		return body
	}
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("error")
	jw.Obj()
	jw.Key("message")
	jw.Str(errMsg)
	jw.Key("type")
	jw.Str(errType)
	jw.Key("param")
	jw.Null()
	jw.Key("code")
	jw.Null()
	jw.EndObj()
	jw.EndObj()
	return jw.Bytes()
}

// OpenAIToAnthropicResponse converts a non-streaming OpenAI response to
// Anthropic Messages format.
func OpenAIToAnthropicResponse(body []byte, requestModel string) ([]byte, error) {
	out, _, err := openAIToAnthropicResponse(body, requestModel, nil, false, false)
	return out, err
}

// openAIToAnthropicResponse is the validator-aware variant: tool_use inputs
// are checked/repaired against the request's tool schemas, returning one
// toolcheck.Issue per offending block. A nil validator is syntax-check-only.
func openAIToAnthropicResponse(body []byte, requestModel string, toolValidator *toolcheck.Validator, thinkTagReasoning, escapeNormalize bool) ([]byte, []toolcheck.Issue, error) {
	if !gjson.ValidBytes(body) {
		return nil, nil, fmt.Errorf("unmarshal openai response: invalid JSON")
	}
	id := gjson.GetBytes(body, "id").String()
	if id == "" {
		// Unique per response: clients (notably ccusage) dedupe usage records
		// by message id, so a constant placeholder undercounts tokens/cost.
		id = "msg_translated_" + randomHex(8)
	}
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		model = requestModel
	}

	firstChoice := gjson.GetBytes(body, "choices.0")
	message := firstChoice.Get("message")
	finishReason := firstChoice.Get("finish_reason").String()
	// Anthropic invariant: tool_use stop_reason iff a tool_use block exists.
	// Some OpenAI-compat upstreams (GLM-5.1/DeepInfra, vLLM Qwen/MiMo) violate
	// it both ways, so we correct both, mirroring emitMessageDelta:
	//   - Promote: named tool call present but finish_reason="stop".
	//   - Demote: finish_reason="tool_calls" but every call was nameless
	//     (dropped below) — else we'd ship tool_use with zero blocks.
	if anyNamedToolCall(message.Get("tool_calls")) {
		finishReason = "tool_calls"
	} else if finishReason == "tool_calls" {
		finishReason = "stop"
	}

	jw := newJSONWriter()
	jw.Obj()
	jw.Key("id")
	jw.Str(id)
	jw.Key("type")
	jw.Str("message")
	jw.Key("role")
	jw.Str("assistant")
	jw.Key("model")
	jw.Str(model)
	jw.Key("content")
	issues := writeAnthropicContentFromOpenAI(jw, message, toolValidator, thinkTagReasoning, escapeNormalize)
	jw.Key("stop_reason")
	jw.Str(openAIFinishToAnthropicStopReason(finishReason))
	jw.Key("stop_sequence")
	jw.Null()
	jw.Key("usage")
	writeAnthropicUsageFromOpenAI(jw, gjson.GetBytes(body, "usage"))
	jw.EndObj()
	return jw.Bytes(), issues, nil
}

func writeAnthropicThinkingBlock(jw *jsonWriter, thinking string) {
	jw.Obj()
	jw.Key("type")
	jw.Str("thinking")
	jw.Key("thinking")
	jw.Str(thinking)
	jw.EndObj()
}

func writeAnthropicContentFromOpenAI(jw *jsonWriter, message gjson.Result, toolValidator *toolcheck.Validator, thinkTagReasoning, escapeNormalize bool) (issues []toolcheck.Issue) {
	jw.Arr()
	reasoning := message.Get("reasoning_content").String()
	if reasoning == "" {
		reasoning = message.Get("reasoning").String()
	}
	text := message.Get("content").String()
	if thinkTagReasoning && text != "" {
		// Reroute a leading <think>…</think> into the thinking channel; the
		// remainder (and any non-leading tag) stays text (see think_tag.go).
		var splitter thinkTagSplitter
		segs := append(splitter.Feed(text), splitter.Flush()...)
		var think, prose strings.Builder
		for _, seg := range segs {
			if seg.kind == segThinking {
				think.WriteString(seg.text)
			} else {
				prose.WriteString(seg.text)
			}
		}
		// Fold into the same thinking block as reasoning_content/reasoning,
		// matching the streaming translator so both channels shape identically.
		reasoning += think.String()
		text = prose.String()
	}
	if reasoning != "" {
		writeAnthropicThinkingBlock(jw, reasoning)
	}
	if text != "" {
		jw.Obj()
		jw.Key("type")
		jw.Str("text")
		jw.Key("text")
		jw.Str(text)
		jw.EndObj()
	}
	message.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
		id := tc.Get("id").String()
		name := tc.Get("function.name").String()
		// Drop nameless tool_calls; see anyNamedToolCall. Mirrors the
		// streaming guard in emitDelta.
		if name == "" {
			return true
		}
		argsStr := tc.Get("function.arguments").String()

		if escapeNormalize && isEditToolName(name) {
			var inputMap map[string]any
			if json.Unmarshal([]byte(argsStr), &inputMap) == nil {
				normalizeEditEscapes(escapeNormalize, name, inputMap)
				if b, err := json.Marshal(inputMap); err == nil {
					argsStr = string(b)
				}
			}
		}
		// Validate (and safely repair) args against the request's tool schema;
		// nil validator degrades to syntax-check + `{}` substitution.
		verdict := toolValidator.Check(name, argsStr)
		if verdict.Issue != nil {
			issues = append(issues, *verdict.Issue)
		}
		inputRaw := verdict.Args

		jw.Obj()
		jw.Key("type")
		jw.Str("tool_use")
		jw.Key("id")
		jw.Str(id)
		jw.Key("name")
		jw.Str(name)
		jw.Key("input")
		jw.Raw(inputRaw)
		jw.EndObj()
		return true
	})
	jw.EndArr()
	return issues
}

// anyNamedToolCall reports whether tool_calls has a call with a non-empty
// function name. Nameless tool_calls (seen intermittently from GLM/Qwen/Kimi/
// gpt-oss on vLLM/SGLang/DeepInfra) are malformed: forwarded as-is they make
// the client invoke tool "" and infinite-loop retrying, so they're dropped
// and must not drive stop_reason promotion.
func anyNamedToolCall(toolCalls gjson.Result) bool {
	found := false
	toolCalls.ForEach(func(_, tc gjson.Result) bool {
		if tc.Get("function.name").String() != "" {
			found = true
			return false
		}
		return true
	})
	return found
}

// isEditToolName reports whether name is a file-edit tool subject to escape normalization.
func isEditToolName(name string) bool {
	_, ok := editToolNames[strings.ToLower(name)]
	return ok
}

// openAIFinishToAnthropicStopReason is the non-streaming variant.
func openAIFinishToAnthropicStopReason(s string) string {
	switch s {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "stop", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func writeAnthropicUsageFromOpenAI(jw *jsonWriter, usage gjson.Result) {
	prompt := usage.Get("prompt_tokens").Int()
	completion := usage.Get("completion_tokens").Int()
	cacheRead := usage.Get("prompt_tokens_details.cached_tokens").Int()
	cacheCreation := usage.Get("prompt_tokens_details.cache_creation_tokens").Int()

	freshInput := prompt - cacheCreation - cacheRead
	if freshInput < 0 {
		freshInput = 0
	}

	jw.Obj()
	jw.Key("input_tokens")
	jw.Int(freshInput)
	jw.Key("output_tokens")
	jw.Int(completion)
	if cacheRead > 0 {
		jw.Key("cache_read_input_tokens")
		jw.Int(cacheRead)
	}
	if cacheCreation > 0 {
		jw.Key("cache_creation_input_tokens")
		jw.Int(cacheCreation)
	}
	jw.EndObj()
}

// OpenAIToAnthropicError re-wraps an OpenAI error as Anthropic format.
func OpenAIToAnthropicError(body []byte) []byte {
	errType := gjson.GetBytes(body, "error.type").String()
	errMsg := gjson.GetBytes(body, "error.message").String()
	if errType == "" && errMsg == "" {
		return body
	}
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("type")
	jw.Str("error")
	jw.Key("error")
	jw.Obj()
	jw.Key("type")
	jw.Str(errType)
	jw.Key("message")
	jw.Str(errMsg)
	jw.EndObj()
	jw.EndObj()
	return jw.Bytes()
}
