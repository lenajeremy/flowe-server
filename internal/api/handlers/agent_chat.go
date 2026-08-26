package handlers

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
)

// ── Chat-with-workflow (agent mode) ───────────────────────────
// A workflow's nodes become tools the chat orchestrator can call one at a
// time, on demand. The node's saved config is the tool's defaults; the tool's
// arguments are per-call overrides merged into a copy — the stored workflow
// is never mutated by chatting. Results accumulate in the session's state
// (the executor outputs map), so {{nodeId.output}} templates keep resolving
// across turns.

const (
	agentMaxToolRounds = 8
	agentStateCap      = 16 << 10 // per-node output kept in session state
	agentResultCap     = 32 << 10 // tool result shown to the model
)

// ── Session CRUD ──────────────────────────────────────────────

type agentSessionSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateChatSession — POST /api/workflows/:id/chat-sessions
func (h *WorkflowHandler) CreateChatSession(c *gin.Context) {
	wf, ok := h.loadOwnedWorkflow(c, c.Param("id"))
	if !ok {
		return
	}
	sess := &models.ChatSession{
		UserID:         auth.UserID(c),
		OrganizationID: currentOrgID(c),
		WorkflowID:     wf.ID.String(),
		Title:          "New chat",
		Messages:       models.JSONB(`[]`),
		State:          models.JSONB(`{}`),
	}
	if err := h.db.DB.Create(sess).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		return
	}
	c.JSON(http.StatusCreated, sess)
}

// ListChatSessions — GET /api/workflows/:id/chat-sessions
func (h *WorkflowHandler) ListChatSessions(c *gin.Context) {
	if _, ok := h.loadOwnedWorkflow(c, c.Param("id")); !ok {
		return
	}
	out := []agentSessionSummary{}
	h.db.DB.Model(&models.ChatSession{}).
		Where("workflow_id = ? AND organization_id = ?", c.Param("id"), orgIDOrDeny(c)).
		Where("NOT EXISTS (SELECT 1 FROM hosted_agent_threads WHERE hosted_agent_threads.chat_session_id = chat_sessions.id AND hosted_agent_threads.deleted_at IS NULL)").
		Order("updated_at desc").Limit(50).
		Select("id, title, created_at, updated_at").Scan(&out)
	c.JSON(http.StatusOK, out)
}

// GetChatSession — GET /api/chat-sessions/:id
func (h *WorkflowHandler) GetChatSession(c *gin.Context) {
	sess, ok := h.loadOwnedSession(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, sess)
}

// DeleteChatSession — DELETE /api/chat-sessions/:id
func (h *WorkflowHandler) DeleteChatSession(c *gin.Context) {
	sess, ok := h.loadOwnedSession(c)
	if !ok {
		return
	}
	h.db.DB.Unscoped().Delete(sess)
	c.Status(http.StatusNoContent)
}

func (h *WorkflowHandler) loadOwnedSession(c *gin.Context) (*models.ChatSession, bool) {
	var sess models.ChatSession
	if err := h.orgScope(c).
		Where("NOT EXISTS (SELECT 1 FROM hosted_agent_threads WHERE hosted_agent_threads.chat_session_id = chat_sessions.id AND hosted_agent_threads.deleted_at IS NULL)").
		First(&sess, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return nil, false
	}
	return &sess, true
}

// ── Tool generation ───────────────────────────────────────────

// agentTool couples a generated tool schema with the node it executes.
type agentTool struct {
	Schema map[string]any
	Node   executor.WorkflowASTNode
}

var toolNameRe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// agentToolName builds a stable, model-friendly tool name from a node.
func agentToolName(node executor.WorkflowASTNode) string {
	label := toolNameRe.ReplaceAllString(strings.ToLower(node.Data.Label), "_")
	label = strings.Trim(label, "_")
	if label == "" {
		label = string(node.Data.NodeType)
	}
	digest := sha256.Sum256([]byte(node.ID))
	suffix := fmt.Sprintf("__%x", digest[:8])
	maxLabel := 64 - len(suffix)
	if len(label) > maxLabel {
		label = strings.Trim(label[:maxLabel], "_")
	}
	return label + suffix
}

// agentSkipNode: control-flow/display/trigger nodes are not tools — the
// orchestrator itself is the branching and the chat is the trigger/output.
func agentSkipNode(t executor.NodeType) bool {
	switch t {
	case executor.NodeTypeBranch, executor.NodeTypeLoop, executor.NodeTypeTextOutput,
		executor.NodeTypeWebhookTrigger, executor.NodeTypeScheduledTrigger,
		executor.NodeTypeIntegrationTrigger, executor.NodeTypeCodingAgent:
		return true
	}
	return false
}

// buildAgentTools generates one tool per eligible canvas node. Input schema
// properties are the node type's overridable fields: names/types come from
// FlowNodeData reflection, descriptions from the AI catalog. All optional —
// omitted fields fall back to the node's saved config.
func buildAgentTools(ast executor.WorkflowAST) []agentTool {
	return buildAgentToolsWithPolicy(ast, nil)
}

func buildAgentToolsWithPolicy(ast executor.WorkflowAST, policy *AgentCapabilityPolicy) []agentTool {
	tools := make([]agentTool, 0, len(ast.Nodes))
	for _, node := range ast.Nodes {
		if agentSkipNode(node.Data.NodeType) {
			continue
		}
		var (
			grant      AgentNodeGrant
			capability AgentNodeCapability
		)
		if policy != nil {
			var exposed bool
			grant, exposed = agentPolicyGrant(*policy, node.ID)
			if !exposed {
				continue
			}
			capability, exposed = agentNodeCapability(node)
			if !exposed {
				continue
			}
		}
		entry := catalogEntry(string(node.Data.NodeType))
		fieldDocs := map[string]any{}
		desc := string(node.Data.NodeType)
		if entry != nil {
			if d, ok := entry["description"].(string); ok {
				desc = d
			}
			if df, ok := entry["dataFields"].(map[string]any); ok {
				fieldDocs = df
			}
		}

		props := map[string]any{}
		var required []string
		allowedFields := map[string]bool{}
		if policy != nil {
			for _, field := range grant.AllowedOverrideFields {
				allowedFields[field] = true
			}
		}
		for field, doc := range fieldDocs {
			if field == "label" || field == "integrationToken" {
				continue
			}
			if policy != nil && field != capability.OperationField && !allowedFields[field] {
				continue
			}
			jsonType, exists := flowDataFieldType(field)
			if !exists {
				continue
			}
			docStr, _ := doc.(string)
			property := map[string]any{"type": jsonType, "description": docStr}
			if policy != nil && field == capability.OperationField {
				property["enum"] = grant.AllowedOperations
				required = append(required, field)
			}
			props[field] = property
		}
		if policy != nil {
			containsWrite := false
			containsRead := false
			for _, operation := range capability.Operations {
				if !stringSliceContains(grant.AllowedOperations, operation.ID) {
					continue
				}
				if operation.Effect == AgentEffectRead {
					containsRead = true
				} else {
					containsWrite = true
				}
			}
			if containsWrite {
				props["reason"] = map[string]any{
					"type":        "string",
					"description": "Why this operation is necessary for the teammate's request. Required for writes and shown in the approval request.",
				}
				if capability.OperationField == "" || !containsRead {
					required = append(required, "reason")
				}
			}
		}

		// Saved config = the tool's defaults; show a secret-safe representation
		// so the model understands pinned behaviour without receiving credentials.
		savedJSON := agentSafeSavedConfig(node.Data)
		toolDesc := fmt.Sprintf(
			"Run the workflow node %q (%s). %s\nSaved configuration (used for any argument you omit): %s\nPass arguments ONLY to adjust behaviour for this one call — the workflow itself is never modified.",
			node.Data.Label, node.Data.NodeType, desc, truncate(savedJSON, 1200),
		)

		inputSchema := map[string]any{
			"type":                 "object",
			"properties":           props,
			"additionalProperties": false,
		}
		if len(required) > 0 {
			inputSchema["required"] = required
		}
		tools = append(tools, agentTool{
			Schema: map[string]any{
				"name":         agentToolName(node),
				"description":  toolDesc,
				"input_schema": inputSchema,
			},
			Node: node,
		})
	}
	return tools
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

var agentSecretConfigFields = map[string]bool{
	"integrationtoken":   true,
	"requestheaders":     true,
	"requestbody":        true,
	"resendheaders":      true,
	"typeformsecret":     true,
	"netlifyenvvalue":    true,
	"netlifyenvvarsjson": true,
	"vercelenvvalue":     true,
	"supabaseauthconfig": true,
	"supabasedbpass":     true,
	"supabaserevealkeys": true,
	"supabasesecrets":    true,
	"gumroadlicensekey":  true,
	"contactspagetoken":  true,
	"frontpagetoken":     true,
}

// agentSafeSavedConfig produces the representation that may be sent to a
// model. Execution still receives the original data through the server-side
// node; this copy exists only to explain its pinned defaults.
func agentSafeSavedConfig(data executor.FlowNodeData) string {
	raw, err := json.Marshal(data)
	if err != nil {
		return "{}"
	}
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		return "{}"
	}
	relevant := agentRelevantConfigFields(data)
	for field := range fields {
		if !relevant[field] || agentSecretConfigFields[strings.ToLower(field)] {
			delete(fields, field)
		}
	}
	safe, err := json.Marshal(fields)
	if err != nil {
		return "{}"
	}
	return string(safe)
}

func agentRelevantConfigFields(data executor.FlowNodeData) map[string]bool {
	relevant := map[string]bool{"nodeType": true, "label": true}
	if entry := catalogEntry(string(data.NodeType)); entry != nil {
		if docs, ok := entry["dataFields"].(map[string]any); ok {
			for field := range docs {
				relevant[field] = true
			}
		}
	}
	return relevant
}

func agentApprovalDisplayDetails(data executor.FlowNodeData) (map[string]any, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	// FlowNodeData is a union shared by every node type, and several older JSON
	// fields are intentionally not omitempty. Only show fields that belong to
	// this node's catalog entry so an approval is readable and does not imply
	// that unrelated empty settings participate in the call.
	relevant := agentRelevantConfigFields(data)
	for field, value := range fields {
		if !relevant[field] {
			delete(fields, field)
			continue
		}
		if !agentSecretConfigFields[strings.ToLower(field)] {
			continue
		}
		encoded, _ := json.Marshal(value)
		if text, ok := value.(string); ok && text == "" {
			delete(fields, field)
			continue
		}
		digest := sha256.Sum256(encoded)
		fields[field] = map[string]any{
			"redacted": true,
			"bytes":    len(encoded),
			"sha256":   fmt.Sprintf("%x", digest[:8]),
		}
	}
	return fields, nil
}

// flowDataFieldType maps a FlowNodeData JSON field to its JSON-schema type.
func flowDataFieldType(field string) (string, bool) {
	t, ok := flowDataFieldTypes()[field]
	return t, ok
}

var (
	flowDataTypesCache map[string]string
	flowDataTypesOnce  sync.Once
)

func flowDataFieldTypes() map[string]string {
	flowDataTypesOnce.Do(func() {
		out := map[string]string{}
		t := reflect.TypeOf(executor.FlowNodeData{})
		for i := 0; i < t.NumField(); i++ {
			tag := t.Field(i).Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			name := strings.Split(tag, ",")[0]
			ft := t.Field(i).Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.String:
				out[name] = "string"
			case reflect.Int, reflect.Int64:
				out[name] = "integer"
			case reflect.Float64:
				out[name] = "number"
			case reflect.Bool:
				out[name] = "boolean"
			}
		}
		flowDataTypesCache = out
	})
	return flowDataTypesCache
}

// ── The turn endpoint ─────────────────────────────────────────

type agentTurnRequest struct {
	Message string `json:"message" binding:"required"`
	Model   string `json:"model,omitempty"`
}

type agentStoredMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls records what ran during an assistant turn, for the UI chips.
	ToolCalls []agentToolCallRecord `json:"toolCalls,omitempty"`
}

type agentToolCallRecord struct {
	Node   string `json:"node"`
	NodeID string `json:"nodeId"`
	// Op is what the call actually did — the per-call integrationOp override
	// when the orchestrator supplied one, else the node's saved op. A node
	// labeled "Create Linear Ticket" that gets called to list issues must
	// surface "List issues", not its label.
	Op     string `json:"op,omitempty"`
	Status string `json:"status"` // ok | error | pending
}

// agentEffectiveOp humanizes the operation a tool call ran ("list_issues" →
// "List issues"), preferring a per-call override. Empty for nodes without ops.
func agentEffectiveOp(data executor.FlowNodeData, overrides map[string]any) string {
	op := data.IntegrationOp
	if v, ok := overrides["integrationOp"].(string); ok && v != "" {
		op = v
	}
	if op == "" {
		return ""
	}
	words := strings.ReplaceAll(op, "_", " ")
	return strings.ToUpper(words[:1]) + words[1:]
}

// AgentChatTurn — POST /api/chat-sessions/:id/message (SSE)
func (h *WorkflowHandler) AgentChatTurn(c *gin.Context) {
	sess, ok := h.loadOwnedSession(c)
	if !ok {
		return
	}
	var req agentTurnRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	plan, err := h.bill.CheckBalance(currentOrgID(c), auth.UserID(c))
	if err != nil {
		if errors.Is(err, billing.ErrOverCap) {
			c.JSON(http.StatusPaymentRequired, gin.H{"error": err.Error(), "limit": billing.KindOf(err)})
			return
		}
		slog.ErrorContext(c.Request.Context(), "agent balance check failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not start"})
		return
	}
	ctx := telemetry.WithSurface(c.Request.Context(), telemetry.SurfaceAgent)
	ctx = telemetry.WithBilling(ctx, billing.BillingContextFor(currentOrgID(c), auth.UserID(c), plan))
	c.Request = c.Request.WithContext(ctx)

	uid := auth.UserID(c)
	if !auth.Allow(c.Request.Context(), h.redis, "rl:agent:"+uid, 30, time.Minute) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests — try again in a minute"})
		return
	}

	turnStart := time.Now()
	slog.InfoContext(c.Request.Context(), "agent chat turn",
		"session_id", sess.ID.String(), "message_chars", len(req.Message))

	wf, ok := h.loadOwnedWorkflow(c, sess.WorkflowID)
	if !ok {
		return
	}
	var ast executor.WorkflowAST
	ast.Name = wf.Name
	if err := json.Unmarshal(wf.Nodes, &ast.Nodes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "workflow nodes unreadable"})
		return
	}
	_ = json.Unmarshal(wf.Edges, &ast.Edges)

	runtimeModel, err := prepareAgentRuntimeModel(req.Model)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// SSE setup
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	flusher, okF := c.Writer.(http.Flusher)
	if !okF {
		fmt.Fprintf(c.Writer, "event: error\ndata: streaming not supported\n\n")
		return
	}
	telemetry.AddSSEStream(c.Request.Context(), "agent_chat", 1)
	defer telemetry.AddSSEStream(c.Request.Context(), "agent_chat", -1)

	sink := func(event AgentTurnEvent) {
		data := event.Text
		if event.Tool != nil {
			encoded, _ := json.Marshal(event.Tool)
			data = string(encoded)
		}
		sendSSE(c.Writer, flusher, string(event.Type), data)
	}
	result, turnErr := h.RunAgentTurn(c.Request.Context(), AgentTurnInput{
		Session:        sess,
		Workflow:       ast,
		OwnerUserID:    uid,
		OrganizationID: currentOrgID(c),
		Message:        req.Message,
		Model:          runtimeModel,
	}, sink)

	slog.InfoContext(c.Request.Context(), "agent chat turn finished",
		"session_id", sess.ID.String(),
		"duration_ms", time.Since(turnStart).Milliseconds(),
		"tool_calls", len(result.ToolCalls),
		"error", turnErr)
	if turnErr != nil {
		slog.WarnContext(c.Request.Context(), "agent chat turn ended with error", "error", turnErr)
	}
}

// agentSystemPrompt frames the workflow-as-agent contract.
//
// The clock is deliberately NOT stitched on here. It changes every second, and
// this prompt is the head of the cached prefix — the provider loops below place
// the clock after the cache breakpoint instead, so the prompt in front of it
// stays byte-identical from turn to turn. The model still receives both, in the
// same order.
func agentSystemPrompt(ast executor.WorkflowAST, tools []agentTool, state map[string]string) string {
	return agentSystemPromptWithGoal(ast, tools, state, "")
}

func agentSystemPromptWithGoal(ast executor.WorkflowAST, tools []agentTool, state map[string]string, goal string) string {
	var names []string
	for _, t := range tools {
		names = append(names, fmt.Sprintf("%s (%s)", t.Node.Data.Label, t.Node.Data.NodeType))
	}
	var appTriggers []string
	for _, node := range ast.Nodes {
		if node.Data.NodeType != executor.NodeTypeIntegrationTrigger {
			continue
		}
		appTriggers = append(appTriggers, fmt.Sprintf(
			"%q: provider=%q, event=%q, resource=%q",
			truncate(node.Data.Label, 120),
			truncate(node.Data.TriggerProvider, 80),
			truncate(node.Data.TriggerEvent, 120),
			truncate(node.Data.TriggerResourceID, 240),
		))
	}
	triggerContext := "none"
	if len(appTriggers) > 0 {
		triggerContext = strings.Join(appTriggers, "; ")
	}
	var stateKeys []string
	for k := range state {
		stateKeys = append(stateKeys, k)
	}
	goalContext := ""
	if strings.TrimSpace(goal) != "" {
		goalContext = fmt.Sprintf("\nThis deployment's AI-inferred, owner-reviewed goal is: %q. Use it to interpret ambiguous requests, but never use it to exceed the tool policy or the teammate's current request.\n", truncate(strings.TrimSpace(goal), 1000))
	}
	return fmt.Sprintf(`You are the workflow %q, acting as a conversational agent for its owner.
%s

Your tools are this workflow's nodes: %s. Each tool's saved configuration is its default behaviour; pass arguments only to adjust a call to the user's current request (e.g. tweak a prompt, change a search query). You NEVER modify the workflow itself. You also have get_current_time, which is not a node — it runs nothing and changes nothing.

This workflow's App Triggers are: %s. They describe which GitHub/GitLab event starts the published automation, but they are context only and are not callable chat tools. A chat message is not a webhook delivery and does not contain an App Trigger payload unless that payload is already present in session state.

Rules:
- Execute tools only when the user's request needs them — don't run everything preemptively.
- Prior tool outputs are stored as state (current keys: [%s]) and template tokens like {{nodeId.output.field}} in tool arguments resolve against that state.
- If the user asks for something no tool can do, say so plainly and describe what this workflow CAN do.
- Be concise. Summarize tool results in plain language rather than dumping raw JSON, unless asked.`,
		ast.Name, goalContext, strings.Join(names, ", "), triggerContext, strings.Join(stateKeys, ", "))
}
