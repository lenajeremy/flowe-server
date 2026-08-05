package billing

import (
	"errors"
	"fmt"
	"time"

	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database/models"
)

// What each plan may do.
//
// These are the customer-facing story. Credits are the internal meter and the
// wrong thing to sell: our buyers want to know what this costs per month, and a
// meter that varies with how chatty the LLM was on Tuesday produces bill anxiety.
// On an unattended product that is worse than on an interactive one, because the
// thing spends money while they sleep. The failure mode is not churn — it is that
// people quietly stop publishing schedules, which kills activation while looking
// like disengagement.
//
// So limits are expressed in units a buyer can picture: how many workflows, how
// often they run, how long history is kept, how many people.

// ErrLimit is a plan limit, not a fault. Handlers return it as 402 with the
// upgrade path in the message — never as a generic error, which reads as a bug.
var ErrLimit = errors.New("plan limit reached")

// Limits is one plan's entitlements.
type Limits struct {
	// MaxWorkflows caps saved workflows. Zero means unlimited.
	MaxWorkflows int
	// MaxPublishedSchedules caps workflows that can fire on a schedule. This is
	// the limit that matters most, because a published schedule is the only thing
	// that spends money unattended.
	MaxPublishedSchedules int
	// MinScheduleInterval is the fastest a schedule may repeat. The free tier is
	// held to daily: a 5-minute schedule is ~8,640 runs a month, which is both a
	// real provider bill and a storage problem on an account paying nothing.
	MinScheduleInterval time.Duration
	// RunHistoryRetention is how long run records are kept. Also the fix for the
	// unflagged storage cost — WorkflowRun.Events is a full JSONB blob of every
	// event including node outputs, and nothing has ever deleted it.
	RunHistoryRetention time.Duration
	// MaxMembers caps org membership. One means personal-only.
	MaxMembers int
	// MonthlyCredits is the allowance, restated here so one lookup answers
	// "what does this plan give me".
	MonthlyCredits int64
	// MaxTokensPerCall bounds a single LLM call.
	MaxTokensPerCall int
	// SharedConnections allows granting an integration credential to the org.
	SharedConnections bool
	// AuditLog and SSO are Business-tier controls, declared here so the pricing
	// page and the enforcement points read from the same table.
	AuditLog bool
	SSO      bool
}

// unlimited is used for count caps that do not apply.
const unlimited = 0

// planLimits is the single source of truth. The pricing page renders from it and
// the enforcement points check against it, so the page cannot advertise something
// the server does not allow.
var planLimits = map[models.Plan]Limits{
	models.PlanFree: {
		MaxWorkflows:          3,
		MaxPublishedSchedules: 1,
		// Daily minimum. Anything faster on a free account is a cost we cannot
		// recover and, historically, the shape abuse takes.
		MinScheduleInterval: 24 * time.Hour,
		RunHistoryRetention: 7 * 24 * time.Hour,
		MaxMembers:          1,
		MonthlyCredits:      credits.GrantFree,
		MaxTokensPerCall:    credits.MaxTokensCeiling(models.PlanFree),
	},
	models.PlanPro: {
		MaxWorkflows:          25,
		MaxPublishedSchedules: 10,
		MinScheduleInterval:   5 * time.Minute,
		RunHistoryRetention:   30 * 24 * time.Hour,
		MaxMembers:            1,
		MonthlyCredits:        credits.GrantPro,
		MaxTokensPerCall:      credits.MaxTokensCeiling(models.PlanPro),
	},
	models.PlanTeam: {
		MaxWorkflows:          unlimited,
		MaxPublishedSchedules: 50,
		MinScheduleInterval:   time.Minute,
		RunHistoryRetention:   90 * 24 * time.Hour,
		MaxMembers:            10,
		MonthlyCredits:        credits.GrantTeam,
		MaxTokensPerCall:      credits.MaxTokensCeiling(models.PlanTeam),
		SharedConnections:     true,
	},
	models.PlanBusiness: {
		MaxWorkflows:          unlimited,
		MaxPublishedSchedules: unlimited,
		MinScheduleInterval:   time.Minute,
		RunHistoryRetention:   365 * 24 * time.Hour,
		MaxMembers:            unlimited,
		MonthlyCredits:        credits.GrantBusiness,
		MaxTokensPerCall:      credits.MaxTokensCeiling(models.PlanBusiness),
		SharedConnections:     true,
		AuditLog:              true,
		SSO:                   true,
	},
}

// LimitsFor returns a plan's entitlements, defaulting to free for anything
// unrecognised. An unknown plan string must never unlock more than the cheapest
// tier.
func LimitsFor(p models.Plan) Limits {
	if l, ok := planLimits[p]; ok {
		return l
	}
	return planLimits[models.PlanFree]
}

// ── Enforcement ──────────────────────────────────────────────────

// CheckWorkflowCount refuses a new workflow past the plan's cap.
func (g *Gate) CheckWorkflowCount(orgID string, plan models.Plan) error {
	lim := LimitsFor(plan)
	if lim.MaxWorkflows == unlimited {
		return nil
	}
	var n int64
	if err := g.db.Table("workflows").
		Where("organization_id = ? AND deleted_at IS NULL", orgID).Count(&n).Error; err != nil {
		return err
	}
	if n >= int64(lim.MaxWorkflows) {
		return fmt.Errorf("%w: the %s plan includes %d workflows. Upgrade for more",
			ErrLimit, plan, lim.MaxWorkflows)
	}
	return nil
}

// CheckPublishSchedule refuses publishing a workflow past the plan's schedule cap.
//
// Checked at PUBLISH rather than at save, because publishing is the moment a
// workflow starts costing money unattended — and it is the moment the user is
// making a decision, so a limit message lands as information rather than as an
// obstruction.
func (g *Gate) CheckPublishSchedule(orgID, workflowID string, plan models.Plan) error {
	lim := LimitsFor(plan)
	if lim.MaxPublishedSchedules == unlimited {
		return nil
	}
	var n int64
	// Counts workflows that are published AND actually have a live schedule; a
	// published workflow with no schedule costs nothing and should not count.
	err := g.db.Table("workflows").
		Joins("JOIN scheduled_triggers st ON st.workflow_id = workflows.id::text").
		Where(`workflows.organization_id = ? AND workflows.deleted_at IS NULL
			AND workflows.published = true AND st.enabled = true
			AND workflows.id::text <> ?`, orgID, workflowID).
		Count(&n).Error
	if err != nil {
		return err
	}
	if n >= int64(lim.MaxPublishedSchedules) {
		if lim.MaxPublishedSchedules == 1 {
			return fmt.Errorf("%w: the %s plan runs one scheduled workflow at a time. "+
				"Unpublish the other one, or upgrade to run more", ErrLimit, plan)
		}
		return fmt.Errorf("%w: the %s plan runs %d scheduled workflows. Upgrade for more",
			ErrLimit, plan, lim.MaxPublishedSchedules)
	}
	return nil
}

// ScheduleInterval clamps a requested schedule to the plan's floor and reports
// whether it had to.
//
// Clamped rather than rejected: a user who asks for every 5 minutes on the free
// plan gets a working daily schedule plus an explanation, which is far better than
// a form that refuses to save. The returned message is shown alongside the saved
// schedule so the difference is never a surprise later.
func ScheduleInterval(plan models.Plan, requested time.Duration) (time.Duration, string) {
	floor := LimitsFor(plan).MinScheduleInterval
	if requested <= 0 || requested >= floor {
		return requested, ""
	}
	return floor, fmt.Sprintf(
		"The %s plan runs schedules at most once every %s, so this was set to %s. "+
			"Upgrade to run it more often.", plan, humanizeInterval(floor), humanizeInterval(floor))
}

// AllowsFrequency reports whether a named frequency is fast enough for the plan.
// The schedule UI offers interval/hourly/daily/weekly/monthly rather than raw
// durations, so the check has to speak the same vocabulary.
func AllowsFrequency(plan models.Plan, frequency string, intervalSeconds int) (bool, string) {
	floor := LimitsFor(plan).MinScheduleInterval
	var d time.Duration
	switch frequency {
	case "interval":
		d = time.Duration(intervalSeconds) * time.Second
	case "hourly":
		d = time.Hour
	case "daily":
		d = 24 * time.Hour
	case "weekly":
		d = 7 * 24 * time.Hour
	case "monthly":
		d = 30 * 24 * time.Hour
	default:
		return true, "" // unknown frequency: not this function's job to reject
	}
	if d >= floor {
		return true, ""
	}
	return false, fmt.Sprintf(
		"The %s plan runs schedules at most once every %s. Choose a slower schedule, "+
			"or upgrade to run it more often.", plan, humanizeInterval(floor))
}

func humanizeInterval(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		if days := int(d.Hours()) / 24; days == 1 {
			return "day"
		}
		return fmt.Sprintf("%d days", int(d.Hours())/24)
	case d >= time.Hour:
		if h := int(d.Hours()); h == 1 {
			return "hour"
		}
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
}
