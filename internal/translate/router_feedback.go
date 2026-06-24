package translate

import (
	"strings"
)

// RouterFeedbackResult holds the parsed outcome of a /router-feedback command.
type RouterFeedbackResult struct {
	// Feedback is the free-form text the user submitted after the command.
	Feedback string
}

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

	var feedback string
	if after, ok := cutAnyPrefix(first, "/router-feedback ", "/rf "); ok {
		feedback = strings.TrimSpace(after)
	} else if first != "/router-feedback" && first != "/rf" {
		return RouterFeedbackResult{}, false, text
	}
	if rest = strings.TrimSpace(rest); rest != "" {
		if feedback != "" {
			feedback += "\n"
		}
		feedback += rest
	}
	return RouterFeedbackResult{Feedback: feedback}, true, strings.TrimSpace(prefix)
}
