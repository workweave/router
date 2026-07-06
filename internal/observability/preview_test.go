package observability_test

import (
	"strings"
	"testing"

	"workweave/router/internal/observability"

	"github.com/stretchr/testify/assert"
)

func TestPreview_ShortStringUnchanged(t *testing.T) {
	assert.Equal(t, "hello", observability.Preview("hello", 10))
}

func TestPreview_EmptyString(t *testing.T) {
	assert.Equal(t, "", observability.Preview("", 10))
}

func TestPreview_ZeroOrNegativeLimit(t *testing.T) {
	assert.Equal(t, "", observability.Preview("hello", 0))
	assert.Equal(t, "", observability.Preview("hello", -1))
}

func TestPreview_TruncatesWithEllipsis(t *testing.T) {
	got := observability.Preview("abcdefghij", 5)
	assert.True(t, strings.HasSuffix(got, "…"), "expected ellipsis suffix, got %q", got)
	assert.LessOrEqual(t, len(got), 5+len("…"))
}

func TestPreview_DoesNotSplitMultiByteRune(t *testing.T) {
	// "日" is 3 bytes (E6 97 A5). Cutting at 4 bytes would land mid-rune
	// without the boundary snap.
	s := "a日本語"
	got := observability.Preview(s, 4)
	assert.True(t, strings.HasSuffix(got, "…"))
	// Every rune in the result (minus the ellipsis) must be valid UTF-8.
	body := strings.TrimSuffix(got, "…")
	assert.True(t, isValidUTF8(body), "preview split a multi-byte rune: %q", got)
}

func TestPreview_SnapsToWhitespaceBoundaryPastMidpoint(t *testing.T) {
	got := observability.Preview("aaaaaaaaaa bbbbbbbbbb", 15)
	// The space at index 10 is past the midpoint (15/2=7), so it should cut there.
	assert.Equal(t, "aaaaaaaaaa…", got)
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
