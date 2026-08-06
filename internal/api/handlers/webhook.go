package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
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
		wh = models.WebhookTrigger{UserID: wf.UserID, OrganizationID: wf.OrganizationID,
			WorkflowID: workflowID, Token: token}
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
	ctx := c.Request.Context()
	token := c.Param("token")
	var wh models.WebhookTrigger
	if err := h.db.DB.Where("token = ?", token).First(&wh).Error; err != nil {
		slog.WarnContext(ctx, "webhook rejected", "reason", "unknown_token")
		telemetry.WebhookReceived(ctx, "unknown_token")
		c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
		return
	}

	var workflow models.Workflow
	if err := h.db.DB.First(&workflow, "id = ?", wh.WorkflowID).Error; err != nil {
		slog.WarnContext(ctx, "webhook rejected", "reason", "workflow_not_found", "workflow_id", wh.WorkflowID)
		telemetry.WebhookReceived(ctx, "unknown_token")
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	// Read incoming body as payload
	var payload map[string]interface{}
	payloadStatus := "accepted"
	if err := c.ShouldBindJSON(&payload); err != nil && c.Request.ContentLength > 0 {
		payloadStatus = "invalid"
		slog.WarnContext(ctx, "webhook payload invalid", "workflow_id", wh.WorkflowID, "workflow_name", workflow.Name)
	}

	payloadJSON, _ := json.Marshal(payload)
	p, admitErr := h.admitRun(ctx, wh.WorkflowID, "webhook",
		injectInto(executor.NodeTypeWebhookTrigger, "", string(payloadJSON)))
	if admitErr != nil {
		// 402 rather than a generic failure: the caller's webhook is fine, the
		// account is out of credit, and those need different responses at the
		// sender's end.
		c.JSON(http.StatusPaymentRequired, gin.H{"error": admitErr.Error()})
		return
	}

	runID := p.RunID()
	slog.InfoContext(ctx, "webhook received",
		"run_id", runID, "workflow_id", wh.WorkflowID, "workflow_name", workflow.Name,
		"payload_bytes", c.Request.ContentLength)
	telemetry.WebhookReceived(ctx, payloadStatus)

	// Link the detached background run to the webhook request's trace.
	bgCtx := trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(c.Request.Context()))
	go h.executeRun(bgCtx, p)

	c.JSON(http.StatusAccepted, gin.H{"run_id": runID, "status": "running"})
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}
