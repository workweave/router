package middleware

import (
	"workweave/router/internal/timing"

	"github.com/gin-gonic/gin"
)

// WithTimingEntry creates a timing.Timing, stamps EntryNanos, and attaches it to the request context for downstream readers (provider adapters, proxy.Service) via timing.TimingFrom.
//
// Must be registered before WithTimeout so the entry stamp reflects the true gin-entry instant, not the post-deadline-setup instant.
func WithTimingEntry() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, t := timing.WithTiming(c.Request.Context())
		t.StampEntry()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
