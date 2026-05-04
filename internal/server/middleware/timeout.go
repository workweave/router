package middleware

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

// WithTimeout bounds request handling to d. The deadline flows through
// c.Request.Context() so downstream callers that respect cancellation abort
// when the budget is exceeded.
func WithTimeout(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
