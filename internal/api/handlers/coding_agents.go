package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/codingagent"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func (h *WorkflowHandler) CodingAgentRuntimes(c *gin.Context) {
	configured := h.codingAgents != nil && h.codingAgents.Available(codingagent.RuntimeCodex)
	connected := false
	var credential any
	if configured {
		if value, err := h.codingAgents.GetCredential(c.Request.Context(), currentOrgID(c), auth.UserID(c), codingagent.RuntimeCodex); err == nil {
			connected = value.Status == "connected"
			credential = value
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load coding agent connection"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"runtimes": []gin.H{{
		"id": codingagent.RuntimeCodex, "name": "Codex", "configured": configured,
		"connected": connected, "credential": credential,
	}}})
}

func (h *WorkflowHandler) StartCodexConnection(c *gin.Context) {
	if h.codingAgents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "coding agent execution is not configured"})
		return
	}
	attempt, created, err := h.codingAgents.StartCodexConnection(c.Request.Context(), currentOrgID(c), auth.UserID(c))
	if err != nil {
		codingAgentError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	c.JSON(status, attempt)
}

func (h *WorkflowHandler) GetCodingAgentAuthAttempt(c *gin.Context) {
	if h.codingAgents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "coding agent execution is not configured"})
		return
	}
	attempt, err := h.codingAgents.GetAuthAttempt(c.Request.Context(), currentOrgID(c), auth.UserID(c), c.Param("id"))
	if err != nil {
		codingAgentError(c, err)
		return
	}
	c.JSON(http.StatusOK, attempt)
}

func (h *WorkflowHandler) CancelCodingAgentAuthAttempt(c *gin.Context) {
	if h.codingAgents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "coding agent execution is not configured"})
		return
	}
	if err := h.codingAgents.CancelAuthAttempt(c.Request.Context(), currentOrgID(c), auth.UserID(c), c.Param("id")); err != nil {
		codingAgentError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *WorkflowHandler) DisconnectCodingAgent(c *gin.Context) {
	if h.codingAgents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "coding agent execution is not configured"})
		return
	}
	runtime := c.Param("runtime")
	if runtime != codingagent.RuntimeCodex {
		c.JSON(http.StatusNotFound, gin.H{"error": "coding agent runtime not found"})
		return
	}
	if err := h.codingAgents.DisconnectCredential(c.Request.Context(), currentOrgID(c), auth.UserID(c), runtime); err != nil {
		codingAgentError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *WorkflowHandler) ListCodingAgentJobs(c *gin.Context) {
	if h.codingAgents == nil {
		c.JSON(http.StatusOK, gin.H{"jobs": []any{}})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	jobs, err := h.codingAgents.ListUserJobs(c.Request.Context(), currentOrgID(c), auth.UserID(c), c.Query("workflowId"), c.Query("nodeId"), limit)
	if err != nil {
		codingAgentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"jobs": jobs})
}

func (h *WorkflowHandler) GetCodingAgentJob(c *gin.Context) {
	if h.codingAgents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "coding agent execution is not configured"})
		return
	}
	details, err := h.codingAgents.GetUserJob(c.Request.Context(), currentOrgID(c), auth.UserID(c), c.Param("id"))
	if err != nil {
		codingAgentError(c, err)
		return
	}
	c.JSON(http.StatusOK, details)
}

func (h *WorkflowHandler) ListCodingAgentJobEvents(c *gin.Context) {
	if h.codingAgents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "coding agent execution is not configured"})
		return
	}
	details, err := h.codingAgents.GetUserJob(c.Request.Context(), currentOrgID(c), auth.UserID(c), c.Param("id"))
	if err != nil {
		codingAgentError(c, err)
		return
	}
	after, _ := strconv.Atoi(c.Query("after"))
	events, err := h.codingAgents.ListEvents(c.Request.Context(), details.Job.OrganizationID, details.Job.ID.String(), after)
	if err != nil {
		codingAgentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}

func (h *WorkflowHandler) CancelCodingAgentJob(c *gin.Context) {
	if h.codingAgents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "coding agent execution is not configured"})
		return
	}
	job, err := h.codingAgents.Cancel(c.Request.Context(), currentOrgID(c), auth.UserID(c), c.Param("id"))
	if err != nil {
		codingAgentError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, job)
}

func (h *WorkflowHandler) ListCodingAgentEnvironments(c *gin.Context) {
	if h.codingAgents == nil {
		c.JSON(http.StatusOK, gin.H{"environments": []any{}})
		return
	}
	environments, err := h.codingAgents.ListUserEnvironments(
		c.Request.Context(), currentOrgID(c), auth.UserID(c), c.Query("workflowId"), c.Query("nodeId"),
	)
	if err != nil {
		codingAgentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"environments": environments})
}

func (h *WorkflowHandler) ResetCodingAgentEnvironment(c *gin.Context) {
	if h.codingAgents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "coding agent execution is not configured"})
		return
	}
	if err := h.codingAgents.ResetEnvironment(c.Request.Context(), currentOrgID(c), auth.UserID(c), c.Param("id")); err != nil {
		codingAgentError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func codingAgentError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, codingagent.ErrJobNotFound), errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "coding agent resource not found"})
	case errors.Is(err, codingagent.ErrCredentialRequired):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, codingagent.ErrJobTerminal):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, codingagent.ErrEnvironmentBusy):
		c.JSON(http.StatusConflict, gin.H{"error": "coding agent environment is in use"})
	case errors.Is(err, codingagent.ErrUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	case errors.Is(err, codingagent.ErrInvalidRequest):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, codingagent.ErrRateLimited):
		c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "coding agent request failed"})
	}
}
