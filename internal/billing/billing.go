package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/telemetry"
)

// Spend enforcement.
//
// This package is the join between measurement and money: telemetry reports what
// a call used, credits knows what that costs, and this decides whether the work
// may proceed and records what it actually consumed.
//
// The shape follows the executor's existing hook pattern (IntegrationCredsLookup,
// DataStores) — a package-level function assigned once at startup — so no provider
// helper needs to know the ledger exists and the ledger needs no knowledge of the
// executor.

// ErrOverCap is returned when an org has no credit left. Callers must surface it
// as a hard stop rather than continuing and billing later: for this ICP
// stop-and-notify beats bill-and-apologise, and overage on an unattended product
// that spends money while people sleep generates chargebacks, not revenue.
var ErrOverCap = errors.New("credit limit reached")

// Gate decides and records. It holds the database rather than taking it per call
// so the installed hooks close over a single instance.
type Gate struct{ db *gorm.DB }

func New(db *gorm.DB) *Gate { return &Gate{db: db} }

// Install wires the metering hook to the ledger.
//
// Before this runs, telemetry measures usage and nothing bills for it — which is
// the correct behaviour for a deployment with billing switched off, and the reason
// UsageSink is a nil-able hook rather than a hard dependency.
func (g *Gate) Install() {
	telemetry.UsageSink = g.recordUsage
	telemetry.NodeSpendSink = g.recordNodeSpend
	slog.Info("billing: usage sinks installed — LLM tokens and node operations now post to the credit ledger")
}

// recordUsage converts one measured call into a ledger row.
//
// Never returns an error to its caller and never blocks the response: the work is
// already done and the tokens are already spent, so a ledger failure must be
// logged loudly rather than surfaced as a request failure the user cannot act on.
func (g *Gate) recordUsage(ctx context.Context, provider, model string, u telemetry.Usage) {
	b := telemetry.BillingFrom(ctx)
	if b.OrgID == "" {
		// An unattributed metered call means we are paying a provider for work we
		// cannot charge anyone for. Worth a warning rather than a silent drop.
		slog.WarnContext(ctx, "billing: metered LLM call has no paying org",
			"provider", provider, "model", model,
			"surface", telemetry.SurfaceFrom(ctx), "tokens", u.Total())
		return
	}

	cost := credits.ForTokens(model, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	if cost <= 0 {
		return
	}

	spend := credits.Spend{
		OrgID:            b.OrgID,
		UserID:           b.UserID,
		Amount:           cost,
		Reason:           models.ReasonLLMUsage,
		RunID:            b.RunID,
		HoldID:           b.HoldID,
		Provider:         provider,
		Model:            model,
		Surface:          telemetry.SurfaceFrom(ctx),
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CachedTokens:     u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}
	applyCallContext(ctx, &spend)

	if err := credits.Record(g.db, spend); err != nil {
		// The tokens were spent regardless, so this is lost revenue and a real
		// incident — not a user-facing error.
		slog.ErrorContext(ctx, "billing: failed to record LLM spend",
			"error", err, "org_id", b.OrgID, "credits", cost,
			"provider", provider, "model", model)
	}
}

// ── Run admission ────────────────────────────────────────────────

// Reservation is a run's admitted budget, returned by AdmitRun.
type Reservation struct {
	OrgID  string
	UserID string
	Plan   models.Plan
	HoldID string
}

// holdTTL bounds how long a reservation survives an abandoned run. Long enough
// for a slow agent workflow, short enough that a crash does not strand credits
// until someone notices.
const holdTTL = 2 * time.Hour

// AdmitRun decides whether a run may start and reserves headroom for it.
//
// Checked before the run rather than during it on purpose: stopping partway
// through leaves a half-finished workflow that has already sent the email or
// charged the card, which is worse than not starting.
func (g *Gate) AdmitRun(orgID, userID, runID string) (*Reservation, error) {
	if orgID == "" {
		return nil, fmt.Errorf("cannot admit a run with no organization")
	}
	org, err := g.org(orgID)
	if err != nil {
		return nil, err
	}
	plan := EffectivePlan(org)

	// The person whose allowance this run draws on. For a scheduled or webhook run
	// that is the workflow's owner, who is not present to be told — so the refusal
	// lands on the run record instead.
	if err := g.CheckMemberAllowance(org, userID); err != nil {
		return nil, err
	}

	hold, err := credits.Reserve(g.db, orgID, runID, credits.HoldForRun(plan), holdTTL)
	if err != nil {
		if errors.Is(err, credits.ErrInsufficient) {
			bal, _ := credits.Balance(g.db, orgID)
			return nil, &LimitError{
				Kind: "credits",
				Message: fmt.Sprintf("Out of credits — %d left. Runs resume when your "+
					"allowance renews, or upgrade for a larger one.", credits.Spendable(bal)),
				sentinel: ErrOverCap,
			}
		}
		return nil, err
	}
	res := &Reservation{OrgID: orgID, UserID: userID, Plan: plan}
	if hold != nil {
		res.HoldID = hold.ID.String()
	}
	return res, nil
}

// Context tags a run's context with everything the spend path needs, so nothing
// downstream has to look up the org again.
func (r *Reservation) Context(ctx context.Context, runID string) context.Context {
	if r == nil {
		return ctx
	}
	return telemetry.WithBilling(ctx, telemetry.BillingContext{
		OrgID:  r.OrgID,
		UserID: r.UserID,
		Plan:   string(r.Plan),
		HoldID: r.HoldID,
		RunID:  runID,
	})
}

// Finish releases a run's unused reservation. Always call it, on every exit path
// including failure — an abandoned hold strands credits the customer paid for, and
// the sweeper only catches it hours later.
func (g *Gate) Finish(res *Reservation) {
	if res == nil || res.HoldID == "" {
		return
	}
	if err := credits.Release(g.db, res.HoldID); err != nil {
		slog.Error("billing: failed to release run hold",
			"error", err, "hold_id", res.HoldID, "org_id", res.OrgID)
	}
}

// ── Interactive surfaces ─────────────────────────────────────────

// CheckBalance refuses a builder or agent request when the org is out of credit.
//
// These surfaces have no run to reserve against — a single turn is short and its
// cost is settled immediately after — so the gate is a balance check rather than a
// hold. The check is deliberately "any credit left" rather than an estimate: one
// turn cannot overshoot far, and refusing on a guess would block work the customer
// can actually afford.
func (g *Gate) CheckBalance(orgID, userID string) (models.Plan, error) {
	if orgID == "" {
		return models.PlanFree, fmt.Errorf("no organization on this request")
	}
	org, err := g.org(orgID)
	if err != nil {
		return models.PlanFree, err
	}
	// Personal share first: its message names a different fix (ask your owner)
	// from the org running dry (upgrade or wait for renewal).
	if err := g.CheckMemberAllowance(org, userID); err != nil {
		return EffectivePlan(org), err
	}
	bal, err := credits.Balance(g.db, orgID)
	if err != nil {
		return models.PlanFree, err
	}
	if credits.Spendable(bal) <= 0 {
		return EffectivePlan(org), &LimitError{
			Kind: "credits",
			Message: "Your credits are used up for this period. Upgrade for a larger " +
				"allowance, or wait for it to renew.",
			sentinel: ErrOverCap,
		}
	}
	return EffectivePlan(org), nil
}

// BillingContextFor builds the paying identity for a non-run surface.
func BillingContextFor(orgID, userID string, plan models.Plan) telemetry.BillingContext {
	return telemetry.BillingContext{OrgID: orgID, UserID: userID, Plan: string(plan)}
}

// ── Plan resolution ──────────────────────────────────────────────

// EffectivePlan is the plan an org is actually entitled to right now.
//
// Not simply org.Plan, because a subscription can be past due or cancelled. The
// rules, in order:
//
//   - A cancelled subscription keeps its plan until the period ends. Downgrading
//     on the cancel click would take away something already paid for.
//   - A lapsed period falls back to free regardless of what the plan column says,
//     so a failed webhook cannot leave an unpaid org on a paid tier indefinitely.
//   - Anything Stripe reports as not live (unpaid, incomplete) is free.
//
// Interpreted at read time rather than written into the column, so a status Stripe
// adds later cannot silently read as entitled.
func EffectivePlan(org *models.Organization) models.Plan {
	if org == nil || org.Plan == "" || org.Plan == models.PlanFree {
		return models.PlanFree
	}
	switch org.PlanStatus {
	case "active", "trialing":
		// Live subscription — the plan stands.
	case "canceled", "cancelled", "past_due":
		// Still entitled until the paid period actually runs out.
		if org.CurrentPeriodEnd == nil || time.Now().After(*org.CurrentPeriodEnd) {
			return models.PlanFree
		}
	case "":
		// No Stripe record at all. Only reachable for a plan set by hand, which is
		// how a Business customer on an invoice gets provisioned — honour it.
	default:
		return models.PlanFree
	}
	if org.CurrentPeriodEnd != nil && time.Now().After(*org.CurrentPeriodEnd) {
		// Grace on the exact boundary is Stripe's job, not ours; once the period is
		// genuinely past and nothing renewed it, entitlement ends.
		if org.PlanStatus != "active" && org.PlanStatus != "trialing" && org.PlanStatus != "" {
			return models.PlanFree
		}
	}
	return org.Plan
}

func (g *Gate) org(orgID string) (*models.Organization, error) {
	var org models.Organization
	if err := g.db.Where("id = ?", orgID).First(&org).Error; err != nil {
		return nil, fmt.Errorf("organization %s: %w", orgID, err)
	}
	return &org, nil
}

// Org exposes the loaded organization for handlers that need plan details.
func (g *Gate) Org(orgID string) (*models.Organization, error) { return g.org(orgID) }

// DB gives limit checks access to the same handle. Kept narrow deliberately —
// callers should reach for the helpers above rather than writing their own
// ledger queries.
func (g *Gate) DB() *gorm.DB { return g.db }

// applyCallContext copies the identity of the node being executed onto a spend, so
// each ledger row says which workflow, run and step it paid for.
//
// Denormalized deliberately — see the comment on CreditLedger.WorkflowID. An audit
// line has to stay legible after run history has been pruned.
func applyCallContext(ctx context.Context, spend *credits.Spend) {
	cc, ok := telemetry.CallContextFrom(ctx)
	if !ok {
		return
	}
	spend.WorkflowID = cc.WorkflowID
	spend.WorkflowName = cc.WorkflowName
	spend.NodeID = cc.NodeID
	spend.NodeLabel = cc.NodeLabel
	spend.Op = cc.Op
	if spend.RunID == "" {
		spend.RunID = cc.RunID
	}
}

// nodeCharge is the flat fee for one completed operation, by node type.
//
// Our marginal cost on integrations is close to zero, so these are value pricing
// and a fair-use brake, not cost recovery — priced low enough to be effectively
// free at honest volume. Web tools are the exception: Brave and Jina bill us per
// call, so that one is real cost.
func nodeCharge(nodeType string) (int64, models.LedgerReason) {
	switch nodeType {
	case "emailSend", "resend", "sendgrid":
		return credits.EmailSend, models.ReasonEmail
	case "webSearch", "webScrape", "webFetch":
		return credits.WebToolCall, models.ReasonWebTool
	case "textInput", "imageInput", "textOutput", "branch", "loop",
		"webhookTrigger", "scheduledTrigger", "humanApproval":
		// Structural nodes do no outbound work and cost us nothing. Charging for
		// them would meter the shape of someone's workflow rather than its work,
		// which pushes people to write worse workflows to save credits.
		return 0, ""
	default:
		return credits.IntegrationOp, models.ReasonIntegration
	}
}

// recordNodeSpend charges one completed node.
func (g *Gate) recordNodeSpend(ctx context.Context, nodeType, op string) {
	amount, reason := nodeCharge(nodeType)
	if amount <= 0 {
		return
	}
	b := telemetry.BillingFrom(ctx)
	if b.OrgID == "" {
		return
	}
	spend := credits.Spend{
		OrgID:    b.OrgID,
		UserID:   b.UserID,
		Amount:   amount,
		Reason:   reason,
		RunID:    b.RunID,
		HoldID:   b.HoldID,
		Provider: nodeType,
		Surface:  telemetry.SurfaceFrom(ctx),
	}
	applyCallContext(ctx, &spend)
	if spend.Op == "" {
		spend.Op = op
	}
	if err := credits.Record(g.db, spend); err != nil {
		slog.ErrorContext(ctx, "billing: failed to record node spend",
			"error", err, "org_id", b.OrgID, "node_type", nodeType)
	}
}
