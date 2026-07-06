package sse

import (
	"bufio"
	"strconv"
)

const hexDigits = "0123456789abcdef"

// writeJSONUnicodeEscape writes c as a \uXXXX escape to w via a manual
// fixed-width hex encode, avoiding fmt.Fprintf's reflection overhead on this
// package's zero-alloc SSE framing path.
func writeJSONUnicodeEscape(w *bufio.Writer, c rune) {
	w.WriteByte('\\')
	w.WriteByte('u')
	w.WriteByte(hexDigits[(c>>12)&0xf])
	w.WriteByte(hexDigits[(c>>8)&0xf])
	w.WriteByte(hexDigits[(c>>4)&0xf])
	w.WriteByte(hexDigits[c&0xf])
}

// WriteJSONString writes s as a quoted, JSON-escaped string to w.
func WriteJSONString(w *bufio.Writer, s string) {
	w.WriteByte('"')
	for _, c := range s {
		switch {
		case c == '"':
			w.WriteByte('\\')
			w.WriteByte('"')
		case c == '\\':
			w.WriteByte('\\')
			w.WriteByte('\\')
		case c < 0x20, c == '\u2028', c == '\u2029':
			writeJSONUnicodeEscape(w, c)
		default:
			w.WriteRune(c)
		}
	}
	w.WriteByte('"')
}

// WriteJSONInt writes n as a decimal integer to w.
func WriteJSONInt(w *bufio.Writer, n int64) {
	var scratch [20]byte
	w.Write(strconv.AppendInt(scratch[:0], n, 10))
}
