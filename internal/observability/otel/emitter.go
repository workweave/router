package otel

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

// EmitterConfig controls the emitter's async export pipeline.
type EmitterConfig struct {
	Endpoint      string
	Headers       map[string]string
	ServiceName   string
	ResourceAttrs map[string]string
	Workers       int
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	ExportTimeout time.Duration
}

// Emitter batches OTLP spans and exports them via HTTP POST. Safe for
// concurrent use. A nil *Emitter means OTel is disabled; all methods no-op.
type Emitter struct {
	queue    chan *tracev1.Span
	client   *http.Client
	endpoint string
	headers  map[string]string
	resource *resourcev1.Resource
	batchSz  int
	flushInt time.Duration
	dropped  atomic.Int64
	wg       sync.WaitGroup
	// closeMu coordinates Shutdown's queue close against in-flight Enqueue
	// calls so a late Enqueue cannot panic with "send on closed channel".
	// Enqueue acquires RLock for the lifetime of the send; Shutdown takes
	// Lock to flip closed and close(queue) atomically.
	closeMu sync.RWMutex
	closed  atomic.Bool
}

// NewEmitter starts the worker pool and returns a ready emitter. Returns
// (nil, nil) when cfg.Endpoint is empty (OTel disabled).
func NewEmitter(cfg EmitterConfig) (*Emitter, error) {
	if cfg.Endpoint == "" {
		return nil, nil
	}

	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1000
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 500 * time.Millisecond
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = 10 * time.Second
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "router"
	}

	e := &Emitter{
		queue:    make(chan *tracev1.Span, cfg.QueueSize),
		client:   &http.Client{Timeout: cfg.ExportTimeout},
		endpoint: strings.TrimRight(cfg.Endpoint, "/") + "/v1/traces",
		headers:  cfg.Headers,
		resource: buildResource(cfg.ServiceName, cfg.ResourceAttrs),
		batchSz:  cfg.BatchSize,
		flushInt: cfg.FlushInterval,
	}

	e.wg.Add(cfg.Workers)
	for range cfg.Workers {
		go e.worker()
	}

	return e, nil
}

// Enqueue submits a span to the export queue. Non-blocking: drops on
// queue-full or post-shutdown. Nil receiver is a no-op.
func (e *Emitter) Enqueue(s *tracev1.Span) {
	if e == nil {
		return
	}
	e.closeMu.RLock()
	defer e.closeMu.RUnlock()
	if e.closed.Load() {
		e.dropped.Add(1)
		return
	}
	select {
	case e.queue <- s:
	default:
		e.dropped.Add(1)
	}
}

// Shutdown closes the queue and waits for workers to drain. Blocks until
// all workers finish or ctx expires. Idempotent; nil receiver is a no-op.
func (e *Emitter) Shutdown(c context.Context) error {
	if e == nil {
		return nil
	}

	e.closeMu.Lock()
	if e.closed.Swap(true) {
		e.closeMu.Unlock()
		return nil
	}
	close(e.queue)
	e.closeMu.Unlock()

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-c.Done():
		return c.Err()
	}

	if n := e.dropped.Load(); n > 0 {
		slog.Warn("Dropped spans during emitter lifetime", "count", n)
	}

	return nil
}

func (e *Emitter) worker() {
	defer e.wg.Done()

	batch := make([]*tracev1.Span, 0, e.batchSz)
	timer := time.NewTimer(e.flushInt)
	defer timer.Stop()

	for {
		select {
		case span, ok := <-e.queue:
			if !ok {
				if len(batch) > 0 {
					e.exportBatch(batch)
				}
				return
			}
			batch = append(batch, span)
			if len(batch) >= e.batchSz {
				e.exportBatch(batch)
				batch = batch[:0]
				// Go 1.23+ drains the channel inside Stop/Reset automatically;
				// the old `if !timer.Stop() { <-timer.C }` idiom deadlocks.
				timer.Reset(e.flushInt)
			}

		case <-timer.C:
			if len(batch) > 0 {
				e.exportBatch(batch)
				batch = batch[:0]
			}
			timer.Reset(e.flushInt)
		}
	}
}

func (e *Emitter) exportBatch(spans []*tracev1.Span) {
	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracev1.ResourceSpans{{
			Resource: e.resource,
			ScopeSpans: []*tracev1.ScopeSpans{{
				Scope: &commonv1.InstrumentationScope{Name: "workweave-router"},
				Spans: spans,
			}},
		}},
	}

	body, err := proto.Marshal(req)
	if err != nil {
		slog.Warn("Failed to marshal OTLP export request", "err", err)
		return
	}

	httpReq, err := http.NewRequest(http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		slog.Warn("Failed to create OTLP export HTTP request", "err", err)
		return
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	for k, v := range e.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		slog.Warn("OTLP export request failed", "err", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("OTLP export returned non-2xx status", "status", resp.StatusCode)
	}
}
