package credits

import (
	"sync"
	"testing"

	"gorm.io/gorm"

	"workflow-ai/server/internal/database/models"
)

// The first two writes to an org that has no balance row yet.
//
// Every other concurrency test here starts from a granted balance, so they all
// skip the branch that CREATES that row — and that branch was the broken one. Two
// operations arriving together for a brand-new org both found no balance and both
// inserted it; one lost on the primary key, and because a failed statement aborts
// the surrounding transaction in Postgres, the recovery the code documented could
// never run. The caller got a raw "duplicate key" instead.
//
// Real shapes of this: two schedules whose first tick lands in the same second, or
// a Stripe webhook granting the signup allowance while the user's first run is
// already reserving credit.

// concurrently runs fn n times at once and returns the first error.
func concurrently(n int, fn func() error) error {
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	return <-errs
}

func ledgerTotal(t *testing.T, db *gorm.DB, org string) int64 {
	t.Helper()
	var total int64
	db.Model(&models.CreditLedger{}).Where("organization_id = ?", org).
		Select("COALESCE(SUM(delta),0)").Scan(&total)
	return total
}

func TestConcurrentFirstGrantsDoNotCollideOnTheBalanceRow(t *testing.T) {
	db := dbForTest(t)
	// Repeated because the loser only exists when the two inserts genuinely overlap;
	// a single attempt passes by luck often enough to be worthless as a regression.
	for attempt := 0; attempt < 20; attempt++ {
		org := newOrg(t, db)
		err := concurrently(2, func() error {
			return GrantPeriodTo(db, org, 1000, models.ReasonMonthlyGrant, "race:"+org)
		})
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		// Idempotence still holds: the period was topped up to 1000, not to 2000. A
		// fix that merely swallowed the conflict could double-credit here.
		if got := ledgerTotal(t, db, org); got != 1000 {
			t.Fatalf("attempt %d: ledger totals %d, want 1000 — the grant was applied twice", attempt, got)
		}
		bal, err := Balance(db, org)
		if err != nil {
			t.Fatalf("balance: %v", err)
		}
		if bal.Balance != 1000 {
			t.Fatalf("attempt %d: balance %d, want 1000 — the cache disagrees with the ledger",
				attempt, bal.Balance)
		}
	}
}

func TestConcurrentGrantsOfTheSameRefApplyOnce(t *testing.T) {
	db := dbForTest(t)
	// Grant guards with a count, which two redeliveries can both read as zero. The
	// unique index is the only thing that can actually settle it, so the loser must
	// come back clean rather than aborting its transaction.
	for attempt := 0; attempt < 20; attempt++ {
		org := newOrg(t, db)
		err := concurrently(3, func() error {
			return Grant(db, org, 500, models.ReasonSignupGrant, "webhook:"+org)
		})
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if got := ledgerTotal(t, db, org); got != 500 {
			t.Fatalf("attempt %d: ledger totals %d, want 500 — a redelivery granted twice", attempt, got)
		}
	}
}
