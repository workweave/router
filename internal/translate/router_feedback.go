package translate

import (
	"strings"
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
