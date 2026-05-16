package translate_test

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// firstToolFunction returns `tools[0].function` from an emitted OpenAI body.
func firstToolFunction(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	tools, _ := doc["tools"].([]any)
	require.NotEmpty(t, tools, "expected at least one tool in emitted body")
	tool, _ := tools[0].(map[string]any)
	require.NotNil(t, tool)
	fn, _ := tool["function"].(map[string]any)
	require.NotNil(t, fn)
	return fn
}

func TestStrictTools_AnthropicSource_SetsStrictAndTightensSchemaForDeepSeek(t *testing.T) {
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"Edit",
			"description":"edit a file",
			"input_schema":{
				"type":"object",
				"properties":{
					"file_path":{"type":"string"},
					"old_string":{"type":"string"},
					"new_string":{"type":"string"},
					"replace_all":{"type":"boolean"}
				},
				"required":["file_path","old_string","new_string"]
			}
		}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	fn := firstToolFunction(t, out.Body)
	assert.Equal(t, true, fn["strict"], "deepseek/* must get strict:true on function defs")

	params, _ := fn["parameters"].(map[string]any)
	require.NotNil(t, params)
	assert.Equal(t, false, params["additionalProperties"], "object schemas must close additionalProperties for strict mode")

	required, _ := params["required"].([]any)
	assert.ElementsMatch(t, []any{"file_path", "old_string", "new_string", "replace_all"}, required,
		"strict mode requires every property to appear in required")

	props, _ := params["properties"].(map[string]any)
	replaceAll, _ := props["replace_all"].(map[string]any)
	require.NotNil(t, replaceAll)
	assert.ElementsMatch(t, []any{"boolean", "null"}, replaceAll["type"],
		"previously-optional properties must accept null to preserve optional semantics")

	filePath, _ := props["file_path"].(map[string]any)
	assert.Equal(t, "string", filePath["type"], "originally-required properties keep their scalar type")
}

func TestStrictTools_AnthropicSource_NoStrictForOtherModels(t *testing.T) {
	cases := []string{"gpt-5", "moonshotai/kimi-k2.5", "qwen/qwen3-max", "google/gemini-2.5-pro"}
	for _, model := range cases {
		t.Run(model, func(t *testing.T) {
			src := []byte(`{
				"model":"claude-opus-4-7",
				"messages":[{"role":"user","content":"hi"}],
				"tools":[{
					"name":"Edit",
					"description":"edit",
					"input_schema":{"type":"object","properties":{"x":{"type":"string"}}}
				}],
				"max_tokens":256
			}`)
			env, err := translate.ParseAnthropic(src)
			require.NoError(t, err)

			out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: model})
			require.NoError(t, err)

			fn := firstToolFunction(t, out.Body)
			_, hasStrict := fn["strict"]
			assert.False(t, hasStrict, "non-deepseek targets must not get strict mode")

			params, _ := fn["parameters"].(map[string]any)
			_, hasAdditional := params["additionalProperties"]
			assert.False(t, hasAdditional, "non-strict targets keep schemas untouched")
		})
	}
}

func TestStrictTools_AnthropicSource_PropagatesIntoNestedObject(t *testing.T) {
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"NestedTool",
			"description":"nested",
			"input_schema":{
				"type":"object",
				"properties":{
					"opts":{
						"type":"object",
						"properties":{
							"flag":{"type":"boolean"}
						}
					}
				},
				"required":["opts"]
			}
		}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	fn := firstToolFunction(t, out.Body)
	params, _ := fn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	opts, _ := props["opts"].(map[string]any)
	require.NotNil(t, opts)
	assert.Equal(t, false, opts["additionalProperties"], "nested objects must also close additionalProperties")
	required, _ := opts["required"].([]any)
	assert.ElementsMatch(t, []any{"flag"}, required, "strict mode propagates to nested objects")
}

func TestStrictTools_AnthropicSource_OptionalNestedObjectStillTightened(t *testing.T) {
	// Regression: an optional nested object schema must still get strict-mode
	// invariants (additionalProperties:false, all properties moved to
	// required) and must appear in its parent's `required` list. The `type`
	// itself stays the scalar "object" — emitting `["object", "null"]`
	// would 400 against DeepSeek's tool-schema parser, which only accepts
	// string|number|integer|boolean|null as members of a type array.
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"NestedOptional",
			"description":"nested optional",
			"input_schema":{
				"type":"object",
				"properties":{
					"opts":{
						"type":"object",
						"properties":{
							"flag":{"type":"boolean"}
						}
					}
				}
			}
		}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	fn := firstToolFunction(t, out.Body)
	params, _ := fn["parameters"].(map[string]any)
	parentRequired, _ := params["required"].([]any)
	assert.Contains(t, parentRequired, "opts",
		"strict mode forces every property into required; the optional becomes effectively required")

	props, _ := params["properties"].(map[string]any)
	opts, _ := props["opts"].(map[string]any)
	require.NotNil(t, opts)

	assert.Equal(t, "object", opts["type"],
		"keep scalar `type: object` — DeepSeek's parser rejects 'object' as a member of a type array")
	assert.Equal(t, false, opts["additionalProperties"],
		"nested object still needs additionalProperties:false for strict mode")
	required, _ := opts["required"].([]any)
	assert.ElementsMatch(t, []any{"flag"}, required,
		"nested object still needs its properties moved to required")
}

func TestStrictTools_AnthropicSource_OptionalNestedArrayKeepsScalarType(t *testing.T) {
	// Mirror of OptionalNestedObject for `type: array`. DeepSeek's parser
	// rejects "array" inside a type union the same way it rejects "object",
	// so the strict-mode pass must keep arrays scalar.
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"NestedArray",
			"description":"nested optional array",
			"input_schema":{
				"type":"object",
				"properties":{
					"items":{
						"type":"array",
						"items":{"type":"string"}
					}
				}
			}
		}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	fn := firstToolFunction(t, out.Body)
	params, _ := fn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	items, _ := props["items"].(map[string]any)
	require.NotNil(t, items)

	assert.Equal(t, "array", items["type"],
		"keep scalar `type: array` — DeepSeek's parser rejects 'array' as a member of a type array")
}

func TestStrictTools_AnthropicSource_EmptyObjectSchemaStaysPermissive(t *testing.T) {
	// DeepSeek rejects {type: object, additionalProperties: false} when
	// there are no properties: "An object with no properties is not
	// allowed." Strict-mode constrained decoding can't constrain an
	// object with no declared shape anyway, so the strict-mode pass must
	// leave additionalProperties unset (defaults to true) on those
	// subschemas. Top-level params here are non-empty so the tool stays
	// strict-eligible; the nested `metadata` field is what would have
	// produced the offending closed-empty object.
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"WithEmptyObject",
			"description":"has a nested object with no declared shape",
			"input_schema":{
				"type":"object",
				"properties":{
					"name":{"type":"string"},
					"metadata":{"type":"object"}
				},
				"required":["name"]
			}
		}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	fn := firstToolFunction(t, out.Body)
	params, _ := fn["parameters"].(map[string]any)
	// Root has properties so strict-mode invariants still apply at the top.
	assert.Equal(t, false, params["additionalProperties"],
		"non-empty root params still get additionalProperties:false for strict mode")

	props, _ := params["properties"].(map[string]any)
	metadata, _ := props["metadata"].(map[string]any)
	require.NotNil(t, metadata)
	assert.Equal(t, "object", metadata["type"])
	_, hasAdditional := metadata["additionalProperties"]
	assert.False(t, hasAdditional,
		"empty nested object must NOT carry additionalProperties:false — DeepSeek rejects closed-empty objects")
	_, hasRequired := metadata["required"]
	assert.False(t, hasRequired,
		"empty nested object must NOT carry required:[] — that's the same closed-empty shape DeepSeek rejects")
}

func TestStrictTools_AnthropicSource_RecursesIntoDefsAndDefinitions(t *testing.T) {
	// Schemas using $ref / $defs reuse type definitions across properties.
	// OpenAI strict mode requires object schemas inside $defs also have
	// additionalProperties:false and all properties in required, otherwise
	// validation 400s once the resolved schema is materialized upstream.
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"WithDefs",
			"description":"uses defs",
			"input_schema":{
				"type":"object",
				"properties":{"point":{"$ref":"#/$defs/Point"}},
				"required":["point"],
				"$defs":{
					"Point":{
						"type":"object",
						"properties":{
							"x":{"type":"number"},
							"y":{"type":"number"}
						},
						"required":["x"]
					}
				},
				"definitions":{
					"Legacy":{
						"type":"object",
						"properties":{"name":{"type":"string"}}
					}
				}
			}
		}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	fn := firstToolFunction(t, out.Body)
	params, _ := fn["parameters"].(map[string]any)

	defs, _ := params["$defs"].(map[string]any)
	require.NotNil(t, defs, "$defs must survive emit")
	point, _ := defs["Point"].(map[string]any)
	require.NotNil(t, point)
	assert.Equal(t, false, point["additionalProperties"], "$defs entries must get additionalProperties:false")
	pointRequired, _ := point["required"].([]any)
	assert.ElementsMatch(t, []any{"x", "y"}, pointRequired, "$defs entries must move all properties to required")

	definitions, _ := params["definitions"].(map[string]any)
	require.NotNil(t, definitions, "legacy definitions must survive emit")
	legacy, _ := definitions["Legacy"].(map[string]any)
	require.NotNil(t, legacy)
	assert.Equal(t, false, legacy["additionalProperties"], "legacy definitions also get tightened")
}

func TestStrictTools_OpenAISource_SetsStrictAndTightensSchemaForDeepSeek(t *testing.T) {
	src := []byte(`{
		"model":"x",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"Edit",
				"parameters":{
					"type":"object",
					"properties":{
						"file_path":{"type":"string"},
						"replace_all":{"type":"boolean"}
					},
					"required":["file_path"]
				}
			}
		}],
		"max_tokens":256
	}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	fn := firstToolFunction(t, out.Body)
	assert.Equal(t, true, fn["strict"], "OpenAI source path must also set strict on function defs for deepseek/*")

	params, _ := fn["parameters"].(map[string]any)
	require.NotNil(t, params)
	assert.Equal(t, false, params["additionalProperties"])
	required, _ := params["required"].([]any)
	assert.ElementsMatch(t, []any{"file_path", "replace_all"}, required)
}

func TestStrictTools_NoTools_NoMutation(t *testing.T) {
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out.Body, &doc))
	_, hasTools := doc["tools"]
	assert.False(t, hasTools, "no tools in source means no tools field in output")
}
