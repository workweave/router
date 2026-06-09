package translate

import (
	"fmt"
	"sort"
	"strings"
)

// strictify limits. Conservative against OpenAI's documented structured-output
// caps so a strictified schema is never rejected for size.
const (
	strictifyMaxDepth      = 10
	strictifyMaxProperties = 1000
)

// strictifyBailKeywords are JSON Schema constructs OpenAI strict mode cannot
// express; their presence anywhere makes the schema non-strictifiable.
var strictifyBailKeywords = []string{"oneOf", "allOf", "not", "if", "then", "else", "patternProperties", "$ref"}

// strictifyDropKeywords are constraint keywords strict mode rejects; they are
// stripped from the node and appended to its description so the model still
// sees the constraint as guidance. Order fixes the appended-text order.
var strictifyDropKeywords = []string{
	"format", "pattern", "default", "examples",
	"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
	"minLength", "maxLength", "minItems", "maxItems", "uniqueItems",
	"minProperties", "maxProperties",
}

// strictifyOpenAISchema converts a tool input_schema (already $ref-inlined by
// inlineSchemaDefs) into the subset OpenAI structured outputs requires for
// `strict:true` function tools: additionalProperties:false on every object,
// every property required, and originally-optional properties made nullable
// (type union with "null") so the call can still omit them semantically.
// Constraint keywords strict mode rejects are moved into descriptions.
//
// Returns ok=false when the schema cannot be faithfully strictified (root is
// not an object schema, bail keywords like oneOf / unresolved $ref are
// present, or size limits are exceeded); the caller then emits the original
// schema without strict. The input is never mutated — the result is a deep
// transform over a copy.
func strictifyOpenAISchema(schema any) (out any, ok bool) {
	root, isMap := schema.(map[string]any)
	if !isMap {
		return nil, false
	}
	if t, _ := root["type"].(string); t != "object" {
		return nil, false
	}
	props := 0
	res, ok := strictifyNode(root, 0, &props)
	if !ok {
		return nil, false
	}
	return res, true
}

// strictifyNode transforms one schema node, recursing into properties, items,
// and anyOf branches.
func strictifyNode(node map[string]any, depth int, propCount *int) (out map[string]any, ok bool) {
	if depth > strictifyMaxDepth {
		return nil, false
	}
	for _, kw := range strictifyBailKeywords {
		if _, present := node[kw]; present {
			return nil, false
		}
	}

	res := make(map[string]any, len(node))
	var droppedNotes []string
	for k, v := range node {
		dropped := false
		for _, kw := range strictifyDropKeywords {
			if k == kw {
				droppedNotes = append(droppedNotes, fmt.Sprintf("%s: %v", k, compactJSONValue(v)))
				dropped = true
				break
			}
		}
		if !dropped {
			res[k] = v
		}
	}
	sort.Strings(droppedNotes)
	if len(droppedNotes) > 0 {
		note := "(" + strings.Join(droppedNotes, ", ") + ")"
		if desc, _ := res["description"].(string); desc != "" {
			res["description"] = desc + " " + note
		} else {
			res["description"] = note
		}
	}

	// anyOf branches recurse; a branch that isn't an object schema bails.
	if branches, present := res["anyOf"].([]any); present {
		outBranches := make([]any, 0, len(branches))
		for _, b := range branches {
			bm, isMap := b.(map[string]any)
			if !isMap {
				return nil, false
			}
			sb, sok := strictifyNode(bm, depth+1, propCount)
			if !sok {
				return nil, false
			}
			outBranches = append(outBranches, sb)
		}
		res["anyOf"] = outBranches
	}

	if items, present := res["items"]; present {
		im, isMap := items.(map[string]any)
		if !isMap {
			// Tuple-form items (array) is not expressible in strict mode.
			return nil, false
		}
		si, sok := strictifyNode(im, depth+1, propCount)
		if !sok {
			return nil, false
		}
		res["items"] = si
	}

	properties, hasProps := res["properties"].(map[string]any)
	isObject := false
	if t, _ := res["type"].(string); t == "object" {
		isObject = true
	}
	if !isObject && hasProps {
		isObject = true
	}
	if !isObject {
		return res, true
	}

	// Object node: additionalProperties:false, all properties required,
	// originally-optional properties become nullable.
	res["additionalProperties"] = false
	originallyRequired := make(map[string]struct{})
	if reqList, present := res["required"].([]any); present {
		for _, r := range reqList {
			if name, isStr := r.(string); isStr {
				originallyRequired[name] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(properties))
	outProps := make(map[string]any, len(properties))
	for name, p := range properties {
		*propCount++
		if *propCount > strictifyMaxProperties {
			return nil, false
		}
		pm, isMap := p.(map[string]any)
		if !isMap {
			return nil, false
		}
		sp, sok := strictifyNode(pm, depth+1, propCount)
		if !sok {
			return nil, false
		}
		if _, req := originallyRequired[name]; !req {
			sp = makeNullable(sp)
		}
		outProps[name] = sp
		names = append(names, name)
	}
	sort.Strings(names)
	required := make([]any, 0, len(names))
	for _, n := range names {
		required = append(required, n)
	}
	res["properties"] = outProps
	res["required"] = required
	return res, true
}

// makeNullable rewrites a property schema so null is an accepted value:
// strict mode requires every property listed in `required`, so optionality is
// expressed as a null union instead of omission.
func makeNullable(node map[string]any) map[string]any {
	switch t := node["type"].(type) {
	case string:
		if t == "null" {
			return node
		}
		node["type"] = []any{t, "null"}
		return node
	case []any:
		for _, v := range t {
			if s, isStr := v.(string); isStr && s == "null" {
				return node
			}
		}
		node["type"] = append(t, "null")
		return node
	}
	if branches, present := node["anyOf"].([]any); present {
		node["anyOf"] = append(branches, map[string]any{"type": "null"})
		return node
	}
	// No type and no anyOf (e.g. bare enum): wrap the node itself.
	return map[string]any{"anyOf": []any{node, map[string]any{"type": "null"}}}
}

// compactJSONValue renders a dropped constraint value for a description note.
func compactJSONValue(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case float64, bool, nil:
		return fmt.Sprint(val)
	default:
		return fmt.Sprintf("%v", val)
	}
}
