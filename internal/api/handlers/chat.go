package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"workflow-ai/server/internal/database/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// GetWorkflowChat returns the stored conversation for a workflow.
func (h *WorkflowHandler) GetWorkflowChat(c *gin.Context) {
	workflowID := c.Param("id")
	if _, ok := h.loadOwnedWorkflow(c, workflowID); !ok {
		return
	}

	var chat models.WorkflowChat
	err := h.db.DB.Where("workflow_id = ?", workflowID).First(&chat).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusOK, gin.H{"messages": []any{}})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var msgs []interface{}
	_ = json.Unmarshal(chat.Messages, &msgs)
	c.JSON(http.StatusOK, gin.H{"messages": msgs})
}

// SaveWorkflowChat upserts the conversation for a workflow.
func (h *WorkflowHandler) SaveWorkflowChat(c *gin.Context) {
	workflowID := c.Param("id")
	wf, ok := h.loadOwnedWorkflow(c, workflowID)
	if !ok {
		return
	}

	var body struct {
		Messages []any `json:"messages"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	raw, _ := json.Marshal(body.Messages)

	var chat models.WorkflowChat
	err := h.db.DB.Where("workflow_id = ?", workflowID).First(&chat).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		chat = models.WorkflowChat{
			UserID:     wf.UserID,
			WorkflowID: workflowID,
			Messages:   raw,
		}
		h.db.DB.Create(&chat)
	} else if err == nil {
		h.db.DB.Model(&chat).Update("messages", raw)
	} else {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}
