package otel

import (
	"crypto/rand"
	"fmt"
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Span is a lightweight telemetry record converted to OTLP protobuf at
// flush time.
type Span struct {
	Name  string
	Start time.Time
	End   time.Time
	Attrs map[string]any
}

func generateTraceID() [16]byte {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("otel: crypto/rand failed: " + err.Error())
	}
	return id
}

func generateSpanID() [8]byte {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("otel: crypto/rand failed: " + err.Error())
	}
	return id
}

func spanToProto(s Span, traceID [16]byte) *tracev1.Span {
	spanID := generateSpanID()
	return &tracev1.Span{
		TraceId:            traceID[:],
		SpanId:             spanID[:],
		Name:               s.Name,
		StartTimeUnixNano:  uint64(s.Start.UnixNano()),
		EndTimeUnixNano:    uint64(s.End.UnixNano()),
		Attributes:         attrsToKeyValues(s.Attrs),
		Kind:               tracev1.Span_SPAN_KIND_INTERNAL,
	}
}

func attrsToKeyValues(attrs map[string]any) []*commonv1.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	kvs := make([]*commonv1.KeyValue, 0, len(attrs))
	for k, v := range attrs {
		kvs = append(kvs, &commonv1.KeyValue{
			Key:   k,
			Value: anyToValue(v),
		})
	}
	return kvs
}

func anyToValue(v any) *commonv1.AnyValue {
	switch val := v.(type) {
	case string:
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: val}}
	case int:
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: int64(val)}}
	case int64:
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: val}}
	case float64:
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: val}}
	case bool:
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_BoolValue{BoolValue: val}}
	default:
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: fmt.Sprintf("%v", v)}}
	}
}

func buildResource(serviceName string, extraAttrs map[string]string) *resourcev1.Resource {
	kvs := make([]*commonv1.KeyValue, 0, 1+len(extraAttrs))
	kvs = append(kvs, &commonv1.KeyValue{
		Key:   "service.name",
		Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: serviceName}},
	})
	for k, v := range extraAttrs {
		kvs = append(kvs, &commonv1.KeyValue{
			Key:   k,
			Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}},
		})
	}
	return &resourcev1.Resource{Attributes: kvs}
}
