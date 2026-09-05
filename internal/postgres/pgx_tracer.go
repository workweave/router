package postgres

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "weave-os/router/internal/postgres"

var (
	// ErrDatabaseNameEmpty is returned when a Postgres tracer has no database namespace.
	ErrDatabaseNameEmpty = errors.New("database name is empty")

	sqlcQueryPattern = regexp.MustCompile(`^(?:--|/\*)\s+name:\s+(\w+) :(\w+)`)

	pgxOperationTypeKey     = attribute.Key("pgx.operation.type")
	pgxPoolConnOperationKey = attribute.Key("pgx.pool.operation")
	dbPIDKey                = attribute.Key("db.client.connection.pid")
	dbQueryCommandTagKey    = attribute.Key("db.query.command.tag")
	sqlcQueryNameKey        = attribute.Key("sqlc.query.name")
	sqlcQueryCommandKey     = attribute.Key("sqlc.query.command")
)

// PGXTracer emits OpenTelemetry spans for pool acquisition and SQL queries.
type PGXTracer interface {
	pgx.QueryTracer
	pgxpool.AcquireTracer
}

type pgxTracer struct {
	databaseName string
	provider     trace.TracerProvider
}

type queryMetadata struct {
	name    string
	command string
}

type queryTrace struct {
	span trace.Span
}

type acquireTrace struct {
	span trace.Span
}

type queryTraceKey struct{}
type acquireTraceKey struct{}

// NewPGXTracer returns a pgx tracer using the process-wide OpenTelemetry provider.
func NewPGXTracer(databaseName string) (PGXTracer, error) {
	return newPGXTracer(databaseName, otel.GetTracerProvider())
}

func newPGXTracer(databaseName string, provider trace.TracerProvider) (*pgxTracer, error) {
	if databaseName == "" {
		return nil, ErrDatabaseNameEmpty
	}
	return &pgxTracer{databaseName: databaseName, provider: provider}, nil
}

func (t *pgxTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx, span := t.provider.Tracer(tracerName).Start(ctx, "postgresql.query", trace.WithSpanKind(trace.SpanKindClient))
	ctx = context.WithValue(ctx, queryTraceKey{}, queryTrace{span: span})
	if !span.IsRecording() {
		return ctx
	}

	metadata := queryMetadataFromSQL(data.SQL)
	if metadata != nil {
		span.SetName("postgresql.query " + metadata.name)
	}

	attrs := []attribute.KeyValue{
		semconv.DBSystemNamePostgreSQL,
		semconv.DBNamespace(t.databaseName),
		pgxOperationTypeKey.String("query"),
		dbPIDKey.Int(int(connectionPID(conn))),
		semconv.DBQueryText(data.SQL),
	}
	if metadata != nil {
		attrs = append(attrs,
			sqlcQueryNameKey.String(metadata.name),
			sqlcQueryCommandKey.String(metadata.command),
			semconv.DBQuerySummary(metadata.name),
		)
	}

	span.SetAttributes(attrs...)
	return ctx
}

func (t *pgxTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	traceData, ok := ctx.Value(queryTraceKey{}).(queryTrace)
	if !ok {
		return
	}
	defer traceData.span.End()
	if !traceData.span.IsRecording() {
		return
	}

	recordSpanError(traceData.span, data.Err)
	if data.Err == nil {
		traceData.span.SetStatus(codes.Ok, "")
		traceData.span.SetAttributes(dbQueryCommandTagKey.String(data.CommandTag.String()))
	}
}

func (t *pgxTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	ctx, span := t.provider.Tracer(tracerName).Start(ctx, "pgxpool.acquire", trace.WithSpanKind(trace.SpanKindClient))
	ctx = context.WithValue(ctx, acquireTraceKey{}, acquireTrace{span: span})
	if span.IsRecording() {
		span.SetAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBNamespace(t.databaseName),
			pgxPoolConnOperationKey.String("acquire"),
		)
	}
	return ctx
}

func (t *pgxTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	traceData, ok := ctx.Value(acquireTraceKey{}).(acquireTrace)
	if !ok {
		return
	}
	defer traceData.span.End()
	if !traceData.span.IsRecording() {
		return
	}

	recordSpanError(traceData.span, data.Err)
	if data.Err == nil {
		traceData.span.SetStatus(codes.Ok, "")
		traceData.span.SetAttributes(dbPIDKey.Int(int(connectionPID(data.Conn))))
	}
}

func queryMetadataFromSQL(sql string) *queryMetadata {
	matches := sqlcQueryPattern.FindStringSubmatch(sql)
	if len(matches) != 3 {
		return nil
	}
	return &queryMetadata{name: matches[1], command: matches[2]}
}

func connectionPID(conn *pgx.Conn) uint32 {
	if conn == nil || conn.PgConn() == nil {
		return 0
	}
	return conn.PgConn().PID()
}

func recordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		span.SetAttributes(semconv.DBResponseStatusCode(pgErr.Code))
	}
}
