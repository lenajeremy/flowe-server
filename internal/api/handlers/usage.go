package handlers

import (
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/tenancy"
)

// The usage report: every charge, itemised.
//
// This is the accountability surface. Its job is to let someone answer "what did
// this cost and why" without trusting us — so each line carries the run and
// workflow it belongs to, the operation, the model, the raw token counts and the
// exact credit amount. The numbers come from the ledger, which is the same
// append-only record the balance is derived from, so the page cannot disagree with
// the bill.
//
// Grants are included alongside spend rather than filtered out. A statement that
// only shows debits cannot be reconciled against a balance, which is the first
// thing anyone actually wants to do with it.

// usageRow is one ledger line as the report presents it.
type usageRow struct {
	ID   string `json:"id"`
	At   string `json:"at"`
	Kind string `json:"kind"` // "spend" | "grant"
	// Reason is the machine value; Label is what to show.
	Reason string `json:"reason"`
	Label  string `json:"label"`
	// Credits is positive for a grant and negative for a charge, matching the
	// ledger's own sign convention rather than inventing a second one.
	Credits int64 `json:"credits"`

	UserID       string `json:"user_id,omitempty"`
	UserName     string `json:"user_name,omitempty"`
	WorkflowID   string `json:"workflow_id,omitempty"`
	WorkflowName string `json:"workflow_name,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	NodeID       string `json:"node_id,omitempty"`
	NodeLabel    string `json:"node_label,omitempty"`
	Op           string `json:"op,omitempty"`
	Surface      string `json:"surface,omitempty"`

	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// Tokens is nil for a non-LLM charge, which is how the UI knows to show a dash
	// rather than four zeroes that look like a measurement failure.
	Tokens *usageTokens `json:"tokens,omitempty"`
}

type usageTokens struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Cached     int `json:"cached"`
	CacheWrite int `json:"cache_write"`
	Total      int `json:"total"`
}

// reasonLabels turn the ledger vocabulary into something a person reads. Kept here
// rather than in the model so the wording can change without a migration.
var reasonLabels = map[models.LedgerReason]string{
	models.ReasonLLMUsage:     "AI",
	models.ReasonIntegration:  "Integration",
	models.ReasonEmail:        "Email",
	models.ReasonWebTool:      "Web search",
	models.ReasonSignupGrant:  "Welcome credits",
	models.ReasonMonthlyGrant: "Monthly allowance",
	models.ReasonTopup:        "Top-up",
	models.ReasonRefund:       "Refund",
	models.ReasonAdjustment:   "Adjustment",
}

func labelFor(r models.LedgerReason) string {
	if l, ok := reasonLabels[r]; ok {
		return l
	}
	return string(r)
}

// usagePeriod resolves the ?period= window.
//
// "current" means since the allowance was last granted, which is the window the
// usage bar on the billing screen reports — the two must agree or one of them is
// lying.
func (h *WorkflowHandler) usagePeriod(orgID, period string) (from, to time.Time, label string) {
	bal, _ := credits.Balance(h.db.DB, orgID)
	start := credits.PeriodStart(bal)
	now := time.Now()

	switch period {
	case "previous":
		// One period back from the current start. Approximated as a month because
		// that is the only billing interval we sell; when annual plans exist this has
		// to come from Stripe's period boundaries instead.
		return start.AddDate(0, -1, 0), start, "Previous period"
	case "all":
		return time.Time{}, now.Add(time.Hour), "All time"
	default:
		if start.IsZero() {
			return time.Time{}, now.Add(time.Hour), "All time"
		}
		return start, now.Add(time.Hour), "Current period"
	}
}

// GetUsage — GET /api/usage?period=current|previous|all&kind=&limit=&offset=
func (h *WorkflowHandler) GetUsage(c *gin.Context) {
	orgID, me := currentOrgID(c), auth.UserID(c)
	from, to, periodLabel := h.usagePeriod(orgID, c.Query("period"))

	// Who may see whose spend. An admin sees the whole organization and can filter
	// to one person; everybody else sees only themselves. Enforced here rather than
	// by hiding a control, because "what is my colleague spending" is exactly the
	// question a plain member should not be able to answer.
	admin := tenancy.CanManageMembers(h.db.DB, orgID, me)
	scopeUser := me
	if admin {
		scopeUser = strings.TrimSpace(c.Query("user_id")) // empty means everyone
	}

	limit := 100
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	offset := 0
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v > 0 {
		offset = v
	}

	q := h.db.DB.Model(&models.CreditLedger{}).
		Where("organization_id = ? AND created_at >= ? AND created_at < ?", orgID, from, to)
	// kind filters to charges or credits; absent means everything, because a
	// statement you cannot reconcile is not a statement. The same value goes to the
	// summary so the cards above the table describe the rows inside it.
	kind := c.Query("kind")
	switch kind {
	case "spend":
		q = q.Where("delta < 0")
	case "grant":
		q = q.Where("delta > 0")
	}
	if wf := strings.TrimSpace(c.Query("workflow_id")); wf != "" {
		q = q.Where("workflow_id = ?", wf)
	}
	if run := strings.TrimSpace(c.Query("run_id")); run != "" {
		q = q.Where("run_id = ?", run)
	}
	if scopeUser != "" {
		// Grants belong to the org rather than to a person, so a per-person view
		// shows charges only — otherwise everyone would appear to have been handed
		// the whole monthly allowance.
		q = q.Where("user_id = ?", scopeUser)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		slog.ErrorContext(c.Request.Context(), "usage: count failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load your usage"})
		return
	}

	var entries []models.CreditLedger
	if err := q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&entries).Error; err != nil {
		slog.ErrorContext(c.Request.Context(), "usage: query failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load your usage"})
		return
	}

	names := h.userNames(orgID)
	rows := make([]usageRow, 0, len(entries))
	for _, e := range entries {
		r := toUsageRow(e)
		if e.UserID != nil {
			r.UserID = *e.UserID
			r.UserName = names[r.UserID]
		}
		rows = append(rows, r)
	}

	summary, err := h.usageSummary(orgID, from, to, scopeUser, kind, admin)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "usage: summary failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not summarise your usage"})
		return
	}

	org, _ := h.bill.Org(orgID)
	alloc, _ := h.bill.AllocationFor(org, me)

	// Whether this org has anyone else in it. Counted from MEMBERSHIP rather than
	// from who happens to have spent: a three-person team where only one person has
	// used anything is still a team, and hiding the per-person view from them would
	// be wrong the moment a second person starts working.
	var memberCount int64
	h.db.DB.Model(&models.OrgMember{}).
		Where("organization_id = ?", orgID).Count(&memberCount)
	c.JSON(http.StatusOK, gin.H{
		"period": gin.H{
			"label": periodLabel,
			"from":  timeOrNil(from),
			"to":    to.Format(time.RFC3339),
		},
		"is_admin":     admin,
		"member_count": memberCount,
		"viewing_user": scopeUser,
		"my_allocation": gin.H{
			"limit":     alloc.Limit,
			"spent":     alloc.Spent,
			"remaining": alloc.Remaining(),
			"percent":   alloc.UsedPercent(),
			"custom":    alloc.Custom,
		},
		"included_credits": billing.LimitsForOrg(org).MonthlyCredits,
		"rows":             rows,
		"total_rows":       total,
		"limit":            limit,
		"offset":           offset,
		"summary":          summary,
	})
}

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339)
}

func toUsageRow(e models.CreditLedger) usageRow {
	kind := "spend"
	if e.Delta > 0 {
		kind = "grant"
	}
	r := usageRow{
		ID:           e.ID.String(),
		At:           e.CreatedAt.Format(time.RFC3339),
		Kind:         kind,
		Reason:       string(e.Reason),
		Label:        labelFor(e.Reason),
		Credits:      e.Delta,
		WorkflowName: e.WorkflowName,
		NodeID:       e.NodeID,
		NodeLabel:    e.NodeLabel,
		Op:           e.Op,
		Surface:      e.Surface,
		Provider:     e.Provider,
		Model:        e.Model,
	}
	if e.WorkflowID != nil {
		r.WorkflowID = *e.WorkflowID
	}
	if e.RunID != nil {
		r.RunID = *e.RunID
	}
	if tot := e.InputTokens + e.OutputTokens + e.CachedTokens + e.CacheWriteTokens; tot > 0 {
		r.Tokens = &usageTokens{
			Input: e.InputTokens, Output: e.OutputTokens,
			Cached: e.CachedTokens, CacheWrite: e.CacheWriteTokens, Total: tot,
		}
	}
	return r
}

// ── Summary ──────────────────────────────────────────────────────

type usageBreakdown struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Credits int64  `json:"credits"`
	Calls   int64  `json:"calls"`
	Tokens  int64  `json:"tokens,omitempty"`
}

// usageSummary aggregates the same window three ways, because "what did this cost"
// has three different useful answers: by type of work, by which workflow, and by
// which model.
//
// `kind` is the same filter the ledger list uses. The breakdowns used to ignore it
// and always describe spend, so selecting "Credits" left a table reading "nothing
// charged" directly beneath three cards itemising charges — the page contradicted
// itself and neither half was labelled as to which one the filter had reached.
func (h *WorkflowHandler) usageSummary(orgID string, from, to time.Time, scopeUser, kind string, admin bool) (gin.H, error) {
	type agg struct {
		Key     string
		Credits int64
		Calls   int64
		Tokens  int64
	}
	// Grouped by the SAME expression that produces the label. Grouping by the raw
	// columns instead splits NULL from empty string, which shows up as two rows with
	// the identical label and figures that look like they should have been added
	// together — and, since they are, a reader has no way to tell which is which.
	grants := kind == "grant"
	scan := func(keyExpr string) ([]usageBreakdown, error) {
		var rows []agg
		err := h.db.DB.Model(&models.CreditLedger{}).
			// SUM(ABS(delta)) rather than -SUM(delta): the same expression has to
			// produce a positive figure whether the rows are charges or credits.
			Select(keyExpr+" AS key, SUM(ABS(delta)) AS credits, COUNT(*) AS calls, "+
				"COALESCE(SUM(input_tokens + output_tokens + cached_tokens + cache_write_tokens),0) AS tokens").
			Where(`organization_id = ? AND created_at >= ? AND created_at < ?`, orgID, from, to).
			Scopes(func(d *gorm.DB) *gorm.DB {
				if grants {
					return d.Where("delta > 0")
				}
				// Restricted to genuine spend reasons so every breakdown sums to the
				// headline "spent" figure. A correction is a negative delta but is not
				// consumption, and a summary whose parts do not add up to its own total
				// is worse than one that leaves a category out — the reader cannot tell
				// which number to trust. Corrections are still visible as rows.
				return d.Where("delta < 0 AND reason IN ?", spendReasonList())
			}).
			Scopes(func(d *gorm.DB) *gorm.DB {
				if scopeUser == "" {
					return d
				}
				return d.Where("user_id = ?", scopeUser)
			}).
			Group(keyExpr).Order("credits DESC").Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		out := make([]usageBreakdown, 0, len(rows))
		for _, r := range rows {
			label := r.Key
			if label == "" {
				label = "Other"
			}
			out = append(out, usageBreakdown{
				Key: r.Key, Label: label, Credits: r.Credits, Calls: r.Calls, Tokens: r.Tokens,
			})
		}
		return out, nil
	}

	byReason, err := scan("reason")
	if err != nil {
		return nil, err
	}
	for i := range byReason {
		byReason[i].Label = labelFor(models.LedgerReason(byReason[i].Key))
	}
	// COALESCE so uncredited work groups under one visible bucket instead of
	// vanishing into a NULL key.
	byWorkflow, err := scan("COALESCE(NULLIF(workflow_name, ''), 'Outside a workflow')")
	if err != nil {
		return nil, err
	}
	// Only LLM rows have a model. A non-LLM charge stores its NODE TYPE in provider,
	// so falling back to it listed "emailSend" and "httpRequest" under a heading that
	// said "By model" — reading as though we bill against models nobody has heard of.
	// Those steps still have to appear or the card stops summing to the total, so
	// they group under one bucket that says what they are.
	byModel, err := scan(fmt.Sprintf(
		`CASE WHEN reason = '%s' THEN COALESCE(NULLIF(model, ''), NULLIF(provider, ''), 'Unknown model')
		      ELSE 'Other steps' END`, models.ReasonLLMUsage))
	if err != nil {
		return nil, err
	}

	var spent, granted int64
	spentQ := h.db.DB.Model(&models.CreditLedger{}).
		Where(`organization_id = ? AND created_at >= ? AND created_at < ? AND delta < 0
			AND reason IN ?`, orgID, from, to, spendReasonList())
	if scopeUser != "" {
		spentQ = spentQ.Where("user_id = ?", scopeUser)
	}
	spentQ.Select("COALESCE(-SUM(delta),0)").Scan(&spent)
	grantQ := h.db.DB.Model(&models.CreditLedger{}).
		Where("organization_id = ? AND created_at >= ? AND created_at < ? AND delta > 0", orgID, from, to)
	if scopeUser != "" {
		// Scoped like everything else on the page. Grants are org-level and carry no
		// user, so this is always 0 in a per-person view — which is the honest answer
		// and the one the ledger below already gives. Left unscoped, a member saw the
		// organization's entire allowance added up next to their own few hundred
		// credits of spend, with an empty Credits tab underneath insisting there were
		// no such rows. The caller decides whether a figure that can only be 0 is
		// worth a card; it must not be a different scope from its neighbours.
		grantQ = grantQ.Where("user_id = ?", scopeUser)
	}
	grantQ.Select("COALESCE(SUM(delta),0)").Scan(&granted)

	out := gin.H{
		"spent":       spent,
		"granted":     granted,
		"by_reason":   byReason,
		"by_workflow": byWorkflow,
		"by_model":    byModel,
	}

	// Who spent what — the whole point of the per-person view, and admin-only. A
	// plain member can already see their own total; showing them the breakdown by
	// colleague would be a different thing entirely.
	if admin {
		perUser, err := credits.SpendPerUserSince(h.db.DB, orgID, from)
		if err != nil {
			return nil, err
		}
		org, _ := h.bill.Org(orgID)
		names := h.userNames(orgID)
		people := make([]gin.H, 0, len(perUser))
		seen := make(map[string]bool, len(perUser))
		for _, u := range perUser {
			label := names[u.UserID]
			if label == "" {
				// Spend that predates per-person attribution, or work by someone who
				// has since left. Shown rather than dropped so the parts still add up
				// to the org total.
				label = "Unattributed"
			}
			row := gin.H{
				"user_id": u.UserID, "name": label,
				"credits": u.Credits, "calls": u.Calls, "tokens": u.Tokens,
			}
			if u.UserID != "" && org != nil {
				if a, err := h.bill.AllocationFor(org, u.UserID); err == nil {
					row["limit"] = a.Limit
					row["percent"] = a.UsedPercent()
					row["remaining"] = a.Remaining()
					row["custom_limit"] = a.Custom
				}
			}
			people = append(people, row)
			seen[u.UserID] = true
		}
		// Everyone else on the team, at zero. Built only from the ledger, this list
		// silently omitted anybody who had not spent in the window — so a two-person
		// team showed one name, and the admin could not select the quiet colleague to
		// confirm they had spent nothing. "No spend" is an answer; an absent row is
		// not, and it makes the team look smaller than it is.
		quiet := make([]string, 0, len(names))
		for uid := range names {
			if !seen[uid] {
				quiet = append(quiet, uid)
			}
		}
		// By name, because Go randomises map iteration and an order that reshuffles on
		// every poll is its own bug in a list people read down.
		sort.Slice(quiet, func(i, j int) bool { return names[quiet[i]] < names[quiet[j]] })
		for _, uid := range quiet {
			row := gin.H{"user_id": uid, "name": names[uid], "credits": 0, "calls": 0, "tokens": 0}
			if org != nil {
				if a, err := h.bill.AllocationFor(org, uid); err == nil {
					row["limit"] = a.Limit
					row["percent"] = a.UsedPercent()
					row["remaining"] = a.Remaining()
					row["custom_limit"] = a.Custom
				}
			}
			people = append(people, row)
		}
		out["by_user"] = people
	}
	return out, nil
}

// userNames maps org member ids to a display name, for labelling usage rows.
func (h *WorkflowHandler) userNames(orgID string) map[string]string {
	var rows []struct{ UserID, Name, Email string }
	h.db.DB.Raw(`
		SELECT m.user_id, u.name, u.email FROM org_members m
		JOIN users u ON u.id = m.user_id WHERE m.organization_id = ?`, orgID).Scan(&rows)
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.Name != "" {
			out[r.UserID] = r.Name
		} else {
			out[r.UserID] = r.Email
		}
	}
	return out
}

// spendReasonList mirrors credits.SpentSince's definition of consumption, so the
// figure on this page equals the one on the billing screen. A correction is a
// negative delta but is not the customer's usage.
func spendReasonList() []models.LedgerReason {
	return []models.LedgerReason{
		models.ReasonLLMUsage, models.ReasonIntegration,
		models.ReasonEmail, models.ReasonWebTool,
	}
}

// ── CSV export ───────────────────────────────────────────────────

// ExportUsage — GET /api/usage/export.csv?period=
//
// Accountability that cannot leave the page is not much use: this is the format
// someone forwards to whoever asks about the bill. Columns are stable and the raw
// ids are included, so a row can be traced back to a specific run.
func (h *WorkflowHandler) ExportUsage(c *gin.Context) {
	orgID, me := currentOrgID(c), auth.UserID(c)
	from, to, label := h.usagePeriod(orgID, c.Query("period"))

	// Same scoping as the page. An export that ignored it would be the easy way
	// around a permission the UI enforces.
	scopeUser := me
	if tenancy.CanManageMembers(h.db.DB, orgID, me) {
		scopeUser = strings.TrimSpace(c.Query("user_id"))
	}

	q := h.db.DB.
		Where("organization_id = ? AND created_at >= ? AND created_at < ?", orgID, from, to)
	// Honours the charges/credits filter too. The button sits beside that control,
	// so an export that quietly ignored it handed back a different set of rows from
	// the one on screen — and nothing in the file said so.
	switch c.Query("kind") {
	case "spend":
		q = q.Where("delta < 0")
	case "grant":
		q = q.Where("delta > 0")
	}
	if scopeUser != "" {
		q = q.Where("user_id = ?", scopeUser)
	}
	var entries []models.CreditLedger
	if err := q.
		Order("created_at DESC").
		// Bounded: an unbounded export on a busy org would build the whole ledger in
		// memory. 50k rows is far more than anyone reconciles by hand and still a
		// small response.
		Limit(50000).Find(&entries).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not export your usage"})
		return
	}

	filename := fmt.Sprintf("fernary-usage-%s-%s.csv",
		strings.ToLower(strings.ReplaceAll(label, " ", "-")), time.Now().Format("2006-01-02"))
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)

	w := csv.NewWriter(c.Writer)
	defer w.Flush()
	names := h.userNames(orgID)
	_ = w.Write([]string{
		"timestamp", "type", "description", "credits",
		"member", "member_id",
		"workflow", "workflow_id", "run_id", "node", "node_id", "operation",
		"surface", "provider", "model",
		"input_tokens", "output_tokens", "cached_tokens", "cache_write_tokens",
	})
	for _, e := range entries {
		runID, wfID := "", ""
		if e.RunID != nil {
			runID = *e.RunID
		}
		if e.WorkflowID != nil {
			wfID = *e.WorkflowID
		}
		kind := "charge"
		if e.Delta > 0 {
			kind = "credit"
		}
		userID := ""
		if e.UserID != nil {
			userID = *e.UserID
		}
		_ = w.Write([]string{
			e.CreatedAt.UTC().Format(time.RFC3339),
			kind,
			labelFor(e.Reason),
			strconv.FormatInt(e.Delta, 10),
			names[userID], userID,
			e.WorkflowName, wfID, runID, e.NodeLabel, e.NodeID, e.Op,
			e.Surface, e.Provider, e.Model,
			itoaZero(e.InputTokens), itoaZero(e.OutputTokens),
			itoaZero(e.CachedTokens), itoaZero(e.CacheWriteTokens),
		})
	}
}

// itoaZero renders a token count, leaving a non-LLM row's cells empty rather than
// writing zeroes that a spreadsheet would sum as if they were measurements.
func itoaZero(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}
