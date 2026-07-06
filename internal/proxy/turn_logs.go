package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"time"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/translate"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
)

// ContentCaptureMode controls how much request/response content the router
// emits over OTLP as high-fidelity `router.call` log records.
type ContentCaptureMode int

const (
	// CaptureOff emits no `router.call` log records. Spans and the existing
	// telemetry row are unaffected. Default for self-hosted / OSS.
	CaptureOff ContentCaptureMode = iota
	// CaptureHashed emits log records with metadata + SHA-256 content hashes
	// but no raw text — dedup/cache analysis without exposing prompts.
	CaptureHashed
	// CaptureFull emits log records with full raw request/response bodies
	// (after the redaction hook). Default for Weave-managed deploys.
	CaptureFull
)

// ContentKind tells the redaction hook whether it is scrubbing a request or a
// response body, so callers can apply asymmetric policies.
type ContentKind int

const (
	// ContentKindRequest marks an inbound request body.
	ContentKindRequest ContentKind = iota
	// ContentKindResponse marks an outbound response body.
	ContentKindResponse
)

// Redactor scrubs sensitive content before it enters the OTLP export queue.
// A nil redactor passes content through unchanged.
type Redactor func(content string, kind ContentKind) string

// ParseCaptureMode maps a config string to a ContentCaptureMode. Unknown or
// empty values fall back to CaptureOff (the safe default).
func ParseCaptureMode(raw string) ContentCaptureMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "full":
		return CaptureFull
	case "hashed":
		return CaptureHashed
	default:
		return CaptureOff
	}
}

func (m ContentCaptureMode) String() string {
	switch m {
	case CaptureFull:
		return "full"
	case CaptureHashed:
		return "hashed"
	default:
		return "off"
	}
}

// maybeCaptureResponse wraps w with a content-capturing writer when capture is
// enabled, mirroring the exact bytes sent to the client (nil when off).
//
// ResponsesWriter translates and emits an eager prelude to its inner writer, so
// wrapping it externally would miss the prelude and capture pre-translation
// bytes; instead we splice the capture writer at its true client boundary.
func (s *Service) maybeCaptureResponse(w http.ResponseWriter) (http.ResponseWriter, *captureWriter) {
	if s.captureMode == CaptureOff {
		return w, nil
	}
	if rw, ok := w.(*translate.ResponsesWriter); ok {
		var cw *captureWriter
		rw.WrapInner(func(inner http.ResponseWriter) http.ResponseWriter {
			cw = newCaptureWriter(inner, s.captureMaxBytes)
			return cw
		})
		return w, cw
	}
	cw := newCaptureWriter(w, s.captureMaxBytes)
	return cw, cw
}

// capturedResponse extracts the buffered response body. truncated is true when
// the body exceeded the capture cap (the buffer is then dropped).
func capturedResponse(c *captureWriter) (body []byte, truncated bool) {
	if c == nil {
		return nil, false
	}
	b, _, ok := c.captured()
	if !ok {
		return nil, true
	}
	return b, false
}

// deferredCallLog lets a wrapping handler run call-log emission after the
// response body is fully written, since /v1/responses' ResponsesWriter only
// calls Finalize after ProxyOpenAIChatCompletion returns (reading the
// captured body earlier would yield empty/partial content).
type deferredCallLog struct {
	fn func()
	// requestBody overrides the captured request body: ProxyOpenAIResponses
	// sets it to the client's original Responses JSON so io.request_body
	// matches, instead of the translated Chat Completions payload.
	requestBody []byte
}

type deferredCallLogKey struct{}

func withDeferredCallLog(ctx context.Context) (context.Context, *deferredCallLog) {
	h := &deferredCallLog{}
	return context.WithValue(ctx, deferredCallLogKey{}, h), h
}

func deferredCallLogFrom(ctx context.Context) *deferredCallLog {
	h, _ := ctx.Value(deferredCallLogKey{}).(*deferredCallLog)
	return h
}

// run invokes the deferred emit if one was registered. Safe on nil receiver
// and when no emit was stored (e.g. the request errored before any call).
func (d *deferredCallLog) run() {
	if d != nil && d.fn != nil {
		d.fn()
	}
}

func (s *Service) redact(content []byte, kind ContentKind) string {
	if s.redactor == nil {
		return string(content)
	}
	return s.redactor(string(content), kind)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// recordCallLog emits a high-fidelity `router.call` OTLP log record for one
// upstream call, reusing the upstream span's attributes as the metadata base
// and appending content attributes per capture mode. No-op when capture is
// off. base is cloned before appending so the span's attributes aren't mutated.
func (s *Service) recordCallLog(ctx context.Context, base []*commonv1.KeyValue, isErr bool, reqBody, respBody []byte, respTruncated bool) {
	if s.captureMode == CaptureOff {
		return
	}

	content := otel.NewAttrBuilder(6).
		Int64("io.request_bytes", int64(len(reqBody))).
		Int64("io.response_bytes", int64(len(respBody))).
		Bool("io.truncated", respTruncated)

	switch s.captureMode {
	case CaptureFull:
		content.String("io.request_body", s.redact(reqBody, ContentKindRequest)).
			String("io.response_body", s.redact(respBody, ContentKindResponse))
	case CaptureHashed:
		content.String("io.request_sha256", sha256Hex(reqBody)).
			String("io.response_sha256", sha256Hex(respBody))
	}

	attrs := append(slices.Clone(base), content.Build()...)
	sev := otel.SeverityInfo
	if isErr {
		sev = otel.SeverityError
	}
	otel.RecordLog(ctx, otel.LogRecord{
		Name:     "router.call",
		Time:     time.Now(),
		Severity: sev,
		Attrs:    attrs,
	})
}
