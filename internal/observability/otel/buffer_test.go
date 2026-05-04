package otel_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"workweave/router/internal/observability/otel"
)

type collector struct {
	server   *httptest.Server
	mu       sync.Mutex
	payloads [][]byte
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c.mu.Lock()
		c.payloads = append(c.payloads, body)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *collector) allSpanNames(t *testing.T) []string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var names []string
	for _, p := range c.payloads {
		var req coltracepb.ExportTraceServiceRequest
		require.NoError(t, proto.Unmarshal(p, &req))
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					names = append(names, s.Name)
				}
			}
		}
	}
	return names
}

func (c *collector) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.payloads)
}

func newTestEmitter(t *testing.T, endpoint string) *otel.Emitter {
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

func makeSpan(name string) otel.Span {
	now := time.Now()
	return otel.Span{
		Name:  name,
		Start: now,
		End:   now.Add(10 * time.Millisecond),
	}
}

func TestNewBuffer_NilEmitter(t *testing.T) {
	buf := otel.NewBuffer(nil)
	assert.Nil(t, buf)
}

func TestBuffer_NilReceiver_NoOps(t *testing.T) {
	var buf *otel.Buffer

	assert.NotPanics(t, func() { buf.Record(makeSpan("x")) })
	assert.NotPanics(t, func() { buf.Flush() })

	ctx := context.Background()
	got := buf.WithContext(ctx)
	assert.Equal(t, ctx, got)
}

func TestBuffer_RecordAndFlush(t *testing.T) {
	coll := newCollector(t)
	em := newTestEmitter(t, coll.server.URL)

	buf := otel.NewBuffer(em)
	require.NotNil(t, buf)

	buf.Record(makeSpan("span-a"))
	buf.Record(makeSpan("span-b"))
	buf.Flush()

	require.NoError(t, em.Shutdown(context.Background()))

	assert.Equal(t, 1, coll.requestCount())
	names := coll.allSpanNames(t)
	assert.ElementsMatch(t, []string{"span-a", "span-b"}, names)
}

func TestBuffer_ContextRoundTrip(t *testing.T) {
	coll := newCollector(t)
	em := newTestEmitter(t, coll.server.URL)

	buf := otel.NewBuffer(em)
	ctx := buf.WithContext(context.Background())

	otel.Record(ctx, makeSpan("via-ctx"))
	otel.Flush(ctx)

	require.NoError(t, em.Shutdown(context.Background()))

	names := coll.allSpanNames(t)
	assert.Equal(t, []string{"via-ctx"}, names)
}

func TestBuffer_ContextNoBuffer(t *testing.T) {
	ctx := context.Background()
	assert.NotPanics(t, func() { otel.Record(ctx, makeSpan("orphan")) })
	assert.NotPanics(t, func() { otel.Flush(ctx) })
}

func TestBuffer_FlushDrainsSpans(t *testing.T) {
	coll := newCollector(t)
	em := newTestEmitter(t, coll.server.URL)

	buf := otel.NewBuffer(em)
	require.NotNil(t, buf)

	buf.Record(makeSpan("first"))
	buf.Record(makeSpan("second"))
	buf.Flush()

	buf.Record(makeSpan("third"))
	buf.Flush()

	require.NoError(t, em.Shutdown(context.Background()))

	names := coll.allSpanNames(t)
	assert.Len(t, names, 3)
	assert.ElementsMatch(t, []string{"first", "second", "third"}, names)
}
