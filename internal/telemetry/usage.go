package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Token usage measurement.
//
// Every provider returns its own token counts in the response body, so nothing
// here is estimated. Two sinks receive them, and the separation is deliberate:
//
//  1. the OTel counter below, for dashboards and alerting;
//  2. UsageSink, which writes billing ledger rows.
//
// Never bill from the metric. It is sampled, aggregated and lossy by design.
// The ledger is the source of truth; conflating the two is how billing systems
// end up both wrong and unauditable.

var llmTokens, _ = meter.Int64Counter("fernary.llm.tokens",
	metric.WithDescription("LLM tokens by provider, model, kind and surface"),
	metric.WithUnit("{token}"))

// Usage is one call's token counts. Cached and uncached input are separate
// fields because they bill at different rates — collapsing them would make any
// prompt-caching win invisible.
type Usage struct {
	InputTokens  int
	OutputTokens int
	// CacheReadTokens were served from a cached prefix, and cost a fraction of
	// an uncached input token.
	CacheReadTokens int
	// CacheWriteTokens were written into the cache, and cost *more* than an
	// uncached input token on some providers — which is why caching a prompt
	// used rarely can be a net loss.
	CacheWriteTokens int
}

func (u Usage) IsZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0
}

// Total is every token the call touched, for coarse reporting only. Billing must
// use the individual kinds, since they have different unit costs.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// UsageSink receives every measured call for billing. A package-level hook,
// mirroring the executor's IntegrationCredsLookup, so provider helpers need no
// knowledge of the ledger and telemetry needs no knowledge of the database.
//
// Left nil in tests and in any deployment without billing, in which case usage
// is still measured as a metric.
var UsageSink func(ctx context.Context, provider, model string, u Usage)

// LLMTokens records one call's usage to both sinks. Surface comes from context
// rather than a parameter so that adding a new LLM call site cannot silently
// report usage as belonging to nothing.
func LLMTokens(ctx context.Context, provider, model string, u Usage) {
	if u.IsZero() {
		// A provider that returned no usage is worth knowing about: it means a
		// call is being billed by someone and measured by nobody.
		UsageUnmeasured(ctx, provider, model)
		return
	}
	surface := SurfaceFrom(ctx)
	for kind, n := range map[string]int{
		"input":       u.InputTokens,
		"output":      u.OutputTokens,
		"cache_read":  u.CacheReadTokens,
		"cache_write": u.CacheWriteTokens,
	} {
		if n <= 0 {
			continue
		}
		llmTokens.Add(ctx, int64(n), metric.WithAttributes(
			attribute.String("provider", provider),
			attribute.String("model", model),
			attribute.String("kind", kind),
			attribute.String("surface", surface)))
	}
	if UsageSink != nil {
		UsageSink(ctx, provider, model, u)
	}
}

var llmUnmeasured, _ = meter.Int64Counter("fernary.llm.unmeasured",
	metric.WithDescription("LLM calls that returned no usage, and so were never billed"),
	metric.WithUnit("{call}"))

// UsageUnmeasured flags a call whose usage could not be read. This is a billing
// leak rather than a cosmetic gap — alert on it.
func UsageUnmeasured(ctx context.Context, provider, model string) {
	llmUnmeasured.Add(ctx, 1, metric.WithAttributes(
		attribute.String("provider", provider),
		attribute.String("model", model),
		attribute.String("surface", SurfaceFrom(ctx))))
}

// NodeSpendSink is called once per successfully completed workflow node, so
// billing can charge the nominal per-operation fee.
//
// Separate from UsageSink because the two are charged on different bases: an LLM
// call bills on the tokens it reported, while an integration call bills a flat
// nominal amount. Calling both for an LLM node would double-charge it, so the
// executor only fires this for nodes that are not token-billed.
//
// Success only. Charging for a node that failed is the complaint that credit
// systems attract most, and the marginal cost of a failed integration call is
// zero anyway.
var NodeSpendSink func(ctx context.Context, nodeType, op string)

// NodeSpend reports a completed node to the billing hook, if one is installed.
func NodeSpend(ctx context.Context, nodeType, op string) {
	if NodeSpendSink != nil {
		NodeSpendSink(ctx, nodeType, op)
	}
}
