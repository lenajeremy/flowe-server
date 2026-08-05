package billing

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database/models"
)

// Per-member allocation against a real Postgres. Opt-in via TEST_DATABASE_URL.
//
// The point of the cap is that one person cannot drain the organisation, so the
// tests that matter are the ones where the ORG still has credit and an individual
// is nonetheless refused.

func allocDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run allocation tests")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.Organization{}, &models.OrgMember{},
		&models.CreditLedger{}, &models.CreditBalance{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// teamOrg builds a Team org with the given seats and members, and grants it a
// period's allowance.
func teamOrg(t *testing.T, db *gorm.DB, seats, members int) (*models.Organization, []string) {
	t.Helper()
	org := models.Organization{
		Name: "Alloc Test", Slug: "alloc-" + uuid.NewString()[:8],
		Plan: models.PlanTeam, PlanStatus: "active", Personal: false, Seats: seats,
	}
	org.ID = uuid.New()
	if err := db.Create(&org).Error; err != nil {
		t.Fatalf("create org: %v", err)
	}
	ids := make([]string, 0, members)
	for i := 0; i < members; i++ {
		uid := uuid.NewString()
		role := models.RoleMember
		if i == 0 {
			role = models.RoleOwner
		}
		if err := db.Create(&models.OrgMember{
			OrganizationID: org.ID.String(), UserID: uid, Role: role,
		}).Error; err != nil {
			t.Fatalf("create member: %v", err)
		}
		ids = append(ids, uid)
	}
	if err := credits.GrantPeriodTo(db, org.ID.String(),
		credits.GrantTeamPerSeat*int64(seats), models.ReasonMonthlyGrant,
		"test:"+org.ID.String()); err != nil {
		t.Fatalf("grant: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM credit_ledger WHERE organization_id = ?`, org.ID)
		db.Exec(`DELETE FROM credit_balances WHERE organization_id = ?`, org.ID)
		db.Unscoped().Where("organization_id = ?", org.ID.String()).Delete(&models.OrgMember{})
		db.Unscoped().Delete(&models.Organization{}, "id = ?", org.ID)
	})
	return &org, ids
}

func spend(t *testing.T, db *gorm.DB, orgID, userID string, amount int64) {
	t.Helper()
	if err := credits.Record(db, credits.Spend{
		OrgID: orgID, UserID: userID, Amount: amount, Reason: models.ReasonLLMUsage,
	}); err != nil {
		t.Fatalf("spend: %v", err)
	}
}

func TestOnePersonCannotDrainTheOrganisation(t *testing.T) {
	db := allocDB(t)
	g := New(db)
	org, ids := teamOrg(t, db, 4, 4) // 320,000 total, 80,000 each

	// Alice burns her whole share. The org still has 240,000 — which is precisely
	// the credit her three colleagues are going to need.
	spend(t, db, org.ID.String(), ids[0], credits.GrantTeamPerSeat)

	err := g.CheckMemberAllowance(org, ids[0])
	if !errors.Is(err, ErrMemberCapReached) {
		t.Fatalf("expected the member to be capped, got %v", err)
	}
	bal, _ := credits.Balance(db, org.ID.String())
	if credits.Spendable(bal) <= 0 {
		t.Fatal("this test is meaningless unless the org still has credit")
	}
	// And her colleague is unaffected.
	if err := g.CheckMemberAllowance(org, ids[1]); err != nil {
		t.Fatalf("a colleague was blocked by someone else's spending: %v", err)
	}
}

func TestTheOrgRunningOutStopsEveryone(t *testing.T) {
	db := allocDB(t)
	g := New(db)
	org, ids := teamOrg(t, db, 2, 2)

	// Both members well inside their caps, but the org balance is gone — a
	// correction, a refund reversal, anything. The org-level check has to still
	// apply, which is why the two are separate gates.
	if err := credits.Record(db, credits.Spend{
		OrgID: org.ID.String(), Amount: credits.GrantTeamPerSeat * 2,
		Reason: models.ReasonAdjustment,
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := g.CheckMemberAllowance(org, ids[0]); err != nil {
		t.Fatalf("member cap should still be clear: %v", err)
	}
	if _, err := g.CheckBalance(org.ID.String(), ids[0]); !errors.Is(err, ErrOverCap) {
		t.Fatalf("expected the org balance check to refuse, got %v", err)
	}
}

func TestSharesSplitBySeatsNotByHeadcount(t *testing.T) {
	db := allocDB(t)
	// 6 seats but only 2 people. Splitting by headcount would give each of them
	// 240,000 — three seats' worth — and adding a third teammate would then silently
	// CUT everyone's allowance, which is the opposite of what buying a seat does.
	org, ids := teamOrg(t, db, 6, 2)
	g := New(db)
	a, err := g.AllocationFor(org, ids[0])
	if err != nil {
		t.Fatalf("allocation: %v", err)
	}
	if a.Limit != credits.GrantTeamPerSeat {
		t.Fatalf("limit = %d, want one seat's worth (%d)", a.Limit, credits.GrantTeamPerSeat)
	}
}

func TestAnAdminOverrideBeatsTheEqualShare(t *testing.T) {
	db := allocDB(t)
	g := New(db)
	org, ids := teamOrg(t, db, 4, 4)

	if err := g.SetMemberLimit(org.ID.String(), ids[1], 200_000); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	a, err := g.AllocationFor(org, ids[1])
	if err != nil {
		t.Fatalf("allocation: %v", err)
	}
	if a.Limit != 200_000 || !a.Custom {
		t.Fatalf("limit = %d custom = %v, want 200000 and custom", a.Limit, a.Custom)
	}
	// They can now spend past the equal share.
	spend(t, db, org.ID.String(), ids[1], 150_000)
	if err := g.CheckMemberAllowance(org, ids[1]); err != nil {
		t.Fatalf("blocked below their raised limit: %v", err)
	}

	// Zero restores the split rather than meaning "unlimited" — so a later seat
	// change re-divides automatically instead of freezing at today's number.
	if err := g.SetMemberLimit(org.ID.String(), ids[1], 0); err != nil {
		t.Fatalf("reset: %v", err)
	}
	a, _ = g.AllocationFor(org, ids[1])
	if a.Limit != credits.GrantTeamPerSeat || a.Custom {
		t.Fatalf("limit = %d custom = %v after reset, want the equal share", a.Limit, a.Custom)
	}
}

func TestSeatChangesRedivideTheDefaultShare(t *testing.T) {
	db := allocDB(t)
	g := New(db)
	org, ids := teamOrg(t, db, 2, 2)
	before, _ := g.AllocationFor(org, ids[0])

	// Buying seats should give everyone MORE, not spread the same pot thinner.
	org.Seats = 6
	if err := db.Model(org).Update("seats", 6).Error; err != nil {
		t.Fatalf("update seats: %v", err)
	}
	after, _ := g.AllocationFor(org, ids[0])
	if after.Limit != before.Limit {
		t.Fatalf("per-seat share changed from %d to %d when seats grew — it should be "+
			"constant per seat", before.Limit, after.Limit)
	}
	// The org total is what grows.
	if LimitsForOrg(org).MonthlyCredits <= credits.GrantTeamPerSeat*2 {
		t.Fatal("the org allowance did not grow with seats")
	}
}

func TestAPersonalOrgHasNoCap(t *testing.T) {
	db := allocDB(t)
	g := New(db)
	// A one-person org has nobody to protect its allowance from, so capping it
	// against itself would just be an extra way to fail.
	org := models.Organization{
		Name: "Solo", Slug: "solo-" + uuid.NewString()[:8],
		Plan: models.PlanPro, PlanStatus: "active", Personal: true, Seats: 1,
	}
	org.ID = uuid.New()
	if err := db.Create(&org).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	uid := uuid.NewString()
	db.Create(&models.OrgMember{OrganizationID: org.ID.String(), UserID: uid, Role: models.RoleOwner})
	t.Cleanup(func() {
		db.Unscoped().Where("organization_id = ?", org.ID.String()).Delete(&models.OrgMember{})
		db.Exec(`DELETE FROM credit_balances WHERE organization_id = ?`, org.ID)
		db.Unscoped().Delete(&models.Organization{}, "id = ?", org.ID)
	})

	a, err := g.AllocationFor(&org, uid)
	if err != nil {
		t.Fatalf("allocation: %v", err)
	}
	if a.Limit != 0 || a.Exhausted() {
		t.Fatalf("a solo org got a cap of %d", a.Limit)
	}
	if err := g.CheckMemberAllowance(&org, uid); err != nil {
		t.Fatalf("a solo org was capped: %v", err)
	}
}

func TestSpendIsCountedPerPersonNotPerOrg(t *testing.T) {
	db := allocDB(t)
	g := New(db)
	org, ids := teamOrg(t, db, 3, 3)

	spend(t, db, org.ID.String(), ids[0], 10_000)
	spend(t, db, org.ID.String(), ids[1], 25_000)
	// Unattributed spend — an older row, or work by someone since removed. It must
	// not land on anybody's personal total.
	spend(t, db, org.ID.String(), "", 5_000)

	a0, _ := g.AllocationFor(org, ids[0])
	a1, _ := g.AllocationFor(org, ids[1])
	a2, _ := g.AllocationFor(org, ids[2])
	if a0.Spent != 10_000 || a1.Spent != 25_000 || a2.Spent != 0 {
		t.Fatalf("per-person spend wrong: %d / %d / %d", a0.Spent, a1.Spent, a2.Spent)
	}

	// But the org total still includes everything, or the books do not balance.
	bal, _ := credits.Balance(db, org.ID.String())
	total, err := credits.SpentSince(db, org.ID.String(), credits.PeriodStart(bal))
	if err != nil {
		t.Fatalf("total: %v", err)
	}
	if total != 40_000 {
		t.Fatalf("org total = %d, want 40000 including the unattributed 5000", total)
	}
}

func TestGrantsAreNotCountedAgainstAnybodysShare(t *testing.T) {
	db := allocDB(t)
	g := New(db)
	org, ids := teamOrg(t, db, 2, 2)
	// The monthly allowance is a credit to the ORG. If it were attributed to a
	// person they would appear to have spent it and be instantly capped.
	a, _ := g.AllocationFor(org, ids[0])
	if a.Spent != 0 {
		t.Fatalf("a member starts the period having 'spent' %d", a.Spent)
	}
	_ = time.Now
}
