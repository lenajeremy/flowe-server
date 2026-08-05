package billing

import (
	"strings"
	"testing"
	"time"

	"workflow-ai/server/internal/database/models"
)

func TestUnknownPlanGetsFreeLimits(t *testing.T) {
	// A renamed Stripe price or a hand-edited column must never unlock a paid tier.
	free := LimitsFor(models.PlanFree)
	for _, p := range []models.Plan{"", "gold", "PRO", "enterprise"} {
		if got := LimitsFor(p); got != free {
			t.Fatalf("plan %q got non-free limits: %+v", p, got)
		}
	}
}

func TestLimitsLoosenMonotonicallyUpTheTiers(t *testing.T) {
	// A tier that is worse than the one below it at anything is a packaging bug
	// that would show up as a customer downgrading to get a feature back.
	//
	// Compared at each tier's SMALLEST SELLABLE size. Team's per-seat figures are
	// below Pro's on purpose — a one-seat Team is not something we sell, the
	// two-seat minimum is — so the per-seat base would be comparing a unit price
	// against a total.
	seatsFor := func(p models.Plan) int {
		if LimitsFor(p).PerSeat {
			return MinSeats
		}
		return 1
	}
	order := []models.Plan{models.PlanFree, models.PlanPro, models.PlanTeam, models.PlanBusiness}
	for i := 1; i < len(order); i++ {
		lo := Scale(LimitsFor(order[i-1]), seatsFor(order[i-1]))
		hi := Scale(LimitsFor(order[i]), seatsFor(order[i]))

		if hi.MinScheduleInterval > lo.MinScheduleInterval {
			t.Fatalf("%s schedules less often than %s", order[i], order[i-1])
		}
		if hi.RunHistoryRetention < lo.RunHistoryRetention {
			t.Fatalf("%s keeps less history than %s", order[i], order[i-1])
		}
		if hi.MonthlyCredits <= lo.MonthlyCredits {
			t.Fatalf("%s grants no more credits than %s", order[i], order[i-1])
		}
		if hi.MaxTokensPerCall < lo.MaxTokensPerCall {
			t.Fatalf("%s allows smaller calls than %s", order[i], order[i-1])
		}
		// Count caps: 0 means unlimited, so it always beats a finite number.
		looser := func(hiV, loV int) bool { return hiV == unlimited || (loV != unlimited && hiV >= loV) }
		if !looser(hi.MaxWorkflows, lo.MaxWorkflows) {
			t.Fatalf("%s allows fewer workflows (%d) than %s (%d)",
				order[i], hi.MaxWorkflows, order[i-1], lo.MaxWorkflows)
		}
		if !looser(hi.MaxPublishedSchedules, lo.MaxPublishedSchedules) {
			t.Fatalf("%s allows fewer schedules (%d) than %s (%d)",
				order[i], hi.MaxPublishedSchedules, order[i-1], lo.MaxPublishedSchedules)
		}
		if !looser(hi.MaxMembers, lo.MaxMembers) {
			t.Fatalf("%s allows fewer members (%d) than %s (%d)",
				order[i], hi.MaxMembers, order[i-1], lo.MaxMembers)
		}
	}
}

func TestFreeTierIsOneScheduleAtADailyMinimum(t *testing.T) {
	// The decision this whole limit exists for: a 5-minute schedule on a free
	// account is ~8,640 runs a month of provider spend and storage that nobody is
	// paying for.
	free := LimitsFor(models.PlanFree)
	if free.MaxPublishedSchedules != 1 {
		t.Fatalf("free tier allows %d published schedules, want 1", free.MaxPublishedSchedules)
	}
	if free.MinScheduleInterval != 24*time.Hour {
		t.Fatalf("free tier minimum interval is %s, want 24h", free.MinScheduleInterval)
	}
}

func TestFrequenciesFasterThanThePlanFloorAreRefused(t *testing.T) {
	cases := []struct {
		plan      models.Plan
		frequency string
		interval  int
		allowed   bool
	}{
		{models.PlanFree, "daily", 0, true},
		{models.PlanFree, "weekly", 0, true},
		{models.PlanFree, "monthly", 0, true},
		{models.PlanFree, "hourly", 0, false},
		{models.PlanFree, "interval", 300, false},
		{models.PlanFree, "interval", 86400, true},
		{models.PlanPro, "hourly", 0, true},
		{models.PlanPro, "interval", 300, true},
		{models.PlanPro, "interval", 60, false},
		{models.PlanTeam, "interval", 60, true},
	}
	for _, tc := range cases {
		ok, msg := AllowsFrequency(tc.plan, tc.frequency, tc.interval)
		if ok != tc.allowed {
			t.Fatalf("%s / %s / %ds: allowed=%v want %v (%s)",
				tc.plan, tc.frequency, tc.interval, ok, tc.allowed, msg)
		}
		if !ok && msg == "" {
			t.Fatalf("%s / %s: refused with no explanation", tc.plan, tc.frequency)
		}
	}
}

func TestRefusalMessagesNameThePlanAndThePathForward(t *testing.T) {
	// A limit message that does not say what to do about it reads as a bug rather
	// than as a limit, and generates support load instead of upgrades.
	_, msg := AllowsFrequency(models.PlanFree, "hourly", 0)
	if !strings.Contains(msg, "free") {
		t.Fatalf("message does not name the plan: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "upgrade") {
		t.Fatalf("message offers no way forward: %q", msg)
	}
}

func TestUnknownFrequencyIsNotThisCheckToReject(t *testing.T) {
	// Frequency validation belongs to the handler; a limit check that also
	// second-guesses the vocabulary would reject a new frequency the moment one is
	// added, with a message about billing.
	if ok, _ := AllowsFrequency(models.PlanFree, "fortnightly", 0); !ok {
		t.Fatal("an unrecognised frequency should pass the limit check untouched")
	}
}

func TestScheduleIntervalClampsRatherThanZeroing(t *testing.T) {
	got, msg := ScheduleInterval(models.PlanFree, 5*time.Minute)
	if got != 24*time.Hour {
		t.Fatalf("clamped to %s, want 24h", got)
	}
	if msg == "" {
		t.Fatal("a clamped interval must be explained, or it is discovered later as a bug")
	}
	// An interval already within the plan is returned untouched and silently.
	if got, msg := ScheduleInterval(models.PlanPro, time.Hour); got != time.Hour || msg != "" {
		t.Fatalf("an allowed interval was altered: %s %q", got, msg)
	}
}

func TestEveryPlanHasAFiniteRetentionWindow(t *testing.T) {
	// Unlimited retention on any tier would reinstate the storage problem the
	// window exists to solve — every run persists a full JSONB event blob.
	for plan := range planLimits {
		if RunRetentionFor(plan) <= 0 {
			t.Fatalf("plan %s keeps run history forever", plan)
		}
	}
}
