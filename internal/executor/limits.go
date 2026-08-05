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
