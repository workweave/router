package openai_test

// Guards the transport-timeout half of router #331. The dominant gpt-5.x failure
// was a stream:false Responses request: OpenAI buffered the whole reasoning+output
// before sending ANY response headers, so the request hung past the header
// timeout and the turn died (~90s, no failover). The fix forces stream:true (a
// regression of which the conformance suite catches via the upstream stream flag)
// AND keeps a bounded — not infinite — ResponseHeaderTimeout. This test guards
// the bound: a stalled-header upstream must surface an error promptly, never hang.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/openai"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProxy_StalledHeadersSurfaceErrorNotHang(t *testing.T) {
	// The upstream never sends headers; it blocks until the test releases it. The
	// only timer is the injected tiny header timeout, so the outcome is
	// deterministic — no sleep, no race. release is closed (defer, LIFO) BEFORE
	// upstream.Close() so the handler returns first; otherwise Close() blocks
	// waiting on the still-serving connection (a client-side header timeout does
	// not reliably cancel the server's request context).
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	const headerTimeout = 50 * time.Millisecond
	c := openai.NewClientWithResponseHeaderTimeout("test-key", upstream.URL, headerTimeout)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))
	prep := providers.PreparedRequest{
		Body:     []byte(`{"model":"gpt-5.5","stream":true}`),
		Headers:  make(http.Header),
		Endpoint: providers.EndpointResponses,
	}

	done := make(chan error, 1)
	go func() {
		done <- c.Proxy(context.Background(), router.Decision{Model: "gpt-5.5"}, prep, rec, clientReq)
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a stalled-header upstream must surface an error, not nil")
		assert.Contains(t, err.Error(), "timeout awaiting response headers",
			"the bounded ResponseHeaderTimeout must fire — a regression to an infinite timeout would hang the turn (router #331)")
	case <-time.After(5 * time.Second):
		t.Fatal("Proxy hung far past the 50ms header timeout — the bounded ResponseHeaderTimeout regressed to unbounded")
	}
}
