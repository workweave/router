package translate_test

import (
	"net/http/httptest"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pins the output-progress classification the output-stall watchdog depends on:
// content_block_delta counts; pings and structural frames must not.

func newAnthropicMarkerStreamingWriter(t *testing.T) (*translate.AnthropicRoutingMarkerWriter, *int) {
	t.Helper()
	w := translate.NewAnthropicRoutingMarkerWriter(httptest.NewRecorder(), "claude-sonnet-5", "[routed]")
	require.NoError(t, w.Prelude(true))
	count := 0
	require.True(t, w.ArmOutputProgress(func() { count++ }),
		"ArmOutputProgress must report armed for a streaming client with a marker")
	return w, &count
}

func TestAnthropicMarkerOutputProgress_ContentDeltaCounts(t *testing.T) {
	w, count := newAnthropicMarkerStreamingWriter(t)
	_, err := w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
	require.NoError(t, err)
	assert.Positive(t, *count, "a content_block_delta must mark output progress")
}

func TestAnthropicMarkerOutputProgress_PingDoesNotCount(t *testing.T) {
	w, count := newAnthropicMarkerStreamingWriter(t)
	_, err := w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	require.NoError(t, err)
	assert.Zero(t, *count, "a ping keepalive must not mark output progress")
}

func TestAnthropicMarkerOutputProgress_StructuralFramesDoNotCount(t *testing.T) {
	w, count := newAnthropicMarkerStreamingWriter(t)
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"content\":[]}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	}
	for _, f := range frames {
		_, err := w.Write([]byte(f))
		require.NoError(t, err)
	}
	assert.Zero(t, *count, "message_start / content_block_start / content_block_stop are structural, not output")
}

func TestAnthropicMarkerOutputProgress_NotArmedWhenNotStreaming(t *testing.T) {
	w := translate.NewAnthropicRoutingMarkerWriter(httptest.NewRecorder(), "claude-sonnet-5", "[routed]")
	require.NoError(t, w.Prelude(false))
	assert.False(t, w.ArmOutputProgress(func() {}),
		"ArmOutputProgress must report not-armed for a non-streaming client")
}

func TestAnthropicMarkerOutputProgress_NotArmedWithoutMarker(t *testing.T) {
	// With no marker the writer is a transparent passthrough — it never parses
	// frames, so it cannot mark and must decline arming.
	w := translate.NewAnthropicRoutingMarkerWriter(httptest.NewRecorder(), "claude-sonnet-5", "")
	require.NoError(t, w.Prelude(true))
	assert.False(t, w.ArmOutputProgress(func() {}),
		"ArmOutputProgress must report not-armed when no marker is configured")
}
