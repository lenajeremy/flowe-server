package handlers

import (
	"net/http"
	"time"

	"workflow-ai/server/internal/database/models"

	"github.com/gin-gonic/gin"
)

// versionSummary is the version-list payload: snapshot metadata only. The
// node/edge JSONB of a snapshot is only ever applied server-side on restore.
type versionSummary struct {
	ID         string    `json:"id"`
	WorkflowID string    `json:"workflow_id"`
	Version    int       `json:"version"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
}

// GET /api/workflows/:id/versions
func (h *WorkflowHandler) ListVersions(c *gin.Context) {
	workflowID := c.Param("id")
	if _, ok := h.loadOwnedWorkflow(c, workflowID); !ok {
		return
	}
	summaries := []versionSummary{}
	if err := h.db.DB.Model(&models.WorkflowVersion{}).
		Select("id, workflow_id, version, name, created_at").
		Where("workflow_id = ?", workflowID).
		Order("version DESC").Scan(&summaries).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, summaries)
}

// POST /api/workflows/:id/versions  — snapshot current workflow state
func (h *WorkflowHandler) SaveVersion(c *gin.Context) {
	workflowID := c.Param("id")

	workflow, ok := h.loadOwnedWorkflow(c, workflowID)
	if !ok {
		return
	}

	// Get next version number
	var count int64
	h.db.DB.Model(&models.WorkflowVersion{}).Where("workflow_id = ?", workflowID).Count(&count)

	var body struct {
		Name string `json:"name"`
	}
	c.ShouldBindJSON(&body)
	versionName := body.Name
	if versionName == "" {
		versionName = workflow.Name
	}

	version := models.WorkflowVersion{
		UserID:     workflow.UserID,
		WorkflowID: workflowID,
		Version:    int(count) + 1,
		Nodes:      workflow.Nodes,
		Edges:      workflow.Edges,
		Name:       versionName,
	}
	if err := h.db.DB.Create(&version).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, version)
}

// POST /api/workflows/:id/versions/:versionId/restore  — restore a snapshot
func (h *WorkflowHandler) RestoreVersion(c *gin.Context) {
	workflowID := c.Param("id")
	versionID := c.Param("versionId")
	if _, ok := h.loadOwnedWorkflow(c, workflowID); !ok {
		return
	}

	var version models.WorkflowVersion
	if err := h.db.DB.Where("id = ? AND workflow_id = ?", versionID, workflowID).First(&version).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "version not found"})
		return
	}

	if err := h.db.DB.Model(&models.Workflow{}).Where("id = ?", workflowID).Updates(map[string]interface{}{
		"nodes": version.Nodes,
		"edges": version.Edges,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Return the updated workflow
	var workflow models.Workflow
	h.db.DB.First(&workflow, "id = ?", workflowID)
	c.JSON(http.StatusOK, workflow)
}
