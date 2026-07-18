package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"workflow-ai/server/config"
	"workflow-ai/server/internal/auth"
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
		UserID:     auth.UserID(c),
		WorkflowID: wf.ID.String(),
		Title:      "New chat",
		Messages:   models.JSONB(`[]`),
		State:      models.JSONB(`{}`),
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
		Where("workflow_id = ? AND user_id = ?", c.Param("id"), auth.UserID(c)).
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
	if err := h.db.DB.First(&sess, "id = ? AND user_id = ?", c.Param("id"), auth.UserID(c)).Error; err != nil {
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
	name := label + "__" + toolNameRe.ReplaceAllString(node.ID, "_")
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// agentSkipNode: control-flow/display/trigger nodes are not tools — the
// orchestrator itself is the branching and the chat is the trigger/output.
func agentSkipNode(t executor.NodeType) bool {
	switch t {
	case executor.NodeTypeBranch, executor.NodeTypeLoop, executor.NodeTypeTextOutput,
		executor.NodeTypeWebhookTrigger, executor.NodeTypeScheduledTrigger:
		return true
	}
	return false
}

// buildAgentTools generates one tool per eligible canvas node. Input schema
// properties are the node type's overridable fields: names/types come from
// FlowNodeData reflection, descriptions from the AI catalog. All optional —
// omitted fields fall back to the node's saved config.
func buildAgentTools(ast executor.WorkflowAST) []agentTool {
	tools := make([]agentTool, 0, len(ast.Nodes))
	for _, node := range ast.Nodes {
		if agentSkipNode(node.Data.NodeType) {
			continue
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
		for field, doc := range fieldDocs {
			if field == "label" || field == "integrationToken" {
				continue
			}
			jsonType, exists := flowDataFieldType(field)
			if !exists {
				continue
			}
			docStr, _ := doc.(string)
			props[field] = map[string]any{"type": jsonType, "description": docStr}
		}

		// Saved config = the tool's defaults; show them so the model knows
		// what happens with no arguments.
		savedJSON, _ := json.Marshal(node.Data)
		toolDesc := fmt.Sprintf(
			"Run the workflow node %q (%s). %s\nSaved configuration (used for any argument you omit): %s\nPass arguments ONLY to adjust behaviour for this one call — the workflow itself is never modified.",
			node.Data.Label, node.Data.NodeType, desc, truncate(string(savedJSON), 1200),
		)

		tools = append(tools, agentTool{
			Schema: map[string]any{
				"name":        agentToolName(node),
				"description": toolDesc,
				"input_schema": map[string]any{
					"type":       "object",
					"properties": props,
				},
			},
			Node: node,
		})
	}
	return tools
}

// flowDataFieldType maps a FlowNodeData JSON field to its JSON-schema type.
func flowDataFieldType(field string) (string, bool) {
	t, ok := flowDataFieldTypes()[field]
	return t, ok
}

var flowDataTypesCache map[string]string

func flowDataFieldTypes() map[string]string {
	if flowDataTypesCache != nil {
		return flowDataTypesCache
	}
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
	return out
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
	Status string `json:"status"` // ok | error
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

	model := resolveChatModel(req.Model)
	prov := chatProviders[model.Provider]
	apiKey := os.Getenv(prov.KeyEnv)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": prov.KeyEnv + " not configured on server"})
		return
	}

	// Session state + history
	state := map[string]string{}
	_ = json.Unmarshal(sess.State, &state)
	var history []agentStoredMessage
	_ = json.Unmarshal(sess.Messages, &history)

	tools := buildAgentTools(ast)

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

	runID := "chat-" + sess.ID.String()
	keys := executor.APIKeys{
		Anthropic: config.GetEnv("ANTHROPIC_API_KEY"),
		OpenAI:    config.GetEnv("OPENAI_API_KEY"),
		Brave:     config.GetEnv("BRAVE_API_KEY"),
		Jina:      config.GetEnv("JINA_API_KEY"),
	}

	// execTool runs one node with overrides against session state.
	var callRecords []agentToolCallRecord
	execTool := func(name string, input any) string {
		var tool *agentTool
		for i := range tools {
			if tools[i].Schema["name"] == name {
				tool = &tools[i]
				break
			}
		}
		if tool == nil {
			return fmt.Sprintf(`{"error":"unknown tool %s"}`, name)
		}
		overrides, _ := input.(map[string]any)
		op := agentEffectiveOp(tool.Node.Data, overrides)
		chip, _ := json.Marshal(map[string]string{"node": tool.Node.Data.Label, "nodeId": tool.Node.ID, "op": op})
		sendSSE(c.Writer, flusher, "tool_start", string(chip))

		out, err := executor.ExecuteSingleNode(c.Request.Context(), tool.Node, overrides, state, ast.Edges, keys, runID, uid, nil)
		rec := agentToolCallRecord{Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op, Status: "ok"}
		if err != nil {
			rec.Status = "error"
			callRecords = append(callRecords, rec)
			result, _ := json.Marshal(map[string]string{"node": tool.Node.Data.Label, "nodeId": tool.Node.ID, "op": op, "status": "error", "error": err.Error()})
			sendSSE(c.Writer, flusher, "tool_result", string(result))
			return fmt.Sprintf(`{"error":%q}`, err.Error())
		}
		callRecords = append(callRecords, rec)
		state[tool.Node.ID] = truncate(out, agentStateCap)
		result, _ := json.Marshal(map[string]string{"node": tool.Node.Data.Label, "nodeId": tool.Node.ID, "op": op, "status": "ok"})
		sendSSE(c.Writer, flusher, "tool_result", string(result))
		return truncate(out, agentResultCap)
	}

	system := agentSystemPrompt(ast, tools, state)

	var finalText string
	if model.Provider == "anthropic" {
		finalText = h.agentAnthropicLoop(c, flusher, model, apiKey, system, history, req.Message, tools, execTool)
	} else {
		finalText = h.agentOpenAILoop(c, flusher, model, apiKey, prov.URL, system, history, req.Message, tools, execTool)
	}
	sendSSE(c.Writer, flusher, "done", "")

	slog.InfoContext(c.Request.Context(), "agent chat turn finished",
		"session_id", sess.ID.String(),
		"duration_ms", time.Since(turnStart).Milliseconds(),
		"tool_calls", len(callRecords))

	// Persist transcript + state. Title from the first user message.
	history = append(history,
		agentStoredMessage{Role: "user", Content: req.Message},
		agentStoredMessage{Role: "assistant", Content: finalText, ToolCalls: callRecords},
	)
	msgJSON, _ := json.Marshal(history)
	stateJSON, _ := json.Marshal(state)
	updates := map[string]any{"messages": models.JSONB(msgJSON), "state": models.JSONB(stateJSON)}
	if sess.Title == "" || sess.Title == "New chat" {
		updates["title"] = truncate(req.Message, 80)
	}
	h.db.DB.Model(sess).Updates(updates)
}

// agentSystemPrompt frames the workflow-as-agent contract.
func agentSystemPrompt(ast executor.WorkflowAST, tools []agentTool, state map[string]string) string {
	var names []string
	for _, t := range tools {
		names = append(names, fmt.Sprintf("%s (%s)", t.Node.Data.Label, t.Node.Data.NodeType))
	}
	var stateKeys []string
	for k := range state {
		stateKeys = append(stateKeys, k)
	}
	return fmt.Sprintf(`You are the workflow %q, acting as a conversational agent for its owner.

Your tools are this workflow's nodes: %s. Each tool's saved configuration is its default behaviour; pass arguments only to adjust a call to the user's current request (e.g. tweak a prompt, change a search query). You NEVER modify the workflow itself.

Rules:
- Execute tools only when the user's request needs them — don't run everything preemptively.
- Prior tool outputs are stored as state (current keys: [%s]) and template tokens like {{nodeId.output.field}} in tool arguments resolve against that state.
- If the user asks for something no tool can do, say so plainly and describe what this workflow CAN do.
- Be concise. Summarize tool results in plain language rather than dumping raw JSON, unless asked.`,
		ast.Name, strings.Join(names, ", "), strings.Join(stateKeys, ", "))
}

// ── Provider loops (mirrors the builder-chat loops, different tools) ──

func (h *WorkflowHandler) agentAnthropicLoop(c *gin.Context, flusher http.Flusher, model chatModelSpec, apiKey, system string, history []agentStoredMessage, userMsg string, tools []agentTool, execTool func(string, any) string) string {
	toolSchemas := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		toolSchemas = append(toolSchemas, t.Schema)
	}
	var messages []map[string]any
	for _, m := range history {
		if m.Content != "" {
			messages = append(messages, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	messages = append(messages, map[string]any{"role": "user", "content": userMsg})

	var finalText strings.Builder
	for round := 0; round < agentMaxToolRounds; round++ {
		body, _ := json.Marshal(map[string]any{
			"model":      model.ID,
			"max_tokens": 8000,
			"thinking":   model.Thinking,
			"stream":     true,
			"system":     system,
			"tools":      toolSchemas,
			"messages":   messages,
		})
		resp, err := doAnthropicRequest(c, apiKey, body)
		if err != nil {
			sendSSE(c.Writer, flusher, "error", fmt.Sprintf("Request failed: %v", err))
			break
		}
		stopReason, assistantContent, err := consumeStream(c, resp, flusher)
		resp.Body.Close()
		if err != nil {
			sendSSE(c.Writer, flusher, "error", fmt.Sprintf("Stream error: %v", err))
			break
		}
		for _, block := range assistantContent {
			if bm, ok := block.(map[string]any); ok && bm["type"] == "text" {
				if txt, ok := bm["text"].(string); ok {
					finalText.WriteString(txt)
				}
			}
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": assistantContent})
		if stopReason != "tool_use" {
			break
		}
		var toolResults []any
		for _, block := range assistantContent {
			bm, ok := block.(map[string]any)
			if !ok || bm["type"] != "tool_use" {
				continue
			}
			name, _ := bm["name"].(string)
			id, _ := bm["id"].(string)
			toolResults = append(toolResults, map[string]any{
				"type": "tool_result", "tool_use_id": id, "content": execTool(name, bm["input"]),
			})
		}
		messages = append(messages, map[string]any{"role": "user", "content": toolResults})
	}
	return finalText.String()
}

func (h *WorkflowHandler) agentOpenAILoop(c *gin.Context, flusher http.Flusher, model chatModelSpec, apiKey, url, system string, history []agentStoredMessage, userMsg string, tools []agentTool, execTool func(string, any) string) string {
	toolSchemas := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		toolSchemas = append(toolSchemas, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Schema["name"],
				"description": t.Schema["description"],
				"parameters":  t.Schema["input_schema"],
			},
		})
	}
	messages := []map[string]any{{"role": "system", "content": system}}
	for _, m := range history {
		if m.Content != "" {
			messages = append(messages, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	messages = append(messages, map[string]any{"role": "user", "content": userMsg})

	var finalText strings.Builder
	for round := 0; round < agentMaxToolRounds; round++ {
		body, _ := json.Marshal(map[string]any{
			"model":    model.ID,
			"stream":   true,
			"messages": messages,
			"tools":    toolSchemas,
		})
		resp, err := doOpenAIRequest(c, url, apiKey, body)
		if err != nil {
			sendSSE(c.Writer, flusher, "error", fmt.Sprintf("Request failed: %v", err))
			break
		}
		content, toolCalls, err := consumeOpenAIStream(c, resp, flusher)
		resp.Body.Close()
		if err != nil {
			sendSSE(c.Writer, flusher, "error", fmt.Sprintf("Stream error: %v", err))
			break
		}
		finalText.WriteString(content)
		assistantMsg := map[string]any{"role": "assistant", "content": content}
		if len(toolCalls) > 0 {
			assistantMsg["tool_calls"] = toolCalls
		}
		messages = append(messages, assistantMsg)
		if len(toolCalls) == 0 {
			break
		}
		for _, tc := range toolCalls {
			fn, _ := tc["function"].(map[string]any)
			name, _ := fn["name"].(string)
			argsRaw, _ := fn["arguments"].(string)
			var input any
			_ = json.Unmarshal([]byte(argsRaw), &input)
			id, _ := tc["id"].(string)
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": id, "content": execTool(name, input),
			})
		}
	}
	return finalText.String()
}
