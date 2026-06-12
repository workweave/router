package translate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"

	"github.com/tidwall/gjson"
)

// PrepareOpenAIResponses builds an OpenAI Responses API (`POST /v1/responses`)
// request from an Anthropic Messages envelope. Reasoning-capable OpenAI models
// (gpt-5.x) reject `reasoning_effort` + tools on `/v1/chat/completions`, so an
// agentic Anthropic client (Claude Code) that wants the model to reason must go
// through the Responses API instead. The upstream call streams (`stream:true`):
// a non-streaming Responses call buffers the entire reasoning+output before
// emitting any response headers, which for gpt-5.x at high effort routinely
// exceeds the transport's response-header timeout and fails the turn with
// "http2: timeout awaiting response headers". Streaming returns headers + the
// first event immediately; ResponsesToAnthropicWriter translates the event
// stream into Anthropic SSE on the fly.
func (e *RequestEnvelope) PrepareOpenAIResponses(in http.Header, opts EmitOptions) (providers.PreparedRequest, error) {
	if e.format != FormatAnthropic {
		return providers.PreparedRequest{}, fmt.Errorf("PrepareOpenAIResponses: only Anthropic ingress is supported, got format %d", e.format)
	}
	body, stats, err := e.buildResponsesFromAnthropic(opts)
	if err != nil {
		return providers.PreparedRequest{}, err
	}
	return providers.PreparedRequest{Body: body, Endpoint: providers.EndpointResponses, Stats: stats}, nil
}

// ResponseTranslator is the common surface the proxy's cross-format OpenAI
// dispatch drives: an http.ResponseWriter the provider writes the upstream
// response into, plus the Prelude/Finalize/Summary lifecycle. Both
// AnthropicSSETranslator (chat/completions) and ResponsesToAnthropicWriter
// (Responses API) implement it, so the dispatch closure can pick either.
type ResponseTranslator interface {
	http.ResponseWriter
	Prelude(streaming bool) error
	Finalize() error
	Summary() ResponseSummary
}

var (
	_ ResponseTranslator = (*AnthropicSSETranslator)(nil)
	_ ResponseTranslator = (*ResponsesToAnthropicWriter)(nil)
)

// ReasoningRequested reports whether the inbound Anthropic request asks the
// model to reason (a `thinking` budget, or an explicit reasoning_effort). Used
// by the proxy to gate the Responses-API dispatch for reasoning OpenAI models.
func (e *RequestEnvelope) ReasoningRequested() bool {
	return reasoningEffortFromAnthropic(e.body) != ""
}

// reasoningEffortFromAnthropic resolves the Responses `reasoning.effort` from an
// Anthropic body: the `thinking` budget (Claude Code) via effortForBudget, or an
// explicit `reasoning_effort` if an OpenAI-format field rode along. "" = none.
func reasoningEffortFromAnthropic(body []byte) string {
	if t := gjson.GetBytes(body, "thinking"); t.Exists() && t.Get("type").String() != "disabled" {
		return effortForBudget(t.Get("budget_tokens").Int())
	}
	if r := gjson.GetBytes(body, "reasoning_effort"); r.Exists() && r.Type == gjson.String {
		return r.String()
	}
	return ""
}

// responsesReasoningEffort applies model-specific effort policy on top of the
// budget-derived effort. gpt-5.x has a measured "medium" dead-zone on hard
// agentic coding (SWE-bench Pro: low 16%, medium 0%, high 41% ≈ opus) — medium
// is strictly dominated by both neighbors, so never serve it to a gpt-5.x
// reasoning model; promote to high. Small budgets still resolve to low via
// effortForBudget, so easy traffic is untouched. Other models pass through.
func responsesReasoningEffort(eff, model string) string {
	if eff == "medium" && strings.HasPrefix(model, "gpt-5") {
		return "high"
	}
	return eff
}

func (e *RequestEnvelope) buildResponsesFromAnthropic(opts EmitOptions) ([]byte, providers.RequestMutationStats, error) {
	var stats providers.RequestMutationStats
	body, removed, err := filterClaudeCodeOnlyToolsFromAnthropicBody(e.body)
	if err != nil {
		return nil, stats, fmt.Errorf("strip claude-code-only tools: %w", err)
	}
	stats.CCOnlyToolsStripped = removed

	jw := newJSONWriter()
	jw.Obj()
	jw.Key("model")
	jw.Str(opts.TargetModel)
	jw.Key("stream")
	jw.Bool(true)
	// Stateless: we send the full history each turn and don't rely on
	// server-side state. Prior OpenAI reasoning items are round-tripped through
	// signed Anthropic thinking blocks and replayed below when the client echoes
	// them back.
	jw.Key("store")
	jw.Bool(false)

	if sys := flattenAnthropicSystemGJSON(gjson.GetBytes(body, "system")); sys != "" {
		jw.Key("instructions")
		jw.Str(sys)
	}

	reasoningEnabled := false
	if opts.Capabilities.Supports(router.CapReasoning) {
		eff := reasoningEffortFromAnthropic(body)
		if opts.ForceReasoningEffort != "" {
			eff = opts.ForceReasoningEffort
		}
		if eff := responsesReasoningEffort(eff, opts.TargetModel); eff != "" {
			reasoningEnabled = true
			jw.Key("reasoning")
			jw.Obj()
			jw.Key("effort")
			jw.Str(eff)
			jw.Key("summary")
			jw.Str("detailed")
			jw.EndObj()
		}
	}
	if reasoningEnabled {
		jw.Key("include")
		jw.Arr()
		jw.Str("reasoning.encrypted_content")
		jw.EndArr()
	}

	writeResponsesInputFromAnthropic(jw, body)
	writeResponsesToolsFromAnthropic(jw, body)
	writeResponsesToolChoiceFromAnthropic(jw, body)

	if mt := gjson.GetBytes(body, "max_tokens"); mt.Exists() && mt.Type == gjson.Number {
		jw.Key("max_output_tokens")
		jw.Int(clampToModelOutputCap(mt.Int(), opts.TargetModel))
	}
	// NB: reasoning models reject temperature != 1 on the Responses API, so we
	// deliberately omit temperature/top_p here.

	jw.EndObj()
	return jw.Bytes(), stats, nil
}

// writeResponsesInputFromAnthropic emits the `input` array: Anthropic messages
// become Responses input items — user/assistant text as typed messages,
// signed OpenAI thinking as reasoning, tool_use as function_call, tool_result as
// function_call_output.
func writeResponsesInputFromAnthropic(jw *jsonWriter, body []byte) {
	jw.Key("input")
	jw.Arr()
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		content := msg.Get("content")
		if content.Type == gjson.String {
			writeResponsesTextMessage(jw, role, content.String())
			return true
		}
		if content.Type != gjson.JSON || !content.IsArray() {
			return true
		}
		var textParts []string
		emittedReasoningSignatures := map[string]struct{}{}
		content.ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").String() {
			case "text":
				if t := block.Get("text").String(); t != "" {
					textParts = append(textParts, t)
				}
			case "thinking":
				// Flush buffered assistant text before the reasoning item so
				// turn order (text → reasoning → tool_use) is preserved.
				sig := block.Get("signature").String()
				if _, emitted := emittedReasoningSignatures[sig]; emitted || !decodeOpenAIReasoningSignatureValid(sig) {
					return true
				}
				if len(textParts) > 0 {
					writeResponsesTextMessage(jw, role, joinNonEmpty(textParts))
					textParts = nil
				}
				emitResponsesReasoningItem(jw, sig)
				emittedReasoningSignatures[sig] = struct{}{}
			case "tool_use":
				// Emit any buffered text first so ordering is preserved.
				callID, sig := extractOpenAIReasoningSignatureFromID(block.Get("id").String())
				if len(textParts) > 0 {
					writeResponsesTextMessage(jw, role, joinNonEmpty(textParts))
					textParts = nil
				}
				// Replay the reasoning item carried on the tool id (the
				// Claude Code round-trip drops the thinking block but keeps the
				// tool_use id) when it wasn't already emitted from a thinking block.
				if sig != "" {
					if _, emitted := emittedReasoningSignatures[sig]; !emitted && emitResponsesReasoningItem(jw, sig) {
						emittedReasoningSignatures[sig] = struct{}{}
					}
				}
				jw.Obj()
				jw.Key("type")
				jw.Str("function_call")
				jw.Key("call_id")
				jw.Str(clampOpenAIToolCallID(callID))
				jw.Key("name")
				jw.Str(block.Get("name").String())
				inputRaw := block.Get("input").Raw
				if inputRaw == "" {
					inputRaw = "{}"
				}
				jw.Key("arguments")
				jw.Str(inputRaw)
				jw.EndObj()
			case "tool_result":
				if len(textParts) > 0 {
					writeResponsesTextMessage(jw, role, joinNonEmpty(textParts))
					textParts = nil
				}
				jw.Obj()
				jw.Key("type")
				jw.Str("function_call_output")
				jw.Key("call_id")
				callID, _ := extractOpenAIReasoningSignatureFromID(block.Get("tool_use_id").String())
				jw.Str(clampOpenAIToolCallID(callID))
				jw.Key("output")
				jw.Str(flattenAnthropicToolResultContent(block.Get("content")))
				jw.EndObj()
			}
			return true
		})
		if len(textParts) > 0 {
			writeResponsesTextMessage(jw, role, joinNonEmpty(textParts))
		}
		return true
	})
	jw.EndArr()
}

func decodeOpenAIReasoningSignatureValid(sig string) bool {
	_, _, ok := decodeOpenAIReasoningSignature(sig)
	return ok
}

func emitResponsesReasoningItem(jw *jsonWriter, sig string) bool {
	id, enc, ok := decodeOpenAIReasoningSignature(sig)
	if !ok {
		return false
	}
	jw.Obj()
	jw.Key("type")
	jw.Str("reasoning")
	jw.Key("id")
	jw.Str(id)
	jw.Key("encrypted_content")
	jw.Str(enc)
	jw.Key("summary")
	jw.Arr()
	jw.EndArr()
	jw.EndObj()
	return true
}

// writeResponsesTextMessage emits one Responses input message with a single
// typed text part (input_text for user, output_text for assistant).
func writeResponsesTextMessage(jw *jsonWriter, role, text string) {
	if text == "" {
		return
	}
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	jw.Obj()
	jw.Key("role")
	jw.Str(role)
	jw.Key("content")
	jw.Arr()
	jw.Obj()
	jw.Key("type")
	jw.Str(partType)
	jw.Key("text")
	jw.Str(text)
	jw.EndObj()
	jw.EndArr()
	jw.EndObj()
}

// flattenAnthropicToolResultContent flattens an Anthropic tool_result `content`
// (string or array of text blocks) into a single string for function_call_output.
func flattenAnthropicToolResultContent(content gjson.Result) string {
	switch content.Type {
	case gjson.String:
		return content.String()
	case gjson.JSON:
		if !content.IsArray() {
			return content.Raw
		}
		var parts []string
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "text" {
				if t := block.Get("text").String(); t != "" {
					parts = append(parts, t)
				}
			}
			return true
		})
		return joinNonEmpty(parts)
	default:
		return ""
	}
}

// writeResponsesToolsFromAnthropic emits the Responses flat function-tool shape
// (`{type:"function", name, description, parameters}` — no nested wrapper).
//
// Each tool whose schema survives strictifyOpenAISchema is emitted with
// `strict:true` + the strictified parameters, turning on grammar-constrained
// decoding so gpt-5.x cannot emit out-of-schema arguments at all (the
// prevention layer in front of toolcheck's detect/repair). Tools whose
// schemas can't be faithfully strictified fall back to non-strict emission of
// the original schema — never fail the request over strictness. Note the
// proxy-side validator (toolcheck) still checks against the ORIGINAL schema:
// strict mode makes optionals nullable, and the explicit nulls gpt-5.x then
// emits are dropped by toolcheck's normalize pass before reaching the client.
func writeResponsesToolsFromAnthropic(jw *jsonWriter, body []byte) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() || tools.Get("#").Int() == 0 {
		return
	}
	jw.Key("tools")
	jw.Arr()
	count := 0
	tools.ForEach(func(_, tool gjson.Result) bool {
		if count >= openAIMaxTools {
			return false
		}
		count++
		var params any
		strict := false
		if schema := tool.Get("input_schema"); schema.Exists() {
			_ = json.Unmarshal([]byte(schema.Raw), &params)
			params = inlineSchemaDefs(params)
			sanitizeOpenAIToolSchema(params)
			if strictParams, ok := strictifyOpenAISchema(params); ok {
				params = strictParams
				strict = true
			} else {
				observability.Get().Info("Responses strictify fallback — emitting non-strict tool",
					"tool_name", tool.Get("name").String())
			}
		}
		jw.Obj()
		jw.Key("type")
		jw.Str("function")
		jw.Key("name")
		jw.Str(tool.Get("name").String())
		if desc := tool.Get("description"); desc.Exists() {
			jw.Key("description")
			jw.Raw(desc.Raw)
		}
		if params != nil {
			if paramBytes, err := json.Marshal(params); err == nil {
				jw.Key("parameters")
				jw.RawBytes(paramBytes)
				jw.Key("strict")
				jw.Bool(strict)
			}
		}
		jw.EndObj()
		return true
	})
	jw.EndArr()
}

// writeResponsesToolChoiceFromAnthropic maps the Anthropic tool_choice to the
// Responses tool_choice shape.
func writeResponsesToolChoiceFromAnthropic(jw *jsonWriter, body []byte) {
	tc := gjson.GetBytes(body, "tool_choice")
	if !tc.Exists() {
		return
	}
	switch tc.Get("type").String() {
	case "auto":
		jw.Key("tool_choice")
		jw.Str("auto")
	case "any":
		jw.Key("tool_choice")
		jw.Str("required")
	case "tool":
		if name := tc.Get("name").String(); name != "" {
			jw.Key("tool_choice")
			jw.Obj()
			jw.Key("type")
			jw.Str("function")
			jw.Key("name")
			jw.Str(name)
			jw.EndObj()
		}
	}
}

func joinNonEmpty(parts []string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += p
	}
	return out
}
