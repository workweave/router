package observability

import (
	"strings"
	"unicode/utf8"
)

// Preview returns a log-safe excerpt of s, truncated to at most n bytes
// with an ellipsis suffix when truncation occurs. The cut point snaps back
// to a full UTF-8 rune boundary, and further back to the nearest preceding
// whitespace when one exists past the midpoint, so previews don't split a
// multi-byte codepoint or a word mid-token. Callers that need the full body
// stay behind LOG_LEVEL=debug.
func Preview(s string, n int) string {
	if s == "" || n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	// Trim back while the tail is a truncated (invalid) multi-byte
	// sequence rather than a complete rune.
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size != 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	if i := strings.LastIndexAny(cut, " \n\t"); i > n/2 {
		cut = cut[:i]
	}
	return cut + "…"
}
