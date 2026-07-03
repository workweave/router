package translate

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ReasonUserForceModel marks a session pin from /force-model. It's an
// immutable sticky: scorer/planner are bypassed until /unforce-model clears it.
const ReasonUserForceModel = "user_forced"

// ReasonLoopEscalation marks a session pin created when the router detects a
// tool-call loop and escalates to opus. Immutable sticky like
// ReasonUserForceModel, so the session doesn't re-route back into the loop.
const ReasonLoopEscalation = "loop_escalation"

// ForceModelResult holds the parsed outcome of a force-model command.
type ForceModelResult struct {
	// Model is the target model name; empty when Clear is true.
	Model string
	// Clear is true for /unforce-model.
	Clear bool
}

// ExtractForceModelCommand scans the last user-role message in env for a
// /force-model <model> or /unforce-model directive, stripping it from
// env.body. Returns (zero, false) when no command is present.
func (env *RequestEnvelope) ExtractForceModelCommand() (ForceModelResult, bool) {
	var res ForceModelResult
	found := env.extractLeadingCommand(func(text string) (bool, string) {
		r, ok, stripped := parseForceModelCommand(text)
		if ok {
			res = r
		}
		return ok, stripped
	})
	return res, found
}

// extractLeadingCommand scans the last user-role message (Anthropic/OpenAI
// shapes only) for a directive recognized by parse, which receives candidate
// text and returns (found, strippedText). On a match, the matched content is
// replaced in env.body with the stripped remainder.
func (env *RequestEnvelope) extractLeadingCommand(parse func(text string) (found bool, stripped string)) bool {
	switch env.format {
	case FormatAnthropic, FormatOpenAI:
	default:
		return false
	}
	msgs := gjson.GetBytes(env.body, "messages")
	if !msgs.IsArray() {
		return false
	}

	lastIdx := -1
	var lastContent gjson.Result
	msgs.ForEach(func(key, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			lastIdx = int(key.Int())
			lastContent = msg.Get("content")
		}
		return true
	})
	if lastIdx < 0 {
		return false
	}

	idxStr := strconv.Itoa(lastIdx)

	switch {
	case lastContent.Type == gjson.String:
		found, stripped := parse(lastContent.String())
		if !found {
			return false
		}
		if newBody, err := sjson.SetBytes(env.body, "messages."+idxStr+".content", stripped); err == nil {
			env.body = newBody
		}
		return true

	case lastContent.Type == gjson.JSON && lastContent.IsArray():
		// Scan every text block: Claude Code sometimes splits the user turn
		// into multiple parts (injected tags in one, typed directive in
		// another), so checking only the first block could miss it.
		type textBlock struct {
			idx  int
			text string
		}
		var blocks []textBlock
		lastContent.ForEach(func(key, block gjson.Result) bool {
			if block.Get("type").String() == "text" {
				blocks = append(blocks, textBlock{idx: int(key.Int()), text: block.Get("text").String()})
			}
			return true
		})
		for _, b := range blocks {
			found, stripped := parse(b.text)
			if !found {
				continue
			}
			blockPath := "messages." + idxStr + ".content." + strconv.Itoa(b.idx) + ".text"
			if newBody, err := sjson.SetBytes(env.body, blockPath, stripped); err == nil {
				env.body = newBody
			}
			return true
		}
		return false

	default:
		return false
	}
}

// parseForceModelCommand scans text for a /force-model (alias /fm) or
// /unforce-model (alias /ufm) directive on the first non-empty line.
// Restricted to the leading line so pasted content (snippets, transcripts)
// starting with "/" can't silently rewrite session routing. The short
// aliases are a fallback for clients without local slash-command expansion
// (pi, opencode, raw API); Claude Code/Codex expand to the canonical form
// client-side.
//
// Leading <tag>...</tag> blocks (e.g. <system-reminder>, <command-name>
// injected by Claude Code) are skipped before the leading-line check, and
// preserved in the stripped output.
func parseForceModelCommand(text string) (res ForceModelResult, found bool, stripped string) {
	prefixEnd := leadingInjectedPrefixEnd(text)
	prefix := text[:prefixEnd]
	body := text[prefixEnd:]

	lines := strings.Split(body, "\n")
	cmdIdx := -1
	cmdTail := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if after, ok := cutAnyPrefix(trimmed, "/force-model ", "/fm "); ok {
			parts := strings.Fields(strings.TrimSpace(after))
			if len(parts) > 0 {
				res = ForceModelResult{Model: parts[0]}
				if len(parts) > 1 {
					cmdTail = strings.Join(parts[1:], " ")
				}
				found = true
				cmdIdx = i
			}
		} else if trimmed == "/unforce-model" || trimmed == "/ufm" {
			res = ForceModelResult{Clear: true}
			found = true
			cmdIdx = i
		}
		break
	}
	if !found {
		return ForceModelResult{}, false, text
	}
	remaining := make([]string, 0, len(lines))
	remaining = append(remaining, lines[:cmdIdx]...)
	if cmdTail != "" {
		remaining = append(remaining, cmdTail)
	}
	remaining = append(remaining, lines[cmdIdx+1:]...)
	bodyStripped := strings.Join(remaining, "\n")
	stripped = strings.TrimSpace(prefix + bodyStripped)
	return res, true, stripped
}

// cutAnyPrefix returns text with the first matching prefix removed. Prefix
// order matters only for overlapping prefixes; the command forms used here
// are disjoint.
func cutAnyPrefix(text string, prefixes ...string) (after string, ok bool) {
	for _, p := range prefixes {
		if after, ok = strings.CutPrefix(text, p); ok {
			return after, true
		}
	}
	return text, false
}

// leadingInjectedPrefixEnd returns the byte offset after leading whitespace
// and complete <tag>...</tag> blocks. Only simple attribute-free tag names are
// recognized, so pasted XML/HTML containing a stray /force-model line can't
// satisfy the guard; unclosed or attribute-bearing tags stop the scan.
func leadingInjectedPrefixEnd(text string) int {
	i := 0
	for i < len(text) {
		// Skip whitespace between blocks.
		j := i
		for j < len(text) {
			c := text[j]
			if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				break
			}
			j++
		}
		if j >= len(text) || text[j] != '<' {
			return i
		}
		// Parse the opening tag name.
		nameStart := j + 1
		nameEnd := nameStart
		for nameEnd < len(text) {
			c := text[nameEnd]
			if c == '>' {
				break
			}
			if !isTagNameByte(c, nameEnd == nameStart) {
				return i
			}
			nameEnd++
		}
		if nameEnd >= len(text) || nameEnd == nameStart {
			return i
		}
		closeTag := "</" + text[nameStart:nameEnd] + ">"
		closeIdx := strings.Index(text[nameEnd+1:], closeTag)
		if closeIdx < 0 {
			return i
		}
		i = nameEnd + 1 + closeIdx + len(closeTag)
	}
	return i
}

func isTagNameByte(c byte, first bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case !first && (c >= '0' && c <= '9' || c == '-' || c == '_'):
		return true
	default:
		return false
	}
}
