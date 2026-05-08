package otel

import (
	"crypto/rand"
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
	Attrs []*commonv1.KeyValue
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

// AttrBuilder constructs OTLP KeyValue attributes directly without
// intermediate map allocations. Not safe for concurrent use.
type AttrBuilder struct {
	attrs []*commonv1.KeyValue
}

// NewAttrBuilder returns a builder pre-sized for cap attributes.
func NewAttrBuilder(cap int) *AttrBuilder {
	return &AttrBuilder{attrs: make([]*commonv1.KeyValue, 0, cap)}
}

// String appends a string attribute.
func (b *AttrBuilder) String(key, val string) *AttrBuilder {
	b.attrs = append(b.attrs, &commonv1.KeyValue{
		Key:   key,
		Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: val}},
	})
	return b
}

// Int64 appends an int64 attribute.
func (b *AttrBuilder) Int64(key string, val int64) *AttrBuilder {
	b.attrs = append(b.attrs, &commonv1.KeyValue{
		Key:   key,
		Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: val}},
	})
	return b
}

// Float64 appends a float64 attribute.
func (b *AttrBuilder) Float64(key string, val float64) *AttrBuilder {
	b.attrs = append(b.attrs, &commonv1.KeyValue{
		Key:   key,
		Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: val}},
	})
	return b
}

// Bool appends a bool attribute.
func (b *AttrBuilder) Bool(key string, val bool) *AttrBuilder {
	b.attrs = append(b.attrs, &commonv1.KeyValue{
		Key:   key,
		Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_BoolValue{BoolValue: val}},
	})
	return b
}

// IntSlice appends an int slice as an OTLP array of int64 values.
// A nil or empty slice still emits the attribute with an empty array, so
// downstream consumers can distinguish "field present, no clusters" from
// "field absent" (e.g. pinned-route turns where Metadata is nil).
func (b *AttrBuilder) IntSlice(key string, vals []int) *AttrBuilder {
	values := make([]*commonv1.AnyValue, len(vals))
	for i, v := range vals {
		values[i] = &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: int64(v)}}
	}
	b.attrs = append(b.attrs, &commonv1.KeyValue{
		Key:   key,
		Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_ArrayValue{ArrayValue: &commonv1.ArrayValue{Values: values}}},
	})
	return b
}

// Build returns the accumulated KeyValue slice. The slice is valid until
// the next method call on the builder.
func (b *AttrBuilder) Build() []*commonv1.KeyValue {
	return b.attrs
}

func spanToProto(s Span, traceID [16]byte) *tracev1.Span {
	spanID := generateSpanID()
	return &tracev1.Span{
		TraceId:           traceID[:],
		SpanId:            spanID[:],
		Name:              s.Name,
		StartTimeUnixNano: uint64(s.Start.UnixNano()),
		EndTimeUnixNano:   uint64(s.End.UnixNano()),
		Attributes:        s.Attrs,
		Kind:              tracev1.Span_SPAN_KIND_INTERNAL,
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
