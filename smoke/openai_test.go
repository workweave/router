//go:build smoke

package smoke

import "testing"

// TestOpenAIResponsesAPI exercises the OpenAI Responses-API path, including a tool
// with a typeless optional param — end-to-end companion to strictify_openai_test.go.
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
