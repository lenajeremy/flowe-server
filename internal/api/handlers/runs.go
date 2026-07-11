package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/hub"

	"github.com/gin-gonic/gin"
)

// GET /api/workflows/:id/events — SSE stream that fires once per run start for this workflow.
// The frontend subscribes to this so it can attach to a run stream before the run completes.
func (h *WorkflowHandler) WorkflowEvents(c *gin.Context) {
	workflowID := c.Param("id")
	if _, ok := h.loadOwnedWorkflow(c, workflowID); !ok {
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	ch := hub.Workflow.Subscribe(workflowID)
	defer hub.Workflow.Unsubscribe(workflowID, ch)

	ctx := c.Request.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case runID, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(c.Writer, "data: %s\n\n", runID)
			flusher.Flush()
		}
	}
}

// GET /api/workflows/:id/runs/active — returns the currently-running run for a workflow (if any)
func (h *WorkflowHandler) GetActiveRun(c *gin.Context) {
	workflowID := c.Param("id")
	if _, ok := h.loadOwnedWorkflow(c, workflowID); !ok {
		return
	}
	var run models.WorkflowRun
	err := h.db.DB.
		Where("workflow_id = ? AND status = ?", workflowID, models.RunStatusRunning).
		Order("created_at DESC").
		First(&run).Error
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active run"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"run_id": run.ID.String()})
}

// runSummary is the run-history payload: status + timing only. The full
// event log (JSONB, often large) is served by GetRun when a run is opened.
type runSummary struct {
	ID           string    `json:"id"`
	WorkflowID   string    `json:"workflow_id"`
	WorkflowName string    `json:"workflow_name"`
	Status       string    `json:"status"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// GET /api/workflows/:id/runs
func (h *WorkflowHandler) ListRuns(c *gin.Context) {
	workflowID := c.Param("id")
	if _, ok := h.loadOwnedWorkflow(c, workflowID); !ok {
		return
	}
	summaries := []runSummary{}
	if err := h.db.DB.Model(&models.WorkflowRun{}).
		Select("id, workflow_id, workflow_name, status, error_message, created_at, updated_at").
		Where("workflow_id = ?", workflowID).
		Order("created_at DESC").Limit(50).Scan(&summaries).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, summaries)
}

// GET /api/runs/:id
func (h *WorkflowHandler) GetRun(c *gin.Context) {
	runID := c.Param("id")
	var run models.WorkflowRun
	if err := h.db.DB.First(&run, "id = ?", runID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	c.JSON(http.StatusOK, run)
}

// GET /api/runs/:id/stream — SSE stream for a specific run (live or replayed from DB)
func (h *WorkflowHandler) StreamRun(c *gin.Context) {
	runID := c.Param("id")

	var run models.WorkflowRun
	if err := h.db.DB.First(&run, "id = ?", runID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	// If run already finished, replay stored events from DB and return
	if run.Status != models.RunStatusRunning {
		slog.Info("stream: replaying completed run from DB", "run_id", runID, "status", run.Status)
		var events []executor.ExecutionEvent
		if err := json.Unmarshal(run.Events, &events); err == nil {
			for _, ev := range events {
				data, _ := json.Marshal(ev)
				fmt.Fprintf(c.Writer, "data: %s\n\n", data)
			}
			slog.Info("stream: replayed events", "run_id", runID, "count", len(events))
		} else {
			slog.Error("stream: failed to unmarshal events", "run_id", runID, "error", err)
		}
		flusher.Flush()
		return
	}

	// Run is still in progress — subscribe to live events (buffer replays past events).
	slog.Info("stream: subscribing to live run", "run_id", runID)
	ch := hub.Global.Subscribe(runID)
	defer hub.Global.Unsubscribe(runID, ch)

	// Guard against the race where the run completed (and ClearBuffer was called)
	// between our initial status check and the Subscribe call above.
	// If the channel is empty and the run is no longer running, replay from DB.
	if len(ch) == 0 {
		if err := h.db.DB.First(&run, "id = ?", runID).Error; err == nil &&
			run.Status != models.RunStatusRunning {
			slog.Info("stream: run finished before subscribe — replaying from DB", "run_id", runID, "status", run.Status)
			var events []executor.ExecutionEvent
			if err2 := json.Unmarshal(run.Events, &events); err2 == nil {
				for _, ev := range events {
					data, _ := json.Marshal(ev)
					fmt.Fprintf(c.Writer, "data: %s\n\n", data)
				}
			}
			flusher.Flush()
			return
		}
	}

	ctx := c.Request.Context()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				slog.Info("stream: channel closed", "run_id", runID)
				return
			}
			slog.Debug("stream: sending event", "run_id", runID, "type", event.Type)
			data, _ := json.Marshal(event)
			fmt.Fprintf(c.Writer, "data: %s\n\n", data)
			flusher.Flush()
			if event.Type == executor.EventWorkflowCompleted || event.Type == executor.EventWorkflowError {
				slog.Info("stream: run finished", "run_id", runID, "type", event.Type)
				return
			}
		case <-ticker.C:
			// Periodically re-check DB in case the run finished but we missed the final event
			// (e.g. server restart left the run as orphaned "running").
			var latest models.WorkflowRun
			if err := h.db.DB.First(&latest, "id = ?", runID).Error; err != nil {
				continue
			}
			if latest.Status != models.RunStatusRunning {
				slog.Info("stream: detected run no longer running via poll", "run_id", runID, "status", latest.Status)
				var events []executor.ExecutionEvent
				if err := json.Unmarshal(latest.Events, &events); err == nil {
					for _, ev := range events {
						data, _ := json.Marshal(ev)
						fmt.Fprintf(c.Writer, "data: %s\n\n", data)
					}
				}
				flusher.Flush()
				return
			}
		}
	}
}

// POST /api/runs/:runId/node/:nodeId/approve
func (h *WorkflowHandler) ApproveRun(c *gin.Context) {
	runID := c.Param("runId")
	nodeID := c.Param("nodeId")
	key := runID + ":" + nodeID
	if !executor.ResolveApproval(key, true) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval for this run/node"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "approved"})
}

// POST /api/runs/:runId/node/:nodeId/reject
func (h *WorkflowHandler) RejectRun(c *gin.Context) {
	runID := c.Param("runId")
	nodeID := c.Param("nodeId")
	key := runID + ":" + nodeID
	if !executor.ResolveApproval(key, false) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval for this run/node"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "rejected"})
}
