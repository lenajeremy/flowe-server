package handlers

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ── Request type ────────────────────────────────────────────────

type aiGenerateRequest struct {
	Prompt       string `json:"prompt"`
	CurrentNodes []any  `json:"currentNodes,omitempty"`
	CurrentEdges []any  `json:"currentEdges,omitempty"`
	// History is the prior conversation as [{role, content}] pairs (user/assistant text only).
	History []map[string]any `json:"history,omitempty"`
}

// ── HTTP client for Anthropic ───────────────────────────────────

var anthropicClient = &http.Client{
	Timeout: 180 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		DisableCompression:    true,
	},
}

// ── Tool definitions ────────────────────────────────────────────

var toolGetNodes = map[string]any{
	"name":        "get_available_nodes",
	"description": "Returns detailed information about all available workflow node types, including their data fields, connection rules, and usage examples. You MUST call this before creating a workflow.",
	"input_schema": map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	},
}

var toolCreateWorkflow = map[string]any{
	"name":        "create_workflow",
	"description": "Creates a workflow on the user's canvas. Call this with the nodes and edges arrays to build the workflow. The workflow will appear on the canvas immediately.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"nodes": map[string]any{
				"type":        "array",
				"description": "Array of workflow nodes",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":   map[string]any{"type": "string", "description": "Unique node ID, e.g. 'node-1'"},
						"type": map[string]any{"type": "string", "description": "Node type matching one of the available types"},
						"position": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"x": map[string]any{"type": "number"},
								"y": map[string]any{"type": "number"},
							},
							"required": []string{"x", "y"},
						},
						"data": map[string]any{
							"type":        "object",
							"description": "Node configuration data. Must include nodeType and label fields, plus type-specific fields from get_available_nodes.",
						},
					},
					"required": []string{"id", "type", "position", "data"},
				},
			},
			"edges": map[string]any{
				"type":        "array",
				"description": "Array of connections between nodes",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":           map[string]any{"type": "string", "description": "Unique edge ID, e.g. 'edge-1'"},
						"source":       map[string]any{"type": "string", "description": "Source node ID"},
						"target":       map[string]any{"type": "string", "description": "Target node ID"},
						"sourceHandle": map[string]any{"type": "string", "description": "For branch nodes: 'true' or 'false'"},
					},
					"required": []string{"id", "source", "target"},
				},
			},
		},
		"required": []string{"nodes", "edges"},
	},
}

func getAvailableNodesResult() string {
	nodes := []map[string]any{
		{
			"type": "textInput", "label": "Text Input", "category": "Inputs",
			"description": "Provides static text as input to downstream nodes. Useful for fixed prompts, API endpoints, or template text.",
			"dataFields":  map[string]any{"defaultValue": "string – the text content this node outputs"},
			"handles":     map[string]any{"inputs": []string{}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "imageInput", "label": "Image Input", "category": "Inputs",
			"description": "Accepts an uploaded image file (base64 data URL). When connected to an LLM node, the image is sent as a vision content block.",
			"dataFields":  map[string]any{"imageUrl": "string – base64 data URL (set by user upload in UI, leave empty)"},
			"handles":     map[string]any{"inputs": []string{}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "llm", "label": "LLM", "category": "AI",
			"description": "Calls an AI language model (OpenAI or Anthropic). Reference upstream outputs in prompts using {{nodeId.output}} template syntax.",
			"dataFields": map[string]any{
				"model": "string – 'gpt-4o', 'gpt-4o-mini', 'claude-sonnet-4-6', 'claude-haiku-4-5-20251001'",
				"systemPrompt": "string – system instructions for the model",
				"userPrompt":   "string – user message. Use {{nodeId.output}} to inject upstream data",
				"temperature":  "number – 0 to 1, controls randomness (default 0.7)",
				"maxTokens":    "number – max response length (default 1024)",
			},
			"handles": map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
			"notes":   "When an imageInput node's output is referenced in userPrompt via {{nodeId.output}}, the image is automatically sent as a vision block.",
		},
		{
			"type": "branch", "label": "Branch", "category": "Logic",
			"description": "Conditional fork. The condition is evaluated against the upstream node's output. Supports JS expressions or plain-English conditions (evaluated by LLM).",
			"dataFields":  map[string]any{"condition": "string – e.g. 'output.includes(\"error\")' or plain English"},
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"true (right-top)", "false (right-bottom)"}},
			"notes":       "Outgoing edges MUST specify sourceHandle as 'true' or 'false'.",
		},
		{
			"type": "loop", "label": "Loop", "category": "Logic",
			"description": "Iterates over a JSON array. The upstream node must output a JSON array or an object with the specified field.",
			"dataFields":  map[string]any{"loopOverField": "string – JSON field name containing the array, or empty if upstream is already an array"},
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right) – connects to loop body"}},
			"notes":       "Loop body nodes connect FROM the loop node. Each iteration receives the current item.",
		},
		{
			"type": "humanApproval", "label": "Human Approval", "category": "Logic",
			"description": "Pauses workflow and waits for human to approve or reject.",
			"dataFields":  map[string]any{"approvalMessage": "string – message shown to reviewer", "approvalTimeout": "number – seconds (default 7 days)", "approvalEmail": "string – optional email to notify"},
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "httpRequest", "label": "HTTP Request", "category": "Actions",
			"description": "Makes an HTTP request. Headers and body support {{nodeId.output}} templates.",
			"dataFields":  map[string]any{"url": "string – request URL", "method": "string – GET, POST, PUT, DELETE, PATCH", "requestHeaders": "string – JSON headers object", "requestBody": "string – body for POST/PUT/PATCH"},
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "emailSend", "label": "Send Email", "category": "Actions",
			"description": "Sends an email via Resend. All fields support {{nodeId.output}} templates.",
			"dataFields":  map[string]any{"emailTo": "string – recipient", "emailSubject": "string – subject", "emailBody": "string – body text"},
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "webhookTrigger", "label": "Webhook Trigger", "category": "Triggers",
			"description": "Starts workflow when an HTTP POST is received. The payload is available as this node's output.",
			"dataFields":  map[string]any{"label": "string – display name"},
			"handles":     map[string]any{"inputs": []string{}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "scheduledTrigger", "label": "Scheduled Trigger", "category": "Triggers",
			"description": "Starts workflow on a recurring schedule.",
			"dataFields":  map[string]any{"interval": "string – '5m', '15m', '30m', '1h', '6h', '12h', '24h'"},
			"handles":     map[string]any{"inputs": []string{}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "notion", "label": "Notion", "category": "Integrations",
			"description": "Notion API: create pages, query databases, append blocks.",
			"dataFields":  map[string]any{"integrationOp": "'create_page'|'query_database'|'append_blocks'", "integrationToken": "string (user fills in UI)", "notionDatabaseId": "string", "notionPageId": "string", "notionTitle": "string (templates ok)", "notionContent": "string (templates ok)", "notionFilter": "string – JSON filter"},
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "linear", "label": "Linear", "category": "Integrations",
			"description": "Linear API: create issues, list issues, create comments.",
			"dataFields":  map[string]any{"integrationOp": "'create_issue'|'get_issues'|'create_comment'", "integrationToken": "string (user fills in UI)", "linearTeamId": "string", "linearIssueId": "string", "linearTitle": "string", "linearDescription": "string", "linearPriority": "number 0-4", "linearCommentBody": "string", "linearLimit": "number"},
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "textOutput", "label": "Text Output", "category": "Outputs",
			"description": "Displays the final result of the pipeline.",
			"dataFields":  map[string]any{"label": "string – display name"},
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{}},
		},
	}

	result := map[string]any{
		"nodes": nodes,
		"connectionRules": map[string]any{
			"general":   "Nodes connect left-to-right via edges: { source, target }.",
			"templates": "LLM, HTTP, email, integrations support {{nodeId.output}} in text fields.",
			"branching": "Branch edges MUST have sourceHandle 'true' or 'false'.",
			"loops":     "Loop body nodes connect FROM the loop node.",
			"triggers":  "Trigger nodes have no inputs — they start workflows.",
			"outputs":   "Output nodes have no outputs — they display results.",
		},
		"layoutGuidelines": map[string]any{
			"spacing":   "~250px horizontal, ~150px vertical between nodes",
			"start":     "Begin at x:100, y:100",
			"flow":      "Left-to-right for linear pipelines",
			"branching": "Offset true/false paths vertically",
		},
	}

	b, _ := json.Marshal(result)
	return string(b)
}

// ── System prompt ───────────────────────────────────────────────

const workflowSystemPrompt = `You are a workflow builder AI. The user describes a workflow in natural language and you build it using your tools.

Your workflow:
1. ALWAYS call get_available_nodes first to learn what nodes exist and their data schemas.
2. Design the workflow based on the user's request.
3. Call create_workflow with the nodes and edges arrays to build it on the canvas.
4. After calling create_workflow, respond with a brief explanation of what you built and any configuration the user needs to do (API keys, tokens, URLs to fill in, etc).

Rules:
- Every node's data object MUST include nodeType (matching the node type) and label (descriptive name).
- Use {{nodeId.output}} in LLM/HTTP/email/integration fields to reference upstream nodes.
- For branch nodes, outgoing edges must have sourceHandle "true" or "false".
- Space nodes ~250px apart horizontally, start at x:100, y:100, flow left-to-right.
- Always call both tools — do not output raw JSON in your text response.`

// ── Handler ─────────────────────────────────────────────────────

func (h *WorkflowHandler) AIGenerate(c *gin.Context) {
	var req aiGenerateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	if anthropicKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ANTHROPIC_API_KEY not configured on server"})
		return
	}

	// Build user prompt with current workflow context
	userPrompt := req.Prompt
	if len(req.CurrentNodes) > 0 {
		nodesJSON, _ := json.Marshal(req.CurrentNodes)
		edgesJSON, _ := json.Marshal(req.CurrentEdges)
		userPrompt += fmt.Sprintf("\n\nCurrent workflow on the canvas:\nNodes: %s\nEdges: %s\n\nYou can modify, extend, or replace this workflow.", nodesJSON, edgesJSON)
	}

	// Set up SSE
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		fmt.Fprintf(c.Writer, "event: error\ndata: streaming not supported\n\n")
		return
	}

	allTools := []map[string]any{toolGetNodes, toolCreateWorkflow}

	// Build message history — prior turns as plain text role/content pairs
	var messages []map[string]any
	for _, h := range req.History {
		role, _ := h["role"].(string)
		content, _ := h["content"].(string)
		if (role == "user" || role == "assistant") && content != "" {
			messages = append(messages, map[string]any{"role": role, "content": content})
		}
	}
	messages = append(messages, map[string]any{"role": "user", "content": userPrompt})

	// Multi-turn tool loop: keep going until the model stops calling tools
	for round := 0; round < 5; round++ {
		sendSSE(c.Writer, flusher, "thinking", statusForRound(round))

		body, _ := json.Marshal(map[string]any{
			"model":      "claude-sonnet-4-6",
			"max_tokens": 16000,
			"thinking": map[string]any{
				"type":          "enabled",
				"budget_tokens": 10000,
			},
			"stream":   true,
			"system":   workflowSystemPrompt,
			"tools":    allTools,
			"messages": messages,
		})

		resp, err := doAnthropicRequest(c, anthropicKey, body)
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

		// Append assistant message — strip thinking blocks since the API
		// requires a signature field on them that we don't capture from streaming.
		var filteredContent []any
		for _, block := range assistantContent {
			bm, ok := block.(map[string]any)
			if ok && bm["type"] == "thinking" {
				continue
			}
			filteredContent = append(filteredContent, block)
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": filteredContent})

		if stopReason != "tool_use" {
			// Model is done — no more tool calls
			break
		}

		// Process tool calls and build results
		var toolResults []any
		for _, block := range assistantContent {
			bm, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if bm["type"] != "tool_use" {
				continue
			}
			toolName, _ := bm["name"].(string)
			toolID, _ := bm["id"].(string)

			var output string
			switch toolName {
			case "get_available_nodes":
				output = getAvailableNodesResult()
			case "create_workflow":
				// Extract the input and send it as a workflow event
				inputJSON, _ := json.Marshal(bm["input"])
				sendSSE(c.Writer, flusher, "workflow", string(inputJSON))
				output = `{"status": "success", "message": "Workflow created on the canvas."}`
			default:
				output = fmt.Sprintf(`{"error": "unknown tool: %s"}`, toolName)
			}

			toolResults = append(toolResults, map[string]any{
				"type":        "tool_result",
				"tool_use_id": toolID,
				"content":     output,
			})
		}

		messages = append(messages, map[string]any{"role": "user", "content": toolResults})
	}

	sendSSE(c.Writer, flusher, "done", "")
}

func statusForRound(round int) string {
	switch round {
	case 0:
		return "Analyzing request..."
	case 1:
		return "\nDesigning workflow..."
	case 2:
		return "\nBuilding on canvas..."
	default:
		return "\nFinalizing..."
	}
}

// consumeStream reads the Anthropic SSE stream, sends thinking/text events to
// the client, and returns the stop_reason + full content blocks array.
func consumeStream(c *gin.Context, resp *http.Response, flusher http.Flusher) (string, []any, error) {
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("anthropic %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		contentBlocks []any
		currentBlock  map[string]any
		toolInputBuf  strings.Builder
		stopReason    string
	)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event streamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "content_block_start":
			if event.ContentBlock != nil {
				currentBlock = map[string]any{"type": event.ContentBlock.Type}
				if event.ContentBlock.Type == "tool_use" {
					currentBlock["id"] = event.ContentBlock.ID
					currentBlock["name"] = event.ContentBlock.Name
					toolInputBuf.Reset()
				}
				if event.ContentBlock.Type == "thinking" {
					currentBlock["thinking"] = ""
				}
				if event.ContentBlock.Type == "text" {
					currentBlock["text"] = ""
				}
			}

		case "content_block_delta":
			if event.Delta != nil && currentBlock != nil {
				switch event.Delta.Type {
				case "thinking_delta":
					sendSSE(c.Writer, flusher, "thinking", event.Delta.Thinking)
					currentBlock["thinking"] = currentBlock["thinking"].(string) + event.Delta.Thinking
				case "text_delta":
					sendSSE(c.Writer, flusher, "text", event.Delta.Text)
					currentBlock["text"] = currentBlock["text"].(string) + event.Delta.Text
				case "input_json_delta":
					toolInputBuf.WriteString(event.Delta.PartialJSON)
				}
			}

		case "content_block_stop":
			if currentBlock != nil {
				if currentBlock["type"] == "tool_use" {
					// Parse the accumulated tool input
					var input any
					if err := json.Unmarshal([]byte(toolInputBuf.String()), &input); err == nil {
						currentBlock["input"] = input
					} else {
						currentBlock["input"] = map[string]any{}
					}
				}
				contentBlocks = append(contentBlocks, currentBlock)
				currentBlock = nil
			}

		case "message_delta":
			if event.Delta != nil && event.Delta.StopReason != "" {
				stopReason = event.Delta.StopReason
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return stopReason, contentBlocks, fmt.Errorf("scanner: %w", err)
	}

	return stopReason, contentBlocks, nil
}

func doAnthropicRequest(c *gin.Context, apiKey string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := anthropicClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	return resp, nil
}

func sendSSE(w io.Writer, flusher http.Flusher, eventType, data string) {
	// SSE data cannot contain raw newlines — each line needs its own "data:" prefix
	lines := strings.Split(data, "\n")
	fmt.Fprintf(w, "event: %s\n", eventType)
	for _, line := range lines {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
	flusher.Flush()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// ── Anthropic streaming types ───────────────────────────────────

type streamEvent struct {
	Type         string        `json:"type"`
	ContentBlock *streamBlock  `json:"content_block,omitempty"`
	Delta        *streamDelta  `json:"delta,omitempty"`
}

type streamBlock struct {
	Type string `json:"type"` // "thinking", "text", "tool_use"
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}
