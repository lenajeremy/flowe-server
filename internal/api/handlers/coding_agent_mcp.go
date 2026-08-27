package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"workflow-ai/server/config"
	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/codingagent"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The sandbox side of granting a coding agent the workflow's own nodes.
//
// The agent runs with no credentials of its own — that is the point of the
// sandbox — so anything it needs to do in a connected account happens here
// instead, on the server, with the user's grants and under the same policy that
// governs a hosted agent. What crosses into the sandbox is a bearer token and a
// URL, never a GitHub token.
//
// This speaks MCP over streamable HTTP, which is what the Codex CLI configures
// from `[mcp_servers.*]`. It is deliberately the narrow subset that transport
// requires: initialize, tools/list, tools/call.

const mcpProtocolVersion = "2025-06-18"

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func mcpResult(c *gin.Context, id json.RawMessage, result any) {
	c.JSON(http.StatusOK, gin.H{"jsonrpc": "2.0", "id": id, "result": result})
}

func mcpFail(c *gin.Context, id json.RawMessage, code int, message string) {
	// JSON-RPC transport errors travel as 200s with an error member. A non-2xx
	// here would make the client retry the transport rather than show the agent
	// what went wrong.
	c.JSON(http.StatusOK, gin.H{"jsonrpc": "2.0", "id": id, "error": mcpError{Code: code, Message: message}})
}

// CodingAgentMCP serves the workflow's granted nodes to a running sandbox.
func (h *WorkflowHandler) CodingAgentMCP(c *gin.Context) {
	var req mcpRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		mcpFail(c, nil, -32700, "request is not valid JSON-RPC")
		return
	}
	// A notification carries no id and expects no reply.
	notification := len(req.ID) == 0 || string(req.ID) == "null"

	job, ok := h.authenticateToolCall(c)
	if !ok {
		if notification {
			c.Status(http.StatusAccepted)
			return
		}
		mcpFail(c, req.ID, -32001, "this coding agent job is not authorized to use workflow tools")
		return
	}
	if h.redis != nil && !auth.Allow(c.Request.Context(), h.redis, "rl:coding-agent-mcp:"+job.ID.String(), 120, time.Minute) {
		mcpFail(c, req.ID, -32002, "this coding agent job is sending tool requests too quickly")
		return
	}

	switch req.Method {
	case "initialize":
		mcpResult(c, req.ID, gin.H{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    gin.H{"tools": gin.H{}},
			"serverInfo":      gin.H{"name": "fernary-workflow", "version": "1"},
		})
	case "notifications/initialized", "notifications/cancelled":
		c.Status(http.StatusAccepted)
	case "tools/list":
		tools, _, _, err := h.toolsForJob(job)
		if err != nil {
			mcpFail(c, req.ID, -32603, err.Error())
			return
		}
		listed := make([]gin.H, 0, len(tools))
		for _, tool := range tools {
			listed = append(listed, gin.H{
				"name":        tool.Schema["name"],
				"description": tool.Schema["description"],
				"inputSchema": tool.Schema["input_schema"],
			})
		}
		mcpResult(c, req.ID, gin.H{"tools": listed})
	case "tools/call":
		h.callToolForJob(c, req, job)
	default:
		if notification {
			c.Status(http.StatusAccepted)
			return
		}
		mcpFail(c, req.ID, -32601, "method not supported: "+req.Method)
	}
}

// authenticateToolCall resolves the bearer token to the job that owns it.
//
// The job's status is part of the check, not just the token: a terminal job
// must not be able to act even in the window before its token is revoked, or a
// sandbox that outlived its run could keep writing to the user's accounts.
func (h *WorkflowHandler) authenticateToolCall(c *gin.Context) (*models.CodingAgentJob, bool) {
	header := strings.TrimSpace(c.GetHeader("Authorization"))
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return nil, false
	}
	token := strings.TrimSpace(header[len("bearer "):])
	if token == "" {
		return nil, false
	}
	var job models.CodingAgentJob
	if err := h.db.DB.Where("tool_token_hash = ? AND tool_token_hash <> ''",
		codingagent.HashToolToken(token)).First(&job).Error; err != nil {
		return nil, false
	}
	if job.Status != models.CodingAgentJobRunning || job.CancelRequestedAt != nil ||
		job.HeartbeatAt == nil || job.HeartbeatAt.Before(time.Now().UTC().Add(-2*time.Minute)) {
		return nil, false
	}
	var member models.OrgMember
	if err := h.db.DB.Where("organization_id = ? AND user_id = ?", job.OrganizationID, job.UserID).
		First(&member).Error; err != nil {
		return nil, false
	}
	return &job, true
}

// toolsForJob resolves only the immutable graph and policy stored on the job.
// A job without a snapshot predates this authority boundary and fails closed.
// Reading the workflow's current mutable graph would let later edits change an
// already-running sandbox's reach.
func (h *WorkflowHandler) toolsForJob(job *models.CodingAgentJob) ([]agentTool, executor.WorkflowAST, AgentCapabilityPolicy, error) {
	var ast executor.WorkflowAST
	if len(job.ToolWorkflow) > 0 && string(job.ToolWorkflow) != "{}" {
		if err := json.Unmarshal(job.ToolWorkflow, &ast); err != nil {
			return nil, executor.WorkflowAST{}, AgentCapabilityPolicy{}, fmt.Errorf("this job's frozen tool graph is invalid")
		}
	} else {
		return nil, executor.WorkflowAST{}, AgentCapabilityPolicy{}, errors.New("this legacy job has no frozen tool graph; start a new run")
	}

	policy := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion}
	if len(job.ToolPolicy) > 0 && string(job.ToolPolicy) != "{}" {
		if err := json.Unmarshal(job.ToolPolicy, &policy); err != nil {
			return nil, executor.WorkflowAST{}, AgentCapabilityPolicy{}, fmt.Errorf("this job's frozen tool policy is invalid")
		}
	}
	if len(policy.Integrations) == 0 && len(policy.Nodes) == 0 {
		var granted []string
		_ = json.Unmarshal(job.ToolNodeIDs, &granted)
		policy = safeLegacyCodingAgentPolicy(ast, granted)
	}
	policy, _ = normalizeAgentCapabilityPolicy(ast, policy)
	return buildAgentToolsWithPolicy(ast, &policy), ast, policy, nil
}

func safeLegacyCodingAgentPolicy(ast executor.WorkflowAST, nodeIDs []string) AgentCapabilityPolicy {
	allowed := make(map[string]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		allowed[id] = true
	}
	policy := defaultSafeAgentPolicy(ast)
	filtered := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion}
	for _, grant := range policy.Integrations {
		selected := AgentIntegrationGrant{NodeType: grant.NodeType, AllowedOperations: grant.AllowedOperations, AllowedOverrideFields: grant.AllowedOverrideFields}
		for _, nodeID := range grant.NodeIDs {
			if allowed[nodeID] {
				selected.NodeIDs = append(selected.NodeIDs, nodeID)
			}
		}
		if len(selected.NodeIDs) > 0 {
			filtered.Integrations = append(filtered.Integrations, selected)
		}
	}
	return filtered
}

func (h *WorkflowHandler) callToolForJob(c *gin.Context, req mcpRequest, job *models.CodingAgentJob) {
	if len(req.ID) == 0 || string(req.ID) == "null" {
		mcpFail(c, req.ID, -32600, "tools/call requires a JSON-RPC request id")
		return
	}
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		mcpFail(c, req.ID, -32602, "tools/call params are malformed")
		return
	}

	tools, ast, policy, err := h.toolsForJob(job)
	if err != nil {
		mcpFail(c, req.ID, -32603, err.Error())
		return
	}
	var tool *agentTool
	for i := range tools {
		if tools[i].Schema["name"] == params.Name {
			tool = &tools[i]
			break
		}
	}
	if tool == nil {
		mcpToolError(c, req.ID, "no tool named "+params.Name+" is available to this job")
		return
	}

	authorized, err := authorizeAgentToolCall(policy, tool.Node, params.Arguments)
	if err != nil {
		mcpToolError(c, req.ID, err.Error())
		return
	}
	call, err := h.prepareCodingAgentToolCall(c, req, job, *tool, authorized, params.Arguments)
	if err != nil {
		mcpToolError(c, req.ID, err.Error())
		return
	}
	if call.Status == models.CodingAgentToolCallSucceeded {
		var recovered struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(call.Result, &recovered)
		mcpResult(c, req.ID, gin.H{"content": []gin.H{{"type": "text", "text": recovered.Text}}})
		return
	}
	if call.Status == models.CodingAgentToolCallPendingApproval {
		call, err = h.waitForCodingAgentToolApproval(c, call)
		if err != nil {
			mcpToolError(c, req.ID, err.Error())
			return
		}
	}
	if call.Status != models.CodingAgentToolCallApproved {
		mcpToolError(c, req.ID, codingAgentToolCallStatusMessage(call))
		return
	}

	out, execErr := h.executeCodingAgentToolCall(c, job, call, *tool, authorized, ast)
	if execErr != nil {
		mcpToolError(c, req.ID, execErr.Error())
		return
	}
	mcpResult(c, req.ID, gin.H{"content": []gin.H{{"type": "text", "text": out}}})
}

func (h *WorkflowHandler) prepareCodingAgentToolCall(
	c *gin.Context, req mcpRequest, job *models.CodingAgentJob, tool agentTool,
	authorized AgentAuthorizedCall, arguments map[string]any,
) (*models.CodingAgentToolCall, error) {
	resolved, err := executor.ResolveSingleNodeData(tool.Node, authorized.Overrides)
	if err != nil {
		return nil, err
	}
	effectiveDetails, err := agentApprovalDisplayDetails(resolved)
	if err != nil {
		return nil, errors.New("tool call details could not be rendered safely")
	}
	effectiveJSON, err := json.Marshal(effectiveDetails)
	if err != nil {
		return nil, errors.New("tool call details could not be recorded")
	}
	argumentsJSON, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("tool arguments could not be recorded")
	}
	if len(argumentsJSON) > 256<<10 || len(effectiveJSON) > 256<<10 {
		return nil, errors.New("tool call details are too large")
	}
	fingerprintRaw, _ := json.Marshal(map[string]any{
		"jobId": job.ID.String(), "nodeId": tool.Node.ID,
		"operation": authorized.Operation.ID, "effectiveConfig": json.RawMessage(effectiveJSON),
	})
	fingerprintDigest := sha256.Sum256(fingerprintRaw)
	fingerprint := hex.EncodeToString(fingerprintDigest[:])
	requestDigest := sha256.Sum256([]byte(string(req.ID) + "\x00" + fingerprint))
	requestKey := hex.EncodeToString(requestDigest[:])

	var call models.CodingAgentToolCall
	err = h.db.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var lockedJob models.CodingAgentJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lockedJob, "id = ?", job.ID).Error; err != nil {
			return err
		}
		if lockedJob.Status != models.CodingAgentJobRunning || lockedJob.CancelRequestedAt != nil {
			return errToolCallJobInactive
		}
		if err := tx.Where("job_id = ? AND request_key = ?", job.ID, requestKey).First(&call).Error; err == nil {
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var unresolved models.CodingAgentToolCall
		if err := tx.Where("job_id = ? AND fingerprint = ? AND status IN ?", job.ID, fingerprint, []models.CodingAgentToolCallStatus{
			models.CodingAgentToolCallPendingApproval, models.CodingAgentToolCallApproved,
			models.CodingAgentToolCallExecuting, models.CodingAgentToolCallOutcomeUnknown,
		}).Order("created_at DESC").First(&unresolved).Error; err == nil {
			call = unresolved
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var count int64
		if err := tx.Model(&models.CodingAgentToolCall{}).Where("job_id = ?", job.ID).Count(&count).Error; err != nil {
			return err
		}
		if count >= maxCodingAgentToolCalls {
			return fmt.Errorf("this job reached its %d-call workflow tool limit", maxCodingAgentToolCalls)
		}
		status := models.CodingAgentToolCallApproved
		eventType, message := "tool_requested", "Coding agent requested a read tool"
		if authorized.Operation.Effect != AgentEffectRead {
			status = models.CodingAgentToolCallPendingApproval
			eventType, message = "tool_approval_requested", "Coding agent is waiting for tool approval"
		}
		now := time.Now().UTC()
		call = models.CodingAgentToolCall{
			OrganizationID: job.OrganizationID, UserID: job.UserID, JobID: job.ID.String(),
			RequestKey: requestKey, Fingerprint: fingerprint, NodeID: tool.Node.ID,
			NodeLabel: tool.Node.Data.Label, ToolName: fmt.Sprint(tool.Schema["name"]),
			Operation: authorized.Operation.ID, Effect: string(authorized.Operation.Effect),
			Reason: authorized.Reason, Arguments: models.JSONB(argumentsJSON),
			EffectiveConfig: models.JSONB(effectiveJSON), Status: status,
			Result: models.JSONB("{}"), RequestedAt: now,
		}
		if status == models.CodingAgentToolCallApproved {
			call.ApprovedAt = &now
		}
		if err := tx.Create(&call).Error; err != nil {
			return err
		}
		return appendCodingAgentEventTx(tx, &lockedJob, eventType, message, map[string]any{
			"toolCallId": call.ID.String(), "nodeId": call.NodeID, "nodeLabel": call.NodeLabel,
			"operation": call.Operation, "effect": call.Effect, "reason": call.Reason,
			"arguments": json.RawMessage(argumentsJSON), "effectiveConfig": json.RawMessage(effectiveJSON),
		})
	})
	if err != nil {
		return nil, err
	}
	return &call, nil
}

func (h *WorkflowHandler) waitForCodingAgentToolApproval(c *gin.Context, call *models.CodingAgentToolCall) (*models.CodingAgentToolCall, error) {
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(10 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return call, errors.New("tool approval is still pending; approve it in Fernary and let the agent retry")
		case <-timer.C:
			return call, errors.New("tool approval timed out after 10 minutes")
		case <-ticker.C:
			if err := h.db.DB.WithContext(c.Request.Context()).First(call, "id = ?", call.ID).Error; err != nil {
				return call, errors.New("tool approval state could not be loaded")
			}
			if call.Status != models.CodingAgentToolCallPendingApproval {
				return call, nil
			}
		}
	}
}

func codingAgentToolCallStatusMessage(call *models.CodingAgentToolCall) string {
	switch call.Status {
	case models.CodingAgentToolCallRejected:
		return "the workflow owner rejected this tool call"
	case models.CodingAgentToolCallExecuting:
		return "this exact tool call is already executing and will not be replayed"
	case models.CodingAgentToolCallOutcomeUnknown:
		return "the external outcome of this exact tool call is unknown; inspect the target system before reconciling it"
	case models.CodingAgentToolCallCancelled:
		return "this tool call was cancelled because the coding agent job stopped"
	case models.CodingAgentToolCallFailed:
		return call.LastError
	default:
		return "this tool call cannot execute in status " + string(call.Status)
	}
}

func (h *WorkflowHandler) executeCodingAgentToolCall(
	c *gin.Context, job *models.CodingAgentJob, call *models.CodingAgentToolCall,
	tool agentTool, authorized AgentAuthorizedCall, ast executor.WorkflowAST,
) (string, error) {
	// Once a call is durably claimed, a sandbox HTTP disconnect must not cancel
	// the external action halfway through and discard its outcome. The bounded,
	// detached context lets Fernary finish and record it exactly once.
	execCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Minute)
	defer cancel()
	var billingErr error
	execCtx, billingErr = h.bill.ContextForRunContinuation(
		execCtx, job.OrganizationID, job.UserID, job.WorkflowRunID,
	)
	if billingErr != nil {
		return "", fmt.Errorf("tool call cannot start: %w", billingErr)
	}
	execCtx = telemetry.WithSurface(execCtx, telemetry.SurfaceTool)
	execCtx = executor.WithWorkflowID(execCtx, job.WorkflowID)
	now := time.Now().UTC()
	err := h.db.DB.WithContext(execCtx).Transaction(func(tx *gorm.DB) error {
		var lockedJob models.CodingAgentJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lockedJob, "id = ?", job.ID).Error; err != nil {
			return err
		}
		if lockedJob.Status != models.CodingAgentJobRunning || lockedJob.CancelRequestedAt != nil {
			return errToolCallJobInactive
		}
		var locked models.CodingAgentToolCall
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND job_id = ?", call.ID, lockedJob.ID,
		).First(&locked).Error; err != nil {
			return err
		}
		if locked.Status != models.CodingAgentToolCallApproved {
			call = &locked
			return errToolCallAlreadyResolved
		}
		if err := tx.Model(&locked).Updates(map[string]any{
			"status": models.CodingAgentToolCallExecuting, "started_at": now,
		}).Error; err != nil {
			return err
		}
		call.Status, call.StartedAt = models.CodingAgentToolCallExecuting, &now
		return appendCodingAgentEventTx(tx, &lockedJob, "tool_started", "Coding agent tool call started", map[string]any{
			"toolCallId": call.ID.String(), "nodeId": call.NodeID, "operation": call.Operation,
		})
	})
	if err != nil {
		if errors.Is(err, errToolCallAlreadyResolved) {
			return "", errors.New(codingAgentToolCallStatusMessage(call))
		}
		return "", errors.New("tool call could not be claimed safely")
	}

	var out string
	var execErr error
	externalStarted := false
	txErr := h.db.DB.WithContext(execCtx).Transaction(func(tx *gorm.DB) error {
		var lockedJob models.CodingAgentJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lockedJob, "id = ?", job.ID).Error; err != nil {
			return err
		}
		freshAfter := time.Now().UTC().Add(-2 * time.Minute)
		if lockedJob.Status != models.CodingAgentJobRunning || lockedJob.CancelRequestedAt != nil ||
			lockedJob.HeartbeatAt == nil || lockedJob.HeartbeatAt.Before(freshAfter) {
			return errToolCallJobInactive
		}
		var member models.OrgMember
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where(
			"organization_id = ? AND user_id = ?", job.OrganizationID, job.UserID,
		).First(&member).Error; err != nil {
			return errToolCallJobInactive
		}
		var lockedCall models.CodingAgentToolCall
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lockedCall, "id = ?", call.ID).Error; err != nil {
			return err
		}
		if lockedCall.Status != models.CodingAgentToolCallExecuting {
			return errToolCallAlreadyResolved
		}

		keys := executor.APIKeys{
			Anthropic: config.GetEnv("ANTHROPIC_API_KEY"), OpenAI: config.GetEnv("OPENAI_API_KEY"),
			Brave: config.GetEnv("BRAVE_API_KEY"), Jina: config.GetEnv("JINA_API_KEY"),
		}
		externalStarted = true
		out, execErr = executor.ExecuteSingleNode(
			execCtx, tool.Node, authorized.Overrides, map[string]string{}, ast.Edges,
			keys, job.WorkflowRunID, job.UserID, job.OrganizationID, nil,
		)
		completed := time.Now().UTC()
		status := models.CodingAgentToolCallSucceeded
		eventType, message := "tool_completed", "Coding agent tool call completed"
		updates := map[string]any{"completed_at": completed}
		if execErr == nil {
			retainedOut := out
			if len(retainedOut) > 256<<10 {
				retainedOut = retainedOut[:256<<10] + "\n… result truncated in durable history"
			}
			resultJSON, _ := json.Marshal(map[string]any{"text": retainedOut})
			updates["status"], updates["result"], updates["last_error"] = status, models.JSONB(resultJSON), ""
			call.Result = models.JSONB(resultJSON)
		} else {
			status = models.CodingAgentToolCallFailed
			eventType, message = "tool_failed", "Coding agent read tool call failed"
			if authorized.Operation.Effect != AgentEffectRead {
				status = models.CodingAgentToolCallOutcomeUnknown
				eventType, message = "tool_outcome_unknown", "Coding agent mutation outcome is unknown"
			}
			updates["status"], updates["last_error"] = status, execErr.Error()
		}
		if err := tx.Model(&lockedCall).Updates(updates).Error; err != nil {
			return err
		}
		call.Status, call.CompletedAt, call.LastError = status, &completed, fmt.Sprint(updates["last_error"])
		return appendCodingAgentEventTx(tx, &lockedJob, eventType, message, map[string]any{
			"toolCallId": call.ID.String(), "nodeId": call.NodeID, "operation": call.Operation,
			"status": status, "result": truncate(out, 32<<10), "error": call.LastError,
		})
	})
	if txErr != nil {
		// The call was durably marked executing before the external boundary. If
		// anything after that boundary cannot commit, leave/block it rather than
		// replaying a possibly completed mutation.
		fallbackStatus := models.CodingAgentToolCallFailed
		fallbackError := "the tool call could not be completed safely"
		if externalStarted && authorized.Operation.Effect != AgentEffectRead {
			fallbackStatus = models.CodingAgentToolCallOutcomeUnknown
			fallbackError = "the external result could not be recorded safely; verify the target system"
		}
		_ = h.db.DB.Model(&models.CodingAgentToolCall{}).Where(
			"id = ? AND status = ?", call.ID, models.CodingAgentToolCallExecuting,
		).Updates(map[string]any{
			"status":       fallbackStatus,
			"last_error":   fallbackError,
			"completed_at": time.Now().UTC(),
		}).Error
		return "", errors.New(fallbackError)
	}
	if execErr != nil {
		return "", errors.New(codingAgentToolCallStatusMessage(call))
	}
	return out, nil
}

func mcpToolError(c *gin.Context, id json.RawMessage, message string) {
	mcpResult(c, id, gin.H{
		"content": []gin.H{{"type": "text", "text": message}},
		"isError": true,
	})
}
