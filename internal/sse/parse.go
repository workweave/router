package sse

import "bytes"

// SplitNext finds the next complete SSE event in buf, returning the event
// bytes (without the trailing delimiter) and the total number of bytes
// consumed (event + delimiter). Returns n=0 when no complete event is
// available. Supports both LF (\n\n) and CRLF (\r\n\r\n) boundaries.
func SplitNext(buf []byte) (event []byte, n int) {
	lf := bytes.Index(buf, []byte("\n\n"))
	crlf := bytes.Index(buf, []byte("\r\n\r\n"))

	switch {
	case crlf >= 0 && (lf < 0 || crlf < lf):
		return buf[:crlf], crlf + 4
	case lf >= 0:
		return buf[:lf], lf + 2
	default:
		return nil, 0
	}
}

// ParseEvent extracts the event type and data payload from a single SSE
// event without allocating. Both return values are subslices of the input.
// Multi-line data: fields return only the first line's content, which is
// sufficient for the single-line JSON payloads both Anthropic and OpenAI emit.
func ParseEvent(event []byte) (eventType, data []byte) {
	remaining := event
	for len(remaining) > 0 {
		var line []byte
		if idx := bytes.IndexByte(remaining, '\n'); idx >= 0 {
			line = remaining[:idx]
			remaining = remaining[idx+1:]
		} else {
			line = remaining
			remaining = nil
		}
		line = bytes.TrimRight(line, "\r")

		if bytes.HasPrefix(line, []byte("event:")) {
			eventType = bytes.TrimSpace(line[6:])
		} else if data == nil && bytes.HasPrefix(line, []byte("data:")) {
			data = bytes.TrimSpace(line[5:])
		}
	}
	return eventType, data
}
