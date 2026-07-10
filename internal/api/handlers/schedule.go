package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/hub"

	"github.com/gin-gonic/gin"
)

var schedulerOnce sync.Once

// StartScheduler starts the background scheduler (call once at server boot).
func (h *WorkflowHandler) StartScheduler() {
	schedulerOnce.Do(func() {
		go h.scheduleLoop()
	})
}

func (h *WorkflowHandler) scheduleLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		<-ticker.C
		h.runDueSchedules()
	}
}

// calcNextRunAt returns the next UTC time this schedule should fire after `from`.
func calcNextRunAt(s models.ScheduledTrigger, from time.Time) time.Time {
	loc := time.UTC
	var h, m int
	fmt.Sscanf(s.RunTime, "%d:%d", &h, &m)

	switch s.Frequency {
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
	h.db.DB.Where("enabled = true AND next_run_at IS NOT NULL AND next_run_at <= ?", now).Find(&schedules)

	for _, sched := range schedules {
		slog.Info("scheduler: firing workflow", "workflow_id", sched.WorkflowID, "frequency", sched.Frequency)

		if sched.Repeat {
			nextRun := calcNextRunAt(sched, now)
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

		go h.runWorkflowByID(sched.WorkflowID)
	}
}

func (h *WorkflowHandler) runWorkflowByID(workflowID string) {
	var workflow models.Workflow
	if err := h.db.DB.First(&workflow, "id = ?", workflowID).Error; err != nil {
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
		slog.Warn("scheduler: workflow has no scheduledTrigger node, disabling schedule", "workflow_id", workflowID)
		h.db.DB.Model(&models.ScheduledTrigger{}).Where("workflow_id = ?", workflowID).Update("enabled", false)
		return
	}

	ast := executor.WorkflowAST{Version: "1.0", Name: workflow.Name, Nodes: nodes, Edges: edges}
	keys := executor.APIKeys{Anthropic: os.Getenv("ANTHROPIC_API_KEY"), OpenAI: os.Getenv("OPENAI_API_KEY"), Brave: os.Getenv("BRAVE_API_KEY"), Jina: os.Getenv("JINA_API_KEY")}

	run := models.WorkflowRun{UserID: workflow.UserID, WorkflowID: workflowID, WorkflowName: workflow.Name, Status: models.RunStatusRunning}
	h.db.DB.Create(&run)
	runID := run.ID.String()

	// Notify any open canvas pages for this workflow so they can attach immediately.
	hub.Workflow.Publish(workflowID, runID)

	var events []executor.ExecutionEvent
	startTime := time.Now()
	finalStatus := models.RunStatusCompleted
	// The schedule fires with no request context — the loaded workflow's
	// owner is what routes integration tokens to the right user.
	executor.RunWorkflow(context.Background(), ast, keys, runID, workflow.UserID, func(ev executor.ExecutionEvent) {
		ev.Timestamp = time.Since(startTime).Milliseconds()
		events = append(events, ev)
		hub.Global.Publish(runID, ev)
		if ev.Type == executor.EventWorkflowError {
			finalStatus = models.RunStatusError
		}
	})
	eventsJSON, _ := json.Marshal(events)
	h.db.DB.Model(&run).Updates(map[string]interface{}{
		"status": finalStatus,
		"events": models.JSONB(eventsJSON),
	})
	hub.Global.ClearBuffer(runID)
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
		Frequency  string `json:"frequency"`
		RunTime    string `json:"run_time"`
		DayOfWeek  int    `json:"day_of_week"`
		DayOfMonth int    `json:"day_of_month"`
		Repeat     *bool  `json:"repeat"`
		Enabled    *bool  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	validFreqs := map[string]bool{"hourly": true, "daily": true, "weekly": true, "monthly": true}
	if !validFreqs[body.Frequency] {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid frequency: %q", body.Frequency)})
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

	now := time.Now().UTC()
	tmp := models.ScheduledTrigger{
		Frequency:  body.Frequency,
		RunTime:    body.RunTime,
		DayOfWeek:  body.DayOfWeek,
		DayOfMonth: body.DayOfMonth,
	}
	nextRun := calcNextRunAt(tmp, now)

	var sched models.ScheduledTrigger
	if err := h.db.DB.Where("workflow_id = ?", workflowID).First(&sched).Error; err != nil {
		sched = models.ScheduledTrigger{
			UserID:     wf.UserID,
			WorkflowID: workflowID,
			Frequency:  body.Frequency,
			RunTime:    body.RunTime,
			DayOfWeek:  body.DayOfWeek,
			DayOfMonth: body.DayOfMonth,
			Repeat:     repeat,
			Enabled:    enabled,
			NextRunAt:  &nextRun,
		}
		h.db.DB.Create(&sched)
	} else {
		h.db.DB.Model(&sched).Updates(map[string]interface{}{
			"frequency":    body.Frequency,
			"run_time":     body.RunTime,
			"day_of_week":  body.DayOfWeek,
			"day_of_month": body.DayOfMonth,
			"repeat":       repeat,
			"enabled":      enabled,
			"next_run_at":  nextRun,
		})
		h.db.DB.Where("workflow_id = ?", workflowID).First(&sched)
	}

	c.JSON(http.StatusOK, sched)
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
