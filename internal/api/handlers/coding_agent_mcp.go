package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"workflow-ai/server/config"
	"workflow-ai/server/internal/codingagent"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"

	"github.com/gin-gonic/gin"
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
	if job.Status.Terminal() {
		return nil, false
	}
	return &job, true
}

// toolsForJob resolves the job's grant against the workflow as it stands now.
func (h *WorkflowHandler) toolsForJob(job *models.CodingAgentJob) ([]agentTool, executor.WorkflowAST, AgentCapabilityPolicy, error) {
	var workflow models.Workflow
	if err := h.db.DB.First(&workflow, "id = ?", job.WorkflowID).Error; err != nil {
		return nil, executor.WorkflowAST{}, AgentCapabilityPolicy{}, fmt.Errorf("this job's workflow no longer exists")
	}
	var nodes []executor.WorkflowASTNode
	var edges []executor.WorkflowASTEdge
	_ = json.Unmarshal(workflow.Nodes, &nodes)
	_ = json.Unmarshal(workflow.Edges, &edges)
	ast := executor.WorkflowAST{Version: "1.0", Name: workflow.Name, Nodes: nodes, Edges: edges}

	var granted []string
	_ = json.Unmarshal(job.ToolNodeIDs, &granted)
	allowed := make(map[string]bool, len(granted))
	for _, id := range granted {
		allowed[id] = true
	}

	// The grant names nodes; what each node can do comes from the capability
	// catalog, so an operation added to a provider later is covered without
	// anyone re-granting, and one removed stops being callable immediately.
	policy := AgentCapabilityPolicy{Version: 1}
	for _, node := range nodes {
		if !allowed[node.ID] {
			continue
		}
		capability, ok := agentNodeCapability(node)
		if !ok {
			continue
		}
		grant := AgentNodeGrant{NodeID: node.ID, AllowedOverrideFields: capability.OverridableFields}
		for _, operation := range capability.Operations {
			grant.AllowedOperations = append(grant.AllowedOperations, operation.ID)
		}
		policy.Nodes = append(policy.Nodes, grant)
	}
	return buildAgentToolsWithPolicy(ast, &policy), ast, policy, nil
}

func (h *WorkflowHandler) callToolForJob(c *gin.Context, req mcpRequest, job *models.CodingAgentJob) {
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

	keys := executor.APIKeys{
		Anthropic: config.GetEnv("ANTHROPIC_API_KEY"),
		OpenAI:    config.GetEnv("OPENAI_API_KEY"),
		Brave:     config.GetEnv("BRAVE_API_KEY"),
		Jina:      config.GetEnv("JINA_API_KEY"),
	}
	out, execErr := executor.ExecuteSingleNode(
		c.Request.Context(), tool.Node, authorized.Overrides, map[string]string{},
		ast.Edges, keys, "coding-agent-"+job.ID.String(), job.UserID, job.OrganizationID, nil,
	)
	if execErr != nil {
		// A failing operation is the agent's problem to work around, not a
		// transport fault: it comes back as tool content so the agent can read
		// the reason and try something else.
		mcpToolError(c, req.ID, execErr.Error())
		return
	}
	slog.InfoContext(c.Request.Context(), "coding agent used a workflow node",
		"job_id", job.ID.String(), "node_id", tool.Node.ID, "op", authorized.Operation.ID)

	mcpResult(c, req.ID, gin.H{
		"content": []gin.H{{"type": "text", "text": out}},
	})
}

func mcpToolError(c *gin.Context, id json.RawMessage, message string) {
	mcpResult(c, id, gin.H{
		"content": []gin.H{{"type": "text", "text": message}},
		"isError": true,
	})
}
