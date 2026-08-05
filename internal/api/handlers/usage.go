package handlers

import (
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database/models"
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
	orgID := currentOrgID(c)
	from, to, periodLabel := h.usagePeriod(orgID, c.Query("period"))

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
	// statement you cannot reconcile is not a statement.
	switch c.Query("kind") {
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

	rows := make([]usageRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, toUsageRow(e))
	}

	summary, err := h.usageSummary(orgID, from, to)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "usage: summary failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not summarise your usage"})
		return
	}

	org, _ := h.bill.Org(orgID)
	c.JSON(http.StatusOK, gin.H{
		"period": gin.H{
			"label": periodLabel,
			"from":  timeOrNil(from),
			"to":    to.Format(time.RFC3339),
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
func (h *WorkflowHandler) usageSummary(orgID string, from, to time.Time) (gin.H, error) {
	type agg struct {
		Key     string
		Credits int64
		Calls   int64
		Tokens  int64
	}
	scan := func(groupBy, keyExpr string) ([]usageBreakdown, error) {
		var rows []agg
		err := h.db.DB.Model(&models.CreditLedger{}).
			Select(keyExpr+" AS key, -SUM(delta) AS credits, COUNT(*) AS calls, "+
				"COALESCE(SUM(input_tokens + output_tokens + cached_tokens + cache_write_tokens),0) AS tokens").
			// Restricted to genuine spend reasons so every breakdown sums to the
			// headline "spent" figure. A correction is a negative delta but is not
			// consumption, and a summary whose parts do not add up to its own total is
			// worse than one that leaves a category out — the reader cannot tell which
			// number to trust. Corrections are still visible as individual rows.
			Where(`organization_id = ? AND created_at >= ? AND created_at < ?
				AND delta < 0 AND reason IN ?`, orgID, from, to, spendReasonList()).
			Group(groupBy).Order("credits DESC").Scan(&rows).Error
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

	byReason, err := scan("reason", "reason")
	if err != nil {
		return nil, err
	}
	for i := range byReason {
		byReason[i].Label = labelFor(models.LedgerReason(byReason[i].Key))
	}
	// COALESCE so uncredited work groups under one visible bucket instead of
	// vanishing into a NULL key.
	byWorkflow, err := scan("workflow_id, workflow_name",
		"COALESCE(NULLIF(workflow_name, ''), 'Outside a workflow')")
	if err != nil {
		return nil, err
	}
	byModel, err := scan("provider, model",
		"COALESCE(NULLIF(model, ''), NULLIF(provider, ''), 'None')")
	if err != nil {
		return nil, err
	}

	var spent, granted int64
	h.db.DB.Model(&models.CreditLedger{}).
		Where(`organization_id = ? AND created_at >= ? AND created_at < ? AND delta < 0
			AND reason IN ?`, orgID, from, to, spendReasonList()).
		Select("COALESCE(-SUM(delta),0)").Scan(&spent)
	h.db.DB.Model(&models.CreditLedger{}).
		Where("organization_id = ? AND created_at >= ? AND created_at < ? AND delta > 0", orgID, from, to).
		Select("COALESCE(SUM(delta),0)").Scan(&granted)

	return gin.H{
		"spent":       spent,
		"granted":     granted,
		"by_reason":   byReason,
		"by_workflow": byWorkflow,
		"by_model":    byModel,
	}, nil
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
	orgID := currentOrgID(c)
	from, to, label := h.usagePeriod(orgID, c.Query("period"))

	var entries []models.CreditLedger
	if err := h.db.DB.
		Where("organization_id = ? AND created_at >= ? AND created_at < ?", orgID, from, to).
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
	_ = w.Write([]string{
		"timestamp", "type", "description", "credits",
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
		_ = w.Write([]string{
			e.CreatedAt.UTC().Format(time.RFC3339),
			kind,
			labelFor(e.Reason),
			strconv.FormatInt(e.Delta, 10),
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
