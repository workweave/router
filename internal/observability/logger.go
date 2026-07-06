package observability

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"github.com/vlad-tokarev/sloggcp"
)

const ginContextKey = "router_logger"

// loggerContextKey is a private type so context values don't collide with
// other packages'.
type loggerContextKey struct{}

// WithLogger attaches a logger to ctx for downstream FromContext calls.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	if log == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerContextKey{}, log)
}

// FromContext returns the logger stashed by WithLogger, or the global default
// if none is set. Always non-nil.
func FromContext(ctx context.Context) *slog.Logger {
	initOnce.Do(initLogger)
	if ctx != nil {
		if v := ctx.Value(loggerContextKey{}); v != nil {
			if logger, ok := v.(*slog.Logger); ok {
				return logger
			}
		}
	}
	return slog.Default()
}

// initOnce installs the LOG_LEVEL-honoring handler on first use; without it
// slog.Default() defaults to INFO and silently drops Debug lines.
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
	slog.SetDefault(slog.New(newHandler(level)))
}

// newHandler builds the slog handler for the resolved format. JSON uses
// sloggcp.ReplaceAttr so lines render correctly in GCP Cloud Logging; tint
// gives colorized output for local dev.
func newHandler(level slog.Level) slog.Handler {
	switch logFormat() {
	case "json":
		return slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level:       level,
			ReplaceAttr: sloggcp.ReplaceAttr,
		})
	case "text":
		return slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	case "tint":
		return tint.NewHandler(os.Stderr, &tint.Options{Level: level, TimeFormat: time.Kitchen})
	}
	// Auto: TTY gets human-readable output (colorized unless disabled); non-TTY
	// gets structured GCP JSON.
	if useColor() {
		return tint.NewHandler(os.Stderr, &tint.Options{Level: level, TimeFormat: time.Kitchen})
	}
	if isTerminal() {
		return slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}
	return slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: sloggcp.ReplaceAttr,
	})
}

// logFormat returns the requested handler format from LOG_FORMAT
// ({json,text,color,tint}), or "" to auto-detect.
func logFormat() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT"))) {
	case "json":
		return "json"
	case "text":
		return "text"
	case "color", "tint":
		return "tint"
	}
	return ""
}

// useColor reports whether auto format should use tint's colorized handler.
// Respects LOG_COLOR={1,true,yes,on}/{0,false,no,off}; otherwise auto-detects
// via TTY + NO_COLOR (https://no-color.org).
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
	return isTerminal()
}

// isTerminal reports whether stderr is an interactive terminal, independent
// of color preference.
func isTerminal() bool {
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

// AccessLog logs one INFO line per request after handlers run — without it, a
// 401 from WithAuth produces zero output at LOG_LEVEL=info, masking traffic.
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
