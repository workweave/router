//go:build smoke

package smoke

import "testing"

// TestOpenAIResponsesAPI targets the OpenAI Responses-API path
// (internal/translate/emit_openai_responses.go), reached when a request is
// pinned to a reasoning-capable gpt-5.x model on the direct OpenAI provider
// with tools present (translate.UseOpenAIResponsesAPI). It is a distinct wire
// format from the Anthropic passthrough every other scenario in this suite
// exercises, and it strictifies tool schemas for OpenAI's structured-output
// mode — a translation step Anthropic-only testing can't reach at all.
//
// Prod repro (found live, 2026-07): a tool with a genuinely typeless optional
// parameter (e.g. Workflow's `args`, documented to accept "arrays/objects as
// actual JSON values" — no declared type by design) 400'd with "In
// context=('properties','args','anyOf','0'), schema must have a 'type' key".
// strictifyOpenAISchema's nullable-wrapping fallback (internal/translate/
// strictify_openai.go: makeNullable) wrapped the bare node in a fresh
// anyOf:[node, {type:"null"}] without checking whether node itself carried a
// strict-expressible type — unlike the anyOf-branch recursion path, which
// already guarded this via schemaHasStrictType. Fixed by applying the same
// guard in the fallback; see strictify_openai_test.go for the unit-level
// coverage. This scenario is the end-to-end companion: it proves a tool
// shaped exactly like the one that broke prod actually round-trips through
// the real OpenAI API now, not just through the translator in isolation.
func TestOpenAIResponsesAPI(t *testing.T) {
	if !cfg.OpenAIEnabled {
		t.Skip("SMOKE_OPENAI_ENABLED=0 (no OPENAI_API_KEY for this recording run)")
	}

	t.Run("tool with typeless optional arg does not 400", func(t *testing.T) {
		// Mirrors the Workflow tool's `args` param: an optional property with
		// no "type", "anyOf", or "enum" at all — deliberately accepts any JSON
		// value verbatim. Pre-fix this made strictify emit an invalid anyOf
		// and OpenAI rejected the whole tool list with a 400.
		workflowLike := tool("Workflow", "Execute a workflow script", map[string]any{
			"scriptPath": map[string]any{"type": "string", "description": "Path to a workflow script file"},
			"args":       map[string]any{"description": "Optional input value, verbatim — any JSON value, no fixed shape"},
		})

		body := newRequest("smoke-openai-typeless-arg").tokens(64).
			withTool(workflowLike).
			text("Reply with exactly the word: ok. Do not call any tool.").
			build(t)
		r := callModel(t, body, cfg.OpenAIPinModel)

		requireOKMessage(t, r)
		assertServedByModel(t, r, cfg.OpenAIPinModel, "openai")
	})

	t.Run("basic turn served by pinned gpt-5.x model", func(t *testing.T) {
		body := newRequest("smoke-openai-basic").tokens(64).
			text("Reply with exactly the word: ok").build(t)
		r := callModel(t, body, cfg.OpenAIPinModel)

		requireOKMessage(t, r)
		if r.message.Usage.InputTokens <= 0 {
			t.Errorf("want input_tokens > 0, got %d", r.message.Usage.InputTokens)
		}
		assertServedByModel(t, r, cfg.OpenAIPinModel, "openai")
	})
}
