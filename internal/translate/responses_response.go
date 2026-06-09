package translate

import (
	"fmt"

	"workweave/router/internal/translate/toolcheck"

	"github.com/tidwall/gjson"
)

// ResponsesToAnthropicResponse converts a non-streaming OpenAI Responses
// `response` object into a non-streaming Anthropic Messages response. The
// Responses `output` array carries reasoning / message / function_call items,
// which map to Anthropic thinking / text / tool_use content blocks.
func ResponsesToAnthropicResponse(body []byte, requestModel string) ([]byte, error) {
	out, _, err := responsesToAnthropicResponse(body, requestModel, nil)
	return out, err
}

// responsesToAnthropicResponse is the validator-aware variant: tool_use
// inputs are checked (and safely repaired) against the request's tool schemas
// via toolValidator, with one toolcheck.Issue returned per offending block. A
// nil validator degrades to syntax-check-only.
func responsesToAnthropicResponse(body []byte, requestModel string, toolValidator *toolcheck.Validator) ([]byte, []toolcheck.Issue, error) {
	if !gjson.ValidBytes(body) {
		return nil, nil, fmt.Errorf("unmarshal responses response: invalid JSON")
	}
	root := gjson.ParseBytes(body)

	id := root.Get("id").String()
	if id == "" {
		id = "msg_" + newResponsesID("resp")
	}
	model := root.Get("model").String()
	if model == "" {
		model = requestModel
	}

	hasToolCall := false
	root.Get("output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "function_call" {
			hasToolCall = true
			return false
		}
		return true
	})

	stopReason := "end_turn"
	if hasToolCall {
		stopReason = "tool_use"
	} else if root.Get("incomplete_details.reason").String() == "max_output_tokens" ||
		root.Get("status").String() == "incomplete" {
		stopReason = "max_tokens"
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
	jw.Arr()
	var issues []toolcheck.Issue
	pendingReasoningSignature := ""
	root.Get("output").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "reasoning":
			text := joinReasoningSummary(item.Get("summary"))
			sig := encodeOpenAIReasoningSignature(item.Get("id").String(), item.Get("encrypted_content").String())
			if sig != "" {
				pendingReasoningSignature = sig
			}
			if text == "" && sig == "" {
				return true
			}
			jw.Obj()
			jw.Key("type")
			jw.Str("thinking")
			jw.Key("thinking")
			jw.Str(text)
			if sig != "" {
				jw.Key("signature")
				jw.Str(sig)
			}
			jw.EndObj()
		case "message":
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "output_text" {
					if t := part.Get("text").String(); t != "" {
						jw.Obj()
						jw.Key("type")
						jw.Str("text")
						jw.Key("text")
						jw.Str(t)
						jw.EndObj()
					}
				}
				return true
			})
		case "function_call":
			jw.Obj()
			jw.Key("type")
			jw.Str("tool_use")
			jw.Key("id")
			id := item.Get("call_id").String()
			if pendingReasoningSignature != "" {
				id = embedOpenAIReasoningSignatureInID(id, pendingReasoningSignature)
				pendingReasoningSignature = ""
			}
			jw.Str(id)
			jw.Key("name")
			name := item.Get("name").String()
			jw.Str(name)
			jw.Key("input")
			// Validate (and safely repair) the args against the request's
			// tool schema; with a nil validator this degrades to the historic
			// syntax-check + `{}` substitution.
			verdict := toolValidator.Check(name, item.Get("arguments").String())
			if verdict.Issue != nil {
				issues = append(issues, *verdict.Issue)
			}
			jw.Raw(verdict.Args)
			jw.EndObj()
		}
		return true
	})
	jw.EndArr()

	jw.Key("stop_reason")
	jw.Str(stopReason)
	jw.Key("stop_sequence")
	jw.Null()

	usage := root.Get("usage")
	jw.Key("usage")
	jw.Obj()
	jw.Key("input_tokens")
	jw.Int(usage.Get("input_tokens").Int())
	jw.Key("output_tokens")
	jw.Int(usage.Get("output_tokens").Int())
	if cached := usage.Get("input_tokens_details.cached_tokens"); cached.Exists() {
		jw.Key("cache_read_input_tokens")
		jw.Int(cached.Int())
	}
	jw.EndObj()

	jw.EndObj()
	return jw.Bytes(), issues, nil
}

// joinReasoningSummary flattens a Responses reasoning `summary` array (items of
// {type:"summary_text", text}) into a single string.
func joinReasoningSummary(summary gjson.Result) string {
	if !summary.Exists() || !summary.IsArray() {
		return ""
	}
	var parts []string
	summary.ForEach(func(_, s gjson.Result) bool {
		if t := s.Get("text").String(); t != "" {
			parts = append(parts, t)
		}
		return true
	})
	return joinNonEmpty(parts)
}

// ResponsesToAnthropicError maps a Responses-API error body to an Anthropic
// error envelope.
func ResponsesToAnthropicError(body []byte) []byte {
	errType := gjson.GetBytes(body, "error.type").String()
	errMsg := gjson.GetBytes(body, "error.message").String()
	if errMsg == "" {
		errMsg = gjson.GetBytes(body, "error").String()
	}
	if errType == "" {
		errType = "api_error"
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
