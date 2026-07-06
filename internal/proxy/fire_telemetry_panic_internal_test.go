package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/observability"

	"github.com/stretchr/testify/assert"
)

// panicTelemetryRepo is a TelemetryRepository whose InsertRequestTelemetry
// always panics, simulating a bug in the telemetry sink.
type panicTelemetryRepo struct{}

func (panicTelemetryRepo) InsertRequestTelemetry(ctx context.Context, p InsertTelemetryParams) error {
	panic("boom: telemetry insert")
}

func (panicTelemetryRepo) GetTelemetrySummary(ctx context.Context, installationID string, from, to time.Time) (TelemetrySummary, error) {
	return TelemetrySummary{}, nil
}

func (panicTelemetryRepo) GetTelemetryTimeseries(ctx context.Context, installationID string, from, to time.Time, granularity string) ([]TelemetryBucket, error) {
	return nil, nil
}

func (panicTelemetryRepo) GetTelemetrySummaryAll(ctx context.Context, from, to time.Time) (TelemetrySummary, error) {
	return TelemetrySummary{}, nil
}

func (panicTelemetryRepo) GetTelemetryTimeseriesAll(ctx context.Context, from, to time.Time, granularity string) ([]TelemetryBucket, error) {
	return nil, nil
}

func (panicTelemetryRepo) GetTelemetryRows(ctx context.Context, installationID string, from, to time.Time, limit int32) ([]TelemetryRow, error) {
	return nil, nil
}

func (panicTelemetryRepo) GetTelemetryRowsAll(ctx context.Context, from, to time.Time, limit int32) ([]TelemetryRow, error) {
	return nil, nil
}

// TestFireTelemetryRecoversFromPanic proves a panic inside the async
// telemetry insert is caught and logged instead of crashing the process.
func TestFireTelemetryRecoversFromPanic(t *testing.T) {
	// Prime observability's sync.Once before overriding slog.Default; otherwise the goroutine's
	// first Get() call races SetDefault and resets the handler.
	observability.Get()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	s := &Service{telemetry: panicTelemetryRepo{}}

	assert.NotPanics(t, func() {
		s.fireTelemetry(InsertTelemetryParams{RequestID: "req-1"})
		// fireTelemetry launches a goroutine; give it a moment to run and recover.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(buf.String(), "Background goroutine panicked") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	assert.Contains(t, buf.String(), "Background goroutine panicked")
	assert.Contains(t, buf.String(), "fireTelemetry")
}
