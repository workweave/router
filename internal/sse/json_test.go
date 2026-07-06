package sse_test

import (
	"bufio"
	"bytes"
	"math"
	"testing"

	"workweave/router/internal/sse"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteJSONString_PlainStringIsQuoted(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	sse.WriteJSONString(w, "hello")
	require.NoError(t, w.Flush())

	assert.Equal(t, `"hello"`, buf.String())
}

func TestWriteJSONString_EscapesQuotesAndBackslashes(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	sse.WriteJSONString(w, `say "hi"\now`)
	require.NoError(t, w.Flush())

	assert.Equal(t, `"say \"hi\"\\now"`, buf.String())
}

func TestWriteJSONString_EscapesControlCharacters(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	sse.WriteJSONString(w, "a\nb\tc")
	require.NoError(t, w.Flush())

	assert.Equal(t, "\"a\\u000ab\\u0009c\"", buf.String())
}

// TestWriteJSONString_EscapesLineSeparators covers U+2028/U+2029, which are
// valid inside a JSON string but illegal as literal characters inside a
// JavaScript string; escaping them keeps output safe for any consumer that
// naively eval's or line-splits the payload.
func TestWriteJSONString_EscapesLineSeparators(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	sse.WriteJSONString(w, "before\u2028middle\u2029after")
	require.NoError(t, w.Flush())

	assert.Equal(t, "\"before\\u2028middle\\u2029after\"", buf.String())
}

func TestWriteJSONString_EmptyStringWritesEmptyQuotes(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	sse.WriteJSONString(w, "")
	require.NoError(t, w.Flush())

	assert.Equal(t, `""`, buf.String())
}

func TestWriteJSONString_PassesThroughMultiByteRunes(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	sse.WriteJSONString(w, "café 中文")
	require.NoError(t, w.Flush())

	assert.Equal(t, "\"café 中文\"", buf.String())
}

func TestWriteJSONInt_Zero(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	sse.WriteJSONInt(w, 0)
	require.NoError(t, w.Flush())

	assert.Equal(t, "0", buf.String())
}

func TestWriteJSONInt_Negative(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	sse.WriteJSONInt(w, -42)
	require.NoError(t, w.Flush())

	assert.Equal(t, "-42", buf.String())
}

func TestWriteJSONInt_LargeValue(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	sse.WriteJSONInt(w, math.MaxInt64)
	require.NoError(t, w.Flush())

	assert.Equal(t, "9223372036854775807", buf.String())
}
