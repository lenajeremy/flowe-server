package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const maxCodingAgentToolCalls = 50

func (h *WorkflowHandler) CodingAgentCapabilities(c *gin.Context) {
	var ast executor.WorkflowAST
	if err := c.ShouldBindJSON(&ast); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflow graph is invalid"})
		return
	}
	if len(ast.Nodes) > 500 || len(ast.Edges) > 2000 {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "workflow graph is too large"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"capabilities":  agentIntegrationCapabilities(ast),
		"defaultPolicy": defaultSafeAgentPolicy(ast),
	})
}

func (h *WorkflowHandler) ListCodingAgentToolCalls(c *gin.Context) {
	jobID := strings.TrimSpace(c.Query("jobId"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jobId is required"})
		return
	}
	var job models.CodingAgentJob
	if err := h.db.DB.Where(
		"id = ? AND organization_id = ? AND user_id = ?", jobID, currentOrgID(c), auth.UserID(c),
	).First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "coding agent job not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load coding agent tool calls"})
		return
	}
	var calls []models.CodingAgentToolCall
	if err := h.db.DB.Where("job_id = ? AND organization_id = ?", jobID, currentOrgID(c)).
		Order("requested_at ASC").Find(&calls).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load coding agent tool calls"})
		return
	}
	c.JSON(http.StatusOK, calls)
}

func (h *WorkflowHandler) GetCodingAgentToolCall(c *gin.Context) {
	var call models.CodingAgentToolCall
	if err := h.db.DB.WithContext(c.Request.Context()).Where(
		"id = ? AND organization_id = ? AND user_id = ?", c.Param("id"), currentOrgID(c), auth.UserID(c),
	).First(&call).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "coding agent tool call not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load coding agent tool call"})
		return
	}
	c.JSON(http.StatusOK, call)
}

func (h *WorkflowHandler) ApproveCodingAgentToolCall(c *gin.Context) {
	h.resolveCodingAgentToolCall(c, true)
}

func (h *WorkflowHandler) RejectCodingAgentToolCall(c *gin.Context) {
	h.resolveCodingAgentToolCall(c, false)
}

func (h *WorkflowHandler) ReconcileCodingAgentToolCall(c *gin.Context) {
	var request struct {
		Outcome string `json:"outcome"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || (request.Outcome != "completed" && request.Outcome != "not_completed") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "outcome must be completed or not_completed"})
		return
	}
	var call models.CodingAgentToolCall
	var candidate models.CodingAgentToolCall
	if err := h.db.DB.WithContext(c.Request.Context()).Where(
		"id = ? AND organization_id = ? AND user_id = ?", c.Param("id"), currentOrgID(c), auth.UserID(c),
	).First(&candidate).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "coding agent tool call not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not reconcile coding agent tool call"})
		}
		return
	}
	err := h.db.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var job models.CodingAgentJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND user_id = ?", candidate.JobID, currentOrgID(c), auth.UserID(c),
		).First(&job).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND job_id = ? AND organization_id = ? AND user_id = ?",
			candidate.ID, job.ID, currentOrgID(c), auth.UserID(c),
		).First(&call).Error; err != nil {
			return err
		}
		if call.Status != models.CodingAgentToolCallOutcomeUnknown {
			return errToolCallAlreadyResolved
		}
		now := time.Now().UTC()
		status := models.CodingAgentToolCallFailed
		message := "Owner confirmed coding agent tool call did not complete"
		result := models.JSONB(`{"reconciled":true,"completed":false}`)
		if request.Outcome == "completed" {
			status = models.CodingAgentToolCallSucceeded
			message = "Owner confirmed coding agent tool call completed externally"
			result = models.JSONB(`{"reconciled":true,"completed":true}`)
		}
		if err := tx.Model(&call).Updates(map[string]any{
			"status": status, "result": result, "last_error": "", "completed_at": now,
			"approved_by_user_id": auth.UserID(c),
		}).Error; err != nil {
			return err
		}
		call.Status, call.Result, call.LastError, call.CompletedAt = status, result, "", &now
		return appendCodingAgentEventTx(tx, &job, "tool_reconciled", message, map[string]any{
			"toolCallId": call.ID.String(), "operation": call.Operation, "outcome": request.Outcome,
		})
	})
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "coding agent tool call not found"})
		case errors.Is(err, errToolCallAlreadyResolved):
			c.JSON(http.StatusConflict, gin.H{"error": "only an outcome-unknown tool call can be reconciled"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not reconcile coding agent tool call"})
		}
		return
	}
	c.JSON(http.StatusOK, call)
}

func (h *WorkflowHandler) resolveCodingAgentToolCall(c *gin.Context, approved bool) {
	callID := strings.TrimSpace(c.Param("id"))
	var call models.CodingAgentToolCall
	var candidate models.CodingAgentToolCall
	if err := h.db.DB.WithContext(c.Request.Context()).Where(
		"id = ? AND organization_id = ? AND user_id = ?", callID, currentOrgID(c), auth.UserID(c),
	).First(&candidate).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "coding agent tool call not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not resolve coding agent tool call"})
		}
		return
	}
	err := h.db.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var job models.CodingAgentJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND user_id = ?", candidate.JobID, currentOrgID(c), auth.UserID(c),
		).First(&job).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND job_id = ? AND organization_id = ? AND user_id = ?",
			candidate.ID, job.ID, currentOrgID(c), auth.UserID(c),
		).First(&call).Error; err != nil {
			return err
		}
		if call.Status != models.CodingAgentToolCallPendingApproval {
			return errToolCallAlreadyResolved
		}
		if job.Status != models.CodingAgentJobRunning || job.CancelRequestedAt != nil {
			return errToolCallJobInactive
		}
		now := time.Now().UTC()
		if approved {
			if err := tx.Model(&call).Updates(map[string]any{
				"status": models.CodingAgentToolCallApproved, "approved_at": now,
				"approved_by_user_id": auth.UserID(c),
			}).Error; err != nil {
				return err
			}
			call.Status, call.ApprovedAt, call.ApprovedByUserID = models.CodingAgentToolCallApproved, &now, auth.UserID(c)
			return appendCodingAgentEventTx(tx, &job, "tool_approved", "Coding agent tool call approved", map[string]any{
				"toolCallId": call.ID.String(), "nodeId": call.NodeID, "operation": call.Operation,
			})
		}
		if err := tx.Model(&call).Updates(map[string]any{
			"status": models.CodingAgentToolCallRejected, "completed_at": now,
			"approved_by_user_id": auth.UserID(c), "last_error": "rejected by the workflow owner",
		}).Error; err != nil {
			return err
		}
		call.Status, call.CompletedAt, call.ApprovedByUserID = models.CodingAgentToolCallRejected, &now, auth.UserID(c)
		return appendCodingAgentEventTx(tx, &job, "tool_rejected", "Coding agent tool call rejected", map[string]any{
			"toolCallId": call.ID.String(), "nodeId": call.NodeID, "operation": call.Operation,
		})
	})
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "coding agent tool call not found"})
		case errors.Is(err, errToolCallAlreadyResolved):
			c.JSON(http.StatusConflict, gin.H{"error": "coding agent tool call is already resolved"})
		case errors.Is(err, errToolCallJobInactive):
			c.JSON(http.StatusConflict, gin.H{"error": "coding agent job is no longer active"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not resolve coding agent tool call"})
		}
		return
	}
	c.JSON(http.StatusOK, call)
}

var (
	errToolCallAlreadyResolved = errors.New("coding agent tool call already resolved")
	errToolCallJobInactive     = errors.New("coding agent job inactive")
)

func appendCodingAgentEventTx(tx *gorm.DB, job *models.CodingAgentJob, eventType, message string, payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	sequence := job.NextEventSequence
	if sequence < 1 {
		sequence = 1
	}
	event := models.CodingAgentEvent{
		OrganizationID: job.OrganizationID, JobID: job.ID.String(), Sequence: sequence,
		Type: eventType, Message: message, Payload: models.JSONB(encoded),
	}
	if err := tx.Create(&event).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.CodingAgentJob{}).Where("id = ?", job.ID).
		Update("next_event_sequence", sequence+1).Error; err != nil {
		return err
	}
	job.NextEventSequence = sequence + 1
	return nil
}
