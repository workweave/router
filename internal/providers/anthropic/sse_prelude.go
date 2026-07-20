package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/sse"
	"workweave/router/internal/timing"
)

const anthropicOverloadedStatus = 529

// inspectSSEPrelude buffers the first complete SSE event. Anthropic can report
// an overloaded request as an event:error inside an otherwise successful HTTP
// 200 response; surfacing it before writing lets the dispatch loop retry
// without corrupting the stream.
func inspectSSEPrelude(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	idleTimeout time.Duration,
	body io.Reader,
	t *timing.Timing,
) (io.Reader, error) {
	mark, stop := httputil.StartIdleWatchdog(ctx, cancel, idleTimeout)
	defer stop()

	var buffered bytes.Buffer
	readBuf := make([]byte, httputil.FlushChunk)
	for buffered.Len() < providers.MaxBufferedErrorBytes {
		n, readErr := body.Read(readBuf)
		if n > 0 {
			mark()
			t.StampUpstreamFirstByte()
			remaining := providers.MaxBufferedErrorBytes - buffered.Len()
			if n > remaining {
				n = remaining
			}
			_, _ = buffered.Write(readBuf[:n])

			event, consumed := sse.SplitNext(buffered.Bytes())
			if consumed != 0 {
				eventType, data := sse.ParseEvent(event)
				if bytes.Equal(eventType, []byte("error")) {
					return nil, &providers.UpstreamErrorResponse{
						Status: anthropicSSEErrorStatus(data),
						Body:   bytes.Clone(data),
					}
				}
				return io.MultiReader(bytes.NewReader(bytes.Clone(buffered.Bytes())), body), nil
			}
		}

		if readErr == io.EOF {
			t.StampUpstreamEOF()
			return bytes.NewReader(bytes.Clone(buffered.Bytes())), nil
		}
		if readErr != nil {
			if cause := context.Cause(ctx); errors.Is(cause, httputil.ErrUpstreamIdleTimeout) {
				return nil, cause
			}
			return nil, readErr
		}
	}

	return io.MultiReader(bytes.NewReader(bytes.Clone(buffered.Bytes())), body), nil
}

func anthropicSSEErrorStatus(data []byte) int {
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return http.StatusInternalServerError
	}
	switch envelope.Error.Type {
	case "overloaded_error":
		return anthropicOverloadedStatus
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	case "invalid_request_error":
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
