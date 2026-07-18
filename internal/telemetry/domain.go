package telemetry

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Domain instruments: everything a debugging session reaches for beyond the
// HTTP/run basics — LLM calls, integration ops, auth flow, SSE fan-out,
// approvals, emails, rate limits, panics, DB queries, hub health.

// Short-tail buckets for local work (DB queries and similar).
var dbBuckets = metric.WithExplicitBucketBoundaries(
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5)

var (
	llmCalls, _ = meter.Int64Counter("flowe.llm.calls",
		metric.WithDescription("LLM API calls by provider, model and outcome"), metric.WithUnit("{call}"))
	llmDuration, _ = meter.Float64Histogram("flowe.llm.duration",
		metric.WithDescription("LLM API call duration"), metric.WithUnit("s"), runBuckets)

	integrationCalls, _ = meter.Int64Counter("flowe.integration.calls",
		metric.WithDescription("Integration node operations by provider, op and outcome"), metric.WithUnit("{call}"))
	integrationDuration, _ = meter.Float64Histogram("flowe.integration.duration",
		metric.WithDescription("Integration node operation duration"), metric.WithUnit("s"), runBuckets)

	authEvents, _ = meter.Int64Counter("flowe.auth.events",
		metric.WithDescription("Auth flow events (login_start, login_verify, oauth, logout, unauthorized)"), metric.WithUnit("{event}"))
	rateLimitHits, _ = meter.Int64Counter("flowe.ratelimit.hits",
		metric.WithDescription("Requests rejected by a rate limiter"), metric.WithUnit("{hit}"))

	sseStreams, _ = meter.Int64UpDownCounter("flowe.sse.streams",
		metric.WithDescription("Open SSE streams by kind"), metric.WithUnit("{stream}"))
	runEvents, _ = meter.Int64Counter("flowe.run.events",
		metric.WithDescription("Workflow execution events emitted, by type"), metric.WithUnit("{event}"))

	webhooksReceived, _ = meter.Int64Counter("flowe.webhooks.received",
		metric.WithDescription("Inbound webhook deliveries by outcome"), metric.WithUnit("{delivery}"))
	scheduleFires, _ = meter.Int64Counter("flowe.schedule.fires",
		metric.WithDescription("Scheduled trigger fires by outcome"), metric.WithUnit("{fire}"))

	approvalsPending, _ = meter.Int64UpDownCounter("flowe.approvals.pending",
		metric.WithDescription("Human-approval nodes currently waiting"), metric.WithUnit("{approval}"))
	approvalWait, _ = meter.Float64Histogram("flowe.approval.wait",
		metric.WithDescription("How long approval nodes waited for a decision"), metric.WithUnit("s"), runBuckets)
	approvalsResolved, _ = meter.Int64Counter("flowe.approvals.resolved",
		metric.WithDescription("Approval outcomes (approved, rejected, timeout, cancelled)"), metric.WithUnit("{approval}"))

	emailsSent, _ = meter.Int64Counter("flowe.emails.sent",
		metric.WithDescription("Emails sent by kind and outcome"), metric.WithUnit("{email}"))
	templateMisses, _ = meter.Int64Counter("flowe.template.misses",
		metric.WithDescription("Template references to a node with no output ([no output from …])"), metric.WithUnit("{miss}"))
	panicsRecovered, _ = meter.Int64Counter("flowe.panics",
		metric.WithDescription("Panics recovered by HTTP middleware"), metric.WithUnit("{panic}"))

	dbQueryDuration, _ = meter.Float64Histogram("flowe.db.query.duration",
		metric.WithDescription("GORM query duration by operation"), metric.WithUnit("s"), dbBuckets)

	hubSubscribers, _ = meter.Int64UpDownCounter("flowe.hub.subscribers",
		metric.WithDescription("Live pub/sub subscribers by hub"), metric.WithUnit("{subscriber}"))
	hubDropped, _ = meter.Int64Counter("flowe.hub.dropped",
		metric.WithDescription("Events dropped because a subscriber channel was full"), metric.WithUnit("{event}"))
)

// SpanAttrs adds attributes to the span in ctx (no-op without a span).
func SpanAttrs(ctx context.Context, attrs ...attribute.KeyValue) {
	trace.SpanFromContext(ctx).SetAttributes(attrs...)
}

// StartLLM opens an llm span and returns a completion func recording
// metrics, span status, and a log line.
func StartLLM(ctx context.Context, provider, model string) (context.Context, func(respChars int, err error)) {
	ctx, span := Tracer.Start(ctx, "llm "+model, trace.WithAttributes(
		attribute.String("gen_ai.system", provider),
		attribute.String("gen_ai.request.model", model),
	))
	start := time.Now()
	return ctx, func(respChars int, err error) {
		status := "ok"
		if err != nil {
			status = "error"
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.ErrorContext(ctx, "llm call failed",
				"provider", provider, "model", model, "duration_ms", time.Since(start).Milliseconds(), "error", err.Error())
		} else {
			slog.InfoContext(ctx, "llm call completed",
				"provider", provider, "model", model, "duration_ms", time.Since(start).Milliseconds(), "response_chars", respChars)
		}
		llmCalls.Add(ctx, 1, metric.WithAttributes(
			attribute.String("provider", provider),
			attribute.String("model", model),
			attribute.String("status", status)))
		llmDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
			attribute.String("provider", provider),
			attribute.String("model", model)))
		span.End()
	}
}

func RecordIntegrationCall(ctx context.Context, provider, operation string, execErr error, d time.Duration) {
	status := "ok"
	if execErr != nil {
		status = "error"
	}
	integrationCalls.Add(ctx, 1, metric.WithAttributes(
		attribute.String("provider", provider),
		attribute.String("operation", operation),
		attribute.String("status", status)))
	integrationDuration.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String("provider", provider)))
}

func AuthEvent(ctx context.Context, event, status string) {
	authEvents.Add(ctx, 1, metric.WithAttributes(
		attribute.String("event", event),
		attribute.String("status", status)))
}

func RateLimitHit(ctx context.Context, scope string) {
	rateLimitHits.Add(ctx, 1, metric.WithAttributes(attribute.String("scope", scope)))
}

func AddSSEStream(ctx context.Context, kind string, delta int64) {
	sseStreams.Add(ctx, delta, metric.WithAttributes(attribute.String("kind", kind)))
}

func RecordRunEvent(ctx context.Context, eventType string) {
	runEvents.Add(ctx, 1, metric.WithAttributes(attribute.String("type", eventType)))
}

func WebhookReceived(ctx context.Context, status string) {
	webhooksReceived.Add(ctx, 1, metric.WithAttributes(attribute.String("status", status)))
}

func ScheduleFire(ctx context.Context, status string) {
	scheduleFires.Add(ctx, 1, metric.WithAttributes(attribute.String("status", status)))
}

func ApprovalPending(ctx context.Context, delta int64) {
	approvalsPending.Add(ctx, delta)
}

func ApprovalResolved(ctx context.Context, result string, waited time.Duration) {
	approvalsResolved.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	approvalWait.Record(ctx, waited.Seconds())
}

func EmailSent(ctx context.Context, kind string, err error) {
	status := "ok"
	if err != nil {
		status = "error"
		slog.ErrorContext(ctx, "email send failed", "kind", kind, "error", err.Error())
	}
	emailsSent.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", kind),
		attribute.String("status", status)))
}

func TemplateMiss(nodeID string) {
	templateMisses.Add(context.Background(), 1)
	slog.Warn("template referenced node with no output", "node_id", nodeID)
}

func RecordPanic(ctx context.Context, route string) {
	panicsRecovered.Add(ctx, 1, metric.WithAttributes(attribute.String("route", route)))
}

func RecordDBQuery(ctx context.Context, operation string, d time.Duration, dbErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dbQueryDuration.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String("operation", operation)))
	_ = dbErr // recorded via gorm tracing spans; histogram stays low-cardinality
}

func AddHubSubscriber(hub string, delta int64) {
	hubSubscribers.Add(context.Background(), delta, metric.WithAttributes(attribute.String("hub", hub)))
}

func HubDropped(hub string) {
	hubDropped.Add(context.Background(), 1, metric.WithAttributes(attribute.String("hub", hub)))
}
