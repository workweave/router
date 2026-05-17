package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"
)

// fakeHandoverProvider is the minimal providers.Client surface needed by
// the summarizer test: it can return a canned non-streaming body, sleep
// past a deadline, or surface a non-2xx status.
type fakeHandoverProvider struct {
	respBody    string
	respStatus  int
	sleep       time.Duration
	upstreamErr error
}

func (f *fakeHandoverProvider) Proxy(ctx context.Context, _ router.Decision, _ providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	if f.sleep > 0 {
		select {
		case <-time.After(f.sleep):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.upstreamErr != nil {
		return f.upstreamErr
	}
	if f.respStatus == 0 {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(f.respStatus)
	}
	_, _ = io.WriteString(w, f.respBody)
	return nil
}

func (f *fakeHandoverProvider) Passthrough(_ context.Context, _ providers.PreparedRequest, _ http.ResponseWriter, _ *http.Request) error {
	return nil
}

// sampleConversation is the test fixture used across the cases. Both
// system and a couple of message turns so buildHandoverRequestBody has
// real content to flatten.
const sampleConversation = `{
  "model": "claude-opus-4-7",
  "system": "You are a helpful assistant.",
  "messages": [
    {"role": "user", "content": "Step 1?"},
    {"role": "assistant", "content": "Done."},
    {"role": "user", "content": "Step 2?"}
  ]
}`

// canonicalAnthropicResponse is what a real non-streaming Anthropic
// /v1/messages response looks like. The summarizer's job is to extract
// the text from the content[] blocks.
const canonicalAnthropicResponse = `{
  "id": "msg_test_001",
  "type": "message",
  "role": "assistant",
  "model": "claude-haiku-4-5",
  "stop_reason": "end_turn",
  "content": [
    {"type": "text", "text": "Refactor in progress: step 1 done, step 2 pending."}
  ],
  "usage": {"input_tokens": 42, "output_tokens": 17}
}`

func TestProviderSummarizer_SuccessReturnsAssistantText(t *testing.T) {
	t.Parallel()

	env, err := translate.ParseAnthropic([]byte(sampleConversation))
	require.NoError(t, err)

	fake := &fakeHandoverProvider{
		respBody:   canonicalAnthropicResponse,
		respStatus: http.StatusOK,
	}
	s := NewProviderSummarizer(fake, "", 200*time.Millisecond)

	got, _, err := s.Summarize(context.Background(), env)
	require.NoError(t, err)
	assert.Equal(t, "Refactor in progress: step 1 done, step 2 pending.", got)
}

func TestProviderSummarizer_TimeoutReturnsError(t *testing.T) {
	t.Parallel()

	env, err := translate.ParseAnthropic([]byte(sampleConversation))
	require.NoError(t, err)

	fake := &fakeHandoverProvider{
		respBody: canonicalAnthropicResponse,
		// Sleep longer than the summarizer's timeout.
		sleep: 200 * time.Millisecond,
	}
	s := NewProviderSummarizer(fake, "", 25*time.Millisecond)

	got, _, err := s.Summarize(context.Background(), env)
	require.Error(t, err)
	assert.Empty(t, got)
	// Either the ctx.Err() bubble or the fake's own ctx-aware return both
	// surface DeadlineExceeded.
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "expected DeadlineExceeded, got %v", err)
}

func TestProviderSummarizer_Non2xxReturnsError(t *testing.T) {
	t.Parallel()

	env, err := translate.ParseAnthropic([]byte(sampleConversation))
	require.NoError(t, err)

	fake := &fakeHandoverProvider{
		respBody:   `{"error":"oops"}`,
		respStatus: http.StatusInternalServerError,
	}
	s := NewProviderSummarizer(fake, "", 200*time.Millisecond)

	got, _, err := s.Summarize(context.Background(), env)
	require.Error(t, err)
	assert.Empty(t, got)
	assert.True(t, strings.Contains(err.Error(), "500"), "error must mention upstream status 500; got %v", err)
}

func TestProviderSummarizer_EmptyContentReturnsErrEmptySummary(t *testing.T) {
	t.Parallel()

	env, err := translate.ParseAnthropic([]byte(sampleConversation))
	require.NoError(t, err)

	// Successful 200 but no text blocks (e.g. truncated, or a stop
	// reason with empty content[]).
	fake := &fakeHandoverProvider{
		respBody:   `{"id":"msg_empty","content":[]}`,
		respStatus: http.StatusOK,
	}
	s := NewProviderSummarizer(fake, "", 200*time.Millisecond)

	got, _, err := s.Summarize(context.Background(), env)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptySummary)
	assert.Empty(t, got)
}

func TestProviderSummarizer_NilEnvelopeReturnsError(t *testing.T) {
	t.Parallel()

	fake := &fakeHandoverProvider{}
	s := NewProviderSummarizer(fake, "", 200*time.Millisecond)

	_, _, err := s.Summarize(context.Background(), nil)
	require.Error(t, err)
}
