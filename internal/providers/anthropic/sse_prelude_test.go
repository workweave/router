package anthropic

import (
	"context"
	"io"
	"testing"
	"time"

	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type chunkReader struct {
	chunks [][]byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	return copy(p, chunk), nil
}

func TestInspectSSEPrelude_ClassifiesSplitOverloadAs529(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	input := &chunkReader{chunks: [][]byte{
		[]byte("event: er"),
		[]byte("ror\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\","),
		[]byte("\"message\":\"Overloaded\"}}\n\n"),
	}}

	reader, err := inspectSSEPrelude(ctx, cancel, time.Second, input, nil)

	assert.Nil(t, reader)
	var upstreamErr *providers.UpstreamErrorResponse
	require.ErrorAs(t, err, &upstreamErr)
	assert.Equal(t, 529, upstreamErr.Status)
	assert.True(t, providers.IsRetryable(err))
	assert.JSONEq(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, string(upstreamErr.Body))
}

func TestInspectSSEPrelude_DoesNotMatchErrorTextInsideData(t *testing.T) {
	frame := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"event: error\"}}\n\n"
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	reader, err := inspectSSEPrelude(ctx, cancel, time.Second, &chunkReader{chunks: [][]byte{[]byte(frame)}}, nil)

	require.NoError(t, err)
	got, readErr := io.ReadAll(reader)
	require.NoError(t, readErr)
	assert.Equal(t, frame, string(got))
}
