package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"workflow-ai/server/config"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
)

// AgentTurnEventType is the host-neutral stream emitted by the workflow agent.
// HTTP/SSE, Slack, and future hosts translate these events for their surface.
type AgentTurnEventType string

const (
	AgentTurnThinking   AgentTurnEventType = "thinking"
	AgentTurnText       AgentTurnEventType = "text"
	AgentTurnToolStart  AgentTurnEventType = "tool_start"
	AgentTurnToolResult AgentTurnEventType = "tool_result"
	AgentTurnApproval   AgentTurnEventType = "approval_required"
	AgentTurnError      AgentTurnEventType = "error"
	AgentTurnDone       AgentTurnEventType = "done"
)

type AgentToolActivity struct {
	Node   string `json:"node"`
	NodeID string `json:"nodeId"`
	Op     string `json:"op,omitempty"`
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

type AgentApprovalActivity struct {
	ApprovalID string         `json:"approvalId"`
	Node       string         `json:"node"`
	NodeID     string         `json:"nodeId"`
	Operation  string         `json:"operation"`
	Effect     AgentEffect    `json:"effect"`
	Reason     string         `json:"reason"`
	Details    map[string]any `json:"details"`
}

type AgentTurnEvent struct {
	Type     AgentTurnEventType
	Text     string
	Tool     *AgentToolActivity
	Approval *AgentApprovalActivity
}

type AgentTurnSink func(AgentTurnEvent)

type agentRuntimeModel struct {
	Spec        chatModelSpec
	APIKey      string
	ProviderURL string
}

// prepareAgentRuntimeModel resolves server-owned model credentials before a
// turn starts. Hosted agents use Fernary's credentials, never a teammate's.
func prepareAgentRuntimeModel(modelID string) (agentRuntimeModel, error) {
	model := resolveChatModel(modelID)
	provider := chatProviders[model.Provider]
	apiKey := os.Getenv(provider.KeyEnv)
	if apiKey == "" {
		return agentRuntimeModel{}, fmt.Errorf("%s not configured on server", provider.KeyEnv)
	}
	return agentRuntimeModel{Spec: model, APIKey: apiKey, ProviderURL: provider.URL}, nil
}

type AgentTurnInput struct {
	Session         *models.ChatSession
	Workflow        executor.WorkflowAST
	Policy          *AgentCapabilityPolicy
	OwnerUserID     string
	OrganizationID  string
	Message         string
	StoredMessage   string
	HistoryLimit    int
	HistoryTextCap  int
	Goal            string
	Model           agentRuntimeModel
	RequestApproval func(context.Context, AgentAuthorizedCall, map[string]string) (*AgentApprovalActivity, error)
}

type AgentTurnResult struct {
	Text      string
	ToolCalls []agentToolCallRecord
}

func emitAgentTurn(sink AgentTurnSink, event AgentTurnEvent) {
	if sink != nil {
		sink(event)
	}
}

var ErrAgentApprovalPending = errors.New("agent tool approval pending")

type agentToolExecution struct {
	Content string
	Pause   bool
}

// RunAgentTurn is the reusable workflow-agent runtime. It has no dependency
// on Gin, HTTP, SSE, or a particular team-chat host.
func (h *WorkflowHandler) RunAgentTurn(ctx context.Context, input AgentTurnInput, sink AgentTurnSink) (AgentTurnResult, error) {
	if input.Session == nil {
		return AgentTurnResult{}, fmt.Errorf("agent session is required")
	}

	state := map[string]string{}
	_ = json.Unmarshal(input.Session.State, &state)
	var history []agentStoredMessage
	_ = json.Unmarshal(input.Session.Messages, &history)
	history = boundedAgentHistory(history, input.HistoryLimit, input.HistoryTextCap)

	policy := input.Policy
	if policy != nil {
		normalized, _ := normalizeAgentCapabilityPolicy(input.Workflow, *policy)
		policy = &normalized
	}
	tools := buildAgentToolsWithPolicy(input.Workflow, policy)
	runID := "chat-" + input.Session.ID.String()
	keys := executor.APIKeys{
		Anthropic: config.GetEnv("ANTHROPIC_API_KEY"),
		OpenAI:    config.GetEnv("OPENAI_API_KEY"),
		Brave:     config.GetEnv("BRAVE_API_KEY"),
		Jina:      config.GetEnv("JINA_API_KEY"),
	}

	var callRecords []agentToolCallRecord
	execTool := func(name string, rawInput any) agentToolExecution {
		if name == executor.ClockToolName {
			tz := ""
			if args, ok := rawInput.(map[string]any); ok {
				tz, _ = args["timezone"].(string)
			}
			return agentToolExecution{Content: executor.CurrentTime(tz)}
		}

		var tool *agentTool
		for i := range tools {
			if tools[i].Schema["name"] == name {
				tool = &tools[i]
				break
			}
		}
		if tool == nil {
			return agentToolExecution{Content: fmt.Sprintf(`{"error":"unknown tool %s"}`, name)}
		}

		overrides, _ := rawInput.(map[string]any)
		op := agentEffectiveOp(tool.Node.Data, overrides)
		if policy != nil {
			authorized, err := authorizeAgentToolCall(*policy, tool.Node, rawInput)
			if err != nil {
				record := agentToolCallRecord{
					Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op, Status: "error",
				}
				callRecords = append(callRecords, record)
				emitAgentTurn(sink, AgentTurnEvent{
					Type: AgentTurnToolResult,
					Tool: &AgentToolActivity{
						Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op,
						Status: "error", Error: err.Error(),
					},
				})
				return agentToolExecution{Content: fmt.Sprintf(`{"error":%q}`, err.Error())}
			}
			overrides = authorized.Overrides
			op = authorized.Operation.Label
			if authorized.Operation.Effect != AgentEffectRead {
				if input.RequestApproval == nil {
					err := fmt.Errorf("operation %q requires teammate approval", authorized.Operation.ID)
					record := agentToolCallRecord{
						Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op, Status: "error",
					}
					callRecords = append(callRecords, record)
					emitAgentTurn(sink, AgentTurnEvent{
						Type: AgentTurnToolResult,
						Tool: &AgentToolActivity{
							Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op,
							Status: "error", Error: err.Error(),
						},
					})
					return agentToolExecution{Content: fmt.Sprintf(`{"error":%q}`, err.Error())}
				}
				stateSnapshot := make(map[string]string, len(state))
				for key, value := range state {
					stateSnapshot[key] = value
				}
				approval, err := input.RequestApproval(ctx, authorized, stateSnapshot)
				if err != nil {
					record := agentToolCallRecord{
						Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op, Status: "error",
					}
					callRecords = append(callRecords, record)
					emitAgentTurn(sink, AgentTurnEvent{
						Type: AgentTurnToolResult,
						Tool: &AgentToolActivity{
							Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op,
							Status: "error", Error: err.Error(),
						},
					})
					return agentToolExecution{Content: fmt.Sprintf(`{"error":%q}`, err.Error())}
				}
				record := agentToolCallRecord{
					Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op, Status: "pending",
				}
				callRecords = append(callRecords, record)
				emitAgentTurn(sink, AgentTurnEvent{
					Type: AgentTurnApproval, Approval: approval,
				})
				return agentToolExecution{
					Content: fmt.Sprintf(`{"status":"approval_pending","approval_id":%q}`, approval.ApprovalID),
					Pause:   true,
				}
			}
		}
		emitAgentTurn(sink, AgentTurnEvent{
			Type: AgentTurnToolStart,
			Tool: &AgentToolActivity{Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op},
		})

		out, err := executor.ExecuteSingleNode(
			ctx,
			tool.Node,
			overrides,
			state,
			input.Workflow.Edges,
			keys,
			runID,
			input.OwnerUserID,
			input.OrganizationID,
			nil,
		)
		record := agentToolCallRecord{
			Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op, Status: "ok",
		}
		if err != nil {
			record.Status = "error"
			callRecords = append(callRecords, record)
			emitAgentTurn(sink, AgentTurnEvent{
				Type: AgentTurnToolResult,
				Tool: &AgentToolActivity{
					Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op,
					Status: "error", Error: err.Error(),
				},
			})
			return agentToolExecution{Content: fmt.Sprintf(`{"error":%q}`, err.Error())}
		}

		callRecords = append(callRecords, record)
		state[tool.Node.ID] = truncate(out, agentStateCap)
		emitAgentTurn(sink, AgentTurnEvent{
			Type: AgentTurnToolResult,
			Tool: &AgentToolActivity{
				Node: tool.Node.Data.Label, NodeID: tool.Node.ID, Op: op, Status: "ok",
			},
		})
		return agentToolExecution{Content: truncate(out, agentResultCap)}
	}

	system := agentSystemPromptWithGoal(input.Workflow, tools, state, input.Goal)
	var (
		finalText string
		turnErr   error
	)
	if input.Model.Spec.Provider == "anthropic" {
		finalText, turnErr = agentAnthropicLoop(
			ctx, input.Model.Spec, input.Model.APIKey, system, history, input.Message, tools, sink, execTool,
		)
	} else {
		finalText, turnErr = agentOpenAILoop(
			ctx, input.Model.Spec, input.Model.APIKey, input.Model.ProviderURL,
			system, history, input.Message, tools, sink, execTool,
		)
	}
	if turnErr != nil && !errors.Is(turnErr, ErrAgentApprovalPending) {
		emitAgentTurn(sink, AgentTurnEvent{Type: AgentTurnError, Text: turnErr.Error()})
	}

	storedMessage := strings.TrimSpace(input.StoredMessage)
	if storedMessage == "" {
		storedMessage = input.Message
	}
	history = append(history,
		agentStoredMessage{Role: "user", Content: storedMessage},
		agentStoredMessage{Role: "assistant", Content: finalText, ToolCalls: callRecords},
	)
	history = boundedAgentHistory(history, input.HistoryLimit, input.HistoryTextCap)
	msgJSON, _ := json.Marshal(history)
	stateJSON, _ := json.Marshal(state)
	updates := map[string]any{
		"messages": models.JSONB(msgJSON),
		"state":    models.JSONB(stateJSON),
	}
	if input.Session.Title == "" || input.Session.Title == "New chat" {
		updates["title"] = truncate(storedMessage, 80)
	}
	if err := h.db.DB.Model(input.Session).Updates(updates).Error; err != nil {
		persistErr := fmt.Errorf("persist agent session: %w", err)
		emitAgentTurn(sink, AgentTurnEvent{Type: AgentTurnError, Text: persistErr.Error()})
		if turnErr == nil {
			turnErr = persistErr
		}
	}
	emitAgentTurn(sink, AgentTurnEvent{Type: AgentTurnDone})

	return AgentTurnResult{Text: finalText, ToolCalls: callRecords}, turnErr
}

func boundedAgentHistory(history []agentStoredMessage, limit, textCap int) []agentStoredMessage {
	if limit > 0 && len(history) > limit {
		history = history[len(history)-limit:]
	}
	if textCap <= 0 {
		return history
	}
	out := make([]agentStoredMessage, len(history))
	copy(out, history)
	for i := range out {
		if len(out[i].Content) > textCap {
			out[i].Content = out[i].Content[:textCap]
		}
	}
	return out
}

func agentAnthropicLoop(ctx context.Context, model chatModelSpec, apiKey, system string, history []agentStoredMessage, userMsg string, tools []agentTool, sink AgentTurnSink, execTool func(string, any) agentToolExecution) (string, error) {
	toolSchemas := make([]map[string]any, 0, len(tools)+1)
	for _, tool := range tools {
		toolSchemas = append(toolSchemas, tool.Schema)
	}
	toolSchemas = append(toolSchemas, map[string]any{
		"name":         executor.ClockToolName,
		"description":  executor.ClockToolDesc,
		"input_schema": executor.ClockToolSchema(),
	})

	var messages []map[string]any
	for _, message := range history {
		if message.Content != "" {
			messages = append(messages, map[string]any{"role": message.Role, "content": message.Content})
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
			"system":     cachedSystem(system),
			"tools":      toolSchemas,
			"messages":   messages,
		})
		resp, err := doAnthropicRequestContext(ctx, apiKey, body)
		if err != nil {
			return finalText.String(), fmt.Errorf("Request failed: %w", err)
		}
		stopReason, assistantContent, streamErr := consumeAnthropicStream(ctx, resp, model.ID, func(eventType, data string) {
			emitAgentTurn(sink, AgentTurnEvent{Type: AgentTurnEventType(eventType), Text: data})
		})
		resp.Body.Close()
		if streamErr != nil {
			return finalText.String(), fmt.Errorf("Stream error: %w", streamErr)
		}
		for _, block := range assistantContent {
			if typed, ok := block.(map[string]any); ok && typed["type"] == "text" {
				if text, ok := typed["text"].(string); ok {
					finalText.WriteString(text)
				}
			}
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": assistantContent})
		if stopReason != "tool_use" {
			break
		}

		var toolResults []any
		for _, block := range assistantContent {
			typed, ok := block.(map[string]any)
			if !ok || typed["type"] != "tool_use" {
				continue
			}
			name, _ := typed["name"].(string)
			id, _ := typed["id"].(string)
			execution := execTool(name, typed["input"])
			toolResults = append(toolResults, map[string]any{
				"type": "tool_result", "tool_use_id": id, "content": execution.Content,
			})
			if execution.Pause {
				return finalText.String(), ErrAgentApprovalPending
			}
		}
		messages = append(messages, map[string]any{"role": "user", "content": toolResults})
	}
	return finalText.String(), nil
}

func agentOpenAILoop(ctx context.Context, model chatModelSpec, apiKey, url, system string, history []agentStoredMessage, userMsg string, tools []agentTool, sink AgentTurnSink, execTool func(string, any) agentToolExecution) (string, error) {
	toolSchemas := make([]map[string]any, 0, len(tools)+1)
	for _, tool := range tools {
		toolSchemas = append(toolSchemas, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Schema["name"],
				"description": tool.Schema["description"],
				"parameters":  tool.Schema["input_schema"],
			},
		})
	}
	toolSchemas = append(toolSchemas, map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": executor.ClockToolName, "description": executor.ClockToolDesc,
			"parameters": executor.ClockToolSchema(),
		},
	})

	messages := cachedSystemMessages(system)
	for _, message := range history {
		if message.Content != "" {
			messages = append(messages, map[string]any{"role": message.Role, "content": message.Content})
		}
	}
	messages = append(messages, map[string]any{"role": "user", "content": userMsg})

	var finalText strings.Builder
	for round := 0; round < agentMaxToolRounds; round++ {
		body, _ := json.Marshal(map[string]any{
			"model": model.ID, "stream": true, "messages": messages, "tools": toolSchemas,
		})
		resp, err := doOpenAIRequestContext(ctx, url, apiKey, body)
		if err != nil {
			return finalText.String(), fmt.Errorf("Request failed: %w", err)
		}
		content, toolCalls, streamErr := consumeOpenAIProviderStream(ctx, resp, model.Provider, model.ID, func(eventType, data string) {
			emitAgentTurn(sink, AgentTurnEvent{Type: AgentTurnEventType(eventType), Text: data})
		})
		resp.Body.Close()
		if streamErr != nil {
			return finalText.String(), fmt.Errorf("Stream error: %w", streamErr)
		}
		finalText.WriteString(content)
		assistantMessage := map[string]any{"role": "assistant", "content": content}
		if len(toolCalls) > 0 {
			assistantMessage["tool_calls"] = toolCalls
		}
		messages = append(messages, assistantMessage)
		if len(toolCalls) == 0 {
			break
		}
		for _, toolCall := range toolCalls {
			function, _ := toolCall["function"].(map[string]any)
			name, _ := function["name"].(string)
			arguments, _ := function["arguments"].(string)
			var toolInput any
			_ = json.Unmarshal([]byte(arguments), &toolInput)
			id, _ := toolCall["id"].(string)
			execution := execTool(name, toolInput)
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": id, "content": execution.Content,
			})
			if execution.Pause {
				return finalText.String(), ErrAgentApprovalPending
			}
		}
	}
	return finalText.String(), nil
}
