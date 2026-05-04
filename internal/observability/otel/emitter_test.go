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
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"

	"workweave/router/internal/observability/otel"
)

// testSpan returns a minimal valid tracev1.Span with the given name.
func testSpan(name string) *tracev1.Span {
	return &tracev1.Span{
		TraceId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:  []byte{1, 2, 3, 4, 5, 6, 7, 8},
		Name:    name,
	}
}

func TestNewEmitter_EmptyEndpoint(t *testing.T) {
	em, err := otel.NewEmitter(otel.EmitterConfig{})
	assert.NoError(t, err)
	assert.Nil(t, em)
}

func TestNewEmitter_NilMethods(t *testing.T) {
	var em *otel.Emitter
	em.Enqueue(testSpan("noop"))
	err := em.Shutdown(context.Background())
	assert.NoError(t, err)
}

func TestEmitter_CustomHeaders(t *testing.T) {
	var mu sync.Mutex
	var got string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Get("X-Custom-Auth")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	em, err := otel.NewEmitter(otel.EmitterConfig{
		Endpoint:      srv.URL,
		Headers:       map[string]string{"X-Custom-Auth": "test-key"},
		Workers:       1,
		QueueSize:     10,
		BatchSize:     1,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)

	em.Enqueue(testSpan("test"))
	require.NoError(t, em.Shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "test-key", got)
}

func TestEmitter_ExportRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	em, err := otel.NewEmitter(otel.EmitterConfig{
		Endpoint:      srv.URL,
		Workers:       1,
		QueueSize:     100,
		BatchSize:     5,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)

	em.Enqueue(testSpan("span-a"))
	em.Enqueue(testSpan("span-b"))
	em.Enqueue(testSpan("span-c"))
	require.NoError(t, em.Shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, bodies)

	var names []string
	for _, raw := range bodies {
		var req coltracepb.ExportTraceServiceRequest
		require.NoError(t, proto.Unmarshal(raw, &req))

		rs := req.ResourceSpans
		require.NotEmpty(t, rs)

		var svcName string
		for _, kv := range rs[0].Resource.Attributes {
			if kv.Key == "service.name" {
				svcName = kv.Value.GetStringValue()
			}
		}
		assert.Equal(t, "router", svcName)

		require.NotEmpty(t, rs[0].ScopeSpans)
		assert.Equal(t, "workweave-router", rs[0].ScopeSpans[0].Scope.Name)

		for _, ss := range rs[0].ScopeSpans {
			for _, s := range ss.Spans {
				names = append(names, s.Name)
			}
		}
	}

	assert.ElementsMatch(t, []string{"span-a", "span-b", "span-c"}, names)
}

func TestEmitter_QueueFullDropsSpan(t *testing.T) {
	blocked := make(chan struct{}, 1)
	release := make(chan struct{})

	var mu sync.Mutex
	var received int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			t.Errorf("failed to unmarshal export request: %v", err)
			return
		}

		n := 0
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				n += len(ss.Spans)
			}
		}

		mu.Lock()
		received += n
		mu.Unlock()

		select {
		case blocked <- struct{}{}:
		default:
		}
		<-release

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	em, err := otel.NewEmitter(otel.EmitterConfig{
		Endpoint:      srv.URL,
		Workers:       1,
		QueueSize:     2,
		BatchSize:     1,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)

	em.Enqueue(testSpan("first"))
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("first export did not reach upstream within 2s; queue/worker pipeline is stuck")
	}

	overflow := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	for _, name := range overflow {
		em.Enqueue(testSpan(name))
	}

	close(release)
	require.NoError(t, em.Shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Less(t, received, 1+len(overflow))
}

func TestEmitter_EndpointTrailingSlash(t *testing.T) {
	var mu sync.Mutex
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	em, err := otel.NewEmitter(otel.EmitterConfig{
		Endpoint:      srv.URL + "/",
		Workers:       1,
		QueueSize:     10,
		BatchSize:     1,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)

	em.Enqueue(testSpan("slash-test"))
	require.NoError(t, em.Shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "/v1/traces", gotPath)
}
