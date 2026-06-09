package translate

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strictifyFromJSON(t *testing.T, schema string) (map[string]any, bool) {
	t.Helper()
	var parsed any
	require.NoError(t, json.Unmarshal([]byte(schema), &parsed))
	out, ok := strictifyOpenAISchema(parsed)
	if !ok {
		return nil, false
	}
	m, isMap := out.(map[string]any)
	require.True(t, isMap)
	return m, true
}

func TestStrictify_OptionalBecomesNullableAndRequired(t *testing.T) {
	out, ok := strictifyFromJSON(t, `{
		"type":"object",
		"properties":{
			"file_path":{"type":"string"},
			"pages":{"type":"string"}
		},
		"required":["file_path"]
	}`)
	require.True(t, ok)

	assert.Equal(t, false, out["additionalProperties"])
	assert.ElementsMatch(t, []any{"file_path", "pages"}, out["required"].([]any),
		"strict mode requires every property listed in required")

	props := out["properties"].(map[string]any)
	filePath := props["file_path"].(map[string]any)
	assert.Equal(t, "string", filePath["type"], "originally-required props keep their type")
	pages := props["pages"].(map[string]any)
	assert.Equal(t, []any{"string", "null"}, pages["type"],
		"originally-optional props become nullable so the model can still 'omit' them")
}

func TestStrictify_NestedObjectsGetAdditionalPropertiesFalse(t *testing.T) {
	out, ok := strictifyFromJSON(t, `{
		"type":"object",
		"properties":{
			"opts":{
				"type":"object",
				"properties":{"depth":{"type":"integer"}},
				"required":["depth"]
			}
		},
		"required":["opts"]
	}`)
	require.True(t, ok)

	opts := out["properties"].(map[string]any)["opts"].(map[string]any)
	assert.Equal(t, false, opts["additionalProperties"], "every object node is closed, not just the root")
	assert.Equal(t, []any{"depth"}, opts["required"].([]any))
}

func TestStrictify_DroppedConstraintsMoveToDescription(t *testing.T) {
	out, ok := strictifyFromJSON(t, `{
		"type":"object",
		"properties":{
			"pattern_field":{"type":"string","pattern":"^a+$","description":"letters"},
			"bounded":{"type":"integer","minimum":1,"maximum":10}
		},
		"required":["pattern_field","bounded"]
	}`)
	require.True(t, ok)

	props := out["properties"].(map[string]any)
	pf := props["pattern_field"].(map[string]any)
	assert.NotContains(t, pf, "pattern", "strict mode rejects pattern — it must be stripped")
	assert.Contains(t, pf["description"], "letters", "original description survives")
	assert.Contains(t, pf["description"], "pattern: ^a+$", "the constraint stays visible as guidance")

	bounded := props["bounded"].(map[string]any)
	assert.NotContains(t, bounded, "minimum")
	assert.NotContains(t, bounded, "maximum")
	assert.Contains(t, bounded["description"], "minimum: 1")
	assert.Contains(t, bounded["description"], "maximum: 10")
}

func TestStrictify_ArrayItemsRecurse(t *testing.T) {
	out, ok := strictifyFromJSON(t, `{
		"type":"object",
		"properties":{
			"edits":{"type":"array","items":{"type":"object","properties":{"old":{"type":"string"}},"required":["old"]}}
		},
		"required":["edits"]
	}`)
	require.True(t, ok)

	edits := out["properties"].(map[string]any)["edits"].(map[string]any)
	items := edits["items"].(map[string]any)
	assert.Equal(t, false, items["additionalProperties"])
}

func TestStrictify_EnumOptionalWrapsInAnyOf(t *testing.T) {
	out, ok := strictifyFromJSON(t, `{
		"type":"object",
		"properties":{"mode":{"enum":["a","b"]}},
		"required":[]
	}`)
	require.True(t, ok)

	mode := out["properties"].(map[string]any)["mode"].(map[string]any)
	branches, has := mode["anyOf"].([]any)
	require.True(t, has, "a typeless enum optional is made nullable via anyOf")
	require.Len(t, branches, 2)
	assert.Equal(t, map[string]any{"type": "null"}, branches[1])
}

func TestStrictify_BailsOnUnsupportedConstructs(t *testing.T) {
	cases := []struct {
		name   string
		schema string
	}{
		{"root not object", `{"type":"string"}`},
		{"root not a schema object", `["nope"]`},
		{"oneOf", `{"type":"object","properties":{"x":{"oneOf":[{"type":"string"},{"type":"integer"}]}},"required":["x"]}`},
		{"allOf", `{"type":"object","properties":{"x":{"allOf":[{"type":"string"}]}},"required":["x"]}`},
		{"patternProperties", `{"type":"object","patternProperties":{"^a":{"type":"string"}}}`},
		{"unresolved ref", `{"type":"object","properties":{"x":{"$ref":"#/$defs/missing"}},"required":["x"]}`},
		{"tuple items", `{"type":"object","properties":{"x":{"type":"array","items":[{"type":"string"}]}},"required":["x"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var parsed any
			require.NoError(t, json.Unmarshal([]byte(tc.schema), &parsed))
			_, ok := strictifyOpenAISchema(parsed)
			assert.False(t, ok, "must fall back to non-strict emission")
		})
	}
}

func TestStrictify_DoesNotMutateInput(t *testing.T) {
	const raw = `{"type":"object","properties":{"pages":{"type":"string"}},"required":[]}`
	var parsed any
	require.NoError(t, json.Unmarshal([]byte(raw), &parsed))
	_, ok := strictifyOpenAISchema(parsed)
	require.True(t, ok)

	reMarshaled, err := json.Marshal(parsed)
	require.NoError(t, err)
	assert.JSONEq(t, raw, string(reMarshaled),
		"the caller may emit the original schema on other paths — strictify must not mutate it")
}
