package billing

import (
	"testing"
	"time"

	"workflow-ai/server/internal/database/models"
)

// EffectivePlan decides entitlement on every request, so its edge cases are the
// difference between giving away paid features and taking away features someone
// paid for. Both are worth testing directly rather than inferring from a webhook.

func ptr(t time.Time) *time.Time { return &t }

func TestActiveSubscriptionKeepsItsPlan(t *testing.T) {
	org := &models.Organization{
		Plan: models.PlanPro, PlanStatus: "active",
		CurrentPeriodEnd: ptr(time.Now().Add(20 * 24 * time.Hour)),
	}
	if got := EffectivePlan(org); got != models.PlanPro {
		t.Fatalf("effective plan = %q, want pro", got)
	}
}

func TestCancelledSubscriptionKeepsItsPlanUntilThePeriodEnds(t *testing.T) {
	// They paid for the month. Downgrading on the cancel click would take away
	// something already bought, which reads as punishment for cancelling.
	org := &models.Organization{
		Plan: models.PlanTeam, PlanStatus: "canceled",
		CancelAtPeriodEnd: true,
		CurrentPeriodEnd:  ptr(time.Now().Add(10 * 24 * time.Hour)),
	}
	if got := EffectivePlan(org); got != models.PlanTeam {
		t.Fatalf("effective plan = %q, want team until the period ends", got)
	}
}

func TestLapsedPeriodFallsBackToFree(t *testing.T) {
	// A webhook that never arrived must not leave an unpaid org on a paid tier
	// indefinitely. The period end is the backstop.
	org := &models.Organization{
		Plan: models.PlanPro, PlanStatus: "canceled",
		CurrentPeriodEnd: ptr(time.Now().Add(-24 * time.Hour)),
	}
	if got := EffectivePlan(org); got != models.PlanFree {
		t.Fatalf("effective plan = %q, want free after the period lapsed", got)
	}
}

func TestPastDueStillWorksUntilThePeriodEnds(t *testing.T) {
	// A failed card retry should not cut off a customer mid-month; Stripe will
	// keep retrying and most recover.
	org := &models.Organization{
		Plan: models.PlanPro, PlanStatus: "past_due",
		CurrentPeriodEnd: ptr(time.Now().Add(3 * 24 * time.Hour)),
	}
	if got := EffectivePlan(org); got != models.PlanPro {
		t.Fatalf("effective plan = %q, want pro while past_due inside the period", got)
	}
}

func TestUnrecognisedStripeStatusIsNotEntitled(t *testing.T) {
	// Statuses Stripe adds later, or ones we never mapped (unpaid, incomplete),
	// must fail closed. Interpreting at read time is what makes this possible —
	// a status written into the plan column could not be re-judged.
	for _, status := range []string{"unpaid", "incomplete", "incomplete_expired", "paused", "something_new"} {
		org := &models.Organization{
			Plan: models.PlanBusiness, PlanStatus: status,
			CurrentPeriodEnd: ptr(time.Now().Add(30 * 24 * time.Hour)),
		}
		if got := EffectivePlan(org); got != models.PlanFree {
			t.Fatalf("status %q yielded plan %q, want free", status, got)
		}
	}
}

func TestManuallyProvisionedPlanWorksWithoutStripe(t *testing.T) {
	// A Business customer billed on an invoice has no Stripe subscription at all.
	// An empty status must mean "set by hand", not "not entitled", or that
	// customer silently loses everything they bought.
	org := &models.Organization{Plan: models.PlanBusiness}
	if got := EffectivePlan(org); got != models.PlanBusiness {
		t.Fatalf("effective plan = %q, want business for a hand-provisioned org", got)
	}
}

func TestMissingOrEmptyOrgIsFree(t *testing.T) {
	if got := EffectivePlan(nil); got != models.PlanFree {
		t.Fatalf("nil org = %q, want free", got)
	}
	if got := EffectivePlan(&models.Organization{}); got != models.PlanFree {
		t.Fatalf("zero org = %q, want free", got)
	}
}
