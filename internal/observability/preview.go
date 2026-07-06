package observability

import (
	"strings"
	"unicode/utf8"
)

// ellipsis is appended to indicate truncation. Its byte length is reserved
// out of n so Preview's result never exceeds n bytes — several migrated
// callers (e.g. toolcheck.truncateDetail's maxDetailBytes, the Anthropic
// meta-preview log field) previously enforced a hard n-byte cap with no
// ellipsis, and relied on that exact bound.
const ellipsis = "…"

// Preview returns a log-safe excerpt of s that is never longer than n
// bytes, including the ellipsis suffix appended when truncation occurs.
// The cut point snaps back to a full UTF-8 rune boundary, and further back
// to the nearest preceding whitespace when one exists past the midpoint,
// so previews don't split a multi-byte codepoint or a word mid-token.
// Callers that need the full body stay behind LOG_LEVEL=debug.
func Preview(s string, n int) string {
	if s == "" || n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	limit := n - len(ellipsis)
	if limit <= 0 {
		// n too small to fit any content plus the ellipsis; hard-truncate
		// on a rune boundary with no ellipsis rather than overshoot n.
		return trimToRuneBoundary(s[:n])
	}
	cut := trimToRuneBoundary(s[:limit])
	if i := strings.LastIndexAny(cut, " \n\t"); i > n/2 {
		cut = cut[:i]
	}
	return cut + ellipsis
}

// trimToRuneBoundary trims trailing bytes off s while its tail is a
// truncated (invalid) multi-byte sequence rather than a complete rune.
func trimToRuneBoundary(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
