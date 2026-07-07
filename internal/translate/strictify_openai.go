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
var strictifyBailKeywords = []string{
	"oneOf", "allOf", "not", "if", "then", "else", "patternProperties", "$ref",
	"dependentSchemas", "unevaluatedProperties", "unevaluatedItems", "prefixItems",
}

// strictifyDropKeywords are constraints strict mode rejects; they're stripped
// from the node and appended to its description as guidance instead.
var strictifyDropKeywords = []string{
	"format", "pattern", "default", "examples",
	"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
	"minLength", "maxLength", "minItems", "maxItems", "uniqueItems",
	"minProperties", "maxProperties",
	// propertyNames rode through untouched and 400'd requests carrying the
	// playwright MCP tools (browser_drop) on the gpt-5.x Responses path.
	"propertyNames", "contains", "minContains", "maxContains", "dependentRequired",
}

// strictifyOpenAISchema converts a $ref-inlined tool input_schema into the
// subset OpenAI strict mode requires: additionalProperties:false everywhere,
// all properties required (optional ones made nullable instead), and rejected
// constraint keywords moved into descriptions.
//
// Returns ok=false if the schema can't be strictified (non-object root, bail
// keywords, unresolved $ref, or size limits exceeded). Input is never mutated.
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
			// OpenAI strict mode requires every anyOf branch to carry a
			// concrete type; a typeless branch — e.g. an "any"/`{}` member of a
			// Union (respan's bulk_create_dataset_logs `expected_output`) — is
			// not expressible strictly. Bail to the non-strict fallback rather
			// than emit a branch OpenAI 400s with
			// "In context=(...anyOf...), schema must have a 'type' key".
			if !schemaHasStrictType(sb) {
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
	// originally-optional properties become nullable. Stamp an explicit
	// type:"object" — a node OpenAI must read as an object (it has properties)
	// but that omitted its own type would otherwise emit typeless and 400 when
	// it sits inside an anyOf branch.
	res["type"] = "object"
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

// makeNullable adds null as an accepted type: strict mode requires every
// property in `required`, so optionality is expressed via null union instead.
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

// schemaHasStrictType reports whether node carries a type OpenAI strict mode
// can consume: an explicit "type", a nested "anyOf", or an "enum".
func schemaHasStrictType(node map[string]any) bool {
	if _, ok := node["type"]; ok {
		return true
	}
	if _, ok := node["anyOf"]; ok {
		return true
	}
	if _, ok := node["enum"]; ok {
		return true
	}
	return false
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
