// Package providers defines the upstream LLM client interface and shared types.
package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"workweave/router/internal/router"
)

// Canonical provider keys used in client registries and router.Decision.Provider.
const (
	ProviderAnthropic  = "anthropic"
	ProviderOpenAI     = "openai"
	ProviderGoogle     = "google"
	ProviderOpenRouter = "openrouter"
	ProviderFireworks  = "fireworks"
)

// HopByHopHeaders are stripped from upstream responses per RFC 7230.
var HopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// CopyUpstreamHeaders copies non-hop-by-hop headers from resp into w. Per
// RFC 7230 §6.1, headers named in the upstream Connection header are also
// treated as hop-by-hop and stripped.
func CopyUpstreamHeaders(w http.ResponseWriter, resp *http.Response) {
	dynamicHop := make(map[string]struct{})
	for _, v := range resp.Header.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				dynamicHop[http.CanonicalHeaderKey(name)] = struct{}{}
			}
		}
	}
	for k, vs := range resp.Header {
		canon := http.CanonicalHeaderKey(k)
		if _, hop := HopByHopHeaders[canon]; hop {
			continue
		}
		if _, hop := dynamicHop[canon]; hop {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// ErrNotImplemented is returned by adapters that don't implement a method.
// The HTTP layer maps it to 501.
var ErrNotImplemented = errors.New("provider: not implemented")

// UpstreamStatusError is returned by Client.Proxy and Client.Passthrough after
// the upstream non-2xx response envelope has already been written through to
// the client. The body is committed before this error is returned, so handlers
// detecting c.Writer.Written() must NOT also write their own JSON envelope.
// Exists so callers can surface upstream rejections that would otherwise hide
// behind a successful proxy of an error response.
type UpstreamStatusError struct {
	Status int
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned status %d", e.Status)
}

// PreparedRequest is the output of translate.RequestEnvelope.Prepare*. Body is
// the fully-encoded target-format request body; Headers carries format-specific
// header overrides (e.g. filtered anthropic-beta) the adapter applies alongside
// its own auth headers.
type PreparedRequest struct {
	Body    []byte
	Headers http.Header
}

type Client interface {
	// Proxy forwards prep.Body verbatim to the upstream and streams the response
	// into w. Implementations must not decode/re-encode the body.
	Proxy(ctx context.Context, decision router.Decision, prep PreparedRequest, w http.ResponseWriter, r *http.Request) error

	// Passthrough forwards an inbound request to the same path on the upstream
	// with no model rewriting and no routing decision applied. Used for
	// ancillary endpoints (model availability, token counting).
	Passthrough(ctx context.Context, prep PreparedRequest, w http.ResponseWriter, r *http.Request) error
}
