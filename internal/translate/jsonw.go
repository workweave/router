// Package translate — jsonWriter is a GoF Builder for structural JSON assembly.
package translate

import (
	"bufio"
	"bytes"
	"strconv"

	"workweave/router/internal/sse"
)

type jsonWriter struct {
	buf      *bytes.Buffer
	bw       *bufio.Writer
	depth    int
	first    []bool
	afterKey bool
}

func newJSONWriter() *jsonWriter {
	buf := &bytes.Buffer{}
	return &jsonWriter{
		buf:   buf,
		bw:    bufio.NewWriterSize(buf, 8192),
		first: []bool{true},
	}
}

func (w *jsonWriter) sep() {
	if w.afterKey {
		w.afterKey = false
		return
	}
	if w.first[w.depth] {
		w.first[w.depth] = false
		return
	}
	w.bw.WriteByte(',')
}

func (w *jsonWriter) Obj() {
	w.sep()
	w.bw.WriteByte('{')
	w.depth++
	w.pushFirst()
}

func (w *jsonWriter) EndObj() {
	w.depth--
	w.bw.WriteByte('}')
}

func (w *jsonWriter) pushFirst() {
	if w.depth >= len(w.first) {
		w.first = append(w.first, true)
	} else {
		w.first[w.depth] = true
	}
}

func (w *jsonWriter) Arr() {
	w.sep()
	w.bw.WriteByte('[')
	w.depth++
	w.pushFirst()
}

func (w *jsonWriter) EndArr() {
	w.depth--
	w.bw.WriteByte(']')
}

func (w *jsonWriter) Key(name string) {
	w.sep()
	sse.WriteJSONString(w.bw, name)
	w.bw.WriteByte(':')
	w.afterKey = true
}

func (w *jsonWriter) Str(s string) {
	w.sep()
	sse.WriteJSONString(w.bw, s)
}

func (w *jsonWriter) Int(n int64) {
	w.sep()
	sse.WriteJSONInt(w.bw, n)
}

func (w *jsonWriter) Float(f float64) {
	w.sep()
	var scratch [32]byte
	w.bw.Write(strconv.AppendFloat(scratch[:0], f, 'f', -1, 64))
}

func (w *jsonWriter) Bool(b bool) {
	w.sep()
	if b {
		w.bw.WriteString("true")
	} else {
		w.bw.WriteString("false")
	}
}

func (w *jsonWriter) Null() {
	w.sep()
	w.bw.WriteString("null")
}

func (w *jsonWriter) Raw(json string) {
	w.sep()
	w.bw.WriteString(json)
}

func (w *jsonWriter) RawBytes(b []byte) {
	w.sep()
	w.bw.Write(b)
}

func (w *jsonWriter) Bytes() []byte {
	w.bw.Flush()
	return w.buf.Bytes()
}
