package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"workflow-ai/server/config"
	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/hub"
	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

type WorkflowHandler struct {
	db    *database.DBClient
	redis *redis.Client
}

func NewWorkflowHandler(db *database.DBClient, rdb *redis.Client) *WorkflowHandler {
	return &WorkflowHandler{db: db, redis: rdb}
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

	// Persist run record
	run := &models.WorkflowRun{
		UserID:       uid,
		WorkflowID:   req.WorkflowID,
		WorkflowName: req.Workflow.Name,
		Status:       models.RunStatusRunning,
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

	executor.RunWorkflow(executor.WithTrigger(c.Request.Context(), "manual"), req.Workflow, keys, runID, uid, emit)

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

// loadOwnedWorkflow fetches a workflow only if it belongs to the session
// user; otherwise it writes a 404 (never 403 — don't leak existence).
func (h *WorkflowHandler) loadOwnedWorkflow(c *gin.Context, id string) (*models.Workflow, bool) {
	var wf models.Workflow
	if err := h.db.DB.First(&wf, "id = ? AND user_id = ?", id, auth.UserID(c)).Error; err != nil {
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

	wf := &models.Workflow{
		UserID: auth.UserID(c),
		Name:   body.Name,
		Nodes:  models.JSONB(body.Nodes),
		Edges:  models.JSONB(body.Edges),
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
			created_at, updated_at`).
		Where("user_id = ?", auth.UserID(c)).
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

// Delete — DELETE /api/workflows/:id
func (h *WorkflowHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	if err := h.db.DB.Where("user_id = ?", auth.UserID(c)).
		Delete(&models.Workflow{}, "id = ?", id).Error; err != nil {
		slog.Error("failed to delete workflow", "id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete workflow"})
		return
	}
	c.Status(http.StatusNoContent)
}
