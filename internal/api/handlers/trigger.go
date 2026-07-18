package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/hub"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
)

// POST /api/trigger/:workflowId
func (h *WorkflowHandler) TriggerWorkflow(c *gin.Context) {
	// Validate Bearer token
	authHeader := c.GetHeader("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Bearer token required"})
		return
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")

	// Hash token and look up in ApiKey table
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	var apiKey models.ApiKey
	if err := h.db.DB.Where("key_hash = ?", hash).First(&apiKey).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
		return
	}

	// Update last used
	now := time.Now()
	h.db.DB.Model(&apiKey).Update("last_used_at", now)

	// Get workflow — the key must belong to the workflow's owner (404, not
	// 403, so foreign workflow IDs are indistinguishable from missing ones).
	workflowID := c.Param("workflowId")
	var workflow models.Workflow
	if err := h.db.DB.First(&workflow, "id = ?", workflowID).Error; err != nil || workflow.UserID != apiKey.UserID {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	// Parse input
	var body struct {
		Input map[string]interface{} `json:"input"`
		Async bool                   `json:"async"`
	}
	c.ShouldBindJSON(&body)

	// Create run record
	run := models.WorkflowRun{
		UserID:       workflow.UserID,
		WorkflowID:   workflowID,
		WorkflowName: workflow.Name,
		Status:       models.RunStatusRunning,
	}
	h.db.DB.Create(&run)

	// Build WorkflowAST from workflow
	var nodes []executor.WorkflowASTNode
	var edges []executor.WorkflowASTEdge
	json.Unmarshal(workflow.Nodes, &nodes)
	json.Unmarshal(workflow.Edges, &edges)

	ast := executor.WorkflowAST{
		Version: "1.0",
		Name:    workflow.Name,
		Nodes:   nodes,
		Edges:   edges,
	}

	keys := executor.APIKeys{
		Anthropic: os.Getenv("ANTHROPIC_API_KEY"),
		OpenAI:    os.Getenv("OPENAI_API_KEY"),
		Brave:     os.Getenv("BRAVE_API_KEY"),
		Jina:      os.Getenv("JINA_API_KEY"),
	}

	runID := run.ID.String()
	slog.InfoContext(c.Request.Context(), "api trigger accepted",
		"run_id", runID, "workflow_id", workflowID, "workflow_name", workflow.Name)

	// Link the detached background run to the API request's trace.
	bgCtx := trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(c.Request.Context()))
	go func() {
		var events []executor.ExecutionEvent
		startTime := time.Now()

		var execErr error
		doneCh := make(chan struct{})

		go func() {
			defer close(doneCh)
			executor.RunWorkflow(executor.WithTrigger(bgCtx, "api"), ast, keys, runID, workflow.UserID, func(event executor.ExecutionEvent) {
				event.Timestamp = time.Since(startTime).Milliseconds()
				events = append(events, event)
				hub.Global.Publish(runID, event)
				if event.Type == executor.EventWorkflowError {
					execErr = fmt.Errorf("%s", event.Message)
				}
			})
		}()

		<-doneCh

		status := models.RunStatusCompleted
		errMsg := ""
		if execErr != nil {
			status = models.RunStatusError
			errMsg = execErr.Error()
		}

		eventsJSON, _ := json.Marshal(events)
		h.db.DB.Model(&run).Updates(map[string]interface{}{
			"status":        status,
			"error_message": errMsg,
			"events":        models.JSONB(eventsJSON),
		})
		hub.Global.ClearBuffer(runID)
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"run_id":      runID,
		"status":      "running",
		"workflow_id": workflowID,
	})
}
