package toolcheck

import "strings"

// repairJSON attempts a minimal, conservative repair of malformed tool
// argument JSON. It handles the observed model failure modes — markdown
// fences around the payload, trailing commas, and truncation mid-stream —
// and nothing speculative. Returns "" when no repair applied (caller falls
// back to "{}").
//
// A full LLM-output JSON repairer (e.g. kaptinlin/jsonrepair) is the upgrade
// path once the module toolchain moves to go 1.26.
func repairJSON(raw string) (out string, actions []string) {
	if len(raw) > maxArgsBytes {
		return "", nil
	}
	out = strings.TrimSpace(raw)

	if stripped, ok := stripMarkdownFence(out); ok {
		out = stripped
		actions = append(actions, "json_repair_strip_fence")
	}
	if !strings.HasPrefix(out, "{") && !strings.HasPrefix(out, "[") {
		return "", nil
	}
	if balanced, balActions := balanceJSON(out); len(balActions) > 0 {
		out = balanced
		actions = append(actions, balActions...)
	}
	if len(actions) == 0 {
		return "", nil
	}
	return out, actions
}

// stripMarkdownFence removes a ```json ... ``` (or bare ```) wrapper.
func stripMarkdownFence(s string) (out string, ok bool) {
	if !strings.HasPrefix(s, "```") {
		return s, false
	}
	body := strings.TrimPrefix(s, "```")
	if nl := strings.IndexByte(body, '\n'); nl >= 0 {
		// Drop the info string ("json") on the opening fence line.
		body = body[nl+1:]
	}
	body = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), "```"))
	return body, true
}

// balanceJSON walks the input tracking string/escape state, drops trailing
// commas, truncates anything after the top-level value closes, and closes
// whatever a truncation left open (string, then brackets/braces). A dangling
// partial member like `,"key":` is stripped back to the last complete value
// before closing.
func balanceJSON(s string) (out string, actions []string) {
	var stack []byte
	inString := false
	escaped := false
	// expectKey tracks whether a string at the current position would be an
	// object KEY rather than a value — a closing quote on a key must not
	// count as "last complete value" or a dangling `"key":` would survive
	// the cut.
	expectKey := false
	stringIsKey := false
	lastComplete := -1 // index AFTER the last byte that ends a complete value
	var b strings.Builder
	b.Grow(len(s) + 4)

	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			b.WriteByte(c)
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
				if !stringIsKey {
					lastComplete = b.Len()
				}
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			stringIsKey = expectKey
			b.WriteByte(c)
		case '{':
			stack = append(stack, c)
			expectKey = true
			b.WriteByte(c)
		case '[':
			stack = append(stack, c)
			expectKey = false
			b.WriteByte(c)
		case ':':
			expectKey = false
			b.WriteByte(c)
		case ',':
			expectKey = len(stack) > 0 && stack[len(stack)-1] == '{'
			b.WriteByte(c)
		case '}', ']':
			want := byte('{')
			if c == ']' {
				want = '['
			}
			if len(stack) == 0 || stack[len(stack)-1] != want {
				// Mismatched closer: unrepairable by this minimal pass.
				return "", nil
			}
			stack = stack[:len(stack)-1]
			expectKey = false
			b.WriteByte(c)
			lastComplete = b.Len()
			if len(stack) == 0 {
				// Top-level value closed; anything after is trailing garbage.
				if i+1 < len(s) && strings.TrimSpace(s[i+1:]) != "" {
					actions = append(actions, "json_repair_strip_trailing")
				}
				return b.String(), actions
			}
		default:
			b.WriteByte(c)
			if c != ',' && c != ':' && !isJSONWhitespace(c) {
				lastComplete = b.Len()
			}
		}
	}

	if len(stack) == 0 && !inString {
		return "", nil // already balanced; the syntax error is something else
	}

	// Truncated input: close an open VALUE string, cut back to the last
	// complete value (dropping any dangling partial member), then close the
	// open scopes in reverse order.
	out = b.String()
	if inString && !stringIsKey {
		out += `"`
		actions = append(actions, "json_repair_close_string")
		lastComplete = len(out)
	}
	if lastComplete < 0 {
		return "", nil
	}
	if lastComplete < len(out) {
		out = out[:lastComplete]
		actions = append(actions, "json_repair_drop_partial_member")
	}
	for trimmed := strings.TrimRight(out, " \t\r\n"); strings.HasSuffix(trimmed, ","); trimmed = strings.TrimRight(out, " \t\r\n") {
		out = trimmed[:len(trimmed)-1]
		actions = append(actions, "json_repair_drop_trailing_comma")
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			out += "}"
		} else {
			out += "]"
		}
	}
	actions = append(actions, "json_repair_close_brackets")
	return out, actions
}

func isJSONWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
