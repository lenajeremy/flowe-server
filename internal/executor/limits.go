package executor

import (
	"context"
	"log/slog"

	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/telemetry"
)

// Per-call token ceiling.
//
// A credit reservation is a check against a plausible worst case, so there has to
// BE a worst case. Without a ceiling a single node could request a two-hundred
// thousand token completion and blow through an entire month's allowance in one
// call — after the affordability check had already passed.
//
// Clamping rather than rejecting is deliberate: a workflow that asked for more
// than its plan allows should still run, just capped. Failing the node instead
// would turn a pricing limit into a broken workflow, and the user cannot tell
// those apart from an error message.
func clampMaxTokens(ctx context.Context, requested int) int {
	plan := models.Plan(telemetry.BillingFrom(ctx).Plan)
	ceiling := credits.MaxTokensCeiling(plan)
	if requested <= 0 {
		return ceiling
	}
	if requested <= ceiling {
		return requested
	}
	slog.WarnContext(ctx, "llm node max_tokens clamped to the plan ceiling",
		"requested", requested, "ceiling", ceiling, "plan", string(plan))
	return ceiling
}

// Event-log ceilings.
//
// A run's events are streamed to the browser AND serialised whole into one
// JSONB column on the run row. Neither had a bound: a 200-item loop over a node
// returning 10KB writes ~2MB into a single column and pushes all of it down the
// wire. That was survivable while nobody could see per-iteration detail; it
// stops being survivable the moment the log invites you to look.
//
// Both caps apply only to the event's copy of a value. The executor's outputs
// map still carries the full text to downstream nodes, so clipping the log can
// never change what a workflow computes.
const (
	// maxEventOutput bounds one event's payload. Large enough that ordinary
	// node output is never touched; small enough that a runaway loop cannot
	// write an unbounded row.
	maxEventOutput = 32 * 1024
	// maxRunEvents bounds how many events a single run may record. Past it the
	// log says so and stops, rather than growing until the write fails.
	maxRunEvents = 10000
)

// tokenBilledNode reports whether a node type is already charged on its token
// usage, and so must not also be charged the flat per-operation fee.
//
// An LLM node's cost is its tokens; an agent turn's likewise. Everything else —
// integrations, email, HTTP, data — is a flat nominal charge, because our marginal
// cost on those is close to zero and the fee is a fair-use brake rather than cost
// recovery.
func tokenBilledNode(t NodeType) bool {
	return t == NodeTypeLLM
}
