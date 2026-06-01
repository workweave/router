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

// loggerContextKey is the private key used to stash a request-scoped
// *slog.Logger on a context.Context. Private type prevents collisions with
// other packages' context values.
type loggerContextKey struct{}

// WithLogger returns a new context that carries the given logger. Downstream
// code reading the logger with FromContext sees this logger (with all its
// pre-bound attributes) instead of the global default.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	if log == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerContextKey{}, log)
}

// FromContext returns the request-scoped logger stashed by WithLogger, or the
// global default logger if none is set. Always non-nil. Initializes the
// LOG_LEVEL-honoring handler on first call, matching Get().
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
	slog.SetDefault(slog.New(newHandler(level)))
}

// newHandler builds the slog handler for the resolved format. JSON output maps
// attributes to GCP Cloud Logging fields (severity/time/message) via
// sloggcp.ReplaceAttr so lines render correctly when the router runs on Cloud
// Run; tint gives a colorized human-readable stream for local dev.
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
	// Auto: a TTY (local dev) gets human-readable output — colorized when color
	// is enabled, plain text when NO_COLOR/LOG_COLOR disables it. Only non-TTY
	// streams (Cloud Run, piped, redirected) get structured GCP JSON.
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

// logFormat returns the explicitly requested handler format, or "" to let the
// handler auto-detect based on whether stderr is a TTY. Honors
// LOG_FORMAT={json,text,color,tint}.
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

// useColor reports whether the auto format should pick tint's ANSI-colored
// handler (vs structured JSON). Respects LOG_COLOR={1,true,yes,on} /
// {0,false,no,off}; otherwise auto-detects based on whether stderr is a TTY and
// NO_COLOR is unset (https://no-color.org).
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

// isTerminal reports whether stderr is an interactive terminal, independent of
// any color preference. The auto format uses this to keep TTY output
// human-readable (text) rather than JSON when color is disabled.
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
