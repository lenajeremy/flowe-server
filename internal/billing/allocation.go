package billing

import (
	"fmt"
	"time"

	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database/models"
)

// Per-person credit allocation.
//
// The org keeps ONE balance and one ledger. A member's allocation is a CAP on how
// much of it they may spend in a period, not a separate wallet. That matters: sub-
// wallets would need their own reconciliation, and the invariant that balance
// equals sum(delta) — which everything else here depends on — would no longer hold.
//
// The cap exists so one person cannot drain the organisation. Someone who reaches
// theirs is stopped even though the org still has credit, because that credit is
// what everyone else is going to need.

// ErrMemberCapReached means this person has spent their share for the period.
// Distinct from ErrOverCap, which is the whole org running out — the two need
// different messages and different fixes.
var ErrMemberCapReached = fmt.Errorf("%w: personal allowance used up", ErrOverCap)

// Allocation is one member's position for the current period.
type Allocation struct {
	UserID string
	// Limit is what they may spend. Zero means unlimited, which is what a personal
	// org gets — capping a single-member org against itself would be pointless.
	Limit int64
	Spent int64
	// Custom is true when an admin set this explicitly rather than it being the
	// equal share, so the UI can show that it was a decision.
	Custom bool
}

func (a Allocation) Remaining() int64 {
	if a.Limit <= 0 {
		return -1 // unlimited
	}
	return max(a.Limit-a.Spent, 0)
}

func (a Allocation) Exhausted() bool {
	return a.Limit > 0 && a.Spent >= a.Limit
}

// UsedPercent is for display, capped at 100.
func (a Allocation) UsedPercent() int {
	if a.Limit <= 0 {
		return 0
	}
	return int(min(a.Spent*100/a.Limit, 100))
}

// EqualShare is the default per-member cap: the org's allowance split across the
// seats it pays for.
//
// Divided by SEATS rather than by current member count on purpose. Splitting by
// members would shrink everyone's allowance each time someone joins, so adding a
// teammate would quietly cut the existing team's budget — the opposite of what
// paying for another seat should do.
func EqualShare(org *models.Organization) int64 {
	if org == nil {
		return 0
	}
	lim := LimitsForOrg(org)
	if !LimitsFor(EffectivePlan(org)).PerSeat {
		// A single-person plan has nobody to protect the allowance from.
		return 0
	}
	seats := max(org.Seats, 1)
	return lim.MonthlyCredits / int64(seats)
}

// AllocationFor returns a member's cap and what they have spent this period.
func (g *Gate) AllocationFor(org *models.Organization, userID string) (Allocation, error) {
	a := Allocation{UserID: userID}
	if org == nil || userID == "" {
		return a, nil
	}

	var member models.OrgMember
	if err := g.db.Where("organization_id = ? AND user_id = ?", org.ID, userID).
		First(&member).Error; err != nil {
		// Not a member — no allocation to speak of. The org-level check still applies.
		return a, nil
	}
	if member.CreditLimit > 0 {
		a.Limit, a.Custom = member.CreditLimit, true
	} else {
		a.Limit = EqualShare(org)
	}

	bal, err := credits.Balance(g.db, org.ID.String())
	if err != nil {
		return a, err
	}
	spent, err := credits.SpentByUserSince(g.db, org.ID.String(), userID, credits.PeriodStart(bal))
	if err != nil {
		return a, err
	}
	a.Spent = spent
	return a, nil
}

// CheckMemberAllowance refuses work when this person has used their share.
//
// Checked in addition to the org balance, not instead of it: the org can be out of
// credit while an individual is well within their cap, and vice versa.
func (g *Gate) CheckMemberAllowance(org *models.Organization, userID string) error {
	a, err := g.AllocationFor(org, userID)
	if err != nil {
		return err
	}
	if !a.Exhausted() {
		return nil
	}
	return fmt.Errorf("%w — you've used your share of this period's credits (%s of %s). "+
		"Your organization's owner can raise your limit or add credits",
		ErrMemberCapReached, formatCredits(a.Spent), formatCredits(a.Limit))
}

// SetMemberLimit records an admin's explicit cap for someone. Zero restores the
// equal share, which is why it is not simply "unlimited".
func (g *Gate) SetMemberLimit(orgID, userID string, limit int64) error {
	if limit < 0 {
		return fmt.Errorf("a credit limit cannot be negative")
	}
	res := g.db.Model(&models.OrgMember{}).
		Where("organization_id = ? AND user_id = ?", orgID, userID).
		Update("credit_limit", limit)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("not a member of this organization")
	}
	return nil
}

// formatCredits renders a credit figure with thousands separators, so a message
// about 80000 credits does not read as a phone number.
func formatCredits(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// PeriodEndsAt is when the current allowance period runs out, for messages that
// need to say when a limit resets.
func PeriodEndsAt(org *models.Organization) *time.Time {
	if org == nil {
		return nil
	}
	return org.CurrentPeriodEnd
}
