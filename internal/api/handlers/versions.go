package handlers

import (
	"net/http"

	"workflow-ai/server/internal/database/models"

	"github.com/gin-gonic/gin"
)

// GET /api/workflows/:id/versions
func (h *WorkflowHandler) ListVersions(c *gin.Context) {
	workflowID := c.Param("id")
	var versions []models.WorkflowVersion
	if err := h.db.DB.Where("workflow_id = ?", workflowID).Order("version DESC").Find(&versions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, versions)
}

// POST /api/workflows/:id/versions  — snapshot current workflow state
func (h *WorkflowHandler) SaveVersion(c *gin.Context) {
	workflowID := c.Param("id")

	var workflow models.Workflow
	if err := h.db.DB.First(&workflow, "id = ?", workflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
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
