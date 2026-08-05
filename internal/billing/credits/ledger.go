package credits

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"workflow-ai/server/internal/database/models"
)

// Ledger writes.
//
// Two invariants hold everywhere in this file:
//
//  1. The ledger row and the balance update happen in ONE transaction, with the
//     balance row locked FOR UPDATE. Overlapping scheduled runs race on the
//     balance exactly as they race on a datastore counter, and this is the same
//     fix used there.
//  2. The balance is always derivable from sum(delta). It is a cache, kept only
//     because summing millions of rows per spend check is not viable. Reconcile
//     verifies that; if it ever disagrees, the ledger is right.

// ErrInsufficient is returned when an org cannot afford a spend. Callers surface
// this to the user as a hard stop, never as a silent overage: for SMB customers
// stop-and-notify beats bill-and-apologise, because overage billing on an
// unattended product generates chargebacks.
var ErrInsufficient = errors.New("not enough credits")

// Spend is one charge to record.
type Spend struct {
	OrgID  string
	Amount int64 // positive; the ledger stores it negated
	Reason models.LedgerReason

	RunID    string
	NodeID   string
	Op       string
	Provider string
	Model    string
	Surface  string

	InputTokens      int
	OutputTokens     int
	CachedTokens     int
	CacheWriteTokens int

	// HoldID settles against a run's reservation instead of only reducing the
	// balance, so the released remainder at run end is correct.
	HoldID string
}

// Balance reads an org's current position, creating a zero row if absent.
func Balance(db *gorm.DB, orgID string) (models.CreditBalance, error) {
	var bal models.CreditBalance
	err := db.Where("organization_id = ?", orgID).First(&bal).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		bal = models.CreditBalance{OrganizationID: orgID}
		if err := db.Create(&bal).Error; err != nil {
			return bal, err
		}
		return bal, nil
	}
	return bal, err
}

// Spendable is what an org may still commit: the balance less anything already
// reserved by in-flight runs. Reserved credits are not spent yet, but treating
// them as available would let concurrent runs each pass a check that only one of
// them can actually afford.
func Spendable(bal models.CreditBalance) int64 {
	return bal.Balance - bal.Reserved
}

// lockBalance loads the balance row for update, creating it if it does not exist.
// Every mutation in this file goes through it — a read-modify-write on a balance
// without the lock is a lost update under concurrency.
func lockBalance(tx *gorm.DB, orgID string) (*models.CreditBalance, error) {
	var bal models.CreditBalance
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ?", orgID).First(&bal).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		bal = models.CreditBalance{OrganizationID: orgID}
		if err := tx.Create(&bal).Error; err != nil {
			return nil, err
		}
		// Re-read under the lock: another transaction may have created it first,
		// in which case ours conflicted and this one is the row that exists.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ?", orgID).First(&bal).Error; err != nil {
			return nil, err
		}
		return &bal, nil
	}
	if err != nil {
		return nil, err
	}
	return &bal, nil
}

// Record posts a spend: one ledger row, one balance decrement, one transaction.
//
// Charging happens AFTER the work succeeded, never optimistically. "You billed me
// for a workflow that errored" is the highest-volume complaint against credit
// systems, and the only way to avoid it is to not take the money until the
// operation actually completed.
//
// Deliberately does NOT fail when the balance would go negative. The true cost of
// an LLM call is unknown until it returns, so the last call of a nearly-empty
// account can overshoot; refusing to record it would mean serving work for free
// and losing the audit row. Overshoot is bounded by MaxTokensCeiling, and the
// next affordability check stops the run.
func Record(db *gorm.DB, s Spend) error {
	if s.Amount <= 0 {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		bal, err := lockBalance(tx, s.OrgID)
		if err != nil {
			return err
		}

		entry := models.CreditLedger{
			OrganizationID:   s.OrgID,
			Delta:            -s.Amount,
			Reason:           s.Reason,
			NodeID:           s.NodeID,
			Op:               s.Op,
			Provider:         s.Provider,
			Model:            s.Model,
			Surface:          s.Surface,
			InputTokens:      s.InputTokens,
			OutputTokens:     s.OutputTokens,
			CachedTokens:     s.CachedTokens,
			CacheWriteTokens: s.CacheWriteTokens,
		}
		if s.RunID != "" {
			entry.RunID = &s.RunID
		}
		if err := tx.Create(&entry).Error; err != nil {
			return err
		}

		bal.Balance -= s.Amount
		bal.LifetimeSpent += s.Amount
		bal.UpdatedAt = time.Now()
		if err := tx.Model(bal).Updates(map[string]any{
			"balance":        bal.Balance,
			"lifetime_spent": bal.LifetimeSpent,
			"updated_at":     bal.UpdatedAt,
		}).Error; err != nil {
			return err
		}

		// Settling against the hold converts reserved headroom into real spend, so
		// the release at run end returns only what was genuinely unused.
		if s.HoldID != "" {
			return settleAgainstHold(tx, bal, s.HoldID, s.Amount)
		}
		return nil
	})
}

// Grant adds credits: a signup or monthly allowance, a top-up, or a correction.
//
// externalRef makes the grant idempotent. Stripe delivers webhooks more than once
// as a matter of course, and without a unique reference a retry grants twice.
func Grant(db *gorm.DB, orgID string, amount int64, reason models.LedgerReason, externalRef string) error {
	if amount <= 0 {
		return fmt.Errorf("grant amount must be positive, got %d", amount)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if externalRef != "" {
			var n int64
			if err := tx.Model(&models.CreditLedger{}).
				Where("external_ref = ?", externalRef).Count(&n).Error; err != nil {
				return err
			}
			if n > 0 {
				return nil // already applied; a redelivered webhook is not an error
			}
		}
		bal, err := lockBalance(tx, orgID)
		if err != nil {
			return err
		}
		entry := models.CreditLedger{
			OrganizationID: orgID,
			Delta:          amount,
			Reason:         reason,
		}
		if externalRef != "" {
			entry.ExternalRef = &externalRef
		}
		if err := tx.Create(&entry).Error; err != nil {
			return err
		}
		now := time.Now()
		updates := map[string]any{
			"balance":    bal.Balance + amount,
			"updated_at": now,
		}
		// Only a periodic allowance moves the grant clock. A top-up must not, or
		// buying credits mid-month would postpone the next monthly refill.
		if reason == models.ReasonMonthlyGrant || reason == models.ReasonSignupGrant {
			updates["last_grant_at"] = now
		}
		return tx.Model(bal).Updates(updates).Error
	})
}

// ── Holds: reserve, settle, release ──────────────────────────────

// Reserve takes a headroom reservation for a run, failing if the org cannot afford
// it. This is the gate that stops a run before it starts rather than partway
// through, which is the difference between a clear error and a half-finished
// workflow that already sent an email.
//
// For LLM work it is explicitly not an estimate — an agent turn can loop several
// rounds and the true cost is unknowable up front. It asks whether a plausible
// worst case is affordable.
func Reserve(db *gorm.DB, orgID, runID string, amount int64, ttl time.Duration) (*models.CreditHold, error) {
	if amount <= 0 {
		return nil, nil
	}
	var hold *models.CreditHold
	err := db.Transaction(func(tx *gorm.DB) error {
		bal, err := lockBalance(tx, orgID)
		if err != nil {
			return err
		}
		if Spendable(*bal) < amount {
			return fmt.Errorf("%w: %d available, %d needed",
				ErrInsufficient, Spendable(*bal), amount)
		}
		h := models.CreditHold{
			OrganizationID: orgID,
			RunID:          runID,
			Amount:         amount,
			Status:         models.HoldActive,
			ExpiresAt:      time.Now().Add(ttl),
		}
		if err := tx.Create(&h).Error; err != nil {
			return err
		}
		if err := tx.Model(bal).Updates(map[string]any{
			"reserved":   bal.Reserved + amount,
			"updated_at": time.Now(),
		}).Error; err != nil {
			return err
		}
		hold = &h
		return nil
	})
	return hold, err
}

// settleAgainstHold records spend against a reservation. Called inside Record's
// transaction, with the balance already locked.
func settleAgainstHold(tx *gorm.DB, bal *models.CreditBalance, holdID string, amount int64) error {
	var h models.CreditHold
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND status = ?", holdID, models.HoldActive).First(&h).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// The hold was already released or swept. The spend itself is still
			// recorded — the money was spent either way — so this is not an error.
			return nil
		}
		return err
	}
	spent := h.Spent + amount
	// A run may legitimately spend past its reservation, since the hold was a
	// headroom check rather than a cap. Clamp the reserved figure at the hold
	// amount so the release cannot credit back more than was ever held.
	release := amount
	if h.Spent >= h.Amount {
		release = 0
	} else if spent > h.Amount {
		release = h.Amount - h.Spent
	}
	if err := tx.Model(&h).Update("spent", spent).Error; err != nil {
		return err
	}
	if release > 0 {
		return tx.Model(bal).Update("reserved", bal.Reserved-release).Error
	}
	return nil
}

// Release ends a hold and returns its unused remainder to the spendable balance.
// Always call it at run end, success or failure: an abandoned reservation strands
// credits the customer paid for.
func Release(db *gorm.DB, holdID string) error {
	if holdID == "" {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var h models.CreditHold
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND status = ?", holdID, models.HoldActive).First(&h).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil // already closed; releasing twice is harmless
			}
			return err
		}
		bal, err := lockBalance(tx, h.OrganizationID)
		if err != nil {
			return err
		}
		// A hold that overspent has no remainder to give back.
		remainder := max(h.Amount-h.Spent, 0)
		if err := tx.Model(&h).Update("status", models.HoldSettled).Error; err != nil {
			return err
		}
		// A negative reservation would permanently inflate spendable credit, so it
		// is floored rather than propagated.
		newReserved := max(bal.Reserved-remainder, 0)
		return tx.Model(bal).Updates(map[string]any{
			"reserved":   newReserved,
			"updated_at": time.Now(),
		}).Error
	})
}

// SweepExpiredHolds reclaims reservations whose run never finished — a crashed
// process, or a container killed mid-run. Without this, every crash permanently
// reduces an org's spendable balance and the only symptom is a customer who
// cannot start runs for no visible reason.
func SweepExpiredHolds(db *gorm.DB) (int, error) {
	var holds []models.CreditHold
	if err := db.Where("status = ? AND expires_at < ?", models.HoldActive, time.Now()).
		Limit(500).Find(&holds).Error; err != nil {
		return 0, err
	}
	swept := 0
	for _, h := range holds {
		err := db.Transaction(func(tx *gorm.DB) error {
			bal, err := lockBalance(tx, h.OrganizationID)
			if err != nil {
				return err
			}
			var cur models.CreditHold
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND status = ?", h.ID, models.HoldActive).First(&cur).Error; err != nil {
				return nil // finished between the scan and the lock
			}
			remainder := max(cur.Amount-cur.Spent, 0)
			if err := tx.Model(&cur).Update("status", models.HoldExpired).Error; err != nil {
				return err
			}
			newReserved := max(bal.Reserved-remainder, 0)
			return tx.Model(bal).Update("reserved", newReserved).Error
		})
		if err != nil {
			return swept, err
		}
		swept++
	}
	return swept, nil
}

// ── Reconciliation ───────────────────────────────────────────────

// Drift is a balance cache that disagrees with the ledger it derives from.
type Drift struct {
	OrgID    string
	Cached   int64
	FromSum  int64
	Reserved int64
}

// Reconcile compares every cached balance against sum(delta) from the ledger.
//
// The cache exists for speed and the ledger is the truth, so any disagreement is
// a bug in a write path, not something to paper over. Reporting rather than
// silently repairing is deliberate: an auto-heal hides the bug and leaves a
// customer's balance quietly wrong in between.
func Reconcile(db *gorm.DB) ([]Drift, error) {
	var rows []Drift
	err := db.Raw(`
		SELECT b.organization_id AS org_id,
		       b.balance         AS cached,
		       COALESCE(l.total, 0) AS from_sum,
		       b.reserved
		FROM credit_balances b
		LEFT JOIN (
			SELECT organization_id, sum(delta) AS total
			FROM credit_ledger GROUP BY organization_id
		) l ON l.organization_id = b.organization_id
		WHERE b.balance <> COALESCE(l.total, 0)`).Scan(&rows).Error
	return rows, err
}
