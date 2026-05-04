package middleware

import (
	"workweave/router/internal/observability/otel"

	"github.com/gin-gonic/gin"
)

// WithTimingEntry creates an otel.Timing, stamps EntryNanos with the
// current wall clock, and attaches the Timing to the request context.
// Provider adapters and proxy.Service read it back via otel.TimingFrom
// to stamp upstream milestones (TTFB, EOF) and compute derived latency
// attributes on the OTel spans.
//
// Must be registered before WithTimeout so the entry stamp reflects the
// true gin-entry instant, not the post-deadline-setup instant.
func WithTimingEntry() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, t := otel.WithTiming(c.Request.Context())
		t.StampEntry()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
