package middleware

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

// WithTimeout bounds request handling to d via c.Request.Context() so cancellation-aware downstream callers abort when the budget is exceeded.
func WithTimeout(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
