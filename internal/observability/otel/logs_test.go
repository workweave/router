package otel_test

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

// pathCollector records request bodies keyed by the URL path so trace and log
// exports can be told apart (both hit the same base endpoint).
type pathCollector struct {
	server *httptest.Server
	mu     sync.Mutex
	byPath map[string][][]byte
}

func newPathCollector(t *testing.T) *pathCollector {
	t.Helper()
	c := &pathCollector{byPath: map[string][][]byte{}}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c.mu.Lock()
		c.byPath[r.URL.Path] = append(c.byPath[r.URL.Path], body)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *pathCollector) logRecords(t *testing.T) []*logRecordView {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*logRecordView
	for path, payloads := range c.byPath {
		if !strings.HasSuffix(path, "/v1/logs") {
			continue
		}
		for _, p := range payloads {
			var req collogspb.ExportLogsServiceRequest
			require.NoError(t, proto.Unmarshal(p, &req))
			for _, rl := range req.ResourceLogs {
				for _, sl := range rl.ScopeLogs {
					for _, lr := range sl.LogRecords {
						out = append(out, &logRecordView{
							body:    lr.GetBody().GetStringValue(),
							traceID: lr.GetTraceId(),
							attrs:   map[string]string{},
							sevText: lr.GetSeverityText(),
						})
						for _, kv := range lr.Attributes {
							out[len(out)-1].attrs[kv.Key] = kv.GetValue().GetStringValue()
						}
					}
				}
			}
		}
	}
	return out
}

type logRecordView struct {
	body    string
	sevText string
	traceID []byte
	attrs   map[string]string
}

func newPathEmitter(t *testing.T, endpoint string) *otel.Emitter {
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
	return em
}

func TestBuffer_RecordLogAndFlush(t *testing.T) {
	coll := newPathCollector(t)
	em := newPathEmitter(t, coll.server.URL)

	buf := otel.NewBuffer(em)
	require.NotNil(t, buf)

	now := time.Now()
	buf.RecordLog(otel.LogRecord{
		Name:     "router.call",
		Time:     now,
		Severity: otel.SeverityInfo,
		Attrs: otel.NewAttrBuilder(2).
			String("decision.model", "claude-opus-4-8").
			String("io.request_body", `{"messages":[]}`).
			Build(),
	})
	buf.RecordLog(otel.LogRecord{
		Name:     "router.call",
		Time:     now,
		Severity: otel.SeverityError,
		Attrs:    otel.NewAttrBuilder(1).Int64("upstream.status_code", 500).Build(),
	})
	buf.Flush()

	require.NoError(t, em.Shutdown(context.Background()))

	recs := coll.logRecords(t)
	require.Len(t, recs, 2)
	assert.Equal(t, "router.call", recs[0].body)
	// Both records share the buffer's single trace ID.
	assert.Equal(t, recs[0].traceID, recs[1].traceID)

	var info, errRec *logRecordView
	for _, r := range recs {
		switch r.sevText {
		case "INFO":
			info = r
		case "ERROR":
			errRec = r
		}
	}
	require.NotNil(t, info)
	require.NotNil(t, errRec)
	assert.Equal(t, "claude-opus-4-8", info.attrs["decision.model"])
	assert.Equal(t, `{"messages":[]}`, info.attrs["io.request_body"])
}

func TestBuffer_SpansAndLogsShareTraceID(t *testing.T) {
	coll := newPathCollector(t)
	em := newPathEmitter(t, coll.server.URL)

	buf := otel.NewBuffer(em)
	buf.Record(makeSpan("router.upstream"))
	buf.RecordLog(otel.LogRecord{Name: "router.call", Time: time.Now(), Severity: otel.SeverityInfo})
	buf.Flush()

	require.NoError(t, em.Shutdown(context.Background()))

	recs := coll.logRecords(t)
	require.Len(t, recs, 1)
	// Spans went to /v1/traces, logs to /v1/logs — distinct endpoints hit.
	assert.Contains(t, coll.byPath, "/v1/traces")
	assert.Contains(t, coll.byPath, "/v1/logs")
}

func TestRecordLog_NilBufferNoPanic(t *testing.T) {
	var buf *otel.Buffer
	assert.NotPanics(t, func() { buf.RecordLog(otel.LogRecord{Name: "x"}) })
	assert.NotPanics(t, func() { otel.RecordLog(context.Background(), otel.LogRecord{Name: "x"}) })
}
