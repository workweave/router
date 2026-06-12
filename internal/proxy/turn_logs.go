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
// enabled. The returned captureWriter (nil when off) mirrors the exact bytes
// delivered to the client, so the captured response is in the client's native
// wire format regardless of which upstream served it.
//
// The /v1/responses surface hands us a *translate.ResponsesWriter that performs
// its own surface translation and emits an eager prelude straight to its inner
// writer. Wrapping it externally would capture the pre-translation event stream
// and miss the prelude, so for that case we splice the capture writer in at the
// ResponsesWriter's true client boundary instead — yielding the exact Responses
// wire, prelude included — and return the ResponsesWriter unchanged as the sink.
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

// deferredCallLog lets a wrapping handler run the call-log emission after the
// response body is fully written. The /v1/responses surface wraps the client
// in a translate.ResponsesWriter and calls Finalize only after
// ProxyOpenAIChatCompletion returns; reading the captured body before Finalize
// would yield empty/partial content. When a holder is present on the context,
// ProxyOpenAIChatCompletion stores its emit closure here instead of running it
// inline, and the wrapper invokes run() post-Finalize.
type deferredCallLog struct {
	fn func()
	// requestBody, when set, overrides the captured request body for the call
	// log. ProxyOpenAIResponses sets it to the client's original Responses JSON
	// so io.request_body matches the Responses-format response body, rather than
	// the translated Chat Completions payload ProxyOpenAIChatCompletion sees.
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
// upstream call. It reuses the upstream span's already-built attributes as the
// metadata base (decision, tokens, cost, latency, cluster scoring) and appends
// content attributes per the capture mode. No-op when capture is off, when the
// emitter is disabled (RecordLog finds no buffer), so callers need not guard.
//
// base is the slice returned by the upstream span's AttrBuilder.Build(); it is
// cloned before appending so the span's attributes are never mutated.
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
