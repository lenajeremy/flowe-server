package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"workflow-ai/server/config"
	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/hub"
	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type WorkflowHandler struct {
	db    *database.DBClient
	redis *redis.Client
	// bill admits runs, checks balances and posts spend. Always non-nil, so no
	// handler needs a "is billing on?" branch — a deployment without billing is
	// expressed by generous plan limits, not by a missing gate.
	bill *billing.Gate
}

func NewWorkflowHandler(db *database.DBClient, rdb *redis.Client) *WorkflowHandler {
	return &WorkflowHandler{db: db, redis: rdb, bill: billing.New(db.DB)}
}

// ── Run (SSE) ─────────────────────────────────────────────────

func (h *WorkflowHandler) Run(c *gin.Context) {
	var req executor.RunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	uid := auth.UserID(c)
	// Per-user cap: runs execute LLM/HTTP work, so throttle abuse.
	if !auth.Allow(c.Request.Context(), h.redis, "rl:run:"+uid, 60, time.Minute) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many runs — try again in a minute"})
		return
	}
	// A run attributed to a saved workflow must target one the user owns.
	if req.WorkflowID != "" {
		if _, ok := h.loadOwnedWorkflow(c, req.WorkflowID); !ok {
			return
		}
	}

	// Credit check before anything runs. Stopping partway through would leave a
	// half-finished workflow that has already sent the email or charged the card,
	// so the run id is minted here and the budget reserved against it first.
	runIDPre := uuid.New()
	res, err := h.bill.AdmitRun(currentOrgID(c), uid, runIDPre.String())
	if err != nil {
		if errors.Is(err, billing.ErrOverCap) {
			c.JSON(http.StatusPaymentRequired, gin.H{"error": err.Error()})
			return
		}
		slog.ErrorContext(c.Request.Context(), "run admission failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not start the run"})
		return
	}
	defer h.bill.Finish(res)

	// Persist run record
	run := &models.WorkflowRun{
		BaseModel:      models.BaseModel{ID: runIDPre},
		UserID:         uid,
		OrganizationID: currentOrgID(c),
		WorkflowID:     req.WorkflowID,
		WorkflowName:   req.Workflow.Name,
		Status:         models.RunStatusRunning,
	}
	if err := h.db.DB.Create(run).Error; err != nil {
		slog.Error("failed to persist workflow run", "error", err)
	}

	// Notify workflow-level subscribers that a run has started (powers the
	// WorkflowEvents SSE so the editor can auto-attach to the stream).
	if req.WorkflowID != "" {
		hub.Workflow.Publish(req.WorkflowID, run.ID.String())
	}

	// SSE setup
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}
	telemetry.AddSSEStream(c.Request.Context(), "run", 1)
	defer telemetry.AddSSEStream(c.Request.Context(), "run", -1)

	finalStatus := models.RunStatusCompleted
	var errMsg string
	var bufferedEvents []executor.ExecutionEvent

	runID := run.ID.String()
	emit := func(event executor.ExecutionEvent) {
		// Publish to hub so /runs/:id/stream subscribers (e.g. the approval page)
		// receive events in real time.
		hub.Global.Publish(runID, event)

		data, _ := json.Marshal(event)
		fmt.Fprintf(c.Writer, "data: %s\n\n", data)
		flusher.Flush()

		bufferedEvents = append(bufferedEvents, event)

		if event.Type == executor.EventWorkflowError {
			finalStatus = models.RunStatusError
			errMsg = event.Message
		}
	}

	keys := executor.APIKeys{
		Anthropic: config.GetEnv("ANTHROPIC_API_KEY"),
		OpenAI:    config.GetEnv("OPENAI_API_KEY"),
		Brave:     config.GetEnv("BRAVE_API_KEY"),
		Jina:      config.GetEnv("JINA_API_KEY"),
	}

	runCtx := executor.WithWorkflowID(c.Request.Context(), req.WorkflowID)
	// Carries the plan (for the per-call token ceiling) and the hold, so each spend
	// settles against this run's reservation rather than only the balance.
	runCtx = res.Context(runCtx, runID)
	executor.RunWorkflow(executor.WithTrigger(runCtx, "manual"), req.Workflow, keys, runID, uid, currentOrgID(c), emit)

	// Serialize buffered events and update run record
	eventsJSON, _ := json.Marshal(bufferedEvents)
	updates := map[string]any{
		"status": finalStatus,
		"events": models.JSONB(eventsJSON),
	}
	if errMsg != "" {
		updates["error_message"] = errMsg
	}
	h.db.DB.Model(run).Updates(updates)

	// Drop the in-memory event buffer now that events are in DB.
	hub.Global.ClearBuffer(runID)
}

// ── Workflow CRUD ─────────────────────────────────────────────

type workflowBody struct {
	Name  string          `json:"name"  binding:"required"`
	Nodes json.RawMessage `json:"nodes"`
	Edges json.RawMessage `json:"edges"`
	// Pointer: editor saves omit it, and that must not blank a stored value.
	Description *string `json:"description"`
}

// loadOwnedWorkflow fetches a workflow only if it belongs to the requesting
// org; otherwise it writes a 404 (never 403 — don't leak existence).
func (h *WorkflowHandler) loadOwnedWorkflow(c *gin.Context, id string) (*models.Workflow, bool) {
	var wf models.Workflow
	if err := h.orgScope(c).First(&wf, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return nil, false
	}
	return &wf, true
}

// Create — POST /api/workflows
func (h *WorkflowHandler) Create(c *gin.Context) {
	var body workflowBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.bill.CheckWorkflowCount(currentOrgID(c), h.planFor(c)); err != nil {
		if errors.Is(err, billing.ErrLimit) {
			c.JSON(http.StatusPaymentRequired, gin.H{"error": err.Error(), "limit": "workflows"})
			return
		}
		slog.Error("workflow count check failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save workflow"})
		return
	}

	wf := &models.Workflow{
		UserID:         auth.UserID(c),
		OrganizationID: currentOrgID(c),
		Name:           body.Name,
		Nodes:          models.JSONB(body.Nodes),
		Edges:          models.JSONB(body.Edges),
	}
	if body.Description != nil {
		wf.Description = *body.Description
	}
	if err := h.db.DB.Create(wf).Error; err != nil {
		slog.Error("failed to create workflow", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save workflow"})
		return
	}
	c.JSON(http.StatusCreated, wf)
}

// Update — PUT /api/workflows/:id
func (h *WorkflowHandler) Update(c *gin.Context) {
	id := c.Param("id")
	var body workflowBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	wf, ok := h.loadOwnedWorkflow(c, id)
	if !ok {
		return
	}

	wf.Name = body.Name
	wf.Nodes = models.JSONB(body.Nodes)
	wf.Edges = models.JSONB(body.Edges)
	if body.Description != nil {
		wf.Description = *body.Description
	}

	if err := h.db.DB.Save(wf).Error; err != nil {
		slog.Error("failed to update workflow", "id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workflow"})
		return
	}
	c.JSON(http.StatusOK, wf)
}

// workflowSummary is the list payload: metadata only. Nodes/edges are heavy
// JSONB and belong to GetOne — the list view never renders them.
type workflowSummary struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	NodeCount   int          `json:"node_count"`
	NodeTypes   models.JSONB `json:"node_types"` // distinct node types, for card icons
	Published   bool         `json:"published"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// List — GET /api/workflows
func (h *WorkflowHandler) List(c *gin.Context) {
	summaries := []workflowSummary{}
	if err := h.db.DB.Model(&models.Workflow{}).
		Select(`id, name, description, jsonb_array_length(nodes) AS node_count,
			(SELECT COALESCE(jsonb_agg(DISTINCT n->'data'->>'nodeType'), '[]'::jsonb)
			 FROM jsonb_array_elements(nodes) n) AS node_types,
			published, created_at, updated_at`).
		Where("organization_id = ?", orgIDOrDeny(c)).
		Order("updated_at desc").Scan(&summaries).Error; err != nil {
		slog.Error("failed to list workflows", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workflows"})
		return
	}
	c.JSON(http.StatusOK, summaries)
}

// GetOne — GET /api/workflows/:id
func (h *WorkflowHandler) GetOne(c *gin.Context) {
	wf, ok := h.loadOwnedWorkflow(c, c.Param("id"))
	if !ok {
		return
	}
	c.JSON(http.StatusOK, wf)
}

// SetPublished — POST /api/workflows/:id/publish and /unpublish.
// Publishing only affects the background scheduler: manual runs, webhooks, and
// API-key triggers work either way.
func (h *WorkflowHandler) SetPublished(published bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		wf, ok := h.loadOwnedWorkflow(c, c.Param("id"))
		if !ok {
			return
		}
		// Only publishing is gated; unpublishing must always work, or a customer who
		// hits a limit cannot get back under it.
		if published {
			if err := h.bill.CheckPublishSchedule(currentOrgID(c), wf.ID.String(), h.planFor(c)); err != nil {
				if errors.Is(err, billing.ErrLimit) {
					c.JSON(http.StatusPaymentRequired, gin.H{"error": err.Error(), "limit": "published_schedules"})
					return
				}
				slog.Error("publish limit check failed", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workflow"})
				return
			}
		}
		if err := h.db.DB.Model(wf).Update("published", published).Error; err != nil {
			slog.Error("failed to set published", "id", wf.ID, "published", published, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workflow"})
			return
		}

		// While unpublished a schedule sits dormant with next_run_at in the past.
		// Re-anchor it on publish so going live schedules the next occurrence
		// instead of instantly firing an overdue one.
		if published {
			var sched models.ScheduledTrigger
			if err := h.db.DB.Where("workflow_id = ?", wf.ID.String()).First(&sched).Error; err == nil {
				now := time.Now().UTC()
				if sched.NextRunAt == nil || sched.NextRunAt.Before(now) {
					next := calcNextRunAt(sched, now)
					h.db.DB.Model(&sched).Update("next_run_at", next)
				}
			}
		}

		slog.InfoContext(c.Request.Context(), "workflow publish state changed",
			"workflow_id", wf.ID.String(), "workflow", wf.Name, "published", published)
		c.JSON(http.StatusOK, gin.H{"id": wf.ID.String(), "published": published})
	}
}

// Delete — DELETE /api/workflows/:id
func (h *WorkflowHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	if err := h.orgScope(c).
		Delete(&models.Workflow{}, "id = ?", id).Error; err != nil {
		slog.Error("failed to delete workflow", "id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete workflow"})
		return
	}
	c.Status(http.StatusNoContent)
}
