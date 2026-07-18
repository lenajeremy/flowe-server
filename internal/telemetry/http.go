package telemetry

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
)

// AccessLog emits one structured, ctx-aware log line per request, so Loki
// carries the full request timeline with trace ids. Routes (not raw paths)
// are logged — raw paths can embed webhook tokens and capability run IDs.
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		status := c.Writer.Status()

		lvl := slog.LevelInfo
		switch {
		case route == "/health":
			lvl = slog.LevelDebug
		case status >= 500:
			lvl = slog.LevelError
		case status >= 400:
			lvl = slog.LevelWarn
		}

		attrs := []any{
			"method", c.Request.Method,
			"route", route,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes", c.Writer.Size(),
			"client_ip", c.ClientIP(),
		}
		if ua := c.Request.UserAgent(); ua != "" {
			if len(ua) > 80 {
				ua = ua[:80]
			}
			attrs = append(attrs, "user_agent", ua)
		}
		if len(c.Errors) > 0 {
			attrs = append(attrs, "gin_errors", c.Errors.String())
		}
		slog.Log(c.Request.Context(), lvl, "http request", attrs...)
	}
}

// Recovery replaces gin.Recovery: same 500-on-panic behaviour, but the panic
// is counted and logged through slog with the request context (trace id) and
// stack attached.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				route := c.FullPath()
				if route == "" {
					route = "unmatched"
				}
				RecordPanic(c.Request.Context(), route)
				slog.ErrorContext(c.Request.Context(), "panic recovered",
					"route", route,
					"method", c.Request.Method,
					"panic", fmt.Sprint(r),
					"stack", string(debug.Stack()),
				)
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}

// loggingTransport logs every outbound HTTP call. It sits inside the otel
// transport wrap, so the request context already carries the client span and
// the log line lands in Loki with that trace id. Query strings are never
// logged — provider APIs put keys and tokens there.
type loggingTransport struct {
	base http.RoundTripper
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	attrs := []any{
		"method", req.Method,
		"host", req.URL.Host,
		"path", req.URL.Path,
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if err != nil {
		slog.ErrorContext(req.Context(), "outbound http failed", append(attrs, "error", err.Error())...)
		return resp, err
	}
	lvl := slog.LevelInfo
	if resp.StatusCode >= 400 {
		lvl = slog.LevelWarn
	}
	slog.Log(req.Context(), lvl, "outbound http", append(attrs, "status", resp.StatusCode)...)
	return resp, err
}
