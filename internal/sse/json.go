package sse

import (
	"bufio"
	"fmt"
	"strconv"
)

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
			fmt.Fprintf(w, `\u%04x`, c)
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
