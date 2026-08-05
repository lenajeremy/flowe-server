package billing

import (
	"log/slog"
	"time"

	"workflow-ai/server/internal/database/models"
)

// Run-history retention.
//
// WorkflowRun.Events is a JSONB blob of every event in a run, node outputs
// included, and a single Drive read can be about a megabyte. A workflow on a
// five-minute schedule is roughly 8,640 runs a month, each carrying a full blob —
// and until now nothing in the codebase ever deleted one. That grows Postgres
// storage and every backup, forever.
//
// Retention windows are a legitimate upsell that happens to solve that: short
// history free, long history paid. Framing it as a tier feature rather than as
// cleanup is honest — a customer debugging a failed run genuinely wants a longer
// window, and we genuinely cannot store everything forever for nothing.

// PruneInterval is how often the sweep runs. Hourly is frequent enough to keep
// storage bounded without adding meaningful load.
const PruneInterval = time.Hour

// pruneBatch caps one pass so a large backlog is worked through gradually instead
// of holding a long transaction against the runs table.
const pruneBatch = 5000

// PruneRunHistory deletes run records past their org's retention window.
//
// Deletes in plan-sized batches rather than with one clever query, because the
// window depends on the org's plan and expressing that as SQL would mean encoding
// the plan table into a CASE expression that then has to be kept in sync with Go.
//
// Runs are hard-deleted. A soft delete would keep the Events blob — the actual
// cost — on disk, which would make this function look like it worked while
// changing nothing.
func (g *Gate) PruneRunHistory() (int64, error) {
	var total int64
	for plan, lim := range planLimits {
		if lim.RunHistoryRetention <= 0 {
			continue // unlimited retention: nothing to prune
		}
		cutoff := time.Now().Add(-lim.RunHistoryRetention)

		// Selects by the org's plan so each tenant gets exactly the window it pays
		// for. EffectivePlan is not consulted here: a lapsed org keeps the retention
		// its stored plan implies rather than having history deleted the moment a
		// card fails, which would be an unrecoverable punishment for a billing
		// hiccup.
		res := g.db.Exec(`
			DELETE FROM workflow_runs
			WHERE id IN (
				SELECT r.id FROM workflow_runs r
				JOIN organizations o ON o.id = r.organization_id
				WHERE o.plan = ? AND r.created_at < ?
				LIMIT ?
			)`, string(plan), cutoff, pruneBatch)
		if res.Error != nil {
			return total, res.Error
		}
		if res.RowsAffected > 0 {
			slog.Info("pruned run history past the plan's retention window",
				"plan", plan, "deleted", res.RowsAffected,
				"retention_days", int(lim.RunHistoryRetention.Hours())/24)
			total += res.RowsAffected
		}
	}

	// Runs whose org no longer exists cannot be attributed to a plan, so they would
	// otherwise be kept forever. The free window is the safe floor for them.
	orphanCutoff := time.Now().Add(-planLimits[models.PlanFree].RunHistoryRetention)
	res := g.db.Exec(`
		DELETE FROM workflow_runs
		WHERE id IN (
			SELECT r.id FROM workflow_runs r
			LEFT JOIN organizations o ON o.id = r.organization_id
			WHERE o.id IS NULL AND r.created_at < ?
			LIMIT ?
		)`, orphanCutoff, pruneBatch)
	if res.Error != nil {
		return total, res.Error
	}
	if res.RowsAffected > 0 {
		slog.Info("pruned run history with no surviving organization", "deleted", res.RowsAffected)
		total += res.RowsAffected
	}
	return total, nil
}

// RunRetentionFor is the window an org's plan allows, for display next to run
// history so the limit is visible before someone goes looking for a run that has
// already gone.
func RunRetentionFor(plan models.Plan) time.Duration {
	return LimitsFor(plan).RunHistoryRetention
}
