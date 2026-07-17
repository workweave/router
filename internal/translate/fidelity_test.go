package translate_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareGemini_SchemaFidelity(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		wantErr error
		check   func(*testing.T, map[string]any)
	}{
		{
			name:   "const preserves numeric type",
			schema: `{"type":"number","const":7}`,
			check: func(t *testing.T, schema map[string]any) {
				assert.Equal(t, []any{float64(7)}, schema["enum"])
			},
		},
		{
			name:   "allOf merges disjoint object properties",
			schema: `{"allOf":[{"type":"object","properties":{"left":{"type":"string"}},"required":["left"]},{"type":"object","properties":{"right":{"type":"boolean"}},"required":["right"]}]}`,
			check: func(t *testing.T, schema map[string]any) {
				assert.Contains(t, schema["properties"].(map[string]any), "left")
				assert.Contains(t, schema["properties"].(map[string]any), "right")
				assert.ElementsMatch(t, []any{"left", "right"}, schema["required"])
			},
		},
		{
			name:    "allOf conflict rejects",
			schema:  `{"allOf":[{"type":"string"},{"type":"number"}]}`,
			wantErr: translate.ErrGeminiSchemaIncompatible,
		},
		{
			name:   "anyOf preserves every branch",
			schema: `{"anyOf":[{"type":"string"},{"type":"number"}]}`,
			check: func(t *testing.T, schema map[string]any) {
				assert.Len(t, schema["anyOf"], 2)
			},
		},
		{
			name:    "oneOf rejects rather than selecting a branch",
			schema:  `{"oneOf":[{"type":"string"},{"type":"number"}]}`,
			wantErr: translate.ErrGeminiSchemaIncompatible,
		},
		{
			name:    "unresolved reference rejects",
			schema:  `{"$ref":"#/$defs/Missing","$defs":{"Present":{"type":"string"}}}`,
			wantErr: translate.ErrGeminiSchemaIncompatible,
		},
		{
			name:    "unsupported constraint rejects instead of dropping",
			schema:  `{"type":"object","additionalProperties":false}`,
			wantErr: translate.ErrGeminiSchemaIncompatible,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"tool","input_schema":` + tt.schema + `}]}`)
			env, err := translate.ParseAnthropic(body)
			require.NoError(t, err)
			prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			var out map[string]any
			require.NoError(t, json.Unmarshal(prep.Body, &out))
			schema := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
			tt.check(t, schema)
		})
	}
}

func TestPrepareGemini_DeduplicatesFunctionDeclarations(t *testing.T) {
	tests := []struct {
		name        string
		body        []byte
		parse       func([]byte) (*translate.RequestEnvelope, error)
		wantErr     error
		declaration func(map[string]any) []any
	}{
		{
			name:  "OpenAI identical duplicates collapse",
			body:  []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read","description":"read","parameters":{"type":"object"}}},{"type":"function","function":{"name":"read","description":"read","parameters":{"type":"object"}}}]}`),
			parse: translate.ParseOpenAI,
			declaration: func(out map[string]any) []any {
				return out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
			},
		},
		{
			name:    "Anthropic conflicting duplicates reject",
			body:    []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"read","description":"one","input_schema":{"type":"object"}},{"name":"read","description":"two","input_schema":{"type":"object"}}]}`),
			parse:   translate.ParseAnthropic,
			wantErr: translate.ErrGeminiToolDeclarationConflict,
		},
		{
			name:  "Gemini identical duplicates collapse",
			body:  []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"read","description":"read","parameters":{"type":"object"}},{"name":"read","description":"read","parameters":{"type":"object"}}]}]}`),
			parse: translate.ParseGemini,
			declaration: func(out map[string]any) []any {
				return out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := tt.parse(tt.body)
			require.NoError(t, err)
			prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			var out map[string]any
			require.NoError(t, json.Unmarshal(prep.Body, &out))
			assert.Len(t, tt.declaration(out), 1)
		})
	}
}

func TestPrepareOpenAIResponses_PreservesMediumReasoningEffort(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`))
	require.NoError(t, err)
	prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{TargetModel: "gpt-5.5", Capabilities: router.Lookup("gpt-5.5")})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, "medium", out["reasoning"].(map[string]any)["effort"])
}

func TestAdaptiveReasoningDelegatesToCrossFormatTargetDefault(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(`{"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"}}`))
	require.NoError(t, err)

	t.Run("OpenAI Responses", func(t *testing.T) {
		prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{TargetModel: "gpt-5.5", Capabilities: router.Lookup("gpt-5.5")})
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(prep.Body, &out))
		assert.NotContains(t, out, "reasoning")
	})

	t.Run("Gemini", func(t *testing.T) {
		prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(prep.Body, &out))
		if config, ok := out["generationConfig"].(map[string]any); ok {
			assert.NotContains(t, config, "thinkingConfig")
		}
	})
}

func TestApplyReasoningIntent_ClampsAndRejectsUnsupportedSemantics(t *testing.T) {
	spec := router.NewSpecWithReasoning(router.ReasoningCapabilities{Levels: []string{"low", "medium", "high"}})
	clamped, err := translate.ApplyReasoningIntent(translate.ReasoningIntent{Kind: translate.ReasoningLevel, Level: "xhigh", Explicit: true}, spec, "")
	require.NoError(t, err)
	assert.Equal(t, "high", clamped.Level)
	assert.NotEmpty(t, clamped.NormalizationNotes)

	_, err = translate.ApplyReasoningIntent(translate.ReasoningIntent{Kind: translate.ReasoningBudget, BudgetTokens: 2048, Explicit: true}, spec, "")
	require.ErrorIs(t, err, translate.ErrReasoningIncompatible)
}

func TestPrepareAnthropic_ClientCacheControlFidelity(t *testing.T) {
	t.Run("ttl is preserved and router uses remaining capacity", func(t *testing.T) {
		env, err := translate.ParseOpenAI([]byte(`{"messages":[{"role":"system","content":"rules","cache_control":{"type":"ephemeral","ttl":"1h"}},{"role":"user","content":"hi"}]}`))
		require.NoError(t, err)
		prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-opus-4-8"})
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(prep.Body, &out))
		system := out["system"].([]any)
		assert.Equal(t, map[string]any{"type": "ephemeral", "ttl": "1h"}, system[0].(map[string]any)["cache_control"])
		lastMessage := out["messages"].([]any)[0].(map[string]any)
		lastBlock := lastMessage["content"].([]any)[0].(map[string]any)
		assert.Equal(t, map[string]any{"type": "ephemeral"}, lastBlock["cache_control"])
	})

	t.Run("explicit overflow returns a stable validation error", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"system","content":"one","cache_control":{"type":"ephemeral"}},{"role":"system","content":"two","cache_control":{"type":"ephemeral"}},{"role":"system","content":"three","cache_control":{"type":"ephemeral"}},{"role":"system","content":"four","cache_control":{"type":"ephemeral"}},{"role":"system","content":"five","cache_control":{"type":"ephemeral"}}]}`)
		env, err := translate.ParseOpenAI(body)
		require.NoError(t, err)
		_, err = env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-opus-4-8"})
		require.ErrorIs(t, err, translate.ErrAnthropicCacheControlOverflow)
		assert.False(t, errors.Is(err, translate.ErrAnthropicCacheControlInvalid))
	})
}
