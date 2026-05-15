package translate

import "strings"

// EnableEditEscapeNormalize gates the escape-sequence repair pass on file-edit
// tool-call arguments. Set once at startup from the composition root. When
// false, normalizeEditEscapes is a no-op for every call. Off by default
// because the transform can corrupt legitimate code containing literal `\n` /
// `\t` sequences in source (e.g. a Python string `"\\n"`).
var EnableEditEscapeNormalize bool

// editToolNames is the set of tool names whose string arguments may carry
// file content that must round-trip whitespace exactly. Comparison is
// case-insensitive against incoming tool names so client-defined casing
// variants are caught.
var editToolNames = map[string]struct{}{
	"edit":      {},
	"write":     {},
	"multiedit": {},
}

// editEscapableFields is the per-tool allowlist of string fields where
// backslash-letter escapes should be unescaped. `file_path` is deliberately
// excluded — paths shouldn't contain newlines, and rewriting one risks
// corrupting an otherwise-valid path containing a literal backslash.
var editEscapableFields = map[string]struct{}{
	"old_string": {},
	"new_string": {},
	"content":    {},
}

// normalizeEditEscapes repairs `\n` / `\t` / `\r` literal sequences that
// upstream models occasionally emit in file-edit tool arguments where a real
// newline / tab / carriage return was intended. Mutates `input` in place.
// No-op when EnableEditEscapeNormalize is false, when `toolName` is not a
// file-edit tool, or when `input` is not a JSON object.
//
// JSON decoding has already converted real escape sequences ("\n" in the wire
// JSON) to actual newlines by the time we see `input`, so any remaining
// backslash-letter sequence here is the broken case: the model emitted
// double-escaped JSON ("\\n" on the wire) and our decoder produced literal
// backslash-n.
//
// MultiEdit nests per-edit `old_string`/`new_string` inside an `edits` array,
// so each entry there is also walked.
func normalizeEditEscapes(toolName string, input any) {
	if !EnableEditEscapeNormalize {
		return
	}
	if _, ok := editToolNames[strings.ToLower(toolName)]; !ok {
		return
	}
	m, ok := input.(map[string]any)
	if !ok {
		return
	}
	rewriteAllowlistedFields(m)
	if edits, ok := m["edits"].([]any); ok {
		for _, e := range edits {
			entry, ok := e.(map[string]any)
			if !ok {
				continue
			}
			rewriteAllowlistedFields(entry)
		}
	}
}

// rewriteAllowlistedFields applies escape repair to every allowlisted string
// field in the given map. Caller gates by tool name.
func rewriteAllowlistedFields(m map[string]any) {
	for key, raw := range m {
		if _, allowed := editEscapableFields[key]; !allowed {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			continue
		}
		m[key] = unescapeBackslashLiterals(s)
	}
}

// unescapeBackslashLiterals rewrites the literal two-character sequences
// `\n` / `\t` / `\r` (a real backslash followed by a real letter) to their
// single-character escape values. Other escape sequences are left alone.
func unescapeBackslashLiterals(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	r := strings.NewReplacer(
		`\n`, "\n",
		`\t`, "\t",
		`\r`, "\r",
	)
	return r.Replace(s)
}
