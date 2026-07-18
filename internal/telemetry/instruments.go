package telemetry

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Tracer is the app-level tracer for spans created by hand (workflow runs,
// node executions). The global delegate makes it safe to use before Setup.
var Tracer trace.Tracer = otel.Tracer(scopeName)

var meter = otel.Meter(scopeName)

// Long-tail buckets: workflow runs and nodes block on LLMs, external APIs,
// and human approval, so seconds-to-minutes is the normal range.
var runBuckets = metric.WithExplicitBucketBoundaries(
	0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800)

var (
	httpActive, _ = meter.Int64UpDownCounter("http.server.active_requests",
		metric.WithDescription("In-flight HTTP requests"), metric.WithUnit("{request}"))

	workflowRuns, _ = meter.Int64Counter("flowe.workflow.runs",
		metric.WithDescription("Completed workflow runs by status and trigger"), metric.WithUnit("{run}"))
	workflowDuration, _ = meter.Float64Histogram("flowe.workflow.run.duration",
		metric.WithDescription("End-to-end workflow run duration"), metric.WithUnit("s"), runBuckets)
	workflowActive, _ = meter.Int64UpDownCounter("flowe.workflow.runs.active",
		metric.WithDescription("Workflow runs currently executing"), metric.WithUnit("{run}"))

	nodeExecutions, _ = meter.Int64Counter("flowe.node.executions",
		metric.WithDescription("Node executions by node type and outcome"), metric.WithUnit("{execution}"))
	nodeDuration, _ = meter.Float64Histogram("flowe.node.duration",
		metric.WithDescription("Single node execution duration"), metric.WithUnit("s"), runBuckets)
)

// GinActiveRequests tracks in-flight requests. Request-duration histograms
// come from otelgin, which has no in-flight instrument of its own.
func GinActiveRequests() gin.HandlerFunc {
	return func(c *gin.Context) {
		httpActive.Add(c.Request.Context(), 1)
		defer httpActive.Add(c.Request.Context(), -1)
		c.Next()
	}
}

func RecordWorkflowRun(ctx context.Context, status, trigger string, d time.Duration) {
	attrs := metric.WithAttributes(
		attribute.String("status", status),
		attribute.String("trigger", trigger),
	)
	workflowRuns.Add(ctx, 1, attrs)
	workflowDuration.Record(ctx, d.Seconds(), attrs)
}

func AddActiveRuns(ctx context.Context, delta int64) {
	workflowActive.Add(ctx, delta)
}

func RecordNodeExecution(ctx context.Context, nodeType string, execErr error, d time.Duration) {
	status := "ok"
	if execErr != nil {
		status = "error"
	}
	nodeExecutions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("node.type", nodeType),
		attribute.String("status", status),
	))
	nodeDuration.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String("node.type", nodeType),
	))
}

// ObserveDBPool exposes sql.DB pool stats as gauges
// (flowe.db.pool.connections{state=open|idle|in_use}).
func ObserveDBPool(db *sql.DB) {
	gauge, err := meter.Int64ObservableGauge("flowe.db.pool.connections",
		metric.WithDescription("Database connection pool state"), metric.WithUnit("{connection}"))
	if err != nil {
		return
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := db.Stats()
		o.ObserveInt64(gauge, int64(s.OpenConnections), metric.WithAttributes(attribute.String("state", "open")))
		o.ObserveInt64(gauge, int64(s.Idle), metric.WithAttributes(attribute.String("state", "idle")))
		o.ObserveInt64(gauge, int64(s.InUse), metric.WithAttributes(attribute.String("state", "in_use")))
		return nil
	}, gauge)
	if err != nil {
		slog.Warn("failed to register db pool metrics", "error", err)
	}
}
