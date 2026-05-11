// Package providers defines the upstream LLM client interface and shared
// request/response types. Provider-specific adapters live in subpackages.
package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"workweave/router/internal/router"
)

// Provider name constants are the canonical keys used in
// map[string]providers.Client registries and router.Decision.Provider.
// Always use these instead of raw string literals.
const (
	ProviderAnthropic  = "anthropic"
	ProviderOpenAI     = "openai"
	ProviderGoogle     = "google"
	ProviderOpenRouter = "openrouter"
	ProviderFireworks  = "fireworks"
)

// HopByHopHeaders are stripped from upstream responses per RFC 7230.
// Connection-specific headers must not be forwarded by a proxy.
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

// CopyUpstreamHeaders copies non-hop-by-hop headers from an upstream response
// into the client-facing ResponseWriter. Per RFC 7230 §6.1, headers named in
// the upstream Connection header are also treated as hop-by-hop and stripped.
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

// ErrNotImplemented is returned by adapters that don't yet implement a given
// method. The HTTP layer maps it to 501. Lives in the inner-ring package so
// handlers can detect it without importing a concrete adapter.
var ErrNotImplemented = errors.New("provider: not implemented")

// UpstreamStatusError is returned by Client.Proxy and Client.Passthrough
// after the implementation has already shoveled an upstream non-2xx response
// envelope through to the client. The body is committed before this error
// is returned, so handlers detecting c.Writer.Written() must NOT also write
// their own JSON envelope. The error exists so callers (proxy.Service and
// monitoring) can surface upstream rejections — e.g. a model-capability
// mismatch like "adaptive thinking is not supported on this model" — that
// would otherwise hide behind a successful proxy of an error response.
type UpstreamStatusError struct {
	Status int
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned status %d", e.Status)
}

// PreparedRequest is the output of translate.RequestEnvelope.Prepare*.
// Contains the fully-encoded target-format request body and any
// format-specific headers the envelope derived (e.g. filtered
// anthropic-beta). Provider adapters apply these to the upstream
// request alongside their own auth headers.
type PreparedRequest struct {
	Body    []byte
	Headers http.Header
}

type Client interface {
	// Proxy forwards a fully-prepared request body to the upstream provider
	// and streams the response into w. The prep.Body is the final wire-format
	// JSON; implementations send it verbatim (no decode/re-encode). prep.Headers
	// contains format-specific header overrides (e.g. filtered anthropic-beta)
	// that the implementation applies alongside its own auth headers.
	Proxy(ctx context.Context, decision router.Decision, prep PreparedRequest, w http.ResponseWriter, r *http.Request) error

	// Passthrough forwards an inbound request to the same path on the upstream
	// provider with no model rewriting and no routing decision applied. Used
	// for ancillary endpoints clients call before the actual prompt — model
	// availability checks (`/v1/models/*`) and token estimation
	// (`/v1/messages/count_tokens`) on Anthropic. Path/method/query come from
	// r; prep contains the (possibly scrubbed) body and derived headers.
	Passthrough(ctx context.Context, prep PreparedRequest, w http.ResponseWriter, r *http.Request) error
}
