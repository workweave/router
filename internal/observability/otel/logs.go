package otel

import (
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
)

// Severity is the log-record level. Kept as a small local enum so the buffer /
// proxy API does not leak the OTLP proto severity type.
type Severity int

const (
	// SeverityInfo marks routine high-fidelity events (decision, call).
	SeverityInfo Severity = iota
	// SeverityError marks upstream/router failures.
	SeverityError
)

// LogRecord is a lightweight high-fidelity event converted to an OTLP log
// record at flush time. Name becomes the log body (event name, matching the
// Claude Code plugin convention); structured fields live in Attrs.
type LogRecord struct {
	Name     string
	Time     time.Time
	Severity Severity
	Attrs    []*commonv1.KeyValue
}

func (s Severity) otlp() logsv1.SeverityNumber {
	if s == SeverityError {
		return logsv1.SeverityNumber_SEVERITY_NUMBER_ERROR
	}
	return logsv1.SeverityNumber_SEVERITY_NUMBER_INFO
}

func (s Severity) text() string {
	if s == SeverityError {
		return "ERROR"
	}
	return "INFO"
}

// logRecordToProto converts a LogRecord into its OTLP wire form. The record
// shares the request's traceID so the collector can correlate it with the
// request's spans, and gets a fresh span ID for uniqueness.
func logRecordToProto(r LogRecord, traceID [16]byte) *logsv1.LogRecord {
	spanID := generateSpanID()
	ts := uint64(r.Time.UnixNano())
	return &logsv1.LogRecord{
		TimeUnixNano:         ts,
		ObservedTimeUnixNano: ts,
		SeverityNumber:       r.Severity.otlp(),
		SeverityText:         r.Severity.text(),
		Body:                 &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: r.Name}},
		Attributes:           r.Attrs,
		TraceId:              traceID[:],
		SpanId:               spanID[:],
	}
}
