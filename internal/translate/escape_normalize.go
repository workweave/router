package translate

import "strings"

// escapeNormalizeToolNames is the set of tools whose args carry file content
// needing exact whitespace round-tripping. Matched case-insensitively.
var escapeNormalizeToolNames = map[string]struct{}{
	"edit":      {},
	"write":     {},
	"multiedit": {},
}

// editEscapableFields is the allowlist of fields to unescape. `file_path` is
// excluded — paths can contain a literal backslash that rewriting would corrupt.
var editEscapableFields = map[string]struct{}{
	"old_string": {},
	"new_string": {},
	"content":    {},
}

// normalizeEditEscapes repairs literal `\n`/`\t`/`\r` sequences that upstream
// models occasionally double-escape (`\\n` on the wire) in file-edit tool
// args. Mutates `input` in place; no-op unless enabled, toolName is a
// file-edit tool, and input is a JSON object. MultiEdit's nested `edits`
// array entries are walked too. The transform can corrupt legitimate source
// containing literal `\n`/`\t` (e.g. a Python string `"\\n"`), so callers
// must gate `enabled` behind an explicit opt-in.
func normalizeEditEscapes(enabled bool, toolName string, input any) {
	if !enabled {
		return
	}
	if _, ok := escapeNormalizeToolNames[strings.ToLower(toolName)]; !ok {
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

// unescapeBackslashLiterals rewrites literal `\n`/`\t`/`\r` two-character
// sequences to their single-character values; other escapes are untouched.
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
