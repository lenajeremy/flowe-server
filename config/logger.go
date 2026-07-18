package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

// PrettyJSONHandler writes colour-coded, pretty-printed JSON logs to stdout.
type PrettyJSONHandler struct {
	opts slog.HandlerOptions
	mu   *sync.Mutex
	out  *os.File
}

func NewPrettyJSONHandler(out *os.File, opts *slog.HandlerOptions) *PrettyJSONHandler {
	h := &PrettyJSONHandler{out: out, mu: &sync.Mutex{}}
	if opts != nil {
		h.opts = *opts
	}
	return h
}

func (h *PrettyJSONHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.opts.Level.Level()
}

func (h *PrettyJSONHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *PrettyJSONHandler) WithGroup(name string) slog.Handler       { return h }

func (h *PrettyJSONHandler) Handle(_ context.Context, r slog.Record) error {
	level := r.Level.String()

	var color string
	switch r.Level {
	case slog.LevelDebug:
		color = "\033[34m" // blue
	case slog.LevelInfo:
		color = "\033[32m" // green
	case slog.LevelWarn:
		color = "\033[33m" // yellow
	case slog.LevelError:
		color = "\033[31m" // red
	default:
		color = "\033[0m"
	}
	reset := "\033[0m"

	fields := map[string]any{
		"time":    r.Time.Format("2006-01-02T15:04:05.000Z07:00"),
		"level":   level,
		"message": r.Message,
	}
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.Any()
		return true
	})

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(fields); err != nil {
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintf(h.out, "%s%s%s\n", color, buf.String(), reset)
	return err
}

// multiHandler fans out log records to multiple slog.Handler instances.
type multiHandler struct{ handlers []slog.Handler }

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}
func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}
func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{hs}
}
func (m *multiHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &multiHandler{hs}
}

// SetupLogger installs the stdout/file logger. Extra handlers (the OTLP
// bridge when telemetry is enabled) are fanned in alongside; nil entries are
// skipped so callers can pass the result of a disabled telemetry setup.
func SetupLogger(extra ...slog.Handler) {
	opts := &slog.HandlerOptions{AddSource: true, Level: slog.LevelDebug}

	handlers := []slog.Handler{NewPrettyJSONHandler(os.Stdout, opts)}
	if logFile, err := os.OpenFile("server.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		handlers = append(handlers, slog.NewJSONHandler(logFile, opts))
	}
	for _, h := range extra {
		if h != nil {
			handlers = append(handlers, h)
		}
	}

	var handler slog.Handler = handlers[0]
	if len(handlers) > 1 {
		handler = &multiHandler{handlers: handlers}
	}

	slog.SetDefault(slog.New(handler))
}
