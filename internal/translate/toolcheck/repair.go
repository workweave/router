package toolcheck

import (
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// maxRepairPasses bounds the repair loop: one coercion can surface the next
// error (e.g. wrap-in-array then item type), but a payload that needs more
// than a few passes is not safely repairable.
const maxRepairPasses = 3

// repairArgs applies validation-error-driven safe coercions to args and
// returns the result plus the action names applied. Between passes it
// re-validates against schema to surface the next layer of errors; the
// caller still does the final re-validation and discards the repair when it
// fails.
//
// Safe means lossless and meaning-preserving:
//   - drop_unknown_key:        keys the schema rejects via additionalProperties
//   - coerce_string_to_number: "5" -> 5 (lossless parse only)
//   - coerce_string_to_bool:   "true" -> true
//   - coerce_to_string:        5 -> "5", true -> "true"
//   - wrap_scalar_in_array:    "x" -> ["x"]
//
// Missing required params and enum violations are NOT repairable — inventing
// values would change the call's meaning.
func repairArgs(schema *jsonschema.Schema, args string, verr error) (out string, actions []string) {
	out = args
	current := verr
	for pass := 0; pass < maxRepairPasses; pass++ {
		validationErr, ok := current.(*jsonschema.ValidationError)
		if !ok {
			return out, actions
		}
		passActions := applyLeafRepairs(&out, validationErr)
		if len(passActions) == 0 {
			return out, actions
		}
		actions = append(actions, passActions...)
		current = validate(schema, out)
		if current == nil {
			return out, actions
		}
	}
	return out, actions
}

// applyLeafRepairs walks every leaf validation error and mutates out in
// place. Returns the actions applied this pass.
func applyLeafRepairs(out *string, verr *jsonschema.ValidationError) (actions []string) {
	for _, leaf := range collectLeaves(verr, nil) {
		path := instancePath(leaf.InstanceLocation)
		switch k := leaf.ErrorKind.(type) {
		case *kind.AdditionalProperties:
			// The validator only emits this where the schema forbids extra
			// keys, so the additionalProperties:false gate is implicit.
			for _, prop := range k.Properties {
				target := joinPath(path, escapeJSONPathToken(prop))
				if next, err := sjson.Delete(*out, target); err == nil {
					*out = next
					actions = append(actions, "drop_unknown_key")
				}
			}
		case *kind.Type:
			if path == "" {
				continue // root-level type mismatch is not repairable
			}
			if action, ok := coerceValue(out, path, k); ok {
				actions = append(actions, action)
			}
		}
	}
	return actions
}

// coerceValue attempts one lossless coercion of the value at path toward the
// schema's wanted types, in fixed preference order.
func coerceValue(out *string, path string, k *kind.Type) (action string, ok bool) {
	val := gjson.Get(*out, path)
	if !val.Exists() {
		return "", false
	}
	want := make(map[string]struct{}, len(k.Want))
	for _, w := range k.Want {
		want[w] = struct{}{}
	}
	_, wantNumber := want["number"]
	_, wantInteger := want["integer"]
	_, wantBool := want["boolean"]
	_, wantString := want["string"]
	_, wantArray := want["array"]

	if val.Type == gjson.String {
		s := val.Str
		if wantNumber || wantInteger {
			if wantInteger {
				if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
					if next, serr := sjson.Set(*out, path, n); serr == nil {
						*out = next
						return "coerce_string_to_number", true
					}
				}
			} else if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				if next, serr := sjson.Set(*out, path, f); serr == nil {
					*out = next
					return "coerce_string_to_number", true
				}
			}
		}
		if wantBool {
			if b, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil {
				if next, serr := sjson.Set(*out, path, b); serr == nil {
					*out = next
					return "coerce_string_to_bool", true
				}
			}
		}
	}
	if wantString && (val.Type == gjson.Number || val.Type == gjson.True || val.Type == gjson.False) {
		if next, err := sjson.Set(*out, path, val.Raw); err == nil {
			*out = next
			return "coerce_to_string", true
		}
	}
	if wantArray && !val.IsArray() {
		if next, err := sjson.SetRaw(*out, path, "["+val.Raw+"]"); err == nil {
			*out = next
			return "wrap_scalar_in_array", true
		}
	}
	return "", false
}

// collectLeaves flattens the error tree into its leaf causes.
func collectLeaves(verr *jsonschema.ValidationError, acc []*jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(verr.Causes) == 0 {
		return append(acc, verr)
	}
	for _, c := range verr.Causes {
		acc = collectLeaves(c, acc)
	}
	return acc
}

// instancePath converts a ValidationError instance location to a gjson path.
func instancePath(location []string) string {
	if len(location) == 0 {
		return ""
	}
	parts := make([]string, 0, len(location))
	for _, token := range location {
		parts = append(parts, escapeJSONPathToken(token))
	}
	return strings.Join(parts, ".")
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}
