package toolcheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readTools mirrors the Claude Code Read tool shape: one required param,
// several optionals, additionalProperties forbidden.
const readTools = `[
	{"name":"Read","input_schema":{
		"type":"object",
		"properties":{
			"file_path":{"type":"string"},
			"pages":{"type":"string"},
			"limit":{"type":"integer"},
			"offset":{"type":"integer"}
		},
		"required":["file_path"],
		"additionalProperties":false
	}},
	{"name":"Ping","input_schema":{"type":"object","properties":{}}}
]`

func compileRead(t *testing.T) *Validator {
	t.Helper()
	v := Compile([]byte(readTools))
	require.NotNil(t, v)
	return v
}

func TestCompile_NoTools(t *testing.T) {
	assert.Nil(t, Compile(nil))
	assert.Nil(t, Compile([]byte(`[]`)))
	assert.Nil(t, Compile([]byte(`{"not":"an array"}`)))
	assert.Nil(t, Compile([]byte(`[{"input_schema":{"type":"object"}}]`)), "nameless tools are skipped")
}

func TestCompile_BrokenSchemaFailsOpen(t *testing.T) {
	// An uncompilable schema must not break the request: the tool becomes
	// uncheckable and args pass through (normalize-only).
	v := Compile([]byte(`[{"name":"Broken","input_schema":{"type":"object","properties":{"x":{"$ref":"#/missing/def"}}}}]`))
	require.NotNil(t, v)
	got := v.Check("Broken", `{"x":{"anything":1},"y":""}`)
	assert.True(t, got.OK)
	assert.Nil(t, got.Issue)
	assert.JSONEq(t, `{"x":{"anything":1}}`, got.Args, "normalize still drops the empty optional")
}

func TestCompileCached_SameBytesSamePointer(t *testing.T) {
	a := CompileCached([]byte(readTools))
	b := CompileCached([]byte(readTools))
	require.NotNil(t, a)
	assert.Same(t, a, b)
}

func TestCheck_NilValidatorStillRunsParseTier(t *testing.T) {
	// No validator (request without tools, or non-Anthropic ingress): schema
	// tiers are skipped but malformed JSON is still repaired/contained, which
	// preserves the translators' historic unconditional syntax check.
	var v *Validator
	got := v.Check("Read", `{"anything":"goes"`)
	assert.False(t, got.OK)
	require.NotNil(t, got.Issue)
	assert.Equal(t, BucketInvalidJSON, got.Issue.Bucket)
	assert.True(t, got.Issue.Repaired)
	assert.JSONEq(t, `{"anything":"goes"}`, got.Args)

	clean := v.Check("Read", `{"a":1}`)
	assert.True(t, clean.OK)
	assert.Equal(t, `{"a":1}`, clean.Args)
}

func TestCheck_ValidPassthrough(t *testing.T) {
	v := compileRead(t)
	in := `{"file_path":"/a.go","limit":2000,"offset":0}`
	got := v.Check("Read", in)
	assert.True(t, got.OK)
	assert.Nil(t, got.Issue)
	assert.JSONEq(t, in, got.Args)
}

func TestCheck_EmptyArgsBecomeEmptyObject(t *testing.T) {
	v := compileRead(t)
	got := v.Check("Ping", "")
	assert.True(t, got.OK)
	assert.Equal(t, `{}`, got.Args)
}

// --- normalize tier (ports tool_optional_args_test.go, #339) ---

func TestCheck_DropsEmptyOptional(t *testing.T) {
	// The gpt-5.x failure mode: Read called with an empty optional pages="".
	v := compileRead(t)
	got := v.Check("Read", `{"file_path":"/a.go","limit":2000,"offset":0,"pages":""}`)
	assert.True(t, got.OK, "normalize-only cleanup keeps the #339 silent-strip semantics")
	assert.JSONEq(t, `{"file_path":"/a.go","limit":2000,"offset":0}`, got.Args,
		"empty optional pages must be dropped so the client tool doesn't error")
}

func TestCheck_DropsNullOptional(t *testing.T) {
	// The strict-mode artifact: strictified schemas force every param to be
	// present, so the model emits explicit nulls for optionals.
	v := compileRead(t)
	got := v.Check("Read", `{"file_path":"/a.go","pages":null,"limit":null}`)
	assert.True(t, got.OK)
	assert.JSONEq(t, `{"file_path":"/a.go"}`, got.Args)
}

func TestCheck_KeepsEmptyRequired(t *testing.T) {
	// A genuinely-missing required arg must still surface its error
	// downstream, so an empty required value is never stripped.
	v := compileRead(t)
	got := v.Check("Read", `{"file_path":""}`)
	assert.True(t, got.OK)
	assert.JSONEq(t, `{"file_path":""}`, got.Args)
}

func TestCheck_KeepsNonEmptyAndNonString(t *testing.T) {
	v := compileRead(t)
	got := v.Check("Read", `{"file_path":"/a.go","pages":"1-5","limit":0}`)
	assert.True(t, got.OK)
	assert.JSONEq(t, `{"file_path":"/a.go","pages":"1-5","limit":0}`, got.Args)
}

// --- parse tier ---

func TestCheck_RepairsTruncatedJSON(t *testing.T) {
	v := compileRead(t)
	got := v.Check("Read", `{"file_path":"/a.go","limit":20`)
	assert.False(t, got.OK)
	require.NotNil(t, got.Issue)
	assert.Equal(t, BucketInvalidJSON, got.Issue.Bucket)
	assert.True(t, got.Issue.Repaired)
	assert.JSONEq(t, `{"file_path":"/a.go","limit":20}`, got.Args)
}

func TestCheck_RepairsDanglingKey(t *testing.T) {
	v := compileRead(t)
	got := v.Check("Read", `{"file_path":"/a.go","pages":`)
	assert.False(t, got.OK)
	require.NotNil(t, got.Issue)
	assert.Equal(t, BucketInvalidJSON, got.Issue.Bucket)
	assert.True(t, got.Issue.Repaired)
	assert.JSONEq(t, `{"file_path":"/a.go"}`, got.Args)
}

func TestCheck_RepairsMarkdownFence(t *testing.T) {
	v := compileRead(t)
	got := v.Check("Read", "```json\n{\"file_path\":\"/a.go\"}\n```")
	assert.False(t, got.OK)
	require.NotNil(t, got.Issue)
	assert.Equal(t, BucketInvalidJSON, got.Issue.Bucket)
	assert.True(t, got.Issue.Repaired)
	assert.JSONEq(t, `{"file_path":"/a.go"}`, got.Args)
}

func TestCheck_UnrepairableJSONFallsBackToEmptyObject(t *testing.T) {
	v := compileRead(t)
	got := v.Check("Read", `not json at all`)
	assert.False(t, got.OK)
	require.NotNil(t, got.Issue)
	assert.Equal(t, BucketInvalidJSON, got.Issue.Bucket)
	assert.False(t, got.Issue.Repaired)
	assert.Equal(t, `{}`, got.Args)
	assert.Contains(t, got.Issue.Actions, "empty_object_fallback")
}

// --- unknown tool ---

func TestCheck_UnknownToolForwardedWithIssue(t *testing.T) {
	v := compileRead(t)
	got := v.Check("Hallucinated", `{"x":1}`)
	assert.False(t, got.OK)
	require.NotNil(t, got.Issue)
	assert.Equal(t, BucketUnknownTool, got.Issue.Bucket)
	assert.JSONEq(t, `{"x":1}`, got.Args, "forward-with-telemetry policy")
}

func TestKnownTool(t *testing.T) {
	v := compileRead(t)
	assert.True(t, v.KnownTool("Read"))
	assert.False(t, v.KnownTool("Hallucinated"))
	var nilV *Validator
	assert.True(t, nilV.KnownTool("anything"), "nil validator fails open")
}

// --- repair tier ---

func TestCheck_DropsUnknownKey(t *testing.T) {
	// additionalProperties:false in the schema gates the drop.
	v := compileRead(t)
	got := v.Check("Read", `{"file_path":"/a.go","hallucinated_param":true}`)
	assert.False(t, got.OK)
	require.NotNil(t, got.Issue)
	assert.Equal(t, BucketSchemaMismatch, got.Issue.Bucket)
	assert.True(t, got.Issue.Repaired)
	assert.Contains(t, got.Issue.Actions, "drop_unknown_key")
	assert.JSONEq(t, `{"file_path":"/a.go"}`, got.Args)
}

func TestCheck_PermissiveSchemaKeepsExtraKeys(t *testing.T) {
	// No additionalProperties:false -> extra keys are schema-valid and kept.
	v := Compile([]byte(`[{"name":"Loose","input_schema":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}}]`))
	require.NotNil(t, v)
	got := v.Check("Loose", `{"a":"x","extra":1}`)
	assert.True(t, got.OK)
	assert.JSONEq(t, `{"a":"x","extra":1}`, got.Args)
}

func TestCheck_CoercesStringToNumber(t *testing.T) {
	v := compileRead(t)
	got := v.Check("Read", `{"file_path":"/a.go","limit":"2000"}`)
	assert.False(t, got.OK)
	require.NotNil(t, got.Issue)
	assert.Equal(t, BucketSchemaMismatch, got.Issue.Bucket)
	assert.True(t, got.Issue.Repaired)
	assert.Contains(t, got.Issue.Actions, "coerce_string_to_number")
	assert.JSONEq(t, `{"file_path":"/a.go","limit":2000}`, got.Args)
}

func TestCheck_RefusesLossyCoercion(t *testing.T) {
	// "20 pages" is not a number; forward the normalized original untouched.
	v := compileRead(t)
	got := v.Check("Read", `{"file_path":"/a.go","limit":"20 pages"}`)
	assert.False(t, got.OK)
	require.NotNil(t, got.Issue)
	assert.Equal(t, BucketSchemaMismatch, got.Issue.Bucket)
	assert.False(t, got.Issue.Repaired)
	assert.JSONEq(t, `{"file_path":"/a.go","limit":"20 pages"}`, got.Args)
}

func TestCheck_CoercesStringToBool(t *testing.T) {
	v := Compile([]byte(`[{"name":"Flagged","input_schema":{"type":"object","properties":{"on":{"type":"boolean"}},"required":["on"]}}]`))
	require.NotNil(t, v)
	got := v.Check("Flagged", `{"on":"true"}`)
	require.NotNil(t, got.Issue)
	assert.True(t, got.Issue.Repaired)
	assert.Contains(t, got.Issue.Actions, "coerce_string_to_bool")
	assert.JSONEq(t, `{"on":true}`, got.Args)
}

func TestCheck_CoercesNumberToString(t *testing.T) {
	v := compileRead(t)
	got := v.Check("Read", `{"file_path":"/a.go","pages":5}`)
	require.NotNil(t, got.Issue)
	assert.True(t, got.Issue.Repaired)
	assert.Contains(t, got.Issue.Actions, "coerce_to_string")
	assert.JSONEq(t, `{"file_path":"/a.go","pages":"5"}`, got.Args)
}

func TestCheck_WrapsScalarInArray(t *testing.T) {
	v := Compile([]byte(`[{"name":"Multi","input_schema":{"type":"object","properties":{"files":{"type":"array","items":{"type":"string"}}},"required":["files"]}}]`))
	require.NotNil(t, v)
	got := v.Check("Multi", `{"files":"a.go"}`)
	require.NotNil(t, got.Issue)
	assert.True(t, got.Issue.Repaired)
	assert.Contains(t, got.Issue.Actions, "wrap_scalar_in_array")
	assert.JSONEq(t, `{"files":["a.go"]}`, got.Args)
}

func TestCheck_MissingRequiredNotRepairable(t *testing.T) {
	// Inventing a required value would change the call's meaning: forward
	// as-is with telemetry, the client surfaces the real error.
	v := compileRead(t)
	got := v.Check("Read", `{"pages":"1-5"}`)
	assert.False(t, got.OK)
	require.NotNil(t, got.Issue)
	assert.Equal(t, BucketSchemaMismatch, got.Issue.Bucket)
	assert.False(t, got.Issue.Repaired)
	assert.JSONEq(t, `{"pages":"1-5"}`, got.Args)
	assert.NotEmpty(t, got.Issue.Detail)
}

func TestCheck_NestedCoercion(t *testing.T) {
	v := Compile([]byte(`[{"name":"Nested","input_schema":{
		"type":"object",
		"properties":{"opts":{"type":"object","properties":{"depth":{"type":"integer"}},"required":["depth"],"additionalProperties":false}},
		"required":["opts"]
	}}]`))
	require.NotNil(t, v)
	got := v.Check("Nested", `{"opts":{"depth":"3","stray":true}}`)
	require.NotNil(t, got.Issue)
	assert.True(t, got.Issue.Repaired)
	assert.JSONEq(t, `{"opts":{"depth":3}}`, got.Args)
}
