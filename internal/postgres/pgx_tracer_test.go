package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestNewPGXTracerRequiresDatabaseName(t *testing.T) {
	_, err := NewPGXTracer("")

	require.ErrorIs(t, err, ErrDatabaseNameEmpty)
}

func TestPGXTracerEmitsNamedSQLCQuerySpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	tracer, err := newPGXTracer("router", provider)
	require.NoError(t, err)

	parentCtx, parent := provider.Tracer("test").Start(context.Background(), "request")
	queryCtx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{
		SQL:  "-- name: GetInstallation :one\nSELECT * FROM router.installations WHERE id = $1",
		Args: []any{"installation-id"},
	})
	tracer.TraceQueryEnd(queryCtx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 1")})
	parent.End()

	span := endedSpanByName(t, recorder.Ended(), "postgresql.query GetInstallation")
	assert.Equal(t, trace.SpanKindClient, span.SpanKind())
	assert.Equal(t, parent.SpanContext().SpanID(), span.Parent().SpanID())
	assert.Equal(t, "postgresql", spanAttribute(t, span.Attributes(), "db.system.name").AsString())
	assert.Equal(t, "router", spanAttribute(t, span.Attributes(), "db.namespace").AsString())
	assert.Equal(t, "query", spanAttribute(t, span.Attributes(), "pgx.operation.type").AsString())
	assert.Equal(t, "GetInstallation", spanAttribute(t, span.Attributes(), "sqlc.query.name").AsString())
	assert.Equal(t, "one", spanAttribute(t, span.Attributes(), "sqlc.query.command").AsString())
	assert.Equal(t, "GetInstallation", spanAttribute(t, span.Attributes(), "db.query.summary").AsString())
	assert.False(t, hasSpanAttribute(span.Attributes(), "db.operation.name"))
	assert.Equal(t, "SELECT 1", spanAttribute(t, span.Attributes(), "db.query.command.tag").AsString())
	assert.Contains(t, spanAttribute(t, span.Attributes(), "db.query.text").AsString(), "router.installations")
	assert.Equal(t, sdktrace.Status{Code: codes.Ok}, span.Status())
}

func TestPGXTracerRecordsQueryErrors(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	tracer, err := newPGXTracer("router", provider)
	require.NoError(t, err)

	queryCtx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	queryErr := &pgconn.PgError{Code: "57014", Message: "canceling statement"}
	tracer.TraceQueryEnd(queryCtx, nil, pgx.TraceQueryEndData{Err: queryErr})

	span := endedSpanByName(t, recorder.Ended(), "postgresql.query")
	assert.Equal(t, sdktrace.Status{Code: codes.Error, Description: queryErr.Error()}, span.Status())
	assert.Equal(t, "57014", spanAttribute(t, span.Attributes(), "db.response.status_code").AsString())
	require.Len(t, span.Events(), 1)
	assert.Equal(t, "exception", span.Events()[0].Name)
}

func TestPGXTracerEmitsPoolAcquireSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	tracer, err := newPGXTracer("router", provider)
	require.NoError(t, err)

	parentCtx, parent := provider.Tracer("test").Start(context.Background(), "request")
	acquireCtx := tracer.TraceAcquireStart(parentCtx, nil, pgxpool.TraceAcquireStartData{})
	tracer.TraceAcquireEnd(acquireCtx, nil, pgxpool.TraceAcquireEndData{})
	parent.End()

	span := endedSpanByName(t, recorder.Ended(), "pgxpool.acquire")
	assert.Equal(t, trace.SpanKindClient, span.SpanKind())
	assert.Equal(t, parent.SpanContext().SpanID(), span.Parent().SpanID())
	assert.Equal(t, "acquire", spanAttribute(t, span.Attributes(), "pgx.pool.operation").AsString())
	assert.Equal(t, int64(0), spanAttribute(t, span.Attributes(), "db.client.connection.pid").AsInt64())
	assert.Equal(t, sdktrace.Status{Code: codes.Ok}, span.Status())
}

func endedSpanByName(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	require.FailNow(t, "span not found", "name=%s", name)
	return nil
}

func spanAttribute(t *testing.T, attrs []attribute.KeyValue, key string) attribute.Value {
	t.Helper()
	for _, attr := range attrs {
		if string(attr.Key) == key {
			return attr.Value
		}
	}
	require.FailNow(t, "span attribute not found", "key=%s", key)
	return attribute.Value{}
}

func hasSpanAttribute(attrs []attribute.KeyValue, key string) bool {
	for _, attr := range attrs {
		if string(attr.Key) == key {
			return true
		}
	}
	return false
}
