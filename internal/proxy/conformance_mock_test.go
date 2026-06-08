package proxy_test

// mockUpstream is a fake provider endpoint for the conformance suite. It records
// the request the router sent (path, body, headers) so a case can assert the
// outbound translation, then replays a canned provider-format response (loaded
// from a testdata fixture) so a case can assert the inbound translation. One
// shape serves every format — OpenAI /v1/chat/completions, OpenAI /v1/responses,
// and Gemini :generateContent / :streamGenerateContent — because the router
// targets them all with the same client.Do call; only the path and body differ.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"workweave/router/internal/sse"

	"github.com/stretchr/testify/require"
)

type mockUpstream struct {
	// response to serve
	streaming bool // serve as text/event-stream (true) or application/json (false)
	status    int  // 0 => 200
	body      []byte

	mu      sync.Mutex
	gotPath string
	gotBody []byte
	gotHdr  http.Header
}

// newMockUpstream starts an httptest server replaying fixture in the given mode
// and returns it plus its base URL. Caller defers srv.Close().
func newMockUpstream(t *testing.T, streaming bool, status int, fixture []byte) (*mockUpstream, *httptest.Server) {
	t.Helper()
	m := &mockUpstream{streaming: streaming, status: status, body: fixture}
	srv := httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(srv.Close)
	return m, srv
}

func (m *mockUpstream) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.gotPath = r.URL.Path
	m.gotBody = body
	m.gotHdr = r.Header.Clone()
	m.mu.Unlock()

	status := m.status
	if status == 0 {
		status = http.StatusOK
	}
	if !m.streaming {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(m.body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)
	// Replay frame-by-frame with a flush between each, mirroring a real upstream
	// streaming the SSE wire. The translator must reassemble incrementally.
	buf := m.body
	for {
		_, n := sse.SplitNext(buf)
		if n == 0 {
			break
		}
		_, _ = w.Write(buf[:n])
		if flusher != nil {
			flusher.Flush()
		}
		buf = buf[n:]
	}
	if len(buf) > 0 {
		_, _ = w.Write(buf)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// captured returns the recorded upstream request path, body, and headers.
func (m *mockUpstream) captured(t *testing.T) (path string, body []byte, hdr http.Header) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.NotEmpty(t, m.gotBody, "mock upstream was never called")
	return m.gotPath, m.gotBody, m.gotHdr
}
