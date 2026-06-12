package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"

	"workweave/router/internal/observability/otel"
)

func TestParseCaptureMode(t *testing.T) {
	assert.Equal(t, CaptureOff, ParseCaptureMode(""))
	assert.Equal(t, CaptureOff, ParseCaptureMode("nonsense"))
	assert.Equal(t, CaptureHashed, ParseCaptureMode("hashed"))
	assert.Equal(t, CaptureFull, ParseCaptureMode("FULL"))
	assert.Equal(t, CaptureFull, ParseCaptureMode(" full "))
	assert.Equal(t, "off", CaptureOff.String())
	assert.Equal(t, "hashed", CaptureHashed.String())
	assert.Equal(t, "full", CaptureFull.String())
}

func TestMaybeCaptureResponse_OffReturnsNil(t *testing.T) {
	s := &Service{captureMode: CaptureOff}
	rec := httptest.NewRecorder()
	sink, cw := s.maybeCaptureResponse(rec)
	assert.Nil(t, cw)
	assert.Same(t, http.ResponseWriter(rec), sink)

	body, trunc := capturedResponse(nil)
	assert.Nil(t, body)
	assert.False(t, trunc)
}

func TestMaybeCaptureResponse_CapturesAndTruncates(t *testing.T) {
	s := &Service{captureMode: CaptureFull, captureMaxBytes: 1024}
	rec := httptest.NewRecorder()
	sink, cw := s.maybeCaptureResponse(rec)
	require.NotNil(t, cw)
	_, _ = sink.Write([]byte("hello world"))
	body, trunc := capturedResponse(cw)
	assert.False(t, trunc)
	assert.Equal(t, "hello world", string(body))
	assert.Equal(t, "hello world", rec.Body.String())

	// Over-cap response is dropped and flagged truncated.
	small := &Service{captureMode: CaptureFull, captureMaxBytes: 4}
	rec2 := httptest.NewRecorder()
	sink2, cap2 := small.maybeCaptureResponse(rec2)
	_, _ = sink2.Write([]byte("way too long"))
	body2, trunc2 := capturedResponse(cap2)
	assert.Nil(t, body2)
	assert.True(t, trunc2)
	// Client still received the full bytes despite capture overflow.
	assert.Equal(t, "way too long", rec2.Body.String())
}

// logCollector captures OTLP /v1/logs exports for assertion.
type logCollector struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newLogCollector(t *testing.T) *logCollector {
	t.Helper()
	c := &logCollector{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/v1/logs") {
			c.mu.Lock()
			c.bodies = append(c.bodies, b)
			c.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *logCollector) attrs(t *testing.T) map[string]string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for _, b := range c.bodies {
		var req collogspb.ExportLogsServiceRequest
		require.NoError(t, proto.Unmarshal(b, &req))
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					for _, kv := range lr.Attributes {
						out[kv.Key] = kv.GetValue().GetStringValue()
					}
				}
			}
		}
	}
	return out
}

func (c *logCollector) count(t *testing.T) int {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, b := range c.bodies {
		var req collogspb.ExportLogsServiceRequest
		require.NoError(t, proto.Unmarshal(b, &req))
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				n += len(sl.LogRecords)
			}
		}
	}
	return n
}

func newServiceWithEmitter(t *testing.T, mode ContentCaptureMode, redactor Redactor, endpoint string) (*Service, *otel.Emitter) {
	t.Helper()
	em, err := otel.NewEmitter(otel.EmitterConfig{
		Endpoint:      endpoint,
		ServiceName:   "test",
		Workers:       1,
		QueueSize:     100,
		BatchSize:     100,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)
	return &Service{emitter: em, captureMode: mode, captureMaxBytes: 1 << 20, redactor: redactor}, em
}

func TestRecordCallLog_OffEmitsNothing(t *testing.T) {
	coll := newLogCollector(t)
	s, em := newServiceWithEmitter(t, CaptureOff, nil, coll.server.URL)

	buf := otel.NewBuffer(em)
	ctx := buf.WithContext(context.Background())
	base := otel.NewAttrBuilder(1).String("decision.model", "claude-opus-4-8").Build()
	s.recordCallLog(ctx, base, false, []byte("req"), []byte("resp"), false)
	otel.Flush(ctx)

	require.NoError(t, em.Shutdown(context.Background()))
	assert.Equal(t, 0, coll.count(t))
}

func TestRecordCallLog_FullCapturesBodies(t *testing.T) {
	coll := newLogCollector(t)
	s, em := newServiceWithEmitter(t, CaptureFull, nil, coll.server.URL)

	buf := otel.NewBuffer(em)
	ctx := buf.WithContext(context.Background())
	base := otel.NewAttrBuilder(1).String("decision.model", "claude-opus-4-8").Build()
	s.recordCallLog(ctx, base, false, []byte(`{"req":1}`), []byte(`{"resp":2}`), false)
	otel.Flush(ctx)

	require.NoError(t, em.Shutdown(context.Background()))
	require.Equal(t, 1, coll.count(t))
	a := coll.attrs(t)
	assert.Equal(t, "claude-opus-4-8", a["decision.model"]) // base metadata carried over
	assert.Equal(t, `{"req":1}`, a["io.request_body"])
	assert.Equal(t, `{"resp":2}`, a["io.response_body"])
	assert.Empty(t, a["io.request_sha256"])
}

func TestRecordCallLog_HashedOmitsRawText(t *testing.T) {
	coll := newLogCollector(t)
	s, em := newServiceWithEmitter(t, CaptureHashed, nil, coll.server.URL)

	buf := otel.NewBuffer(em)
	ctx := buf.WithContext(context.Background())
	s.recordCallLog(ctx, nil, false, []byte("secret-prompt"), []byte("secret-response"), false)
	otel.Flush(ctx)

	require.NoError(t, em.Shutdown(context.Background()))
	a := coll.attrs(t)
	assert.NotEmpty(t, a["io.request_sha256"])
	assert.NotEmpty(t, a["io.response_sha256"])
	assert.Empty(t, a["io.request_body"])
	assert.NotContains(t, a["io.request_sha256"], "secret")
}

func TestDeferredCallLog_ReadsBodyAtRunTime(t *testing.T) {
	coll := newLogCollector(t)
	s, em := newServiceWithEmitter(t, CaptureFull, nil, coll.server.URL)

	ctx, h := withDeferredCallLog(context.Background())
	require.NotNil(t, deferredCallLogFrom(ctx), "holder must be retrievable from ctx")

	buf := otel.NewBuffer(em)
	ctx = buf.WithContext(ctx)

	// Simulate the ProxyOpenAIChatCompletion tail: capture writer is wired, but
	// the (buffered) ResponsesWriter hasn't written the body yet — the emit is
	// deferred rather than run inline.
	rec := httptest.NewRecorder()
	_, cw := s.maybeCaptureResponse(rec)
	base := otel.NewAttrBuilder(1).String("decision.model", "m").Build()
	h.fn = func() {
		body, trunc := capturedResponse(cw)
		s.recordCallLog(ctx, base, false, []byte("req"), body, trunc)
		otel.Flush(ctx)
	}

	// Body arrives during "Finalize", after the deferral was registered.
	_, _ = cw.Write([]byte(`{"final":"body"}`))
	h.run()

	require.NoError(t, em.Shutdown(context.Background()))
	a := coll.attrs(t)
	assert.Equal(t, `{"final":"body"}`, a["io.response_body"], "deferred emit must read the post-Finalize body")
}

func TestDeferredCallLog_SafeWhenAbsent(t *testing.T) {
	assert.Nil(t, deferredCallLogFrom(context.Background()))
	var d *deferredCallLog
	assert.NotPanics(t, func() { d.run() })                    // nil receiver
	assert.NotPanics(t, func() { (&deferredCallLog{}).run() }) // no fn registered
}

func TestRecordCallLog_RedactorApplied(t *testing.T) {
	coll := newLogCollector(t)
	redactor := func(content string, kind ContentKind) string {
		if kind == ContentKindRequest {
			return "REDACTED-REQ"
		}
		return "REDACTED-RESP"
	}
	s, em := newServiceWithEmitter(t, CaptureFull, redactor, coll.server.URL)

	buf := otel.NewBuffer(em)
	ctx := buf.WithContext(context.Background())
	s.recordCallLog(ctx, nil, false, []byte("raw-req"), []byte("raw-resp"), false)
	otel.Flush(ctx)

	require.NoError(t, em.Shutdown(context.Background()))
	a := coll.attrs(t)
	assert.Equal(t, "REDACTED-REQ", a["io.request_body"])
	assert.Equal(t, "REDACTED-RESP", a["io.response_body"])
}
