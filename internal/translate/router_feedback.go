package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// RouterFeedbackResult holds the parsed outcome of a /router-feedback command.
type RouterFeedbackResult struct {
	// Rating is the thumbs verdict: "up", "down", or "" when the user gave a
	// note-only command. Set from the /rf+ /rf- shortcuts or a leading
	// 👍/👎/+/-/up/down token in the note.
	Rating string
	// Feedback is the free-form note the user submitted after the command,
	// minus any leading rating token.
	Feedback string
}

// RouterFeedbackRatingUp and RouterFeedbackRatingDown are the canonical
// RouterFeedbackResult.Rating values, shared with telemetry and the handler so
// the string never drifts.
const (
	RouterFeedbackRatingUp   = "up"
	RouterFeedbackRatingDown = "down"
)

// ExtractRouterFeedbackCommand scans the last user-role message in env for a
// /router-feedback <text> directive. When found, it strips the command line
// from env.body and returns the feedback text. A bare /router-feedback with
// no text still matches (found=true, empty Feedback) so the handler can ack
// with usage guidance instead of forwarding the command to an upstream model.
// Returns (zero, false) when no command is present.
func (env *RequestEnvelope) ExtractRouterFeedbackCommand() (RouterFeedbackResult, bool) {
	var res RouterFeedbackResult
	found := env.extractLeadingCommand(func(text string) (bool, string) {
		r, ok, stripped := parseRouterFeedbackCommand(text)
		if ok {
			res = r
		}
		return ok, stripped
	})
	return res, found
}

// StripRouterFeedbackArtifacts removes prior router-feedback command turns and
// synthetic ack turns from model-visible history. The current trailing user
// command is preserved so ExtractRouterFeedbackCommand can still record it.
func (env *RequestEnvelope) StripRouterFeedbackArtifacts() int {
	switch env.format {
	case FormatAnthropic, FormatOpenAI:
	default:
		return 0
	}
	msgs := gjson.GetBytes(env.body, "messages")
	if !msgs.IsArray() {
		return 0
	}

	lastUserIdx := -1
	msgs.ForEach(func(key, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			lastUserIdx = int(key.Int())
		}
		return true
	})

	removed := 0
	rebuilt := make([]string, 0, len(msgs.Array()))
	msgs.ForEach(func(key, msg gjson.Result) bool {
		idx := int(key.Int())
		role := msg.Get("role").String()
		content := msg.Get("content")
		if role == "user" && idx != lastUserIdx && isRouterFeedbackCommandOnlyContent(content) {
			removed++
			return true
		}
		if role == "assistant" && isRouterFeedbackAckOnlyContent(content) {
			removed++
			return true
		}
		rebuilt = append(rebuilt, msg.Raw)
		return true
	})
	if removed == 0 {
		return 0
	}
	return env.setMessages(rebuilt, removed)
}

// parseRouterFeedbackCommand scans text for a /router-feedback (alias /rf)
// directive on the first non-empty line, applying the same leading-line +
// injected-prefix guards as parseForceModelCommand (see that function for the
// rationale; the router-side alias serves clients without local slash-command
// expansion). Unlike /force-model, everything after the command — same line
// AND following lines — is the feedback payload, so the whole body is
// consumed and the stripped output keeps only the injected prefix.
func parseRouterFeedbackCommand(text string) (res RouterFeedbackResult, found bool, stripped string) {
	prefixEnd := leadingInjectedPrefixEnd(text)
	prefix := text[:prefixEnd]
	body := text[prefixEnd:]

	trimmedBody := strings.TrimSpace(body)
	first, rest, _ := strings.Cut(trimmedBody, "\n")
	first = strings.TrimSpace(first)

	rating, inline, ok := matchRouterFeedbackCommand(first)
	if !ok {
		return RouterFeedbackResult{}, false, text
	}

	feedback := strings.TrimSpace(inline)
	if rest = strings.TrimSpace(rest); rest != "" {
		if feedback != "" {
			feedback += "\n"
		}
		feedback += rest
	}
	// A note that opens with a bare verdict ("/rf 👍 too slow") promotes to a
	// rating, so a single command can carry both verdict and explanation.
	if rating == "" {
		rating, feedback = splitLeadingRating(feedback)
	}
	return RouterFeedbackResult{Rating: rating, Feedback: feedback}, true, strings.TrimSpace(prefix)
}

func isRouterFeedbackCommandOnlyContent(content gjson.Result) bool {
	switch {
	case content.Type == gjson.String:
		_, found, stripped := parseRouterFeedbackCommand(content.String())
		return found && isOnlyInjectedCommandText(stripped)
	case content.Type == gjson.JSON && content.IsArray():
		seenCommand := false
		allSynthetic := true
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "text" {
				allSynthetic = false
				return false
			}
			text := block.Get("text").String()
			if strings.TrimSpace(text) == "" || isClaudeCodeInjectedBlock(text) {
				return true
			}
			_, found, stripped := parseRouterFeedbackCommand(text)
			if found && isOnlyInjectedCommandText(stripped) {
				seenCommand = true
				return true
			}
			allSynthetic = false
			return false
		})
		return seenCommand && allSynthetic
	default:
		return false
	}
}

func isOnlyInjectedCommandText(text string) bool {
	trimmed := strings.TrimSpace(text)
	return trimmed == "" || leadingInjectedPrefixEnd(trimmed) == len(trimmed)
}

func isRouterFeedbackAckOnlyContent(content gjson.Result) bool {
	switch {
	case content.Type == gjson.String:
		return isRouterFeedbackAckText(content.String())
	case content.Type == gjson.JSON && content.IsArray():
		seenAck := false
		allSynthetic := true
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "text" {
				allSynthetic = false
				return false
			}
			text := block.Get("text").String()
			if strings.TrimSpace(text) == "" {
				return true
			}
			if isRouterFeedbackAckText(text) {
				seenAck = true
				return true
			}
			allSynthetic = false
			return false
		})
		return seenAck && allSynthetic
	default:
		return false
	}
}

func isRouterFeedbackAckText(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "Weave Router: Feedback recorded") ||
		strings.HasPrefix(trimmed, "Weave Router: router-feedback needs a verdict") ||
		strings.HasPrefix(trimmed, "✦ **Weave Router** → Feedback recorded") ||
		strings.HasPrefix(trimmed, "✦ **Weave Router** → Router-feedback needs a verdict")
}

// matchRouterFeedbackCommand recognizes the command token at the start of the
// first line. It returns the rating encoded directly in the token (for the
// /rf+ and /rf- shortcuts), the inline text following the token, and whether a
// command matched at all.
func matchRouterFeedbackCommand(first string) (rating, inline string, ok bool) {
	for _, c := range []struct{ tok, rating string }{
		{"/rf+", RouterFeedbackRatingUp},
		{"/rf👍", RouterFeedbackRatingUp},
		{"/rf-", RouterFeedbackRatingDown},
		{"/rf👎", RouterFeedbackRatingDown},
	} {
		if first == c.tok {
			return c.rating, "", true
		}
		if after, found := strings.CutPrefix(first, c.tok+" "); found {
			return c.rating, after, true
		}
	}
	if first == "/router-feedback" || first == "/rf" {
		return "", "", true
	}
	if after, found := cutAnyPrefix(first, "/router-feedback ", "/rf "); found {
		return "", after, true
	}
	return "", "", false
}

// splitLeadingRating promotes a leading 👍/👎/+/-/up/down token in a note to a
// rating, returning the rating and the remaining note. Returns ("", s) when the
// note does not open with a recognized verdict.
func splitLeadingRating(s string) (rating, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	tok, after, _ := strings.Cut(s, " ")
	switch strings.ToLower(tok) {
	case "+", "👍", "up", "thumbsup":
		return RouterFeedbackRatingUp, strings.TrimSpace(after)
	case "-", "👎", "down", "thumbsdown":
		return RouterFeedbackRatingDown, strings.TrimSpace(after)
	}
	return "", s
}
