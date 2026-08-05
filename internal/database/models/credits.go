package models

import "time"

// Credits: a ledger, not a counter.
//
// CreditLedger is append-only and is the source of truth. CreditBalance is a
// derived cache of sum(delta) maintained in the SAME transaction as the ledger
// insert, because summing millions of rows on every spend check is not viable.
// A scheduled job reconciles the cache against the ledger.
//
// Nothing here bills from the OTel metric. That metric is sampled and aggregated
// by design; treating it as money is how billing systems end up simultaneously
// wrong and unauditable.

// LedgerReason says why credits moved. Kept as an explicit vocabulary because a
// free-text reason column becomes unqueryable within a month.
type LedgerReason string

const (
	// Grants (positive delta).
	ReasonSignupGrant  LedgerReason = "signup_grant"
	ReasonMonthlyGrant LedgerReason = "monthly_grant"
	ReasonTopup        LedgerReason = "topup"
	ReasonRefund       LedgerReason = "refund"
	ReasonAdjustment   LedgerReason = "adjustment"

	// Spends (negative delta).
	ReasonLLMUsage    LedgerReason = "llm_usage"
	ReasonIntegration LedgerReason = "integration_op"
	ReasonEmail       LedgerReason = "email_send"
	ReasonWebTool     LedgerReason = "web_tool"
)

// CreditLedger is one immutable movement of credits. Never updated, never
// deleted — a correction is a new compensating row, so history stays auditable.
type CreditLedger struct {
	BaseModel
	OrganizationID string `json:"organization_id" gorm:"type:uuid;not null;index:idx_ledger_org_time,priority:1;index:idx_ledger_org_user,priority:1"`
	// Delta is signed: positive grants, negative spends. Credits are integers
	// (millicredits, see credits.PerThousandTokens) so no float ever touches a
	// balance.
	Delta  int64        `json:"delta"  gorm:"not null"`
	Reason LedgerReason `json:"reason" gorm:"type:varchar(32);not null;index"`

	// Provenance — what was actually spent on. All optional, since a grant has none.
	//
	// RunID is a pointer because the column is a real uuid and a grant genuinely
	// has no run: an empty string is not a valid uuid, so the alternatives are NULL
	// or giving up type checking on the column. Set it through credits.Record,
	// which does the conversion in one place.
	// UserID is whose allocation this spend came out of.
	//
	// For interactive work it is the signed-in person. For an unattended run —
	// scheduled or webhook — it is the workflow's OWNER, because they are who set it
	// running; attributing it to nobody would make a team's biggest cost invisible in
	// the per-person view.
	//
	// Nullable: grants belong to the org, not to a person.
	UserID *string `json:"user_id,omitempty"   gorm:"type:uuid;index:idx_ledger_org_user,priority:2"`
	RunID  *string `json:"run_id,omitempty"    gorm:"type:uuid;index"`
	// WorkflowID and WorkflowName are DENORMALIZED rather than joined through
	// run_id. An audit row has to stay readable on its own: run history is pruned
	// on a per-plan retention window, so a join would silently lose the attribution
	// on exactly the older rows an audit is most likely to ask about. The name is
	// stored as it was at the time for the same reason — a workflow renamed later
	// should not rewrite what an old charge says it was for.
	WorkflowID   *string `json:"workflow_id,omitempty" gorm:"type:uuid;index"`
	WorkflowName string  `json:"workflow_name,omitempty"`
	NodeID       string  `json:"node_id,omitempty"`
	// NodeLabel is the user's own name for the step, which is what makes a usage
	// line legible without opening the workflow.
	NodeLabel string `json:"node_label,omitempty"`
	Op        string `json:"op,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`

	// Raw token counts, kept alongside the credit delta so a pricing change can
	// be replayed against history and so COGS can be computed per surface. Cached
	// input is separate because it bills at a different rate.
	InputTokens      int    `json:"input_tokens,omitempty"`
	OutputTokens     int    `json:"output_tokens,omitempty"`
	CachedTokens     int    `json:"cached_tokens,omitempty"`
	CacheWriteTokens int    `json:"cache_write_tokens,omitempty"`
	Surface          string `json:"surface,omitempty" gorm:"type:varchar(16)"`

	// ExternalRef ties a row to the thing outside our system that caused it — a
	// Stripe event or invoice id. Unique when present, so a webhook delivered
	// twice cannot grant twice.
	ExternalRef *string `json:"external_ref,omitempty" gorm:"uniqueIndex"`
}

// TableName pins the name from the doc's schema rather than GORM's pluralisation
// of the Go type.
func (CreditLedger) TableName() string { return "credit_ledger" }

// CreditBalance is the authoritative value for reads, derived from the ledger.
// One row per org, guarded by SELECT … FOR UPDATE on every mutation — overlapping
// scheduled runs race on this exactly as they race on a datastore counter.
type CreditBalance struct {
	OrganizationID string `json:"organization_id" gorm:"type:uuid;primaryKey"`
	// Balance may legitimately go slightly negative: an LLM call's true cost is
	// unknowable until it returns, so the last call of a nearly-empty account can
	// overshoot. Bounded by the MaxTokens ceiling rather than by the check.
	Balance int64 `json:"balance" gorm:"not null;default:0"`
	// Reserved is the sum of active holds. Spendable is Balance - Reserved.
	Reserved int64 `json:"reserved" gorm:"not null;default:0"`
	// LifetimeSpent is monotonic and never reset by a grant, so usage reporting
	// does not have to sum the ledger.
	LifetimeSpent int64     `json:"lifetime_spent" gorm:"not null;default:0"`
	UpdatedAt     time.Time `json:"updated_at"`
	// LastGrantAt gates the monthly refill: a grant is idempotent per period, so a
	// webhook retry or a double-fired cron cannot double-credit.
	LastGrantAt *time.Time `json:"last_grant_at"`
}

func (CreditBalance) TableName() string { return "credit_balances" }

// HoldStatus tracks a reservation's fate. A hold is never deleted; it is settled
// or released, so a leaked reservation is visible instead of invisible.
type HoldStatus string

const (
	HoldActive   HoldStatus = "active"
	HoldSettled  HoldStatus = "settled"
	HoldReleased HoldStatus = "released"
	// HoldExpired marks a hold reaped by the sweeper because its run died without
	// releasing — a crashed process, or a container killed mid-run.
	HoldExpired HoldStatus = "expired"
)

// CreditHold is a headroom reservation taken at run start and released at run end.
//
// For LLM ops it is explicitly NOT an exact reservation: the true cost is unknown
// until the call returns. It is a check that the org could afford a plausible
// worst case, bounded by the MaxTokens ceiling.
type CreditHold struct {
	BaseModel
	OrganizationID string     `json:"organization_id" gorm:"type:uuid;not null;index:idx_hold_org_status,priority:1"`
	RunID          string     `json:"run_id"          gorm:"type:uuid;not null;index"`
	Amount         int64      `json:"amount"          gorm:"not null"`
	Status         HoldStatus `json:"status"          gorm:"type:varchar(16);not null;default:'active';index:idx_hold_org_status,priority:2"`
	// Spent accumulates settled cost against this hold, so the release at run end
	// returns the remainder rather than the whole amount.
	Spent int64 `json:"spent" gorm:"not null;default:0"`
	// ExpiresAt lets the sweeper reclaim reservations whose run never finished.
	// Without it a crash strands credits until someone notices.
	ExpiresAt time.Time `json:"expires_at" gorm:"not null;index"`
}

func (CreditHold) TableName() string { return "credit_holds" }
