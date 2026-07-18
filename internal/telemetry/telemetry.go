// Package telemetry wires OpenTelemetry traces, metrics, and logs for the
// server. Everything is exported over OTLP/HTTP to a collector (the local
// LGTM stack in dev, anything OTLP-speaking in prod).
//
// Setup is a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set, so
// environments without a collector (current Railway) run exactly as before.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const scopeName = "workflow-ai/server"

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Setup configures the global tracer/meter/logger providers and returns a
// shutdown func plus an slog.Handler that ships logs over OTLP (nil when
// telemetry is disabled). It also wraps http.DefaultTransport so every
// outbound call made through the default transport is traced and measured.
func Setup(ctx context.Context) (func(context.Context) error, slog.Handler) {
	noop := func(context.Context) error { return nil }
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return noop, nil
	}

	hostname, _ := os.Hostname()
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(envOr("OTEL_SERVICE_NAME", "flowe-server")),
			semconv.ServiceInstanceID(hostname),
			attribute.String("deployment.environment", envOr("RAILWAY_ENVIRONMENT", "local")),
		),
	)
	if err != nil {
		slog.Warn("otel resource setup failed", "error", err)
		return noop, nil
	}

	traceExp, terr := otlptracehttp.New(ctx)
	metricExp, merr := otlpmetrichttp.New(ctx)
	logExp, lerr := otlploghttp.New(ctx)
	if err := errors.Join(terr, merr, lerr); err != nil {
		slog.Warn("otel exporter setup failed, telemetry disabled", "error", err)
		return noop, nil
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	// otelgin's http.server.request.duration ships semconv default buckets
	// capped at 10s; /api/run and the AI chat stream SSE for minutes, so
	// extend the tail or every streaming request collapses into +Inf.
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(10*time.Second))),
		sdkmetric.WithResource(res),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.duration"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
			}},
		)),
	)
	otel.SetMeterProvider(mp)
	if err := runtime.Start(); err != nil {
		slog.Warn("otel runtime metrics failed to start", "error", err)
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	logglobal.SetLoggerProvider(lp)

	// Every client built with a nil Transport (integrationHTTP, webtools,
	// the LLM calls via http.DefaultClient, …) inherits this wrap. Logging
	// sits inside the otel span so each line carries the trace id.
	http.DefaultTransport = otelhttp.NewTransport(&loggingTransport{base: http.DefaultTransport})

	shutdown := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}
	otlpHandler := otelslog.NewHandler(scopeName, otelslog.WithLoggerProvider(lp))
	return shutdown, &logfmtBodyHandler{inner: otlpHandler}
}

// logfmtBodyHandler rewrites each record's message into a self-descriptive
// logfmt line ("workflow run failed run_id=… reason=…") before handing it to
// the OTLP bridge. Without this the Loki log *body* is just the bare message
// — the attributes exist only as structured metadata behind a click, which
// makes the log panels unreadable. Attributes are still forwarded, so LogQL
// filtering on metadata keeps working; `| logfmt` now parses the body too.
type logfmtBodyHandler struct {
	inner slog.Handler
}

func (h *logfmtBodyHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// WithAttrs/WithGroup attrs won't be inlined into the body (nothing in this
// codebase uses them — loggers are called directly with per-call attrs).
func (h *logfmtBodyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logfmtBodyHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *logfmtBodyHandler) WithGroup(name string) slog.Handler {
	return &logfmtBodyHandler{inner: h.inner.WithGroup(name)}
}

func (h *logfmtBodyHandler) Handle(ctx context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		v := a.Value.String()
		if len(v) > 300 {
			v = v[:300] + "…"
		}
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		if strings.ContainsAny(v, " \"=\n\t") || v == "" {
			b.WriteString(strconv.Quote(v))
		} else {
			b.WriteString(v)
		}
		return true
	})
	nr := slog.NewRecord(r.Time, r.Level, b.String(), r.PC)
	r.Attrs(func(a slog.Attr) bool { nr.AddAttrs(a); return true })
	return h.inner.Handle(ctx, nr)
}

// WrapTransport instruments a custom transport (clients that don't use
// http.DefaultTransport, e.g. the SSRF-guarded one) with the same
// span + outbound-log pair as the default transport.
func WrapTransport(rt http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(&loggingTransport{base: rt})
}
