package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
)

var schedulerOnce sync.Once

// StartScheduler starts the background scheduler (call once at server boot).
func (h *WorkflowHandler) StartScheduler() {
	schedulerOnce.Do(func() {
		go h.scheduleLoop()
	})
}

// scheduleLoop drives everything that fires on its own: due schedules, due
// polls, and subscriptions about to lapse.
//
// One ticker for all three because they share the same constraint — exactly one
// instance may run them — and a single loop makes that impossible to forget.
// Each sweep is independent, so a provider outage inside one cannot stop the
// others from running on the next tick.
func (h *WorkflowHandler) scheduleLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		<-ticker.C
		h.runDueSchedules()
		h.pollDueTriggers()
		h.renewExpiringSubscriptions()
	}
}

// calcNextRunAt returns the next UTC time this schedule should fire after `from`.
func calcNextRunAt(s models.ScheduledTrigger, from time.Time) time.Time {
	loc := time.UTC
	var h, m int
	fmt.Sscanf(s.RunTime, "%d:%d", &h, &m)

	switch s.Frequency {
	case "interval":
		secs := s.IntervalSeconds
		if secs < 60 {
			secs = 60 // the scheduler loop only ticks every minute
		}
		return from.Add(time.Duration(secs) * time.Second)

	case "hourly":
		return from.Truncate(time.Hour).Add(time.Hour)

	case "daily":
		next := time.Date(from.Year(), from.Month(), from.Day(), h, m, 0, 0, loc)
		if !next.After(from) {
			next = next.AddDate(0, 0, 1)
		}
		return next

	case "weekly":
		target := time.Weekday(s.DayOfWeek)
		next := time.Date(from.Year(), from.Month(), from.Day(), h, m, 0, 0, loc)
		for next.Weekday() != target || !next.After(from) {
			next = next.AddDate(0, 0, 1)
		}
		return next

	case "monthly":
		day := s.DayOfMonth
		if day < 1 {
			day = 1
		}
		next := time.Date(from.Year(), from.Month(), day, h, m, 0, 0, loc)
		if !next.After(from) {
			next = time.Date(from.Year(), from.Month()+1, day, h, m, 0, 0, loc)
		}
		return next
	}

	return from.Add(time.Hour)
}

func (h *WorkflowHandler) runDueSchedules() {
	var schedules []models.ScheduledTrigger
	now := time.Now().UTC()
	// Only PUBLISHED, live workflows auto-fire. Unpublished ones stay dormant
	// (the row is untouched, so publishing resumes it) and deleted ones never
	// fire. Manual/webhook/API triggers bypass this entirely.
	h.db.DB.
		Joins("JOIN workflows ON workflows.id::text = scheduled_triggers.workflow_id").
		Where(`workflows.published = true AND workflows.deleted_at IS NULL
			AND scheduled_triggers.enabled = true
			AND scheduled_triggers.next_run_at IS NOT NULL
			AND scheduled_triggers.next_run_at <= ?`, now).
		Find(&schedules)

	for _, sched := range schedules {
		slog.Info("scheduler: firing workflow", "workflow_id", sched.WorkflowID, "frequency", sched.Frequency)

		var nextRunPtr *time.Time
		if sched.Repeat {
			nextRun := calcNextRunAt(sched, now)
			nextRunPtr = &nextRun
			h.db.DB.Model(&sched).Updates(map[string]interface{}{
				"last_run_at": now,
				"next_run_at": nextRun,
			})
		} else {
			h.db.DB.Model(&sched).Updates(map[string]interface{}{
				"last_run_at": now,
				"enabled":     false,
			})
		}

		go h.runWorkflowByID(sched.WorkflowID, nextRunPtr)
	}
}

func (h *WorkflowHandler) runWorkflowByID(workflowID string, nextRun *time.Time) {
	// The schedule fires with no request context; telemetry and logs use the
	// background context (a fresh trace is started inside RunWorkflow).
	ctx := context.Background()
	var workflow models.Workflow
	if err := h.db.DB.First(&workflow, "id = ?", workflowID).Error; err != nil {
		slog.WarnContext(ctx, "scheduler: failed to load workflow", "workflow_id", workflowID, "error", err)
		telemetry.ScheduleFire(ctx, "error")
		return
	}
	var nodes []executor.WorkflowASTNode
	var edges []executor.WorkflowASTEdge
	json.Unmarshal(workflow.Nodes, &nodes)
	json.Unmarshal(workflow.Edges, &edges)

	// Safety check: only fire if the workflow still has a scheduledTrigger node.
	// If the node was removed but the DB record wasn't cleaned up, disable the schedule.
	hasScheduledNode := false
	for _, n := range nodes {
		if n.Type == executor.NodeTypeScheduledTrigger {
			hasScheduledNode = true
			break
		}
	}
	if !hasScheduledNode {
		slog.WarnContext(ctx, "scheduler: workflow has no scheduledTrigger node, disabling schedule", "workflow_id", workflowID)
		telemetry.ScheduleFire(ctx, "error")
		h.db.DB.Model(&models.ScheduledTrigger{}).Where("workflow_id = ?", workflowID).Update("enabled", false)
		return
	}

	// A scheduled run is the case the credit cap exists for: it spends money while
	// nobody is watching. admitRun refuses — and records WHY on the run — which is
	// what makes the stop visible instead of looking like the schedule died.
	p, err := h.admitRun(ctx, workflowID, "schedule", nil)
	if err != nil {
		telemetry.ScheduleFire(ctx, "error")
		return
	}

	fireAttrs := []any{"workflow_id", workflowID, "workflow_name", workflow.Name, "run_id", p.RunID()}
	if nextRun != nil {
		fireAttrs = append(fireAttrs, "next_run", *nextRun)
	}
	slog.InfoContext(ctx, "schedule fired", fireAttrs...)
	telemetry.ScheduleFire(ctx, "ok")

	// The schedule fires with no request context — the loaded workflow's
	// owner is what routes integration tokens to the right user.
	h.executeRun(ctx, p)
}

// GET /api/workflows/:id/schedule
func (h *WorkflowHandler) GetSchedule(c *gin.Context) {
	workflowID := c.Param("id")
	if _, ok := h.loadOwnedWorkflow(c, workflowID); !ok {
		return
	}
	var sched models.ScheduledTrigger
	if err := h.db.DB.Where("workflow_id = ?", workflowID).First(&sched).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no schedule"})
		return
	}
	c.JSON(http.StatusOK, sched)
}

// POST /api/workflows/:id/schedule
func (h *WorkflowHandler) SetSchedule(c *gin.Context) {
	workflowID := c.Param("id")
	wf, ok := h.loadOwnedWorkflow(c, workflowID)
	if !ok {
		return
	}
	var body struct {
		Frequency       string `json:"frequency"`
		IntervalSeconds int    `json:"interval_seconds"`
		RunTime         string `json:"run_time"`
		DayOfWeek       int    `json:"day_of_week"`
		DayOfMonth      int    `json:"day_of_month"`
		Repeat          *bool  `json:"repeat"`
		Enabled         *bool  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	validFreqs := map[string]bool{"interval": true, "hourly": true, "daily": true, "weekly": true, "monthly": true}
	if !validFreqs[body.Frequency] {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid frequency: %q", body.Frequency)})
		return
	}

	// Interval schedules can't fire faster than the scheduler's 60s tick.
	if body.Frequency == "interval" {
		if body.IntervalSeconds < 60 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "interval_seconds must be at least 60"})
			return
		}
	}

	// A published schedule is the only thing that spends money with nobody
	// watching, so its frequency is the limit that actually protects unit
	// economics. Refused rather than clamped here because the user is looking at a
	// form with a frequency picker — silently saving something they did not choose
	// would be discovered later, as a workflow that "isn't running".
	plan := h.planFor(c)
	if err := billing.CheckFrequency(plan, body.Frequency, body.IntervalSeconds); err != nil {
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error": err.Error(), "limit": billing.KindOf(err)})
		return
	}

	repeat := true
	if body.Repeat != nil {
		repeat = *body.Repeat
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	sched := h.upsertSchedule(wf.UserID, wf.OrganizationID, workflowID, models.ScheduledTrigger{
		Frequency:       body.Frequency,
		IntervalSeconds: body.IntervalSeconds,
		RunTime:         body.RunTime,
		DayOfWeek:       body.DayOfWeek,
		DayOfMonth:      body.DayOfMonth,
		Repeat:          repeat,
		Enabled:         enabled,
	})
	c.JSON(http.StatusOK, sched)
}

// upsertSchedule writes a workflow's schedule and recomputes next_run_at.
// Shared by the sidebar's REST endpoint and the AI builder's set_schedule tool
// so both produce identical rows.
func (h *WorkflowHandler) upsertSchedule(userID, orgID, workflowID string, in models.ScheduledTrigger) models.ScheduledTrigger {
	nextRun := calcNextRunAt(in, time.Now().UTC())

	var sched models.ScheduledTrigger
	if err := h.db.DB.Where("workflow_id = ?", workflowID).First(&sched).Error; err != nil {
		sched = in
		sched.UserID = userID
		sched.OrganizationID = orgID
		sched.WorkflowID = workflowID
		sched.NextRunAt = &nextRun
		h.db.DB.Create(&sched)
	} else {
		h.db.DB.Model(&sched).Updates(map[string]interface{}{
			"frequency":        in.Frequency,
			"interval_seconds": in.IntervalSeconds,
			"run_time":         in.RunTime,
			"day_of_week":      in.DayOfWeek,
			"day_of_month":     in.DayOfMonth,
			"repeat":           in.Repeat,
			"enabled":          in.Enabled,
			"next_run_at":      nextRun,
		})
		h.db.DB.Where("workflow_id = ?", workflowID).First(&sched)
	}
	return sched
}

// setScheduleForAI backs the builder's set_schedule tool. It writes through the
// same upsert the sidebar uses, so the AI can finish a "run every 2 minutes"
// request instead of handing the cadence back to the user.
func (h *WorkflowHandler) setScheduleForAI(c *gin.Context, workflowID string, input any) string {
	if workflowID == "" {
		return `{"error":"this workflow has not been saved yet, so it has no schedule — tell the user to save it, then ask again"}`
	}
	var wf models.Workflow
	if err := h.orgScope(c).First(&wf, "id = ?", workflowID).Error; err != nil {
		return `{"error":"workflow not found"}`
	}

	var body struct {
		Frequency       string   `json:"frequency"`
		IntervalMinutes *float64 `json:"interval_minutes"`
		RunTime         string   `json:"run_time"`
		DayOfWeek       int      `json:"day_of_week"`
		DayOfMonth      int      `json:"day_of_month"`
		Repeat          *bool    `json:"repeat"`
	}
	if b, err := json.Marshal(input); err == nil {
		_ = json.Unmarshal(b, &body)
	}

	validFreqs := map[string]bool{"interval": true, "hourly": true, "daily": true, "weekly": true, "monthly": true}
	if !validFreqs[body.Frequency] {
		return `{"error":"frequency must be one of interval, hourly, daily, weekly, monthly"}`
	}

	intervalSeconds := 0
	if body.Frequency == "interval" {
		if body.IntervalMinutes == nil || *body.IntervalMinutes < 1 {
			return `{"error":"frequency=interval needs interval_minutes of at least 1"}`
		}
		intervalSeconds = int(*body.IntervalMinutes * 60)
	}

	// The same plan limit the form enforces. This tool was previously unguarded, so
	// the builder could set a cadence the UI refuses — the worst kind of gap,
	// because it works until somebody opens the node and tries to change it.
	//
	// The refusal carries the fastest cadence that IS allowed, so the builder can
	// offer that instead of only reporting a failure.
	plan := h.planFor(c)
	if err := billing.CheckFrequency(plan, body.Frequency, intervalSeconds); err != nil {
		out, _ := json.Marshal(map[string]any{
			"error":           err.Error(),
			"limit":           billing.KindOf(err),
			"slowest_allowed": billing.SlowestAllowed(plan),
			"hint": "Tell the user this plan's limit, set the fastest cadence it does " +
				"allow instead, and mention that upgrading lifts it. Do not retry the " +
				"cadence that was refused.",
		})
		return string(out)
	}
	runTime := body.RunTime
	if runTime == "" {
		runTime = "09:00"
	}
	repeat := true
	if body.Repeat != nil {
		repeat = *body.Repeat
	}

	sched := h.upsertSchedule(wf.UserID, wf.OrganizationID, workflowID, models.ScheduledTrigger{
		Frequency:       body.Frequency,
		IntervalSeconds: intervalSeconds,
		RunTime:         runTime,
		DayOfWeek:       body.DayOfWeek,
		DayOfMonth:      body.DayOfMonth,
		Repeat:          repeat,
		Enabled:         true,
	})

	slog.InfoContext(c.Request.Context(), "schedule set by AI builder",
		"workflow_id", workflowID, "frequency", sched.Frequency, "interval_seconds", sched.IntervalSeconds)

	out, _ := json.Marshal(map[string]any{
		"status":           "saved",
		"frequency":        sched.Frequency,
		"interval_seconds": sched.IntervalSeconds,
		"run_time":         sched.RunTime,
		"next_run_at":      sched.NextRunAt,
		"published":        wf.Published,
		"message": "Schedule saved — do NOT ask the user to set the cadence themselves. " +
			"Times are UTC. If published is false, tell them to hit Publish, since only published workflows run on schedule.",
	})
	return string(out)
}

// DELETE /api/workflows/:id/schedule
func (h *WorkflowHandler) DeleteSchedule(c *gin.Context) {
	workflowID := c.Param("id")
	if _, ok := h.loadOwnedWorkflow(c, workflowID); !ok {
		return
	}
	h.db.DB.Where("workflow_id = ?", workflowID).Delete(&models.ScheduledTrigger{})
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}
