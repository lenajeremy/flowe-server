package handlers

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database/models"
)

// Billing endpoints: what plan am I on, take me to checkout, take me to the
// portal, and the Stripe webhook.

// planView is what the pricing page and the billing screen render from.
//
// Plan FEATURES are the story, not credits. Credits appear as a single
// "included" figure and a remaining balance, never as the unit a buyer is asked
// to reason in — a meter that varies with how chatty the LLM was on Tuesday
// produces bill anxiety, and on an unattended product that quietly stops people
// publishing schedules.
type planView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Tagline string `json:"tagline"`
	// Price is 0 for free and -1 for "contact us".
	Price    int      `json:"price"`
	Currency string   `json:"currency"`
	Interval string   `json:"interval"` // "month" | ""
	PerSeat  bool     `json:"per_seat"`
	MinSeats int      `json:"min_seats,omitempty"`
	CTA      string   `json:"cta"`
	Features []string `json:"features"`
	// Highlight marks the plan the page leads with.
	Highlight bool `json:"highlight"`
	// SelfServe is false for tiers sold by conversation.
	SelfServe bool `json:"self_serve"`
}

// Prices are stated in EUR because that is the currency the Stripe prices are
// created in, and Adaptive Pricing only converts FROM the account's settlement
// currency — which for this account is the euro. A price in any other currency
// falls outside the mechanism entirely and every customer would just see euros.
//
// What a customer actually pays is converted at checkout, so these are reference
// figures, not necessarily the charged ones.
const (
	planCurrency = "EUR"
	priceProEUR  = 29
	// Team is per seat. Seats meter nothing we spend, so the credit allowance
	// scales with them too — see credits.GrantTeamPerSeat.
	priceTeamPerSeatEUR = 25
)

// PublicPlans — GET /api/billing/plans
//
// Unauthenticated, because the pricing page is public. Rendered from the same
// limits table the server enforces, so the page cannot promise something a
// handler will refuse.
func (h *WorkflowHandler) PublicPlans(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"plans": planCatalog()})
}

func planCatalog() []planView {
	free := billing.LimitsFor(models.PlanFree)
	pro := billing.LimitsFor(models.PlanPro)
	team := billing.LimitsFor(models.PlanTeam)
	business := billing.LimitsFor(models.PlanBusiness)

	return []planView{
		{
			ID: string(models.PlanFree), Name: "Free",
			Tagline:   "Build an agent and see it work.",
			Price:     0,
			Currency:  planCurrency,
			CTA:       "Start free",
			SelfServe: true,
			Features: []string{
				countPhrase(free.MaxWorkflows, "workflow", "workflows"),
				schedulePhrase(free),
				retentionPhrase(free),
				"Unlimited manual runs and webhooks",
				"Every integration included",
				"Approvals on every action",
			},
		},
		{
			ID: string(models.PlanPro), Name: "Pro",
			Tagline:   "Put agents to work on a schedule.",
			Price:     priceProEUR,
			Currency:  planCurrency,
			Interval:  "month",
			CTA:       "Upgrade to Pro",
			Highlight: true,
			SelfServe: true,
			Features: []string{
				countPhrase(pro.MaxWorkflows, "workflow", "workflows"),
				schedulePhrase(pro),
				retentionPhrase(pro),
				"Larger AI context per step",
				"Priority email support",
			},
		},
		{
			ID: string(models.PlanTeam), Name: "Team",
			Tagline:  "Share agents and credentials with your team.",
			Price:    priceTeamPerSeatEUR,
			Currency: planCurrency,
			Interval: "month",
			PerSeat:  true,
			MinSeats: billing.MinSeats,
			CTA:      "Contact us",
			// Not self-serve YET. The seat count is fully wired through checkout,
			// the webhook and the allowance — but member invites do not exist, so
			// selling five seats today would sell four nobody can fill. Flipping
			// this to true is the only change needed once invites ship.
			SelfServe: false,
			Features: []string{
				countPhrase(team.MaxWorkflows, "workflow", "workflows"),
				schedulePhrase(team),
				retentionPhrase(team),
				"Every seat brings its own AI allowance",
				"Shared integration connections",
				"Delegated approvals",
			},
		},
		{
			ID: string(models.PlanBusiness), Name: "Business",
			Tagline:   "Controls for agents that touch money.",
			Price:     -1,
			Currency:  planCurrency,
			CTA:       "Contact us",
			SelfServe: false,
			Features: []string{
				"Unlimited workflows and schedules",
				retentionPhrase(business),
				"Unlimited members",
				"Per-operation approval policy",
				"SSO and audit log",
				"Least-privilege credentials",
			},
		},
	}
}

// countPhrase renders a cap, treating zero as unlimited.
func countPhrase(n int, singular, plural string) string {
	if n == 0 {
		return "Unlimited " + plural
	}
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + plural
}

// schedulePhrase states both halves of the schedule limit together — how many can
// run and how often — because either alone is misleading.
func schedulePhrase(l billing.Limits) string {
	var howOften string
	switch {
	case l.MinScheduleInterval >= 24*time.Hour:
		howOften = "daily"
	case l.MinScheduleInterval >= time.Hour:
		howOften = "hourly"
	case l.MinScheduleInterval == time.Minute:
		howOften = "as often as every minute"
	default:
		howOften = "as often as every " + strconv.Itoa(int(l.MinScheduleInterval.Minutes())) + " minutes"
	}
	if l.MaxPublishedSchedules == 0 {
		return "Unlimited scheduled agents, " + howOften
	}
	if l.MaxPublishedSchedules == 1 {
		return "1 scheduled agent, running " + howOften
	}
	return strconv.Itoa(l.MaxPublishedSchedules) + " scheduled agents, " + howOften
}

func retentionPhrase(l billing.Limits) string {
	days := int(l.RunHistoryRetention.Hours()) / 24
	if days >= 365 {
		return strconv.Itoa(days/365) + "-year run history"
	}
	return strconv.Itoa(days) + "-day run history"
}

// ── Current subscription ─────────────────────────────────────────

// GetBilling — GET /api/billing
//
// The org's plan, its limits, and what it has left. Usage is reported as a
// percentage and a remaining figure rather than as a raw credit count, so the
// answer to "am I about to run out" does not require knowing what a credit is.
func (h *WorkflowHandler) GetBilling(c *gin.Context) {
	orgID := currentOrgID(c)
	org, err := h.bill.Org(orgID)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "billing: org lookup failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load your plan"})
		return
	}
	plan := billing.EffectivePlan(org)
	// Scaled by the seats the org pays for — the per-seat base figures would
	// under-report a team's real allowance.
	lim := billing.LimitsForOrg(org)

	bal, err := credits.Balance(h.db.DB, orgID)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "billing: balance lookup failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load your usage"})
		return
	}
	remaining := credits.Spendable(bal)
	if remaining < 0 {
		remaining = 0
	}
	var usedPct int
	if lim.MonthlyCredits > 0 {
		usedPct = int((lim.MonthlyCredits - remaining) * 100 / lim.MonthlyCredits)
		if usedPct < 0 {
			usedPct = 0
		}
		if usedPct > 100 {
			usedPct = 100
		}
	}

	// Counts that back the limit displays, so the UI can show "2 of 3 workflows"
	// without a second round of requests.
	var workflows, schedules int64
	h.db.DB.Table("workflows").
		Where("organization_id = ? AND deleted_at IS NULL", orgID).Count(&workflows)
	h.db.DB.Table("workflows").
		Joins("JOIN scheduled_triggers st ON st.workflow_id = workflows.id::text").
		Where(`workflows.organization_id = ? AND workflows.deleted_at IS NULL
			AND workflows.published = true AND st.enabled = true`, orgID).Count(&schedules)

	out := gin.H{
		"plan":                 string(plan),
		"plan_name":            planDisplayName(plan),
		"status":               org.PlanStatus,
		"cancel_at_period_end": org.CancelAtPeriodEnd,
		"personal":             org.Personal,
		"seats":                org.Seats,
		"per_seat":             billing.LimitsFor(plan).PerSeat,
		"has_billing_account":  org.StripeCustomerID != "",
		"usage": gin.H{
			"included_credits":  lim.MonthlyCredits,
			"remaining_credits": remaining,
			"used_percent":      usedPct,
			"workflows":         workflows,
			"scheduled_agents":  schedules,
		},
		"limits": gin.H{
			"max_workflows":        lim.MaxWorkflows,
			"max_scheduled_agents": lim.MaxPublishedSchedules,
			"min_schedule_minutes": int(lim.MinScheduleInterval.Minutes()),
			"run_history_days":     int(lim.RunHistoryRetention.Hours()) / 24,
			"max_members":          lim.MaxMembers,
			"max_tokens_per_call":  lim.MaxTokensPerCall,
			"shared_connections":   lim.SharedConnections,
		},
	}
	if org.CurrentPeriodEnd != nil {
		out["current_period_end"] = org.CurrentPeriodEnd
	}
	c.JSON(http.StatusOK, out)
}

func planDisplayName(p models.Plan) string {
	switch p {
	case models.PlanPro:
		return "Pro"
	case models.PlanTeam:
		return "Team"
	case models.PlanBusiness:
		return "Business"
	default:
		return "Free"
	}
}

// ── Checkout and portal ──────────────────────────────────────────

// StartCheckout — POST /api/billing/checkout {"plan":"pro"}
func (h *WorkflowHandler) StartCheckout(c *gin.Context) {
	var body struct {
		Plan  string `json:"plan"`
		Seats int    `json:"seats"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	plan := models.Plan(strings.ToLower(strings.TrimSpace(body.Plan)))

	// Refuse tiers we do not sell self-serve, before touching Stripe. Team is
	// per-seat and fully wired, but gated until invites exist — selling seats
	// nobody can fill is a refund, not revenue.
	for _, p := range planCatalog() {
		if p.ID == string(plan) && !p.SelfServe {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "this plan is set up with us directly — get in touch and we'll take it from there"})
			return
		}
	}

	org, err := h.bill.Org(currentOrgID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load your account"})
		return
	}

	// The signed-in user's email prefills checkout. Looked up rather than taken
	// from the request so a client cannot start a subscription against someone
	// else's address.
	var user models.User
	h.db.DB.Where("id = ?", auth.UserID(c)).First(&user)

	base := clientBaseURL(c)
	session, err := h.bill.StartCheckout(c.Request.Context(), org, user.Email, plan, body.Seats,
		base+"/settings/billing?checkout=success",
		base+"/pricing?checkout=cancelled")
	if err != nil {
		if errors.Is(err, billing.ErrStripeNotConfigured) {
			slog.ErrorContext(c.Request.Context(), "billing: checkout attempted with Stripe unconfigured", "error", err)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "checkout is not available yet — please get in touch and we'll set you up"})
			return
		}
		slog.ErrorContext(c.Request.Context(), "billing: checkout failed", "error", err, "plan", plan)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	slog.InfoContext(c.Request.Context(), "billing: checkout started",
		"org_id", org.ID.String(), "plan", plan, "session_id", session.ID)
	c.JSON(http.StatusOK, gin.H{"url": session.URL})
}

// OpenPortal — POST /api/billing/portal
func (h *WorkflowHandler) OpenPortal(c *gin.Context) {
	org, err := h.bill.Org(currentOrgID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load your account"})
		return
	}
	url, err := h.bill.PortalURL(c.Request.Context(), org, clientBaseURL(c)+"/settings/billing")
	if err != nil {
		if errors.Is(err, billing.ErrStripeNotConfigured) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "billing is not available yet"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url})
}

// ── Webhook ──────────────────────────────────────────────────────

// StripeWebhook — POST /api/billing/stripe/webhook
//
// Public by necessity, authenticated by signature. Two things are essential:
//
//   - the RAW body is what gets verified, so it is read before any binding. A
//     JSON round-trip would change the bytes and every signature would fail.
//   - a verification failure is 400, not 500. Stripe retries 5xx, so returning
//     500 for a permanently-bad signature would produce an endless retry loop.
func (h *WorkflowHandler) StripeWebhook(c *gin.Context) {
	payload, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "could not read request body"})
		return
	}

	if err := billing.VerifyWebhook(payload, c.GetHeader("Stripe-Signature")); err != nil {
		slog.WarnContext(c.Request.Context(), "billing: rejected unverified Stripe webhook",
			"error", err, "remote", c.ClientIP())
		c.JSON(http.StatusBadRequest, gin.H{"error": "signature verification failed"})
		return
	}

	if err := h.bill.HandleWebhook(c.Request.Context(), payload); err != nil {
		// 500 so Stripe retries: the event was genuine and we failed to apply it,
		// which usually means a transient database problem. Swallowing it would
		// leave a paying customer on the free plan with no second chance.
		slog.ErrorContext(c.Request.Context(), "billing: failed to apply Stripe webhook", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not process event"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"received": true})
}
