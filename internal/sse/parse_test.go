package sse_test

import (
	"testing"

	"workweave/router/internal/sse"

	"github.com/stretchr/testify/assert"
)

func TestSplitNext_LFBoundary(t *testing.T) {
	event, n := sse.SplitNext([]byte("event: message\ndata: {}\n\nrest"))

	assert.Equal(t, "event: message\ndata: {}", string(event))
	assert.Equal(t, len("event: message\ndata: {}\n\n"), n)
}

func TestSplitNext_CRLFBoundary(t *testing.T) {
	event, n := sse.SplitNext([]byte("event: message\r\ndata: {}\r\n\r\nrest"))

	assert.Equal(t, "event: message\r\ndata: {}", string(event))
	assert.Equal(t, len("event: message\r\ndata: {}\r\n\r\n"), n)
}

func TestSplitNext_IncompleteBufferReturnsZero(t *testing.T) {
	event, n := sse.SplitNext([]byte("event: message\ndata: {}"))

	assert.Nil(t, event)
	assert.Equal(t, 0, n)
}

func TestSplitNext_EmptyBufferReturnsZero(t *testing.T) {
	event, n := sse.SplitNext(nil)

	assert.Nil(t, event)
	assert.Equal(t, 0, n)
}

// TestSplitNext_CRLFPrecedenceWhenBothBoundariesPresent covers the case where
// an earlier LF-only boundary and a later CRLF boundary both appear in the
// buffer; SplitNext must prefer whichever boundary starts first, not always
// CRLF, so this pins down the crpf < lf tie-break in the switch.
func TestSplitNext_EarlierLFBoundaryWinsOverLaterCRLF(t *testing.T) {
	buf := []byte("a\n\nb\r\n\r\nc")

	event, n := sse.SplitNext(buf)

	assert.Equal(t, "a", string(event))
	assert.Equal(t, 3, n)
}

func TestSplitNext_EarlierCRLFBoundaryWinsOverLaterLF(t *testing.T) {
	buf := []byte("a\r\n\r\nb\n\nc")

	event, n := sse.SplitNext(buf)

	assert.Equal(t, "a", string(event))
	assert.Equal(t, 5, n)
}

func TestParseEvent_ExtractsEventTypeAndData(t *testing.T) {
	eventType, data := sse.ParseEvent([]byte("event: content_block_delta\ndata: {\"type\":\"text\"}"))

	assert.Equal(t, "content_block_delta", string(eventType))
	assert.Equal(t, `{"type":"text"}`, string(data))
}

func TestParseEvent_NoEventLineLeavesEventTypeEmpty(t *testing.T) {
	eventType, data := sse.ParseEvent([]byte("data: {\"ok\":true}"))

	assert.Empty(t, eventType)
	assert.Equal(t, `{"ok":true}`, string(data))
}

// TestParseEvent_MultiLineDataReturnsOnlyFirstLine pins down the documented
// behavior: for a multi-line `data:` field, only the first line is returned.
// This holds regardless of which provider emitted the event — Anthropic,
// OpenAI, and Gemini (via internal/translate/gemini_stream.go) all call
// ParseEvent and all only ever emit single-line JSON payloads, so returning
// just the first `data:` line is safe for all three.
func TestParseEvent_MultiLineDataReturnsOnlyFirstLine(t *testing.T) {
	eventType, data := sse.ParseEvent([]byte("event: message\ndata: first-line\ndata: second-line"))

	assert.Equal(t, "message", string(eventType))
	assert.Equal(t, "first-line", string(data))
}

func TestParseEvent_IgnoresCarriageReturnAtLineEnd(t *testing.T) {
	eventType, data := sse.ParseEvent([]byte("event: message\r\ndata: {}\r"))

	assert.Equal(t, "message", string(eventType))
	assert.Equal(t, "{}", string(data))
}

func TestParseEvent_EmptyEventReturnsEmptyValues(t *testing.T) {
	eventType, data := sse.ParseEvent(nil)

	assert.Empty(t, eventType)
	assert.Nil(t, data)
}
