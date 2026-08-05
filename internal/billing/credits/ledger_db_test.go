package credits

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"workflow-ai/server/internal/database/models"
)

// The ledger against a real Postgres. Opt-in via TEST_DATABASE_URL:
//
//	TEST_DATABASE_URL="host=localhost user=postgres password=postgres dbname=workflow_ai port=5434 sslmode=disable"
//
// These tests are about the properties that cannot be checked by reading the
// code: that concurrent spenders do not lose updates, that reservations cannot be
// double-committed, and that the cached balance always equals sum(delta). A
// billing system that is wrong under concurrency is wrong in the only case that
// matters, because that is exactly when money is moving.

func dbForTest(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run ledger tests")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.CreditLedger{}, &models.CreditBalance{}, &models.CreditHold{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// newOrg returns a fresh org id with cleanup registered.
func newOrg(t *testing.T, db *gorm.DB) string {
	t.Helper()
	org := uuid.NewString()
	t.Cleanup(func() {
		db.Exec(`DELETE FROM credit_ledger WHERE organization_id = ?`, org)
		db.Exec(`DELETE FROM credit_holds WHERE organization_id = ?`, org)
		db.Exec(`DELETE FROM credit_balances WHERE organization_id = ?`, org)
	})
	return org
}

func mustBalance(t *testing.T, db *gorm.DB, org string) models.CreditBalance {
	t.Helper()
	bal, err := Balance(db, org)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return bal
}

// assertDerivable is the invariant that makes the cache trustworthy.
func assertDerivable(t *testing.T, db *gorm.DB, org string) {
	t.Helper()
	var sum int64
	db.Raw(`SELECT COALESCE(sum(delta),0) FROM credit_ledger WHERE organization_id = ?`, org).Scan(&sum)
	bal := mustBalance(t, db, org)
	if bal.Balance != sum {
		t.Fatalf("cached balance %d does not equal ledger sum %d — the cache is lying",
			bal.Balance, sum)
	}
}

func TestGrantAndSpendKeepTheBalanceDerivable(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	if err := Grant(db, org, 1000, models.ReasonSignupGrant, "test:"+org); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := Record(db, Spend{OrgID: org, Amount: 250, Reason: models.ReasonLLMUsage,
		Provider: "anthropic", Model: "claude-haiku-4-5", InputTokens: 1000, OutputTokens: 200}); err != nil {
		t.Fatalf("record: %v", err)
	}

	bal := mustBalance(t, db, org)
	if bal.Balance != 750 {
		t.Fatalf("balance = %d, want 750", bal.Balance)
	}
	if bal.LifetimeSpent != 250 {
		t.Fatalf("lifetime spent = %d, want 250", bal.LifetimeSpent)
	}
	assertDerivable(t, db, org)
}

func TestGrantIsIdempotentPerExternalRef(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	// Stripe redelivers webhooks as a matter of course. Without the reference check
	// a retry grants twice, which is a free money bug.
	ref := "evt_" + uuid.NewString()
	for i := 0; i < 4; i++ {
		if err := Grant(db, org, 500, models.ReasonMonthlyGrant, ref); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	if bal := mustBalance(t, db, org); bal.Balance != 500 {
		t.Fatalf("balance = %d after 4 deliveries of one event, want 500", bal.Balance)
	}
	assertDerivable(t, db, org)
}

func TestConcurrentSpendsDoNotLoseUpdates(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	if err := Grant(db, org, 10_000, models.ReasonSignupGrant, "test:"+org); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Overlapping scheduled runs hit the same balance row simultaneously. Without
	// SELECT … FOR UPDATE this is a classic lost update: each transaction reads the
	// same starting balance and the last write wins, so most of the spend vanishes.
	const workers, each = 8, 10
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := Record(db, Spend{OrgID: org, Amount: 10, Reason: models.ReasonIntegration}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent spend: %v", err)
	}

	want := int64(10_000 - workers*each*10)
	if bal := mustBalance(t, db, org); bal.Balance != want {
		t.Fatalf("balance = %d, want %d — %d credits were lost to a race",
			bal.Balance, want, want-bal.Balance)
	}
	assertDerivable(t, db, org)
}

func TestReserveRefusesWhatTheOrgCannotAfford(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	if err := Grant(db, org, 100, models.ReasonSignupGrant, "test:"+org); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := Reserve(db, org, uuid.NewString(), 500, time.Minute); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("expected ErrInsufficient, got %v", err)
	}
	// A refused reservation must not have moved anything.
	if bal := mustBalance(t, db, org); bal.Reserved != 0 || bal.Balance != 100 {
		t.Fatalf("failed reserve left state behind: %+v", bal)
	}
}

func TestConcurrentReservesCannotOvercommitTheBalance(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	// Room for exactly three reservations of 300. Ten runs start at once; only
	// three may win, or we have promised work we cannot pay for.
	if err := Grant(db, org, 900, models.ReasonSignupGrant, "test:"+org); err != nil {
		t.Fatalf("grant: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := Reserve(db, org, uuid.NewString(), 300, time.Minute)
			if err == nil && h != nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted != 3 {
		t.Fatalf("%d reservations granted, want 3 — the balance was overcommitted", granted)
	}
	bal := mustBalance(t, db, org)
	if bal.Reserved != 900 || Spendable(bal) != 0 {
		t.Fatalf("reserved = %d, spendable = %d, want 900 and 0", bal.Reserved, Spendable(bal))
	}
}

func TestReserveSettleReleaseReturnsOnlyTheUnusedRemainder(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	if err := Grant(db, org, 1000, models.ReasonSignupGrant, "test:"+org); err != nil {
		t.Fatalf("grant: %v", err)
	}
	runID := uuid.NewString()
	hold, err := Reserve(db, org, runID, 400, time.Minute)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if bal := mustBalance(t, db, org); Spendable(bal) != 600 {
		t.Fatalf("spendable = %d after reserving 400 of 1000, want 600", Spendable(bal))
	}

	// The run actually spends 120 of its 400.
	if err := Record(db, Spend{OrgID: org, Amount: 120, Reason: models.ReasonLLMUsage,
		RunID: runID, HoldID: hold.ID.String()}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := Release(db, hold.ID.String()); err != nil {
		t.Fatalf("release: %v", err)
	}

	bal := mustBalance(t, db, org)
	if bal.Balance != 880 {
		t.Fatalf("balance = %d, want 880 (only the 120 actually spent)", bal.Balance)
	}
	if bal.Reserved != 0 {
		t.Fatalf("reserved = %d after release, want 0 — credits are stranded", bal.Reserved)
	}
	assertDerivable(t, db, org)
}

func TestReleasingTwiceDoesNotCreditTwice(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	if err := Grant(db, org, 1000, models.ReasonSignupGrant, "test:"+org); err != nil {
		t.Fatalf("grant: %v", err)
	}
	hold, err := Reserve(db, org, uuid.NewString(), 300, time.Minute)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// A run can plausibly release on both its success path and a deferred cleanup.
	for i := 0; i < 3; i++ {
		if err := Release(db, hold.ID.String()); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}
	bal := mustBalance(t, db, org)
	if bal.Reserved != 0 || bal.Balance != 1000 {
		t.Fatalf("double release corrupted state: %+v", bal)
	}
}

func TestSpendingPastTheHoldDoesNotInflateSpendableCredit(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	if err := Grant(db, org, 1000, models.ReasonSignupGrant, "test:"+org); err != nil {
		t.Fatalf("grant: %v", err)
	}
	runID := uuid.NewString()
	hold, err := Reserve(db, org, runID, 100, time.Minute)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// A hold is a headroom check, not a cap: an LLM call's true cost is unknown
	// until it returns, so a run CAN exceed its reservation. What must not happen
	// is the release crediting back more reservation than was ever taken.
	if err := Record(db, Spend{OrgID: org, Amount: 250, Reason: models.ReasonLLMUsage,
		RunID: runID, HoldID: hold.ID.String()}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := Release(db, hold.ID.String()); err != nil {
		t.Fatalf("release: %v", err)
	}

	bal := mustBalance(t, db, org)
	if bal.Balance != 750 {
		t.Fatalf("balance = %d, want 750", bal.Balance)
	}
	if bal.Reserved != 0 {
		t.Fatalf("reserved = %d, want 0", bal.Reserved)
	}
	if Spendable(bal) != 750 {
		t.Fatalf("spendable = %d, want 750 — overspend leaked into the reservation",
			Spendable(bal))
	}
	assertDerivable(t, db, org)
}

func TestSweepReclaimsHoldsFromCrashedRuns(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	if err := Grant(db, org, 1000, models.ReasonSignupGrant, "test:"+org); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// A run that died without releasing. Without the sweeper, every crash
	// permanently reduces spendable credit and the customer's only symptom is
	// being unable to start runs for no visible reason.
	hold, err := Reserve(db, org, uuid.NewString(), 400, -time.Minute)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if bal := mustBalance(t, db, org); Spendable(bal) != 600 {
		t.Fatalf("spendable = %d before sweep, want 600", Spendable(bal))
	}

	if _, err := SweepExpiredHolds(db); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if bal := mustBalance(t, db, org); Spendable(bal) != 1000 {
		t.Fatalf("spendable = %d after sweep, want 1000", Spendable(bal))
	}

	var status string
	db.Raw(`SELECT status FROM credit_holds WHERE id = ?`, hold.ID).Scan(&status)
	if status != string(models.HoldExpired) {
		t.Fatalf("hold status = %q, want expired — a swept hold must stay visible", status)
	}
}

func TestReconcileReportsDriftRatherThanHidingIt(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	if err := Grant(db, org, 500, models.ReasonSignupGrant, "test:"+org); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Corrupt the cache behind the ledger's back, the way a buggy write path would.
	if err := db.Exec(
		`UPDATE credit_balances SET balance = 999 WHERE organization_id = ?`, org).Error; err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	drifts, err := Reconcile(db)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	found := false
	for _, d := range drifts {
		if d.OrgID == org {
			found = true
			if d.Cached != 999 || d.FromSum != 500 {
				t.Fatalf("drift misreported: %+v", d)
			}
		}
	}
	if !found {
		t.Fatal("reconcile did not notice a balance that disagrees with its ledger")
	}
}

func TestUsageComesFromTheLedgerNotTheBalance(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	// The exact shape that produced the bug: credit carried over, so the balance
	// ends up ABOVE one period's allowance even though real spending happened.
	if err := Grant(db, org, 25_000, models.ReasonSignupGrant, "signup:"+org); err != nil {
		t.Fatalf("signup grant: %v", err)
	}
	if err := Grant(db, org, 90_000, models.ReasonMonthlyGrant, "period1:"+org); err != nil {
		t.Fatalf("monthly grant: %v", err)
	}
	if err := Record(db, Spend{OrgID: org, Amount: 11_556, Reason: models.ReasonLLMUsage,
		Provider: "openai", Model: "gpt-5.5"}); err != nil {
		t.Fatalf("spend: %v", err)
	}

	bal, _ := Balance(db, org)
	const allowance = 90_000
	if bal.Balance <= allowance {
		t.Fatalf("balance %d should exceed one allowance for this test to mean anything", bal.Balance)
	}
	// The old formula, kept here to show what it produced.
	oldFormula := (allowance - Spendable(bal)) * 100 / allowance
	if oldFormula > 0 {
		t.Fatalf("expected the balance-based formula to go non-positive, got %d", oldFormula)
	}

	spent, err := SpentSince(db, org, time.Time{})
	if err != nil {
		t.Fatalf("spent: %v", err)
	}
	if spent != 11_556 {
		t.Fatalf("usage = %d, want 11556 — the ledger is the only honest source", spent)
	}
}

func TestGrantsAndCorrectionsAreNotCountedAsUsage(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)

	if err := Grant(db, org, 100_000, models.ReasonMonthlyGrant, "g:"+org); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := Record(db, Spend{OrgID: org, Amount: 500, Reason: models.ReasonLLMUsage}); err != nil {
		t.Fatalf("llm: %v", err)
	}
	if err := Record(db, Spend{OrgID: org, Amount: 40, Reason: models.ReasonIntegration}); err != nil {
		t.Fatalf("integration: %v", err)
	}
	// A compensating correction is NEGATIVE but is not the customer's consumption.
	// Counting it would report a fix to our own bug as their usage — which is
	// exactly what a naive "sum every negative delta" would do.
	if err := Record(db, Spend{OrgID: org, Amount: 90_000, Reason: models.ReasonAdjustment}); err != nil {
		t.Fatalf("adjustment: %v", err)
	}

	spent, err := SpentSince(db, org, time.Time{})
	if err != nil {
		t.Fatalf("spent: %v", err)
	}
	if spent != 540 {
		t.Fatalf("usage = %d, want 540 (500 llm + 40 integration, excluding the correction)", spent)
	}
}

func TestUsageIsScopedToThePeriod(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)
	if err := Grant(db, org, 90_000, models.ReasonMonthlyGrant, "g:"+org); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := Record(db, Spend{OrgID: org, Amount: 7_000, Reason: models.ReasonLLMUsage}); err != nil {
		t.Fatalf("spend: %v", err)
	}
	// Backdate that spend to before the period started — last month's consumption
	// must not appear in this month's figure.
	db.Exec(`UPDATE credit_ledger SET created_at = now() - interval '40 days'
		WHERE organization_id = ? AND reason = ?`, org, models.ReasonLLMUsage)

	bal, _ := Balance(db, org)
	spent, err := SpentSince(db, org, PeriodStart(bal))
	if err != nil {
		t.Fatalf("spent: %v", err)
	}
	if spent != 0 {
		t.Fatalf("usage = %d, want 0 — spend from a previous period leaked in", spent)
	}
	// Whereas an all-time query still sees it.
	if all, _ := SpentSince(db, org, time.Time{}); all != 7_000 {
		t.Fatalf("all-time usage = %d, want 7000", all)
	}
}

func TestSeatIncreasesTopUpRatherThanMintCredit(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)
	const perSeat = 80_000
	ref := "sub:sub_test:period:1788594498"

	// The exact path that minted credit: a period granted at 2 seats, then raised to
	// 5. Granting afresh each time gave 160,000 + 400,000 for a period worth 400,000,
	// and stepping 2→3→4→5 would have produced 1,120,000.
	for _, seats := range []int64{2, 3, 4, 5} {
		if err := GrantPeriodTo(db, org, perSeat*seats, models.ReasonMonthlyGrant, ref); err != nil {
			t.Fatalf("grant to %d seats: %v", seats, err)
		}
	}
	bal := mustBalance(t, db, org)
	if bal.Balance != perSeat*5 {
		t.Fatalf("balance = %d after stepping 2→3→4→5 seats, want %d (5 seats' worth)",
			bal.Balance, perSeat*5)
	}
	assertDerivable(t, db, org)
}

func TestReachingTheSameTargetTwiceGrantsNothingExtra(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)
	ref := "sub:sub_test:period:1788594498"
	for i := 0; i < 5; i++ {
		if err := GrantPeriodTo(db, org, 240_000, models.ReasonMonthlyGrant, ref); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	// Stripe redelivers webhooks routinely; each redelivery must be a no-op.
	if bal := mustBalance(t, db, org); bal.Balance != 240_000 {
		t.Fatalf("balance = %d after 5 deliveries of one period, want 240000", bal.Balance)
	}
}

func TestSeatReductionDoesNotClawBackCredit(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)
	ref := "sub:sub_test:period:1788594498"
	if err := GrantPeriodTo(db, org, 400_000, models.ReasonMonthlyGrant, ref); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Dropping to 2 seats. They paid for five and the reduction only takes effect at
	// the period boundary, so taking the credit back now would be charging them for
	// something and then removing it.
	if err := GrantPeriodTo(db, org, 160_000, models.ReasonMonthlyGrant, ref); err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if bal := mustBalance(t, db, org); bal.Balance != 400_000 {
		t.Fatalf("balance = %d after a mid-period reduction, want 400000 kept", bal.Balance)
	}
}

func TestEachPeriodGetsItsOwnAllowance(t *testing.T) {
	db := dbForTest(t)
	org := newOrg(t, db)
	// Consecutive periods must NOT be treated as one target, or the second month
	// would grant nothing.
	if err := GrantPeriodTo(db, org, 240_000, models.ReasonMonthlyGrant,
		"sub:sub_test:period:1788594498"); err != nil {
		t.Fatalf("period 1: %v", err)
	}
	if err := GrantPeriodTo(db, org, 240_000, models.ReasonMonthlyGrant,
		"sub:sub_test:period:1791186498"); err != nil {
		t.Fatalf("period 2: %v", err)
	}
	if bal := mustBalance(t, db, org); bal.Balance != 480_000 {
		t.Fatalf("balance = %d after two periods, want 480000", bal.Balance)
	}
}
