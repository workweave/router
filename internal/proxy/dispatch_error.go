package proxy

import (
	"errors"
	"log/slog"
	"net/http"

	"workweave/router/internal/providers"
	"workweave/router/internal/router/bandit"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/router/hmm"
	"workweave/router/internal/router/rl"
)

// DispatchErrorKind identifies which sentinel a dispatch error (from
// ProxyMessages, ProxyOpenAIChatCompletion, ProxyOpenAIResponses, or
// ProxyGeminiGenerateContent) matched, so a format-specific handler can pick
// its own error-envelope "type"/"status" string without re-deriving the
// classification itself.
type DispatchErrorKind int

const (
	// DispatchErrorNone means ClassifyDispatchError found no match; the
	// caller falls back to its own generic upstream-failure response.
	DispatchErrorNone DispatchErrorKind = iota
	DispatchErrorUpstreamStatus
	DispatchErrorNotImplemented
	DispatchErrorProviderNotConfigured
	DispatchErrorRequestNotJSONObject
	DispatchErrorNoEligibleProvider
	DispatchErrorContextWindowExceeded
	DispatchErrorInvalidRoutingKnobs
	DispatchErrorRLPolicyUnavailable
	DispatchErrorBanditUnavailable
	DispatchErrorHMMUnavailable
	DispatchErrorClusterUnavailable
)

// DispatchErrorClass is the format-agnostic classification of a dispatch
// error: the HTTP status to return, the client-facing message, whether to
// set Retry-After, and how (if at all) the handler should log it. Format
// packages own only the envelope shape (writeAnthropicError/writeOpenAIError/
// writeGeminiError) and any per-Kind "type" string their wire format needs.
type DispatchErrorClass struct {
	Kind       DispatchErrorKind
	Status     int
	Message    string
	RetryAfter bool
	// LogLevel is "", "warn", or "error". Empty means the handler should not
	// log at all (the error is either already logged upstream, e.g. a
	// buffered UpstreamStatusError, or a routine client-input problem).
	LogLevel   string
	LogMessage string
}

// ClassifyDispatchError maps the sentinel errors shared by every dispatch
// entry point (ProxyMessages, ProxyOpenAIChatCompletion, ProxyOpenAIResponses,
// ProxyGeminiGenerateContent) to a DispatchErrorClass. The second return value
// is false when err doesn't match any known sentinel, meaning the caller
// should fall back to its own generic upstream-failure response.
//
// Callers must check c.Writer.Written() (mid-stream failure) before calling
// this — that's HTTP-plumbing, not classification — and must handle
// format-specific sentinels (e.g. proxy.ErrGeminiCrossFormatUnsupported)
// themselves before falling through here.
func ClassifyDispatchError(err error) (DispatchErrorClass, bool) {
	var statusErr *providers.UpstreamStatusError
	switch {
	case errors.As(err, &statusErr):
		return DispatchErrorClass{
			Kind:    DispatchErrorUpstreamStatus,
			Status:  statusErr.Status,
			Message: "Upstream call failed.",
		}, true
	case errors.Is(err, providers.ErrNotImplemented):
		return DispatchErrorClass{
			Kind:    DispatchErrorNotImplemented,
			Status:  http.StatusNotImplemented,
			Message: "Provider not implemented.",
		}, true
	case errors.Is(err, ErrProviderNotConfigured):
		return DispatchErrorClass{
			Kind:    DispatchErrorProviderNotConfigured,
			Status:  http.StatusBadGateway,
			Message: "Provider not configured.",
		}, true
	case errors.Is(err, ErrRequestNotJSONObject):
		return DispatchErrorClass{
			Kind:    DispatchErrorRequestNotJSONObject,
			Status:  http.StatusBadRequest,
			Message: "Request body must be a JSON object.",
		}, true
	case errors.Is(err, cluster.ErrNoEligibleProvider):
		return DispatchErrorClass{
			Kind:       DispatchErrorNoEligibleProvider,
			Status:     http.StatusBadRequest,
			Message:    "No provider keys available for any deployed model: register a BYOK key or supply a provider Authorization header.",
			LogLevel:   "warn",
			LogMessage: "No eligible provider for request",
		}, true
	case errors.Is(err, ErrContextWindowExceeded):
		return DispatchErrorClass{
			Kind:       DispatchErrorContextWindowExceeded,
			Status:     http.StatusRequestEntityTooLarge,
			Message:    "Request context exceeds the largest available model's context window even after compaction. Reduce the conversation (e.g. /compact or start a new session).",
			LogLevel:   "warn",
			LogMessage: "Request context exceeds every eligible model's window after compaction",
		}, true
	case errors.Is(err, cluster.ErrInvalidRoutingKnobs):
		return DispatchErrorClass{
			Kind:       DispatchErrorInvalidRoutingKnobs,
			Status:     http.StatusBadRequest,
			Message:    "Invalid routing knobs supplied.",
			LogLevel:   "warn",
			LogMessage: "Invalid routing knobs supplied",
		}, true
	case errors.Is(err, rl.ErrPolicyUnavailable):
		return DispatchErrorClass{
			Kind:       DispatchErrorRLPolicyUnavailable,
			Status:     http.StatusServiceUnavailable,
			Message:    "Router unavailable: RL policy router failed and no fallback is configured.",
			RetryAfter: true,
			LogLevel:   "error",
			LogMessage: "RL routing unavailable",
		}, true
	case errors.Is(err, bandit.ErrBanditUnavailable):
		return DispatchErrorClass{
			Kind:       DispatchErrorBanditUnavailable,
			Status:     http.StatusServiceUnavailable,
			Message:    "Router unavailable: bandit router failed and no fallback is configured.",
			RetryAfter: true,
			LogLevel:   "error",
			LogMessage: "Bandit routing unavailable",
		}, true
	case errors.Is(err, hmm.ErrHMMUnavailable):
		return DispatchErrorClass{
			Kind:       DispatchErrorHMMUnavailable,
			Status:     http.StatusServiceUnavailable,
			Message:    "Router unavailable: HMM policy router failed and no fallback is configured.",
			RetryAfter: true,
			LogLevel:   "error",
			LogMessage: "HMM routing unavailable",
		}, true
	case errors.Is(err, cluster.ErrClusterUnavailable):
		return DispatchErrorClass{
			Kind:       DispatchErrorClusterUnavailable,
			Status:     http.StatusServiceUnavailable,
			Message:    "Router unavailable: cluster scorer failed and no fallback is configured.",
			RetryAfter: true,
			LogLevel:   "error",
			LogMessage: "Cluster routing unavailable",
		}, true
	default:
		return DispatchErrorClass{}, false
	}
}

// IsClientError reports whether the classified error stems from a bad
// request (as opposed to an upstream/routing failure), which anthropic and
// openai handlers surface as the "invalid_request_error" envelope type
// rather than "api_error".
func (k DispatchErrorKind) IsClientError() bool {
	switch k {
	case DispatchErrorRequestNotJSONObject, DispatchErrorNoEligibleProvider, DispatchErrorContextWindowExceeded, DispatchErrorInvalidRoutingKnobs:
		return true
	default:
		return false
	}
}

// LogDispatchErrorClass logs cls at the level it prescribes (a no-op when
// LogLevel is empty, e.g. a well-understood client-input problem that
// doesn't warrant a log line).
func LogDispatchErrorClass(log *slog.Logger, cls DispatchErrorClass, err error) {
	switch cls.LogLevel {
	case "warn":
		log.Warn(cls.LogMessage, "err", err)
	case "error":
		log.Error(cls.LogMessage, "err", err)
	}
}
