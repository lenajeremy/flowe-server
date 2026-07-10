package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/hub"

	"github.com/gin-gonic/gin"
)

// GET /api/workflows/:id/webhook  — get (or create) webhook for this workflow
func (h *WorkflowHandler) GetWebhook(c *gin.Context) {
	workflowID := c.Param("id")
	wf, ok := h.loadOwnedWorkflow(c, workflowID)
	if !ok {
		return
	}
	var wh models.WebhookTrigger
	if err := h.db.DB.Where("workflow_id = ?", workflowID).First(&wh).Error; err != nil {
		// Create one; handle races by re-fetching on unique constraint violation
		token, _ := randomHex(24)
		wh = models.WebhookTrigger{UserID: wf.UserID, WorkflowID: workflowID, Token: token}
		if err2 := h.db.DB.Create(&wh).Error; err2 != nil {
			if err3 := h.db.DB.Where("workflow_id = ?", workflowID).First(&wh).Error; err3 != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err2.Error()})
				return
			}
		}
	}
	c.JSON(http.StatusOK, wh)
}

// GET /api/webhooks/:token  — return public info about a webhook (workflow name, id)
func (h *WorkflowHandler) WebhookInfo(c *gin.Context) {
	token := c.Param("token")
	var wh models.WebhookTrigger
	if err := h.db.DB.Where("token = ?", token).First(&wh).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
		return
	}
	var workflow models.Workflow
	if err := h.db.DB.First(&workflow, "id = ?", wh.WorkflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token":         wh.Token,
		"workflow_id":   wh.WorkflowID,
		"workflow_name": workflow.Name,
	})
}

// DELETE /api/workflows/:id/webhook  — regenerate token (delete + GetWebhook will recreate)
func (h *WorkflowHandler) DeleteWebhook(c *gin.Context) {
	workflowID := c.Param("id")
	if _, ok := h.loadOwnedWorkflow(c, workflowID); !ok {
		return
	}
	h.db.DB.Where("workflow_id = ?", workflowID).Delete(&models.WebhookTrigger{})
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// POST /api/webhooks/:token  — receive external webhook, trigger workflow
func (h *WorkflowHandler) ReceiveWebhook(c *gin.Context) {
	token := c.Param("token")
	var wh models.WebhookTrigger
	if err := h.db.DB.Where("token = ?", token).First(&wh).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
		return
	}

	var workflow models.Workflow
	if err := h.db.DB.First(&workflow, "id = ?", wh.WorkflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	// Read incoming body as payload
	var payload map[string]interface{}
	c.ShouldBindJSON(&payload)

	run := models.WorkflowRun{
		UserID:       workflow.UserID,
		WorkflowID:   wh.WorkflowID,
		WorkflowName: workflow.Name,
		Status:       models.RunStatusRunning,
	}
	h.db.DB.Create(&run)

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
	slog.Info("webhook triggered", "run_id", runID, "workflow_id", wh.WorkflowID, "workflow_name", workflow.Name)
	hub.Workflow.Publish(wh.WorkflowID, runID)

	// Inject webhook payload into webhookTrigger nodes via DefaultValue
	payloadJSON, _ := json.Marshal(payload)
	payloadStr := string(payloadJSON)
	for i := range ast.Nodes {
		if ast.Nodes[i].Data.NodeType == executor.NodeTypeWebhookTrigger {
			ast.Nodes[i].Data.DefaultValue = &payloadStr
		}
	}

	go func() {
		var events []executor.ExecutionEvent
		startTime := time.Now()
		finalStatus := models.RunStatusCompleted
		executor.RunWorkflow(context.Background(), ast, keys, runID, workflow.UserID, func(event executor.ExecutionEvent) {
			event.Timestamp = time.Since(startTime).Milliseconds()
			events = append(events, event)
			hub.Global.Publish(runID, event)
			slog.Debug("webhook run event", "run_id", runID, "type", event.Type, "node_id", event.NodeID)
			if event.Type == executor.EventWorkflowError {
				finalStatus = models.RunStatusError
			}
		})
		slog.Info("webhook run finished", "run_id", runID, "status", finalStatus, "event_count", len(events))
		eventsJSON, _ := json.Marshal(events)
		h.db.DB.Model(&run).Updates(map[string]interface{}{
			"status": finalStatus,
			"events": models.JSONB(eventsJSON),
		})
		hub.Global.ClearBuffer(runID)
	}()

	c.JSON(http.StatusAccepted, gin.H{"run_id": runID, "status": "running"})
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}
