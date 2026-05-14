package observability

import (
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
)

const ginContextKey = "router_logger"

// initOnce installs a slog handler honoring LOG_LEVEL on first Get(). Without
// this, slog.Default() falls back to Go's stdlib handler at INFO, silently
// dropping Debug lines emitted elsewhere in the codebase.
var initOnce sync.Once

func initLogger() {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	var handler slog.Handler
	if useColor() {
		handler = tint.NewHandler(os.Stderr, &tint.Options{
			Level:      level,
			TimeFormat: time.Kitchen,
		})
	} else {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}
	slog.SetDefault(slog.New(handler))
}

// useColor returns true when tint's ANSI-colored handler should be used.
// Respects LOG_COLOR={1,true,yes,on} / {0,false,no,off}; otherwise auto-detects
// based on whether stderr is a TTY and NO_COLOR is unset (https://no-color.org).
func useColor() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_COLOR"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())
}

func Get() *slog.Logger {
	initOnce.Do(initLogger)
	return slog.Default()
}

func FromGin(c *gin.Context) *slog.Logger {
	initOnce.Do(initLogger)
	if v, ok := c.Get(ginContextKey); ok {
		if logger, ok := v.(*slog.Logger); ok {
			return logger
		}
	}
	return slog.Default()
}

func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		logger := slog.Default().With(
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
		)
		c.Set(ginContextKey, logger)
		c.Next()
	}
}

// AccessLog logs one INFO line per request after handlers run. Without this,
// a 401 from WithAuth produces zero output at LOG_LEVEL=info, making "no logs"
// look like "the server isn't being hit" when it actually is.
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		FromGin(c).Info("http request",
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP(),
		)
	}
}
