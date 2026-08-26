package handlers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/telemetry"
	"workflow-ai/server/internal/triggers"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
)

// ── Request type ────────────────────────────────────────────────

type aiGenerateRequest struct {
	Prompt       string `json:"prompt"`
	Model        string `json:"model,omitempty"`
	CurrentNodes []any  `json:"currentNodes,omitempty"`
	CurrentEdges []any  `json:"currentEdges,omitempty"`
	// WorkflowID is the canvas workflow's db id — scopes list_data_stores to
	// stores this workflow can actually use. Empty for unsaved workflows.
	WorkflowID string `json:"workflowId,omitempty"`
	// History is the prior conversation as [{role, content}] pairs (user/assistant text only).
	History []map[string]any `json:"history,omitempty"`
}

// applyOpsToCanvas mirrors the frontend's applyPatch onto the request's working
// copy of the canvas, so get_current_workflow sees the edits the model just made
// instead of the canvas as it was when the turn started.
func applyOpsToCanvas(nodes, edges []any, ops []any) ([]any, []any) {
	idOf := func(v any, key string) string {
		m, _ := v.(map[string]any)
		s, _ := m[key].(string)
		return s
	}
	dropNode := func(list []any, id string) []any {
		out := make([]any, 0, len(list))
		for _, n := range list {
			if idOf(n, "id") != id {
				out = append(out, n)
			}
		}
		return out
	}

	for _, raw := range ops {
		op, _ := raw.(map[string]any)
		switch name, _ := op["op"].(string); name {
		case "add_node":
			if n, ok := op["node"].(map[string]any); ok {
				nodes = append(dropNode(nodes, idOf(n, "id")), n)
			}
		case "remove_node":
			id, _ := op["node_id"].(string)
			nodes = dropNode(nodes, id)
			kept := make([]any, 0, len(edges))
			for _, e := range edges {
				if idOf(e, "source") != id && idOf(e, "target") != id {
					kept = append(kept, e)
				}
			}
			edges = kept
		case "add_edge":
			if e, ok := op["edge"].(map[string]any); ok {
				edges = append(edges, e)
			}
		case "remove_edge":
			id, _ := op["edge_id"].(string)
			kept := make([]any, 0, len(edges))
			for _, e := range edges {
				if idOf(e, "id") != id {
					kept = append(kept, e)
				}
			}
			edges = kept
		case "update_node":
			id, _ := op["node_id"].(string)
			patch, _ := op["data"].(map[string]any)
			for _, n := range nodes {
				nm, _ := n.(map[string]any)
				if idOf(n, "id") != id || nm == nil {
					continue
				}
				data, _ := nm["data"].(map[string]any)
				if data == nil {
					data = map[string]any{}
				}
				for k, v := range patch {
					data[k] = v
				}
				nm["data"] = data
			}
		}
	}
	return nodes, edges
}

// ── Chat models ─────────────────────────────────────────────────

// chatProviderSpec describes how to reach one model provider. Anthropic uses
// its native Messages API; the rest are driven through their OpenAI-compatible
// chat-completions endpoints so one code path covers all of them.
type chatProviderSpec struct {
	Label  string
	KeyEnv string
	URL    string // OpenAI-compatible chat completions URL; empty for anthropic
}

var chatProviders = map[string]chatProviderSpec{
	"anthropic": {Label: "Anthropic", KeyEnv: "ANTHROPIC_API_KEY"},
	"openai":    {Label: "OpenAI", KeyEnv: "OPENAI_API_KEY", URL: "https://api.openai.com/v1/chat/completions"},
	"google":    {Label: "Google", KeyEnv: "GEMINI_API_KEY", URL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
	"xai":       {Label: "xAI", KeyEnv: "XAI_API_KEY", URL: "https://api.x.ai/v1/chat/completions"},
}

// chatModelSpec describes a model the builder chat can use. Thinking config
// applies to Anthropic models only: Fable 5 / Opus 4.8 / Sonnet 4.6 take
// adaptive thinking, while Haiku 4.5 only supports manual budgets (adaptive
// returns a 400).
type chatModelSpec struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Provider      string         `json:"provider"`
	ProviderLabel string         `json:"providerLabel"`
	KeyEnv        string         `json:"keyEnv"`
	Available     bool           `json:"available"`
	Thinking      map[string]any `json:"-"`
}

var chatModels = []chatModelSpec{
	{
		ID: "claude-fable-5", Name: "Fable 5", Provider: "anthropic",
		Description: "Most capable — complex multi-step workflows",
		Thinking:    map[string]any{"type": "adaptive", "display": "summarized"},
	},
	{
		ID: "claude-opus-4-8", Name: "Opus 4.8", Provider: "anthropic",
		Description: "Deep reasoning for demanding builds",
		Thinking:    map[string]any{"type": "adaptive", "display": "summarized"},
	},
	{
		ID: "claude-sonnet-4-6", Name: "Sonnet 4.6", Provider: "anthropic",
		Description: "Balanced speed and intelligence",
		Thinking:    map[string]any{"type": "adaptive", "display": "summarized"},
	},
	{
		ID: "claude-haiku-4-5-20251001", Name: "Haiku 4.5", Provider: "anthropic",
		Description: "Fastest — quick edits and simple flows",
		Thinking:    map[string]any{"type": "enabled", "budget_tokens": 8000},
	},
	{
		ID: "gpt-5.5", Name: "GPT-5.5", Provider: "openai",
		Description: "OpenAI's flagship — strong all-round reasoning",
	},
	{
		ID: "gpt-5.4-mini", Name: "GPT-5.4 Mini", Provider: "openai",
		Description: "Fast and affordable OpenAI model",
	},
	{
		ID: "gemini-3.1-pro-preview", Name: "Gemini 3.1 Pro", Provider: "google",
		Description: "Google's most capable model",
	},
	{
		ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash", Provider: "google",
		Description: "Fast Google model built for agentic tasks",
	},
	{
		ID: "gemini-3-flash-preview", Name: "Gemini 3 Flash", Provider: "google",
		Description: "Quick and capable — works on the free tier",
	},
	{
		ID: "grok-4.5", Name: "Grok 4.5", Provider: "xai",
		Description: "xAI's most intelligent model",
	},
	{
		ID: "grok-4.3", Name: "Grok 4.3", Provider: "xai",
		Description: "Faster, lower-cost Grok",
	},
}

const defaultChatModel = "gpt-5.5"

func resolveChatModel(id string) chatModelSpec {
	var fallback chatModelSpec
	for _, m := range chatModels {
		if m.ID == id {
			return m
		}
		if m.ID == defaultChatModel {
			fallback = m
		}
	}
	return fallback
}

// AIModels returns the models the builder chat may use, so the frontend
// picker stays in sync with what the server accepts. Models whose provider
// key is missing from the environment are flagged unavailable.
func (h *WorkflowHandler) AIModels(c *gin.Context) {
	out := make([]chatModelSpec, len(chatModels))
	for i, m := range chatModels {
		prov := chatProviders[m.Provider]
		m.ProviderLabel = prov.Label
		m.KeyEnv = prov.KeyEnv
		m.Available = os.Getenv(prov.KeyEnv) != ""
		out[i] = m
	}
	c.JSON(http.StatusOK, gin.H{"models": out, "default": defaultChatModel})
}

// ── HTTP client for Anthropic ───────────────────────────────────

var anthropicClient = &http.Client{
	Timeout: 180 * time.Second,
	// Custom transport bypasses the globally-instrumented default one, so it
	// gets wrapped explicitly for otel client spans + outbound request logs.
	Transport: telemetry.WrapTransport(&http.Transport{
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		DisableCompression:    true,
	}),
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

var toolGetCurrentWorkflow = map[string]any{
	"name":        "get_current_workflow",
	"description": "Returns the current workflow on the canvas — all nodes with their full configuration and all edges. Call this before making any edits to an existing workflow.",
	"input_schema": map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	},
}

var toolUpdateWorkflow = map[string]any{
	"name":        "update_workflow",
	"description": "Makes surgical edits to the existing workflow without replacing it. Use this to add or remove individual nodes or edges, or to update configuration fields inside an existing node. Prefer this over create_workflow when the user asks to change, add, or remove something specific.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"operations": map[string]any{
				"type":        "array",
				"description": "Ordered list of operations to apply",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"op": map[string]any{
							"type": "string",
							"enum": []string{"add_node", "remove_node", "add_edge", "remove_edge", "update_node"},
						},
						"node":    map[string]any{"type": "object", "description": "add_node: full node with id, type, position {x,y}, data"},
						"node_id": map[string]any{"type": "string", "description": "remove_node / update_node: target node id"},
						"edge":    map[string]any{"type": "object", "description": "add_edge: object with id, source, target, optional sourceHandle"},
						"edge_id": map[string]any{"type": "string", "description": "remove_edge: target edge id"},
						"data":    map[string]any{"type": "object", "description": "update_node: partial data fields to merge into the node"},
					},
					"required": []string{"op"},
				},
			},
		},
		"required": []string{"operations"},
	},
}

var toolListIntegrationResources = map[string]any{
	"name":        "list_integration_resources",
	"description": "Lists the user's connected integrations and the concrete resources each exposes — including Notion databases/pages, Linear teams/projects, GitHub repos, GitLab projects, monday.com boards, Asana projects, Gmail labels, Stripe prices and Shopify products — with their IDs and names. ALWAYS call this before configuring an integration action or App Trigger so you can set real IDs instead of placeholders. If a provider is not connected, leave the ID empty and tell the user to hit Connect in the node settings. Never ask the user to paste API tokens — OAuth connections are used automatically.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"enum":        []string{"gmail", "googlecalendar", "googledrive", "googledocs", "googlesheets", "googleslides", "googleforms", "googlemeet", "googlechat", "googletasks", "googlekeep", "outlook", "slack", "notion", "linear", "github", "gitlab", "monday", "asana", "jira", "confluence", "bitbucket", "stripe", "shopify", "granola", "resend", "sendgrid", "kit", "airtable", "clickup", "typeform", "calendly", "dropbox", "netlify", "vercel", "supabase", "gumroad", "googlesearchconsole", "googlecontacts", "hubspot", "front"},
				"description": "Which provider to inspect. Omit to list all.",
			},
		},
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

var toolListDataStores = map[string]any{
	"name":        "list_data_stores",
	"description": "Lists the user's persistent Data stores — id, name, kind (kv | collection | text), scope (run | workflow | account), and schema. Data nodes reference a store via dataStoreId, so ALWAYS call this before configuring a data node and use a REAL id from the response. If no suitable store exists, call create_data_store to propose one.",
	"input_schema": map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	},
}

// proposalWait is how long the builder stays paused on a data-store approval
// card before giving up and finishing without the store.
const proposalWait = 10 * time.Minute

var toolCreateDataStore = map[string]any{
	"name":        "create_data_store",
	"description": "Asks the user to approve a new persistent Data store. This call BLOCKS until they answer: the result tells you whether it was approved (with the new store's real store_id, ready to use as dataStoreId) or rejected. Call it BEFORE building nodes that depend on the store, then continue once you have the answer. Never propose the same store twice.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "Short human name, e.g. 'email counter' or 'orders seen'"},
			"kind": map[string]any{
				"type": "string", "enum": []string{"kv", "collection", "text"},
				"description": "kv: keyed values/counters · collection: a table of records · text: one running text blob",
			},
			"scope": map[string]any{
				"type": "string", "enum": []string{"run", "workflow", "account"},
				"description": "run: scratch within one run · workflow: persists across this workflow's runs (default choice) · account: shared across all the user's workflows",
			},
			"schema": map[string]any{
				"type":        "array",
				"description": "Optional typed columns for a collection: [{name, type}] with type one of text|number|boolean|datetime|json. Omit for schemaless.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
						"type": map[string]any{"type": "string", "enum": []string{"text", "number", "boolean", "datetime", "json"}},
					},
					"required": []string{"name", "type"},
				},
			},
			"reason": map[string]any{"type": "string", "description": "One sentence shown on the approval card: what the workflow uses this store for"},
		},
		"required": []string{"name", "kind", "scope", "reason"},
	},
}

var toolSetSchedule = map[string]any{
	"name":        "set_schedule",
	"description": "Sets when a workflow with a scheduledTrigger node actually runs. The cadence lives outside the node's data, so placing a scheduledTrigger is NOT enough — call this whenever the user states or implies a cadence ('every 2 minutes', 'daily at 9am', 'every Monday'), and never tell them to set it by hand. Also remind them the workflow must be Published for schedules to fire.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"frequency": map[string]any{
				"type": "string", "enum": []string{"interval", "hourly", "daily", "weekly", "monthly"},
				"description": "interval = every N minutes/hours (use interval_minutes); hourly = on the hour; daily/weekly/monthly use run_time",
			},
			"interval_minutes": map[string]any{
				"type":        "number",
				"description": "For frequency=interval: how many minutes between runs. Minimum 1.",
			},
			"run_time": map[string]any{
				"type":        "string",
				"description": "For daily/weekly/monthly: 'HH:MM' 24h in UTC. Convert if the user names a timezone, and say which time you set.",
			},
			"day_of_week":  map[string]any{"type": "number", "description": "weekly only: 0=Sunday … 6=Saturday"},
			"day_of_month": map[string]any{"type": "number", "description": "monthly only: 1–28"},
			"repeat":       map[string]any{"type": "boolean", "description": "false = run once then disable itself (default true)"},
		},
		"required": []string{"frequency"},
	},
}

var toolListRuns = map[string]any{
	"name":        "list_runs",
	"description": "Lists this workflow's recent executions, newest first — status, when it started, the error message, and which node failed. Call this FIRST whenever the user says something didn't work, ran wrong, or stopped working, then open the relevant run with get_run_logs. Includes runs started by the schedule, so it shows failures the user may not have watched happen.",
	"input_schema": map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	},
}

var toolGetRunLogs = map[string]any{
	"name":        "get_run_logs",
	"description": "Returns the logs of one run: every node it touched, in order, with the output that node produced or the error that stopped it. Use the exact error to fix the offending node instead of guessing. Get run ids from list_runs.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"run_id": map[string]any{"type": "string", "description": "The run to inspect, from list_runs"},
		},
		"required": []string{"run_id"},
	},
}

var toolGetCurrentTime = map[string]any{
	"name":         executor.ClockToolName,
	"description":  executor.ClockToolDesc + " The system prompt already states the current UTC time; call this to convert into the user's timezone before setting a schedule, or after a long conversation.",
	"input_schema": executor.ClockToolSchema(),
}

// builderTools is the single source of truth for the AI builder's tool set
// (Anthropic uses it directly; openAIToolDefs converts it).
func builderTools() []map[string]any {
	return []map[string]any{toolGetNodes, toolGetCurrentWorkflow, toolCreateWorkflow, toolUpdateWorkflow, toolListIntegrationResources, toolListDataStores, toolCreateDataStore, toolSetSchedule, toolListRuns, toolGetRunLogs, toolGetCurrentTime}
}

// integrationTriggerCatalogEntry is built from the trigger adapters rather
// than a second handwritten list. That keeps the AI builder on the same event
// ids, filters and sample payloads as the sidebar and webhook dispatcher.
// Adding an event to an adapter therefore teaches the builder about it in the
// same deploy instead of leaving it to invent an unsupported event name.
func integrationTriggerCatalogEntry() map[string]any {
	return map[string]any{
		"type": "integrationTrigger", "label": "App Trigger", "category": "Triggers",
		"description": "Starts the published workflow when an event arrives from a connected app. The provider registry defines the supported events and webhook registration. The normalized event is this node's output: provider, event, resource, occurred_at and data.",
		"dataFields": map[string]any{
			"triggerProvider":      "string – required; use an exact provider key from eventCatalog",
			"triggerEvent":         "string – required; use an exact event id from eventCatalog[triggerProvider]",
			"triggerResourceId":    "string – required when the event declares resource_kind; use a REAL resource id from list_integration_resources",
			"triggerResourceLabel": "string – the selected resource's human-readable name from list_integration_resources",
			"triggerFilters":       "object – optional event filters using only keys declared by that event in eventCatalog (for example branch, base, author or label)",
		},
		"eventCatalog": triggers.Catalog(),
		"auth":         "OAuth connection used automatically — never set integrationToken. GitHub requires the Fernary GitHub App on the exact repository; GitLab needs Maintainer/Owner project access; monday.com needs its Signing Secret configured; Asana performs a synchronous webhook handshake.",
		"handles":      map[string]any{"inputs": []string{}, "outputs": []string{"source (right)"}},
		"notes":        "Call list_integration_resources for the chosen provider before placing this node. Configure the canvas fields, then tell the user to open App Trigger and click Start listening; the workflow must be saved before registration and Published before events start runs. Downstream templates read event fields through {{nodeId.output.data.field}}.",
	}
}

// nodeCatalog documents every node type (fields, semantics) — shared by the
// builder's get_available_nodes tool and agent-chat tool-schema generation.
func nodeCatalog() []map[string]any {
	return []map[string]any{
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
				"model":        "string – 'gpt-4o', 'gpt-4o-mini', 'claude-sonnet-4-6', 'claude-haiku-4-5-20251001'",
				"systemPrompt": "string – system instructions for the model",
				"userPrompt":   "string – user message. Use {{nodeId.output}} to inject upstream data",
				"temperature":  "number – 0 to 1, controls randomness (default 0.7)",
				"maxTokens":    "number – max response length (default 1024)",
			},
			"handles": map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
			"notes":   "When an imageInput node's output is referenced in userPrompt via {{nodeId.output}}, the image is automatically sent as a vision block.",
		},
		{
			"type": "codingAgent", "label": "Coding Agent", "category": "AI",
			"description": "Runs a bounded Codex task in an isolated Daytona workspace against a connected GitHub or GitLab repository. The user must connect Codex and the selected repository provider before running it.",
			"dataFields": map[string]any{
				"codingAgentRuntime":            "string – currently 'codex'",
				"codingAgentTask":               "string – concrete coding task; supports {{nodeId.output}} templates",
				"codingAgentRepositoryProvider": "string – 'github' or 'gitlab'; repository access comes from the user's connected integration",
				"codingAgentRepositoryId":       "string – selected resource ID; GitHub owner/repository or GitLab numeric project ID",
				"codingAgentRepository":         "string – GitHub owner/repository or GitLab namespace/project path, without a URL",
				"codingAgentBranch":             "string – optional branch to clone",
				"codingAgentModel":              "string – optional Codex model override; leave empty for the account default",
				"codingAgentWorkspaceMode":      "string – 'persistent' reuses this node's workspace; 'ephemeral' deletes it after the run",
				"codingAgentConversationKey":    "string – optional stable key for Codex thread continuity; supports templates",
				"codingAgentMaxDuration":        "number – seconds, 30 to 7200 (default 1800)",
				"codingAgentAutoStopMinutes":    "number – idle minutes before provider stop (default 15)",
				"codingAgentAutoDeleteMinutes":  "number – retention minutes, at least auto-stop and at most 43200 (default 10080)",
				"codingAgentAllowedDomains":     "string[] – extra domain names the task may reach; runtime, npm, and the selected repository provider are always included",
				"codingAgentAllowWrite":         "boolean – allow repository file changes; false makes the agent read-only",
			},
			"handles": map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
			"notes":   "Never put tokens in node data or prompts. The node cannot push, deploy, open pull requests, or contact people. It returns a durable job result plus retained Git status/patch artifacts.",
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
			"dataFields":  map[string]any{"approvalMessage": "string – message shown to reviewer", "approvalTimeout": "number – seconds to wait before giving up; REQUIRED and always bounded (default 86400 = 24h, ceiling 259200 = 3 days). Never 0 — an unbounded gate strands the run, and on a schedule strands one every cycle.", "approvalEmail": "string – optional email to notify"},
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
			"dataFields":  map[string]any{"emailTo": "string – recipient(s), comma-separated; multiple recipients each get a private copy (broadcast)", "emailSubject": "string – subject", "emailBody": "string – body text"},
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
			"description": "Starts the workflow on a recurring schedule. The cadence is NOT node data — place this node and then call set_schedule to choose it (every N minutes, hourly, daily/weekly/monthly at a time). Schedules only fire once the workflow is Published.",
			"dataFields":  map[string]any{"label": "string – display name"},
			"handles":     map[string]any{"inputs": []string{}, "outputs": []string{"source (right)"}},
		},
		integrationTriggerCatalogEntry(),
		{
			"type": "data", "label": "Data Store", "category": "Data",
			"description": "Reads/writes persistent memory (a Data store). This is how workflows remember things across runs: counters ('email #N'), dedup lists ('orders already handled'), cursors, accumulating digests. Ops by store kind — kv: get/set/increment/delete · collection: append/query/update/delete/count/clear · text: get/set/append. Output is the op result as JSON (increment returns the new number). A kv/text get on something never written yields an EMPTY string (not 'null'), so first-run branches should test for empty; prefer increment over get+set for counters since it is atomic and starts at 0.",
			"dataFields": map[string]any{
				"dataStoreId":  "string – REAL store id from list_data_stores (leave empty only when a create_data_store proposal is pending approval)",
				"dataOp":       "kv: 'get'|'set'|'increment'|'delete' · collection: 'append'|'query'|'update'|'delete'|'count'|'clear' · text: 'get'|'set'|'append'",
				"dataKey":      "string – kv key (templates ok)",
				"dataValue":    "string – kv value / text content (templates ok; JSON kept as JSON, other text stored as a string)",
				"dataAmount":   "string – increment amount (default 1)",
				"dataRecord":   "string – JSON object record for append/update (templates ok)",
				"dataFilter":   "string – JSON equality filter for query, e.g. {\"status\":\"open\"}",
				"dataRecordId": "string – record id for update/delete (records carry _id in query results)",
				"dataLimit":    "string – query row limit (default 100)",
			},
			"handles": map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "gmail", "label": "Gmail", "category": "Integrations",
			"description": "Gmail API: send/reply from the user's own address, search/list/read messages and threads, manage labels and read state, archive/trash, and work with drafts.",
			"dataFields":  map[string]any{"integrationOp": "'send_email'|'reply_to_message'|'list_messages'|'get_message'|'get_thread'|'create_draft'|'list_drafts'|'send_draft'|'list_labels'|'create_label'|'add_label'|'remove_label'|'mark_read'|'mark_unread'|'archive_message'|'trash_message'", "gmailTo": "string (templates ok; reply_to_message defaults to the original sender)", "gmailCc": "string", "gmailSubject": "string (templates ok)", "gmailBody": "string (templates ok)", "gmailQuery": "string – Gmail search syntax e.g. 'is:unread newer_than:1d'", "gmailMessageId": "string – target message (get/reply/label/read-state/archive/trash)", "gmailThreadId": "string – for get_thread", "gmailLabelId": "string – label id from list_labels (add_label/remove_label)", "gmailLabelName": "string – for create_label", "gmailDraftId": "string – for send_draft", "gmailLimit": "number (default 10)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. Prefer gmail over the generic emailSend node when the user wants mail sent from their own Gmail address or wants to read their inbox.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googlecalendar", "label": "Google Calendar", "category": "Integrations",
			"description": "Google Calendar API: list/get/create/update/delete events, natural-language quick add, list calendars, find free/busy windows, respond to invitations.",
			"dataFields":  map[string]any{"integrationOp": "'list_events'|'get_event'|'create_event'|'update_event'|'delete_event'|'quick_add'|'list_calendars'|'find_free_time'|'respond_to_event'", "gcalCalendarId": "string – calendar id from list_integration_resources (default 'primary')", "gcalEventId": "string – target event (get/update/delete/respond)", "gcalSummary": "string – event title (templates ok)", "gcalDescription": "string (templates ok)", "gcalStart": "string – RFC3339 e.g. 2026-07-20T15:00:00Z (also find_free_time window start)", "gcalEnd": "string – RFC3339 (also find_free_time window end)", "gcalAttendees": "string – comma-separated emails", "gcalText": "string – natural language for quick_add e.g. 'Lunch with Sam Friday 1pm'", "gcalResponse": "'accepted'|'declined'|'tentative' for respond_to_event", "gcalLimit": "number (default 10)"},
			"auth":        "OAuth connection used automatically — never set integrationToken.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googledrive", "label": "Google Drive", "category": "Integrations",
			"description": "Google Drive API: list/search files, read file content (Docs exported as text), upload text files, copy/move/rename, share (email or anyone-with-link), list permissions, trash or permanently delete.",
			"dataFields":  map[string]any{"integrationOp": "'list_files'|'search'|'get_file'|'read_file'|'upload_file'|'create_folder'|'copy_file'|'move_file'|'rename_file'|'share_file'|'list_permissions'|'trash_file'|'delete_file'", "gdriveFileId": "string – target file", "gdriveName": "string – name for create_folder/upload_file/copy_file/rename_file", "gdriveContent": "string – text body for upload_file (templates ok)", "gdriveMimeType": "'text/plain'|'text/markdown'|'text/csv'|'application/json' for upload_file", "gdriveQuery": "string – Drive query, e.g. \"name contains 'report'\"", "gdriveParentId": "string – parent/destination folder id", "gdriveEmail": "string – share_file recipient (empty → anyone with link)", "gdriveRole": "'reader'|'commenter'|'writer' for share_file", "gdriveLimit": "number (default 20)"},
			"auth":        "OAuth connection used automatically — never set integrationToken.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googledocs", "label": "Google Docs", "category": "Integrations",
			"description": "Google Docs API: create a document, read text, append or prepend text, find/replace across the doc, and create a document from a template with placeholder replacements.",
			"dataFields":  map[string]any{"integrationOp": "'create_document'|'get_document'|'append_text'|'insert_text_at_start'|'replace_text'|'create_from_template'", "gdocsDocumentId": "string – target document", "gdocsTitle": "string – title (create_document / create_from_template copy name)", "gdocsText": "string – text to insert (templates ok)", "gdocsFindText": "string – replace_text needle", "gdocsReplaceText": "string – replace_text replacement (templates ok)", "gdocsTemplateId": "string – source doc id for create_from_template", "gdocsReplacements": "string – JSON object of find→replace pairs e.g. {\"{{name}}\":\"Jane\"}"},
			"auth":        "OAuth connection used automatically — never set integrationToken.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googlesheets", "label": "Google Sheets", "category": "Integrations",
			"description": "Google Sheets API: read/update/clear ranges, append one or many rows, manage sheet tabs (list/add/delete), find/replace across all sheets, delete row ranges, create spreadsheets.",
			"dataFields":  map[string]any{"integrationOp": "'read_range'|'append_row'|'append_rows'|'update_range'|'clear_range'|'list_sheets'|'add_sheet'|'delete_sheet'|'delete_rows'|'find_replace'|'create_spreadsheet'", "gsheetsSpreadsheetId": "string – target spreadsheet", "gsheetsRange": "string – A1 notation e.g. Sheet1!A1:C10", "gsheetsValues": "string – comma-separated cells for one row (templates ok)", "gsheetsRows": "string – JSON array of arrays for append_rows e.g. [[\"a\",\"b\"],[\"c\",\"d\"]]", "gsheetsSheetTitle": "string – tab name (add/delete_sheet, delete_rows)", "gsheetsFind": "string – find_replace needle", "gsheetsReplace": "string – find_replace replacement", "gsheetsStartRow": "number – delete_rows first row (1-based)", "gsheetsEndRow": "number – delete_rows last row (inclusive)", "gsheetsTitle": "string – title for create_spreadsheet"},
			"auth":        "OAuth connection used automatically — never set integrationToken.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googleslides", "label": "Google Slides", "category": "Integrations",
			"description": "Google Slides API: create presentations, add/duplicate/delete slides, fill title and body placeholders, add text boxes and images, speaker notes, find-and-replace across a deck, slide thumbnails, and build a deck from a template.",
			"dataFields":  map[string]any{"integrationOp": "'create_presentation'|'get_presentation'|'list_slides'|'add_slide'|'duplicate_slide'|'delete_slide'|'delete_object'|'replace_all_text'|'add_text_box'|'add_image'|'update_speaker_notes'|'get_thumbnail'|'create_from_template'", "slidesPresentationId": "string", "slidesTitle": "string – deck title (templates ok)", "slidesSlideId": "string – page objectId from list_slides", "slidesLayout": "'TITLE_AND_BODY'|'TITLE_ONLY'|'SECTION_HEADER'|'BLANK' (default TITLE_AND_BODY)", "slidesHeading": "string – title placeholder text (templates ok)", "slidesBody": "string – body placeholder or text-box text (templates ok)", "slidesFind": "string – replace_all_text", "slidesReplace": "string – replace_all_text", "slidesImageUrl": "string – must be publicly reachable; Google fetches it", "slidesObjectId": "string – delete_object target", "slidesNotes": "string – speaker notes", "slidesTemplateId": "string – source deck for create_from_template", "slidesReplacements": "string – JSON map {\"{{name}}\":\"Jane\"}", "slidesIndex": "number – insertion position"},
			"auth":        "OAuth connection used automatically — never set integrationToken.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googleforms", "label": "Google Forms", "category": "Integrations",
			"description": "Google Forms API: create forms, add questions of every answer type, edit form info and settings, turn quiz mode on, open or close responses, and read submitted responses.",
			"dataFields":  map[string]any{"integrationOp": "'create_form'|'get_form'|'add_question'|'update_form_info'|'set_quiz_mode'|'delete_item'|'list_responses'|'get_response'|'set_publish_settings'", "formsFormId": "string", "formsTitle": "string (templates ok)", "formsDescription": "string – form description, or a question's helper text", "formsQuestion": "string – the question text (templates ok)", "formsQuestionType": "'TEXT'|'PARAGRAPH'|'RADIO'|'CHECKBOX'|'DROPDOWN'|'SCALE'|'DATE'|'TIME'", "formsOptions": "string – comma-separated choices; for SCALE the low,high bounds e.g. 1,5", "formsRequired": "'true'|'false'", "formsItemId": "string", "formsResponseId": "string", "formsIndex": "number – item position (0-based)", "formsIsQuiz": "'true'|'false'", "formsAccepting": "'true'|'false' – whether the form takes new responses", "formsLimit": "number (default 25)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. forms.create only accepts a title, so questions are added with add_question afterwards.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googlemeet", "label": "Google Meet", "category": "Integrations",
			"description": "Google Meet API: create and configure meeting spaces (returns the join URL), read conference records, participants, recordings and transcripts — including the full transcript text for summarising a meeting.",
			"dataFields":  map[string]any{"integrationOp": "'create_space'|'get_space'|'update_space'|'end_active_conference'|'list_conference_records'|'get_conference_record'|'list_participants'|'list_recordings'|'list_transcripts'|'get_transcript_text'|'list_transcript_entries'", "meetSpace": "string – spaces/{id} or a meeting code", "meetAccessType": "'OPEN'|'TRUSTED'|'RESTRICTED'", "meetModeration": "'ON'|'OFF'", "meetConferenceRecord": "string – conferenceRecords/{id} from list_conference_records", "meetTranscript": "string – transcript name from list_transcripts", "meetFilter": "string – list filter, e.g. space.name=\"spaces/abc\"", "meetLimit": "number (default 25)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. The meetings.space.created scope only reaches spaces this connection created, so create_space first; a space made by hand in the Meet UI is not visible.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googlechat", "label": "Google Chat", "category": "Integrations",
			"description": "Google Chat API: list/create/update/delete spaces, set a space up with members in one call, find a direct message, send/update/delete messages, reply in a thread, react, list messages, and manage membership.",
			"dataFields":  map[string]any{"integrationOp": "'list_spaces'|'get_space'|'create_space'|'setup_space'|'update_space'|'delete_space'|'find_direct_message'|'send_message'|'reply_in_thread'|'get_message'|'update_message'|'delete_message'|'list_messages'|'add_reaction'|'list_members'|'add_member'|'remove_member'", "chatSpace": "string – spaces/{id} from list_integration_resources", "chatMessageId": "string – spaces/{s}/messages/{m}", "chatText": "string – message text (templates ok)", "chatThread": "string – thread name, or any key to group replies", "chatDisplayName": "string – space name", "chatSpaceType": "'SPACE'|'GROUP_CHAT' (default SPACE)", "chatMemberEmail": "string – user email; comma-separated for setup_space", "chatMembership": "string – spaces/{s}/members/{m} from list_members", "chatEmoji": "string – a literal emoji, not a :shortcode:", "chatFilter": "string – list filter", "chatLimit": "number (default 25)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. Google Chat needs a Workspace account (not personal @gmail.com) and a configured Chat app in the Cloud project; user credentials only reach spaces the user is a member of.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googletasks", "label": "Google Tasks", "category": "Integrations",
			"description": "Google Tasks API: task lists CRUD, tasks CRUD, complete a task, reorder or reparent tasks, move a task between lists, and clear completed tasks.",
			"dataFields":  map[string]any{"integrationOp": "'list_task_lists'|'get_task_list'|'create_task_list'|'update_task_list'|'delete_task_list'|'list_tasks'|'get_task'|'create_task'|'update_task'|'complete_task'|'delete_task'|'move_task'|'clear_completed'", "tasksListId": "string – list id from list_task_lists; omit to use the primary list", "tasksTaskId": "string", "tasksTitle": "string (templates ok)", "tasksNotes": "string (templates ok)", "tasksDue": "string – RFC3339; Tasks keeps only the date", "tasksStatus": "'needsAction'|'completed'", "tasksParent": "string – parent task id, to make a subtask", "tasksPrevious": "string – sibling to position after", "tasksShowCompleted": "'true'|'false' (default false)", "tasksDueMin": "string – RFC3339 filter", "tasksDueMax": "string – RFC3339 filter", "tasksDestinationList": "string – move_task target list", "tasksLimit": "number (default 25)"},
			"auth":        "OAuth connection used automatically — never set integrationToken.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googlekeep", "label": "Google Keep", "category": "Integrations",
			"description": "Google Keep API: create text or checklist notes, read and list notes, delete notes, and share or unshare them.",
			"dataFields":  map[string]any{"integrationOp": "'create_note'|'get_note'|'list_notes'|'delete_note'|'share_note'|'unshare_note'", "keepNoteName": "string – notes/{id}", "keepTitle": "string (templates ok)", "keepText": "string – body prose (templates ok)", "keepListItems": "string – one checklist item per line; use instead of keepText", "keepEmail": "string – comma-separated emails to share with", "keepFilter": "string – list filter", "keepLimit": "number (default 25)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. The Keep API is Google Workspace only (never personal @gmail.com) and a Workspace admin must enable it for the domain — warn the user before building on it.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "outlook", "label": "Outlook", "category": "Integrations",
			"description": "Microsoft Outlook (Graph): full mail (send/reply/forward/draft/move/read-state/flag/delete/folders), calendar (list/create/update/delete/respond to events), and contacts (list/create).",
			"dataFields":  map[string]any{"integrationOp": "'send_email'|'reply_to_message'|'forward_message'|'create_draft'|'list_messages'|'get_message'|'move_message'|'mark_read'|'flag_message'|'delete_message'|'list_folders'|'create_event'|'list_events'|'update_event'|'delete_event'|'respond_to_event'|'list_contacts'|'create_contact'", "outlookTo": "string (templates ok; also forward recipients)", "outlookCc": "string", "outlookSubject": "string (templates ok)", "outlookBody": "string – HTML (templates ok)", "outlookComment": "string – reply/forward/respond comment", "outlookQuery": "string – search text (list_messages/list_contacts)", "outlookMessageId": "string – target message", "outlookFolderId": "string – move_message destination (from list_folders or the folder resource)", "outlookEventId": "string – target event (update/delete/respond)", "outlookResponse": "'accept'|'decline'|'tentativelyAccept'", "outlookContactName": "string", "outlookContactEmail": "string", "outlookLimit": "number (default 10)", "outlookStart": "string – RFC3339 (create/update event; with outlookEnd filters list_events window)", "outlookEnd": "string – RFC3339"},
			"auth":        "OAuth connection used automatically — never set integrationToken.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "slack", "label": "Slack", "category": "Integrations",
			"description": "Slack: post/reply/update/delete/schedule messages (bot or user identity), DMs (always as the user), reactions and pins, channel management (create/archive/join/invite/topic), text file uploads, user lookup, and workspace message search (user identity).",
			"dataFields":  map[string]any{"integrationOp": "'send_message'|'send_dm'|'reply_in_thread'|'update_message'|'delete_message'|'schedule_message'|'add_reaction'|'pin_message'|'create_channel'|'archive_channel'|'join_channel'|'invite_to_channel'|'set_channel_topic'|'upload_file'|'list_channels'|'get_channel_history'|'list_users'|'get_user_by_email'|'search_messages'", "slackChannel": "string – channel id (e.g. C0123) from list_integration_resources", "slackText": "string – message text / search query for search_messages (templates ok)", "slackSendAs": "'bot' (default) | 'user' – identity for send_message/reply_in_thread", "slackBotName": "string – optional display-name override for bot sends", "slackUserId": "string – DM recipient or invite targets (comma-sep user ids)", "slackThreadTs": "string – parent message ts for reply_in_thread", "slackMessageTs": "string – target message ts (update/delete/add_reaction/pin)", "slackEmoji": "string – reaction name without colons e.g. 'tada'", "slackChannelName": "string – for create_channel", "slackPrivate": "'true'|'false' – create_channel visibility", "slackTopic": "string – for set_channel_topic", "slackFileName": "string – upload_file name", "slackFileContent": "string – upload_file text content (templates ok)", "slackEmail": "string – for get_user_by_email", "slackPostAt": "string – RFC3339 time for schedule_message", "slackLimit": "number (default 100/20)"},
			"auth":        "OAuth connection used automatically — never set integrationToken.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "notion", "label": "Notion", "category": "Integrations",
			"description": "Notion API: create/update/archive pages, subpages, databases (create/get schema/query), read page content, search, comments (add/list), list workspace users.",
			"dataFields":  map[string]any{"integrationOp": "'create_page'|'create_subpage'|'query_database'|'append_blocks'|'update_page'|'archive_page'|'get_page_content'|'search'|'add_comment'|'list_comments'|'create_database'|'get_database'|'list_users'", "notionDatabaseId": "string – REAL id from list_integration_resources", "notionPageId": "string – REAL id from list_integration_resources", "notionParentPageId": "string – parent page for create_database/create_subpage", "notionSchema": "string – JSON property definitions for create_database (a Name title property is added automatically)", "notionTitle": "string (templates ok)", "notionContent": "string (templates ok)", "notionFilter": "string – JSON filter", "notionQuery": "string – search text", "notionProperties": "string – JSON object of page properties for update_page"},
			"auth":        "OAuth connection used automatically — never set integrationToken; call list_integration_resources for real database/page IDs",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "linear", "label": "Linear", "category": "Integrations",
			"description": "Linear API: create/update/get/archive issues, search, comments (create/list), labels (list/add), workflow states, teams, users, cycles, create projects.",
			"dataFields":  map[string]any{"integrationOp": "'create_issue'|'get_issues'|'create_comment'|'list_comments'|'update_issue'|'archive_issue'|'search_issues'|'list_projects'|'create_project'|'get_issue'|'list_teams'|'list_users'|'list_states'|'list_labels'|'add_label'|'list_cycles'", "linearTeamId": "string – REAL id from list_integration_resources", "linearIssueId": "string", "linearTitle": "string (also project name)", "linearDescription": "string", "linearPriority": "number 0-4", "linearCommentBody": "string", "linearLimit": "number", "linearStateId": "string – workflow state id for update_issue (from list_states)", "linearAssigneeId": "string", "linearLabelId": "string – label id for add_label (from list_labels)", "linearQuery": "string – search text", "linearProjectId": "string – REAL id from list_integration_resources"},
			"auth":        "OAuth connection used automatically — never set integrationToken; call list_integration_resources for real team/project IDs",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "github", "label": "GitHub", "category": "Integrations",
			"description": "GitHub API: issues (create/get/update/list/search/comment), pull requests (create/merge/list/inspect/files), codebase inspection (repository details, recursive file structure, read file), file commits, branches, commits, releases, Actions workflows (trigger/list runs), list repos. To understand an unfamiliar codebase: call get_repo_details, then list_repo_tree, then get_file for the README, manifests, configuration, and likely entrypoints. Never guess file paths before listing the tree.",
			"dataFields":  map[string]any{"integrationOp": "'create_issue'|'get_issue'|'update_issue'|'list_issues'|'search_issues'|'create_comment'|'create_pull_request'|'merge_pull_request'|'list_pull_requests'|'get_pull_request'|'list_pr_files'|'list_commits'|'list_branches'|'get_repo_details'|'list_repo_tree'|'get_file'|'create_or_update_file'|'list_releases'|'create_release'|'trigger_workflow'|'list_workflow_runs'|'list_repos'", "githubRepo": "string – 'owner/name', REAL value from list_integration_resources", "githubTitle": "string (templates ok; also PR/release title)", "githubBody": "string (templates ok; also trigger_workflow JSON inputs)", "githubIssueNumber": "string", "githubPrNumber": "string", "githubLabels": "string – comma-separated", "githubState": "'open'|'closed'|'all'", "githubBranch": "string – PR head / commit branch", "githubBase": "string – PR base (default main)", "githubMergeMethod": "'merge'|'squash'|'rebase'", "githubPath": "string – file path; for list_repo_tree, an optional directory prefix used to narrow results", "githubContent": "string – file content (templates ok)", "githubCommitMessage": "string", "githubRef": "string – branch/tag/sha for reads, repository trees, and workflow dispatch; list_repo_tree defaults to the repository's default branch", "githubTag": "string – create_release tag", "githubWorkflowId": "string – workflow file name e.g. deploy.yml", "githubQuery": "string – search_issues query (GitHub search syntax)", "githubSince": "string – ISO 8601 time filter: list_commits since / list_issues updated after / list_workflow_runs created from", "githubUntil": "string – ISO 8601 time filter: list_commits until / list_workflow_runs created to", "githubLimit": "number (default 10)", "githubTreeLimit": "number – list_repo_tree result cap (default 1000, maximum 5000)"},
			"auth":        "OAuth connection used automatically — never set integrationToken; call list_integration_resources for real repo names",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "gitlab", "label": "GitLab", "category": "Integrations",
			"description": "GitLab API: issues (create/get/update/list/comment), merge requests (create/merge/list/inspect), branches, commits, pipelines (list/trigger), repository files (read/commit).",
			"dataFields":  map[string]any{"integrationOp": "'create_issue'|'get_issue'|'update_issue'|'list_issues'|'create_comment'|'create_merge_request'|'merge_mr'|'list_merge_requests'|'get_merge_request'|'list_branches'|'list_commits'|'list_pipelines'|'trigger_pipeline'|'get_file'|'commit_file'", "gitlabProjectId": "string – REAL id from list_integration_resources", "gitlabTitle": "string (templates ok)", "gitlabDescription": "string (templates ok)", "gitlabIssueIid": "string", "gitlabMrIid": "string", "gitlabLabels": "string – comma-separated", "gitlabState": "'opened'|'closed'|'all'", "gitlabStateEvent": "'close'|'reopen' for update_issue", "gitlabSourceBranch": "string – MR source", "gitlabTargetBranch": "string – MR target (default main)", "gitlabRef": "string – branch for commits/pipeline/file ops (default main)", "gitlabPath": "string – file path", "gitlabContent": "string – file content (templates ok)", "gitlabCommitMessage": "string", "gitlabSince": "string – ISO 8601 time filter: commits since / issues+MRs created after / pipelines updated after", "gitlabUntil": "string – ISO 8601 time filter: commits until / issues+MRs created before / pipelines updated before", "gitlabLimit": "number (default 10)"},
			"auth":        "OAuth connection used automatically — never set integrationToken; call list_integration_resources for real project IDs",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "jira", "label": "Jira", "category": "Integrations",
			"description": "Jira Cloud: search issues with JQL, create/read/update/delete issues, transition status, assign, comment, log work, attach files, link issues, browse projects and issue types, plus Agile boards and sprints.",
			"dataFields":  map[string]any{"integrationOp": "'search_issues'|'get_issue'|'create_issue'|'update_issue'|'delete_issue'|'assign_issue'|'transition_issue'|'list_transitions'|'link_issues'|'add_comment'|'list_comments'|'add_worklog'|'list_worklogs'|'add_attachment'|'list_projects'|'get_project'|'list_issue_types'|'search_users'|'get_current_user'|'list_boards'|'list_sprints'|'get_sprint_issues'|'create_sprint'|'move_issues_to_sprint'", "jiraIssueKey": "string – e.g. ENG-1234 (comma-separated for move_issues_to_sprint)", "jiraProjectKey": "string – e.g. ENG (required for create_issue)", "jiraSummary": "string – issue title (templates ok)", "jiraDescription": "string – plain text, converted to Jira's rich format (templates ok)", "jiraIssueType": "'Task'|'Bug'|'Story'|'Epic'|'Sub-task' (default Task)", "jiraJql": "string – JQL, e.g. project = ENG AND status != Done ORDER BY created DESC", "jiraFields": "string – comma-separated fields to return from search_issues", "jiraAssignee": "string – accountId, email, or 'me'; empty on assign_issue unassigns", "jiraPriority": "'Highest'|'High'|'Medium'|'Low'|'Lowest'", "jiraLabels": "string – comma-separated; spaces become hyphens", "jiraParentKey": "string – parent issue for a sub-task or epic", "jiraDueDate": "string – YYYY-MM-DD", "jiraTransition": "string – target status name, e.g. 'In Progress' or 'Done'", "jiraComment": "string – comment text (also an optional note on transition_issue / add_worklog)", "jiraTimeSpent": "string – e.g. '3h 30m' (add_worklog)", "jiraStarted": "string – RFC3339 worklog start", "jiraLinkType": "'Blocks'|'Relates'|'Duplicate'|'Cloners'", "jiraLinkedIssue": "string – the other issue key for link_issues", "jiraQuery": "string – search_users query", "jiraBoardId": "string – from list_boards", "jiraSprintId": "string – from list_sprints, or 'backlog' to move issues out of a sprint", "jiraSprintName": "string – create_sprint", "jiraStartDate": "string – RFC3339 (create_sprint)", "jiraEndDate": "string – RFC3339 (create_sprint)", "jiraAttachName": "string – attachment file name", "jiraAttachBody": "string – attachment text content (templates ok)", "jiraLimit": "number (default 25)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. The connected Jira site is used automatically.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "confluence", "label": "Confluence", "category": "Integrations",
			"description": "Confluence Cloud: list spaces, read/create/update/delete pages, find pages by title or CQL search, child pages, blog posts, comments, labels and attachments.",
			"dataFields":  map[string]any{"integrationOp": "'list_spaces'|'get_space'|'list_pages'|'get_page'|'find_page_by_title'|'list_child_pages'|'create_page'|'update_page'|'delete_page'|'search_pages'|'list_blog_posts'|'create_blog_post'|'add_comment'|'list_comments'|'list_labels'|'add_label'|'list_attachments'|'upload_attachment'|'get_current_user'", "confluenceSpaceKey": "string – REAL space key from list_integration_resources (required to create a page or blog post)", "confluencePageId": "string – REAL target page id from list_integration_resources", "confluenceTitle": "string – page title (templates ok)", "confluenceBody": "string – page content; plain text becomes paragraphs, or pass storage-format XHTML (templates ok)", "confluenceParentId": "string – REAL parent page id from list_integration_resources, to nest a new page", "confluenceCql": "string – CQL for search_pages, e.g. text ~ \"onboarding\" AND space = ENG", "confluenceComment": "string – comment text", "confluenceLabel": "string – comma-separated labels", "confluenceStatus": "'current'|'draft' (default current)", "confluenceAttachName": "string – attachment file name", "confluenceAttachBody": "string – attachment text content", "confluenceLimit": "number (default 25)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. The connected Confluence site is used automatically.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "bitbucket", "label": "Bitbucket", "category": "Integrations",
			"description": "Bitbucket Cloud: repositories, pull requests (create/merge/decline/approve/comment/diff), branches, commits, read and commit files, the issue tracker, and Pipelines.",
			"dataFields":  map[string]any{"integrationOp": "'list_repositories'|'get_repository'|'create_repository'|'list_pull_requests'|'get_pull_request'|'create_pull_request'|'merge_pull_request'|'decline_pull_request'|'approve_pull_request'|'comment_on_pull_request'|'list_pr_comments'|'list_pr_commits'|'get_pr_diff'|'list_branches'|'create_branch'|'delete_branch'|'list_commits'|'get_commit'|'get_file'|'commit_file'|'list_issues'|'get_issue'|'create_issue'|'comment_on_issue'|'list_pipelines'|'trigger_pipeline'|'list_workspaces'|'get_current_user'", "bitbucketWorkspace": "string – workspace slug; omit to use the connected workspace", "bitbucketRepo": "string – repository slug (required for repository operations)", "bitbucketPrId": "string – pull request id", "bitbucketTitle": "string – PR or issue title (templates ok)", "bitbucketBody": "string – PR description, comment text, or issue body (templates ok)", "bitbucketSource": "string – PR source branch", "bitbucketDest": "string – PR destination branch (defaults to the repo's main branch)", "bitbucketBranch": "string – branch to create/delete, commit to, or run a pipeline on", "bitbucketRef": "string – branch, tag, or commit hash to read from (default main)", "bitbucketPath": "string – file path, e.g. docs/readme.md", "bitbucketContent": "string – file content for commit_file (templates ok)", "bitbucketMessage": "string – commit or merge message", "bitbucketMergeStrategy": "'merge_commit'|'squash'|'fast_forward'", "bitbucketState": "PR state 'OPEN'|'MERGED'|'DECLINED'|'SUPERSEDED', or issue state 'new'|'open'|'resolved'|'closed'", "bitbucketQuery": "string – repository list filter (Bitbucket query syntax)", "bitbucketPrivate": "'true'|'false' – create_repository (default true)", "bitbucketIssueId": "string – issue tracker id", "bitbucketKind": "'bug'|'enhancement'|'proposal'|'task'", "bitbucketPriority": "'trivial'|'minor'|'major'|'critical'|'blocker'", "bitbucketLimit": "number (default 25)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. Bitbucket scopes are fixed on the OAuth consumer, so a 403 means the consumer lacks that permission.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "resend", "label": "Resend", "category": "Integrations",
			"description": "Resend: send transactional email (single, batch, scheduled, templated), read sent and received mail, manage sending domains, and the marketing side — contacts, segments, broadcasts, templates, suppressions, webhooks and logs.",
			"dataFields":  map[string]any{"integrationOp": "'send_email'|'send_batch'|'get_email'|'list_sent_emails'|'list_received_emails'|'get_received_email'|'reschedule_email'|'cancel_email'|'list_domains'|'get_domain'|'create_domain'|'verify_domain'|'delete_domain'|'create_contact'|'get_contact'|'update_contact'|'list_contacts'|'delete_contact'|'add_contact_to_segment'|'remove_contact_from_segment'|'list_contact_segments'|'create_segment'|'list_segments'|'get_segment'|'delete_segment'|'list_segment_contacts'|'create_broadcast'|'list_broadcasts'|'get_broadcast'|'send_broadcast'|'delete_broadcast'|'get_broadcast_metrics'|'create_template'|'list_templates'|'get_template'|'publish_template'|'delete_template'|'add_suppression'|'list_suppressions'|'remove_suppression'|'list_webhooks'|'create_webhook'|'delete_webhook'|'list_logs'|'list_api_keys'", "resendFrom": "string – sender, must be on a domain verified in Resend; 'Name <a@b.com>' allowed", "resendTo": "string – comma-separated recipients, max 50 (templates ok)", "resendCc": "string – comma-separated", "resendBcc": "string – comma-separated", "resendReplyTo": "string – comma-separated", "resendSubject": "string (templates ok)", "resendHtml": "string – HTML body (templates ok)", "resendText": "string – plain-text body (templates ok)", "resendScheduledAt": "string – ISO 8601 or natural language like 'in 1 hour'", "resendHeaders": "string – JSON object of custom headers", "resendTags": "string – JSON object, e.g. {\"campaign\":\"launch\"}", "resendBatch": "string – JSON array of complete email objects for send_batch", "resendEmailId": "string", "resendDomain": "string – domain name for create_domain", "resendDomainId": "string", "resendRegion": "string – e.g. us-east-1", "resendEmail": "string – contact or suppression address", "resendContactId": "string – contact id; an email also works", "resendFirstName": "string", "resendLastName": "string", "resendUnsubscribed": "'true'|'false'", "resendProperties": "string – JSON object of custom contact properties", "resendSegmentId": "string – segment id; comma-separated when setting a contact's segments", "resendName": "string – segment, broadcast or template name", "resendBroadcastId": "string", "resendTemplateId": "string – set it on send_email to send a template instead of html/text", "resendTemplateVars": "string – JSON object of template variables", "resendUrl": "string – webhook endpoint", "resendEvents": "string – comma-separated webhook events", "resendWebhookId": "string", "resendLimit": "number (default 25)"},
			"auth":        "API key, stored per user — never set integrationToken. Sending fails until the from-address domain is verified in Resend, and a template must be published before send_email can reference it.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "sendgrid", "label": "SendGrid", "category": "Integrations",
			"description": "SendGrid: send transactional email (with dynamic templates and scheduling), plus Marketing Campaigns — contacts, lists, segments, single sends, templates, suppressions, delivery stats and custom fields.",
			"dataFields":  map[string]any{"integrationOp": "'send_email'|'upsert_contact'|'get_import_status'|'get_contact'|'search_contacts'|'list_contacts'|'delete_contact'|'get_contact_count'|'list_lists'|'create_list'|'get_list'|'update_list'|'delete_list'|'remove_contacts_from_list'|'list_segments'|'get_segment'|'create_segment'|'delete_segment'|'list_single_sends'|'get_single_send'|'create_single_send'|'schedule_single_send'|'delete_single_send'|'list_templates'|'get_template'|'create_template'|'delete_template'|'list_bounces'|'list_blocks'|'list_spam_reports'|'list_invalid_emails'|'list_global_unsubscribes'|'add_global_unsubscribe'|'delete_bounce'|'delete_global_unsubscribe'|'get_stats'|'list_verified_senders'|'list_custom_fields'|'create_custom_field'|'get_account'|'list_key_scopes'", "sendgridFrom": "string – sender, must be a verified sender in SendGrid", "sendgridTo": "string – comma-separated recipients (templates ok)", "sendgridCc": "string – comma-separated", "sendgridBcc": "string – comma-separated", "sendgridReplyTo": "string", "sendgridSubject": "string (templates ok; omit when using a dynamic template)", "sendgridHtml": "string – HTML body (templates ok)", "sendgridText": "string – plain-text body (templates ok)", "sendgridSendAt": "string – unix seconds for send_email (max 72h ahead); 'now' or a timestamp for schedule_single_send", "sendgridTemplateId": "string – dynamic template; supplies subject and body", "sendgridTemplateData": "string – JSON object of template variables", "sendgridEmail": "string – contact or suppression address", "sendgridContactId": "string – contact id from search_contacts; comma-separated for list removal", "sendgridFirstName": "string", "sendgridLastName": "string", "sendgridCustomFields": "string – JSON keyed by custom field ID, not name", "sendgridListId": "string – list id; comma-separated where several are allowed", "sendgridSegmentId": "string", "sendgridSingleSendId": "string", "sendgridJobId": "string – returned by upsert_contact; poll it with get_import_status", "sendgridName": "string – list, segment, template or single-send name", "sendgridQuery": "string – SGQL, e.g. email LIKE '%@acme.com'", "sendgridFieldType": "'Text'|'Number'|'Date'", "sendgridStartDate": "string – YYYY-MM-DD", "sendgridEndDate": "string – YYYY-MM-DD", "sendgridAggregate": "'day'|'week'|'month'", "sendgridLimit": "number (default 25)"},
			"auth":        "API key, stored per user — never set integrationToken. Sending needs a verified sender. Contact upserts are ASYNCHRONOUS: upsert_contact returns a job id and the contact is not readable immediately, so never chain a get_contact straight after one — poll get_import_status instead. A 403 usually means the key lacks a scope; list_key_scopes shows what it can do.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "kit", "label": "Kit", "category": "Integrations",
			"description": "Kit (formerly ConvertKit): subscribers, tags, forms, sequences, broadcasts with stats and link clicks, custom fields, purchases, webhooks, segments, email templates and account growth stats.",
			"dataFields":  map[string]any{"integrationOp": "'create_subscriber'|'list_subscribers'|'get_subscriber'|'update_subscriber'|'unsubscribe'|'get_subscriber_stats'|'list_subscriber_tags'|'list_tags'|'create_tag'|'rename_tag'|'tag_subscriber'|'untag_subscriber'|'list_tag_subscribers'|'list_forms'|'add_subscriber_to_form'|'list_form_subscribers'|'list_sequences'|'get_sequence'|'create_sequence'|'add_subscriber_to_sequence'|'list_sequence_subscribers'|'list_broadcasts'|'get_broadcast'|'create_broadcast'|'update_broadcast'|'delete_broadcast'|'get_broadcast_stats'|'get_broadcast_link_clicks'|'list_custom_fields'|'create_custom_field'|'delete_custom_field'|'list_purchases'|'get_purchase'|'create_purchase'|'list_webhooks'|'create_webhook'|'delete_webhook'|'list_segments'|'list_email_templates'|'get_account'|'get_email_stats'|'get_growth_stats'", "kitEmail": "string – subscriber address (templates ok)", "kitFirstName": "string", "kitState": "'active'|'inactive'|'bounced'|'cancelled'", "kitFields": "string – JSON object of custom field values; the field must already exist in Kit", "kitSubscriberId": "string – find one with list_subscribers filtered by kitEmail", "kitCreatedAfter": "string – RFC3339 filter", "kitTagId": "string – tag id; comma-separated to target a broadcast at several tags", "kitFormId": "string", "kitSequenceId": "string", "kitBroadcastId": "string", "kitFieldId": "string", "kitPurchaseId": "string", "kitPurchase": "string – JSON object with email_address, transaction_id, currency and a products array", "kitWebhookId": "string", "kitUrl": "string – webhook target URL", "kitEvent": "string – webhook event name", "kitName": "string – tag, sequence or custom-field label", "kitSubject": "string – broadcast subject (templates ok)", "kitContent": "string – broadcast HTML (templates ok)", "kitDescription": "string – internal broadcast note", "kitSendAt": "string – RFC3339; omit to leave the broadcast as a draft", "kitLimit": "number (default 25)"},
			"auth":        "API key, stored per user — never set integrationToken. It must be a V4 key (Settings → Developer); a v3 key is rejected. 120 requests a minute. Kit has no delete-subscriber endpoint — unsubscribe is the closest action.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "airtable", "label": "Airtable", "category": "Integrations",
			"description": "Airtable: records (list with formula/view/sort, get, create, update, upsert, delete — singly or batched), record comments, base and table schema, field creation, and webhooks.",
			"dataFields":  map[string]any{"integrationOp": "'list_records'|'get_record'|'create_record'|'create_records'|'update_record'|'update_records'|'upsert_records'|'delete_record'|'delete_records'|'list_comments'|'create_comment'|'update_comment'|'delete_comment'|'list_bases'|'get_base_schema'|'create_base'|'create_table'|'update_table'|'create_field'|'update_field'|'list_webhooks'|'create_webhook'|'delete_webhook'|'refresh_webhook'|'list_webhook_payloads'|'whoami'", "airtableBaseId": "string – base id from list_integration_resources (starts app…)", "airtableTable": "string – table name exactly as in Airtable, or its table id", "airtableTableId": "string – schema operations need the table ID, not its name", "airtableRecordId": "string – record id (starts rec…); comma-separated for delete_records", "airtableFields": "string – JSON object keyed by column name, e.g. {\"Name\":\"Acme\"}", "airtableRecords": "string – JSON array for batch ops, MAX 10 per request; include an id on each record when updating", "airtableTypecast": "'false' to stop Airtable coercing strings into numbers, dates and select options (defaults on)", "airtableFormula": "string – filterByFormula, e.g. {Status}=\"Active\"", "airtableView": "string – restrict list_records to a view", "airtableFieldNames": "string – comma-separated columns to return", "airtableSortField": "string", "airtableSortDirection": "'asc'|'desc'", "airtableOffset": "string – pagination offset from a previous list_records", "airtableMergeOn": "string – comma-separated key field(s) for upsert_records", "airtableComment": "string", "airtableCommentId": "string", "airtableName": "string – base, table or field name", "airtableDescription": "string", "airtableWorkspaceId": "string – required by create_base", "airtableTables": "string – JSON array of table definitions for create_base", "airtableTableFields": "string – JSON array of field definitions, e.g. [{\"name\":\"Name\",\"type\":\"singleLineText\"}]", "airtableFieldType": "string – e.g. singleLineText, number, singleSelect, date", "airtableFieldOptions": "string – JSON object; shape depends on the field type", "airtableFieldId": "string", "airtableUrl": "string – webhook notification URL", "airtableWebhookId": "string", "airtableCursor": "string", "airtableLimit": "number"},
			"auth":        "OAuth connection used automatically — never set integrationToken. Batch writes are capped at 10 records per request, so loop for more. Webhooks expire after 7 days unless refresh_webhook is called. Airtable allows 5 requests a second per base.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "clickup", "label": "ClickUp", "category": "Integrations",
			"description": "ClickUp: the workspace hierarchy (spaces, folders, lists), tasks with filtering and cross-list search, comments, checklists, tags, custom fields, dependencies and links, time tracking with a live timer, goals, members, views and webhooks.",
			"dataFields":  map[string]any{"integrationOp": "'list_workspaces'|'list_spaces'|'get_space'|'list_folders'|'list_lists'|'get_list'|'create_list'|'list_tasks'|'search_tasks'|'get_task'|'create_task'|'update_task'|'delete_task'|'list_comments'|'create_comment'|'update_comment'|'delete_comment'|'create_checklist'|'create_checklist_item'|'update_checklist_item'|'delete_checklist'|'list_space_tags'|'add_tag_to_task'|'remove_tag_from_task'|'list_custom_fields'|'set_custom_field_value'|'remove_custom_field_value'|'add_dependency'|'delete_dependency'|'link_tasks'|'unlink_tasks'|'list_time_entries'|'create_time_entry'|'get_running_timer'|'start_timer'|'stop_timer'|'list_attachments'|'list_goals'|'create_goal'|'list_list_members'|'list_task_members'|'list_views'|'list_webhooks'|'create_webhook'|'delete_webhook'|'get_authorized_user'", "clickupWorkspaceId": "string – workspace id from list_integration_resources (ClickUp calls this a team)", "clickupSpaceId": "string", "clickupFolderId": "string – omit to work with lists that sit directly in a space", "clickupListId": "string – list id; comma-separated to scope search_tasks", "clickupTaskId": "string", "clickupCustomTaskIds": "'true' when the task id is a custom one; also needs clickupWorkspaceId", "clickupName": "string – task, list, checklist or goal name (templates ok)", "clickupDescription": "string (templates ok)", "clickupStatus": "string – a status that exists in the target list", "clickupStatuses": "string – comma-separated status filter", "clickupPriority": "string – 1 urgent, 2 high, 3 normal, 4 low", "clickupDueDate": "string – unix timestamp in MILLISECONDS", "clickupTimeEstimate": "string – milliseconds", "clickupAssignees": "string – comma-separated NUMERIC ClickUp user IDs, not emails", "clickupParent": "string – parent task id, making this a subtask", "clickupTagName": "string – tag name; comma-separated on create_task", "clickupSubtasks": "'true' to include subtasks in list_tasks", "clickupIncludeClosed": "'true' to include closed tasks", "clickupOrderBy": "string – e.g. created, updated, due_date", "clickupComment": "string – comment text, or a time-entry description (templates ok)", "clickupCommentId": "string", "clickupChecklistId": "string", "clickupChecklistItemId": "string", "clickupResolved": "'true'|'false' for a checklist item", "clickupFieldId": "string – from list_custom_fields", "clickupFieldValue": "string – JSON for a typed field, plain text otherwise", "clickupDependsOn": "string – the task this one waits for", "clickupDependencyOf": "string – a task that waits for this one", "clickupLinksTo": "string – the other task in a link", "clickupDuration": "string – milliseconds", "clickupStartDate": "string – unix milliseconds", "clickupEndDate": "string – unix milliseconds", "clickupUrl": "string – webhook endpoint", "clickupEvents": "string – comma-separated, e.g. taskCreated,taskUpdated", "clickupWebhookId": "string", "clickupLimit": "number"},
			"auth":        "OAuth connection used automatically — never set integrationToken. ClickUp has no OAuth scopes: consent is per workspace, so start from list_workspaces. Dates are unix MILLISECONDS, not seconds, and assignees are numeric user IDs rather than emails. Tokens do not expire.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "monday", "label": "monday.com", "category": "Integrations",
			"description": "monday.com boards: list and inspect boards and items, create/update/move/archive/delete items, add or read updates, and list account users.",
			"dataFields": map[string]any{
				"integrationOp":      "'list_boards'|'get_board'|'list_items'|'get_item'|'create_item'|'update_item'|'move_item_to_group'|'archive_item'|'delete_item'|'create_update'|'list_updates'|'list_users'",
				"mondayBoardId":      "string – REAL board id from list_integration_resources",
				"mondayItemId":       "string – item id; may come from a trigger or prior node",
				"mondayGroupId":      "string – group id from the selected board's child resources",
				"mondayItemName":     "string – new item name (templates ok)",
				"mondayColumnValues": "string – JSON object keyed by column id, e.g. {\"status\":{\"label\":\"Done\"}}",
				"mondayUpdateBody":   "string – update/comment body (templates ok)",
				"mondayCursor":       "string – items_page cursor from a prior list_items response",
				"mondayLimit":        "number (default 25, max 100)",
			},
			"auth":    "OAuth connection used automatically — never set integrationToken. Call list_integration_resources for a real board, group and column id. Column values must use column IDs, not titles.",
			"handles": map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "asana", "label": "Asana", "category": "Integrations",
			"description": "Asana workspaces and projects: browse projects, sections and tasks; create, update and delete tasks or subtasks; add tasks to projects; and add or read comments.",
			"dataFields": map[string]any{
				"integrationOp":     "'list_workspaces'|'list_projects'|'list_sections'|'list_tasks'|'get_task'|'create_task'|'create_subtask'|'update_task'|'delete_task'|'add_comment'|'list_comments'|'add_task_to_project'",
				"asanaWorkspaceId":  "string – REAL workspace id from list_integration_resources",
				"asanaProjectId":    "string – REAL project id from list_integration_resources",
				"asanaSectionId":    "string – section id from the selected project's child resources",
				"asanaTaskId":       "string – target task id",
				"asanaParentTaskId": "string – parent task for create_subtask",
				"asanaName":         "string – task name (templates ok)",
				"asanaNotes":        "string – task description (templates ok)",
				"asanaAssignee":     "string – Asana user gid or 'me'",
				"asanaDueOn":        "string – YYYY-MM-DD",
				"asanaCompleted":    "'true'|'false' for update_task",
				"asanaComment":      "string – comment text (templates ok)",
				"asanaLimit":        "number (default 50, max 100)",
			},
			"auth":    "OAuth connection used automatically — never set integrationToken. Call list_integration_resources for real workspace/project IDs and its child resources for sections/tasks.",
			"handles": map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "typeform", "label": "Typeform", "category": "Integrations",
			"description": "Typeform: forms (list, get, create, replace, delete), responses with date and cursor filters, a readable question-and-answer rendering of responses, insights, workspaces, themes, images and webhooks.",
			"dataFields":  map[string]any{"integrationOp": "'list_forms'|'get_form'|'create_form'|'update_form'|'delete_form'|'get_form_messages'|'list_responses'|'get_response_text'|'delete_responses'|'get_insights'|'list_workspaces'|'get_workspace'|'create_workspace'|'delete_workspace'|'list_themes'|'get_theme'|'delete_theme'|'list_images'|'list_webhooks'|'create_webhook'|'delete_webhook'|'get_current_user'", "typeformFormId": "string – form id from list_forms", "typeformTitle": "string – form or workspace name (templates ok)", "typeformDefinition": "string – full JSON form definition; required by update_form because a PUT replaces the whole form", "typeformWorkspaceId": "string", "typeformThemeId": "string", "typeformSearch": "string – filter list_forms by name", "typeformSince": "string – RFC3339 lower bound on responses", "typeformUntil": "string – RFC3339 upper bound", "typeformAfter": "string – response token cursor, for picking up where the last run stopped", "typeformCompleted": "'true'|'false' – only finished responses", "typeformQuery": "string – free-text search across answers", "typeformResponseIds": "string – comma-separated response tokens for delete_responses", "typeformUrl": "string – webhook URL", "typeformTag": "string – webhook tag; the same tag replaces an existing webhook (default 'fernary')", "typeformSecret": "string – webhook signing secret", "typeformLimit": "number (default 25)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. Prefer get_response_text over list_responses when the point is to read or summarise answers: raw responses reference questions by field id and are unreadable on their own.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "calendly", "label": "Calendly", "category": "Integrations",
			"description": "Calendly: event types, availability, booking a meeting outright via the Scheduling API, single-use scheduling links, scheduled events and cancellation, invitees and no-shows, availability schedules and busy times, organization membership and invitations, routing forms, and webhooks.",
			"dataFields":  map[string]any{"integrationOp": "'get_current_user'|'get_user'|'list_event_types'|'get_event_type'|'list_available_times'|'create_booking'|'create_scheduling_link'|'list_scheduled_events'|'get_scheduled_event'|'cancel_event'|'list_invitees'|'get_invitee'|'mark_no_show'|'undo_no_show'|'list_availability_schedules'|'list_busy_times'|'list_memberships'|'remove_member'|'invite_to_organization'|'list_invitations'|'list_routing_forms'|'list_routing_form_submissions'|'list_webhooks'|'create_webhook'|'delete_webhook'|'delete_invitee_data'", "calendlyUser": "string – user URI; omit to use the connected account", "calendlyOrganization": "string – organization URI; omit to resolve it automatically", "calendlyScope": "'user'|'organization' – whose records to list (default user)", "calendlyEventType": "string – event type URI from list_event_types", "calendlyEvent": "string – scheduled event URI", "calendlyInvitee": "string – invitee URI", "calendlyNoShow": "string – the no-show URI returned by mark_no_show", "calendlyMembership": "string – membership URI from list_memberships", "calendlyRoutingForm": "string – routing form URI", "calendlyStatus": "'active'|'canceled'", "calendlyStartTime": "string – RFC3339; also the booking start time and the lower bound on listings", "calendlyEndTime": "string – RFC3339; availability windows cannot exceed 7 days", "calendlyEmail": "string – invitee filter, invitation address, or GDPR deletion target", "calendlyReason": "string – cancellation reason", "calendlyInviteeName": "string – who the booking is for", "calendlyInviteeEmail": "string – required by create_booking", "calendlyTimezone": "string – IANA zone, e.g. Europe/Dublin", "calendlyGuests": "string – comma-separated additional attendee emails", "calendlyAnswers": "string – JSON array like [{\"question\":\"Company\",\"answer\":\"Acme\",\"position\":0}]", "calendlyUrl": "string – webhook callback", "calendlyEvents": "string – comma-separated, e.g. invitee.created,invitee.canceled", "calendlyWebhookId": "string – webhook URI", "calendlyLimit": "number (default 25)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. Calendly addresses everything by full URI, not short ids, and the connected user's URI is resolved automatically. create_booking requires a PAID Calendly plan and a start time that appears in list_available_times, so call that first. Use create_scheduling_link instead when a human should pick the slot.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "dropbox", "label": "Dropbox", "category": "Integrations",
			"description": "Dropbox: browse and search files, read and write file contents, temporary direct links, folders, move/copy/delete, revisions and restore, shared links and file members, file requests, and account info.",
			"dataFields":  map[string]any{"integrationOp": "'list_folder'|'list_folder_continue'|'get_metadata'|'search'|'download'|'upload'|'get_temporary_link'|'create_folder'|'delete'|'move'|'copy'|'list_revisions'|'restore'|'create_shared_link'|'list_shared_links'|'revoke_shared_link'|'add_file_member'|'list_file_members'|'share_folder'|'list_file_requests'|'create_file_request'|'get_current_account'|'get_space_usage'", "dropboxPath": "string – ABSOLUTE path from the Dropbox root starting with a slash, e.g. /Reports/q3.txt; leave empty for the root itself", "dropboxToPath": "string – destination for move and copy", "dropboxContent": "string – text to upload (templates ok)", "dropboxOverwrite": "'true' to replace an existing file; otherwise Dropbox autorenames", "dropboxRecursive": "'true' to walk subfolders in list_folder", "dropboxCursor": "string – cursor from a previous list_folder, for list_folder_continue", "dropboxQuery": "string – search terms", "dropboxRev": "string – revision id from list_revisions", "dropboxUrl": "string – shared link URL for revoke_shared_link", "dropboxVisibility": "'public'|'team_only'|'password' – the restricted options need a paid Dropbox plan", "dropboxEmail": "string – comma-separated emails to share with", "dropboxAccessLevel": "'viewer'|'editor'", "dropboxMessage": "string – note sent with a share", "dropboxTitle": "string – file request title", "dropboxLimit": "number (default 100)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. Paths are absolute from the account root and must start with a slash; the root is the empty string. download returns TEXT, so it suits .txt/.md/.csv/.json and will produce unusable output for a PDF or an image — use get_temporary_link and an httpRequest node for binaries.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "netlify", "label": "Netlify", "category": "Integrations",
			"description": "Netlify: sites, deploys (create, cancel, rollback, lock), builds, environment variables, forms and submissions, DNS zones and records, build hooks, notification hooks, deploy keys and account info.",
			"dataFields":  map[string]any{"integrationOp": "'list_sites'|'list_account_sites'|'get_site'|'create_site'|'update_site'|'delete_site'|'enable_site'|'disable_site'|'list_deploys'|'get_deploy'|'create_deploy'|'cancel_deploy'|'restore_deploy'|'rollback_site'|'lock_deploy'|'unlock_deploy'|'delete_deploy'|'list_builds'|'get_build'|'start_build'|'get_account_build_status'|'list_env_vars'|'list_site_env_vars'|'get_env_var'|'create_env_vars'|'update_env_var'|'set_env_var_value'|'delete_env_var'|'delete_env_var_value'|'list_forms'|'delete_form'|'list_site_submissions'|'list_form_submissions'|'get_submission'|'delete_submission'|'list_dns_zones'|'get_dns_zone'|'create_dns_zone'|'delete_dns_zone'|'list_dns_records'|'get_dns_record'|'create_dns_record'|'delete_dns_record'|'get_site_dns'|'configure_site_dns'|'list_build_hooks'|'get_build_hook'|'create_build_hook'|'update_build_hook'|'delete_build_hook'|'list_hooks'|'get_hook'|'create_hook'|'update_hook'|'delete_hook'|'enable_hook'|'list_hook_types'|'list_deploy_keys'|'get_deploy_key'|'create_deploy_key'|'delete_deploy_key'|'get_current_user'|'list_accounts'|'get_account'|'list_account_members'|'list_audit_events'", "netlifySiteId": "string – site id or its api id", "netlifyAccountId": "string – team ID; resolved from the site automatically when omitted", "netlifyAccountSlug": "string – the team's URL name, NOT its ID; member and per-team site listings need the slug and 404 on the id", "netlifyDeployId": "string", "netlifyBuildId": "string", "netlifyFormId": "string", "netlifySubmissionId": "string", "netlifyZoneId": "string", "netlifyRecordId": "string", "netlifyHookId": "string", "netlifyBuildHookId": "string", "netlifyKeyId": "string", "netlifyName": "string – site, hook or record name", "netlifyTitle": "string – deploy title", "netlifyCustomDomain": "string", "netlifySiteConfig": "string – raw JSON site body", "netlifyRepo": "string – raw JSON repo settings", "netlifyConfigureDns": "'true'|'false'", "netlifyBranch": "string", "netlifyPage": "number", "netlifyPerPage": "number", "netlifyFilter": "string", "netlifyEnvKey": "string – environment variable name", "netlifyEnvValue": "string", "netlifyEnvContext": "string – all | production | deploy-preview | branch-deploy | dev", "netlifyEnvValues": "string – JSON array of {value,context} objects; a PUT REPLACES every context", "netlifyEnvScopes": "string – comma-separated: builds, functions, runtime, post-processing", "netlifyDeployFiles": "string – JSON manifest of path → SHA1; an empty one would empty the site", "netlifyDraft": "'true' for a draft deploy that does not go live", "netlifyEvent": "string – hook event, e.g. deploy_succeeded", "netlifyHookType": "string – hook type, e.g. email or slack", "netlifyHookData": "string – raw JSON hook data", "netlifyRecordType": "string – A, CNAME, TXT, MX…", "netlifyRecordValue": "string", "netlifyTtl": "number", "netlifyDnsZoneName": "string", "netlifyPublicKey": "string – deploy key", "netlifyLimit": "number"},
			"auth":        "OAuth connection used automatically — never set integrationToken. Netlify has NO scopes and tokens never expire. Three ids are not interchangeable: site_id, account_id (team UUID) and account_slug — using the id where the slug is wanted returns 404. Environment variables are account-scoped: leaving the site blank writes a TEAM-WIDE variable rather than erroring. Deploys are rate-limited to 3 a minute and 100 a day, so avoid looping over many sites. Prefer start_build over create_deploy unless you genuinely have a full SHA1 file manifest.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "vercel", "label": "Vercel", "category": "Integrations",
			"description": "Vercel: deployments (list, inspect, redeploy, cancel, delete), build logs and runtime logs, promote and roll back production, projects and their settings, environment variables, domains and aliases, teams and the token's own account.",
			"dataFields":  map[string]any{"integrationOp": "'list_deployments'|'get_deployment'|'get_deployment_events'|'get_runtime_logs'|'redeploy'|'cancel_deployment'|'delete_deployment'|'list_deployment_aliases'|'assign_alias'|'list_projects'|'get_project'|'update_project'|'promote_deployment'|'rollback_deployment'|'list_env_vars'|'get_env_var_value'|'create_env_var'|'update_env_var'|'delete_env_var'|'list_domains'|'get_domain'|'list_project_domains'|'add_project_domain'|'verify_project_domain'|'remove_project_domain'|'list_teams'|'get_current_user'", "vercelTeamId": "string – REQUIRED for anything owned by a team, not a personal account; without it the request runs against the token owner's personal scope and a team project returns 404. Get it from list_teams", "vercelTeamSlug": "string – alternative to vercelTeamId", "vercelProjectId": "string – project id OR its name", "vercelDeploymentId": "string – deployment id OR a deployment URL, so a value read from a webhook or a log line works directly", "vercelName": "string – project name; redeploy reads it off the source deployment when omitted", "vercelTarget": "'production'|'preview'|'development'", "vercelState": "string – filter list_deployments: READY, ERROR, BUILDING, QUEUED, INITIALIZING, CANCELED, BLOCKED (comma-separated allowed)", "vercelBranch": "string – filter list_deployments by git branch", "vercelSha": "string – filter list_deployments by commit SHA", "vercelAlias": "string – hostname for assign_alias", "vercelDomain": "string – domain name", "vercelRedirect": "string – redirect target when adding a project domain", "vercelGitBranch": "string – branch for a branch-scoped env var or a branch-locked domain", "vercelEnvKey": "string – environment variable name", "vercelEnvValue": "string – environment variable value", "vercelEnvVarId": "string – the variable's id, from list_env_vars; the key alone will not do", "vercelEnvTarget": "string – comma-separated: production, preview, development. REQUIRED by create_env_var", "vercelEnvType": "'encrypted'|'plain'|'sensitive' (default encrypted)", "vercelProjectConfig": "string – raw JSON body of project settings for update_project", "vercelUrl": "string", "vercelSearch": "string – filter list_projects by name", "vercelBuildId": "string – narrows get_deployment_events to a single build; Vercel calls this 'name' on that endpoint", "vercelLimit": "number"},
			"auth":        "API key connection used automatically — never set integrationToken. Vercel authenticates with a personal access token, so a 401 means it expired or was revoked and the only fix is reconnecting. THE MOST COMMON FAILURE IS A MISSING TEAM: a token can see several teams but defaults to its owner's personal scope, so set vercelTeamId whenever the project belongs to a team — the error is a 404 that looks like a wrong id. Paths are versioned per endpoint, which is why op names matter more than URLs here. redeploy inherits every setting from the deployment it copies and always forces a fresh build. get_deployment_events is the BUILD log; get_runtime_logs is per-request and needs both the project and the deployment. Both are truncated from the START, keeping the end, because that is where a failure is. list_env_vars returns values ENCRYPTED — get_env_var_value is the separate, deliberately-approvable way to read one secret. promote_deployment and rollback_deployment change what production serves.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "supabase", "label": "Supabase", "category": "Integrations",
			"description": "Supabase Management API — the control plane, not the data plane. Projects (create, pause, restore, delete), API keys, organizations, Edge Functions and deploys, secrets, database SQL and backups, auth config, branches, custom hostnames, network restrictions and TypeScript types. Reading table rows is PostgREST on the project's own host and is NOT available here.",
			"dataFields":  map[string]any{"integrationOp": "'list_projects'|'get_project'|'get_project_health'|'list_regions'|'create_project'|'delete_project'|'pause_project'|'restore_project'|'restart_project'|'list_api_keys'|'create_api_key'|'delete_api_key'|'list_organizations'|'get_organization'|'list_organization_projects'|'list_organization_members'|'run_sql_read_only'|'run_sql'|'get_database_metadata'|'list_migrations'|'apply_migration'|'rollback_migrations'|'list_backups'|'restore_pitr'|'list_functions'|'get_function'|'get_function_body'|'create_function'|'update_function'|'deploy_function'|'delete_function'|'list_secrets'|'create_secrets'|'delete_secrets'|'get_auth_config'|'update_auth_config'|'list_storage_buckets'|'list_branches'|'get_branch'|'create_branch'|'delete_branch'|'merge_branch'|'reset_branch'|'get_custom_hostname'|'set_custom_hostname'|'verify_custom_hostname'|'activate_custom_hostname'|'delete_custom_hostname'|'get_network_restrictions'|'apply_network_restrictions'|'list_network_bans'|'delete_network_bans'|'get_postgrest_config'|'update_postgrest_config'|'generate_types'|'list_snippets'|'get_snippet'", "supabaseAllowWrite": "'true' is required before run_sql will execute", "supabaseAllowedCidrs": "string", "supabaseAllowedCidrsV6": "string", "supabaseApiKeyId": "string", "supabaseApiKeyType": "string", "supabaseAuthConfig": "string", "supabaseBranchName": "string", "supabaseBranchRef": "string", "supabaseConfirmDelete": "string", "supabaseCursor": "string", "supabaseDbPass": "database password for a new project", "supabaseEntrypointPath": "string", "supabaseForce": "string", "supabaseFunctionBody": "string", "supabaseFunctionSlug": "string", "supabaseGitBranch": "string", "supabaseHostname": "string", "supabaseImportMapPath": "string", "supabaseIncludedSchemas": "string", "supabaseInstanceSize": "string", "supabaseIpAddresses": "string", "supabaseLimit": "number", "supabaseMigrationName": "string", "supabaseMigrationVersion": "string", "supabaseName": "string", "supabaseOrgSlug": "string", "supabasePersistent": "string", "supabasePostgrestMaxRows": "number", "supabasePostgrestSchema": "string", "supabasePostgrestSearchPath": "string", "supabaseProjectRef": "the 20-character project ref, NOT the project UUID", "supabaseRecoveryTimeUnix": "string", "supabaseRegion": "string", "supabaseRevealKeys": "string", "supabaseRollbackSql": "string", "supabaseSecretNames": "string", "supabaseSecrets": "JSON object or array of name/value pairs", "supabaseSiteUrl": "string", "supabaseSnippetId": "string", "supabaseSortBy": "string", "supabaseSortOrder": "string", "supabaseSql": "raw SQL; prefer parameters over string interpolation", "supabaseSqlParams": "JSON array bound to $1, $2 … in the statement", "supabaseUriAllowList": "string", "supabaseVerifyJwt": "string", "supabaseWithData": "string"},
			"auth":        "OAuth connection used automatically — never set integrationToken. Project endpoints key off the 20-character project REF, not the project UUID; passing the id returns an opaque 404. run_sql executes arbitrary SQL as the database owner — DROP, DELETE and ALTER all succeed and nothing is written to migration history — so it refuses to run unless supabaseAllowWrite is 'true'. Use run_sql_read_only for anything that only reads, and pass values through supabaseSqlParams rather than interpolating them into the statement, because a template substituted into SQL is injectable. A 403 is almost always a missing OAuth scope rather than a permission problem.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "gumroad", "label": "Gumroad", "category": "Integrations",
			"description": "Gumroad: products (create, update, enable, disable, delete), variants and variant categories, offer codes, custom fields, sales with date filters, refunds and shipping, subscribers, licence keys (verify, enable, disable), and webhooks.",
			"dataFields":  map[string]any{"integrationOp": "'get_user'|'list_products'|'get_product'|'create_product'|'update_product'|'delete_product'|'enable_product'|'disable_product'|'list_variant_categories'|'create_variant_category'|'list_variants'|'create_variant'|'list_offer_codes'|'get_offer_code'|'create_offer_code'|'update_offer_code'|'delete_offer_code'|'list_custom_fields'|'create_custom_field'|'delete_custom_field'|'list_sales'|'get_sale'|'mark_as_shipped'|'refund_sale'|'list_subscribers'|'get_subscriber'|'verify_license'|'enable_license'|'list_webhooks'|'create_webhook'|'delete_webhook'|'disable_license'|'decrement_license_uses'", "gumroadAfter": "YYYY-MM-DD, exclusive", "gumroadAmount": "refund amount in cents; omit to refund in full", "gumroadAmountOff": "cents, or a percentage when the offer type is percent", "gumroadBefore": "YYYY-MM-DD, exclusive", "gumroadCategoryId": "string", "gumroadCode": "string", "gumroadCustomPermalink": "string", "gumroadDescription": "string", "gumroadEmail": "string", "gumroadIncrementUses": "'true' counts this check against the licence uses", "gumroadLicenseKey": "string", "gumroadMaxPurchases": "string", "gumroadName": "string", "gumroadOfferCodeId": "string", "gumroadOfferType": "cents | percent", "gumroadPageKey": "opaque paging key from a previous list_sales", "gumroadPrice": "CENTS — 1000 is $10.00", "gumroadPriceDifference": "variant surcharge, in cents", "gumroadProductId": "string", "gumroadRequired": "string", "gumroadResourceName": "sale | refund | dispute | cancellation | subscription_updated", "gumroadSaleId": "string", "gumroadSubscriberId": "string", "gumroadTitle": "string", "gumroadTrackingUrl": "string", "gumroadUrl": "string", "gumroadWebhookId": "string"},
			"auth":        "OAuth connection used automatically — never set integrationToken. All money is in CENTS, so 1000 means $10.00; sending 10 sells the product for ten cents. Gumroad answers 200 with success:false for a rejected request, which this node surfaces as an error rather than a result. verify_license counts a use by default, so set gumroadIncrementUses to false for a read-only check. Most resources are nested under a product, so a product ID is needed more often than not.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googlesearchconsole", "label": "Google Search Console", "category": "Integrations",
			"description": "Google Search Console: search analytics with dimensions and filters, properties, sitemaps, and URL inspection.",
			"dataFields":  map[string]any{"integrationOp": "'list_sites'|'get_site'|'add_site'|'delete_site'|'list_sitemaps'|'get_sitemap'|'submit_sitemap'|'delete_sitemap'|'query_search_analytics'|'inspect_url'", "gscSiteUrl": "https://example.com/ or sc-domain:example.com", "gscFeedPath": "full sitemap URL", "gscStartDate": "YYYY-MM-DD, Pacific time", "gscEndDate": "string", "gscDimensions": "query, page, country, device, date", "gscSearchType": "web | image | video | news | discover", "gscDataState": "final (default) | all", "gscFilterExpression": "one 'dimension operator value' per line", "gscRowLimit": "number", "gscStartRow": "number", "gscInspectionUrl": "string", "gscLanguageCode": "string"},
			"auth":        "OAuth connection used automatically — never set integrationToken. A property is identified by its own URL (https://example.com/, trailing slash included) or as sc-domain:example.com for a Domain property; the wrong form reads as a permissions error. Search Console reports in Pacific time and lags about two days, so a query ending yesterday often returns nothing — that is the product, not a failure. query_search_analytics without dimensions returns a single aggregate row, so pass query, page, country, device or date.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "googlecontacts", "label": "Google Contacts", "category": "Integrations",
			"description": "Google Contacts via the People API: read, create, update and delete contacts, search, contact groups and membership, and the 'other contacts' you have corresponded with but never saved.",
			"dataFields":  map[string]any{"integrationOp": "'get_my_profile'|'list_contacts'|'get_contact'|'search_contacts'|'list_other_contacts'|'search_other_contacts'|'create_contact'|'update_contact'|'delete_contact'|'batch_delete_contacts'|'copy_other_contact'|'list_contact_groups'|'get_contact_group'|'create_contact_group'|'update_contact_group'|'delete_contact_group'|'modify_group_members'", "contactsResourceName": "people/c123; comma-separated for batch delete", "contactsFields": "personFields mask; a sensible default applies", "contactsQuery": "string", "contactsPageToken": "string", "contactsSortOrder": "string", "contactsGivenName": "string", "contactsFamilyName": "string", "contactsEmail": "comma-separated", "contactsPhone": "comma-separated", "contactsOrganization": "string", "contactsJobTitle": "string", "contactsAddress": "string", "contactsNotes": "string", "contactsRawPerson": "extra People-API fields as JSON", "contactsGroupId": "string", "contactsGroupName": "string", "contactsAddMembers": "comma-separated resource names", "contactsRemoveMembers": "string", "contactsLimit": "number"},
			"auth":        "OAuth connection used automatically — never set integrationToken. update_contact reads the contact first to obtain its etag, because Google rejects a write without one rather than overwriting a concurrent edit; it therefore fails safely if the contact changed since. Only the fields supplied are updated, so an omitted field is left alone. search_contacts runs against a cache that needs warming, which this node handles, though a brand-new contact can still take a moment to become searchable.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "hubspot", "label": "HubSpot", "category": "Integrations",
			"description": "HubSpot CRM: one uniform set of operations over every object type — contacts, companies, deals, tickets, line items, quotes and the engagement types (notes, tasks, calls, emails, meetings) — plus search, batch operations, associations, property schema, pipelines, owners and lists.",
			"dataFields":  map[string]any{"integrationOp": "'list_objects'|'get_object'|'search_objects'|'create_object'|'update_object'|'delete_object'|'batch_create_objects'|'list_associations'|'associate_objects'|'disassociate_objects'|'list_properties'|'get_property'|'create_property'|'list_pipelines'|'list_owners'|'search_lists'|'get_list'|'list_memberships'|'add_to_list'|'batch_update_objects'|'batch_read_objects'|'batch_archive_objects'|'remove_from_list'", "hubspotAfter": "string", "hubspotArchived": "string", "hubspotAssociations": "JSON array of associations to create alongside the record", "hubspotBatchInputs": "JSON array, max 100 per request", "hubspotFieldType": "string", "hubspotFilters": "JSON array of filter groups for search_objects", "hubspotGroupName": "string", "hubspotIdProperty": "look a record up by a unique property such as email instead of its id", "hubspotLabel": "string", "hubspotLimit": "number", "hubspotListId": "string", "hubspotObjectId": "string", "hubspotObjectType": "contacts | companies | deals | tickets | notes | tasks | calls | emails | meetings, or a custom type id", "hubspotProperties": "comma-separated property names to return; v3 omits anything not asked for", "hubspotPropertyName": "string", "hubspotPropertyType": "string", "hubspotPropertyValues": "JSON object keyed by HubSpot's internal names, e.g. firstname not First Name", "hubspotQuery": "string", "hubspotSortDirection": "string", "hubspotSortProperty": "string", "hubspotToObjectId": "string", "hubspotToObjectType": "string"},
			"auth":        "OAuth connection used automatically — never set integrationToken. Every operation takes hubspotObjectType, so contacts and deals share one set of ops and a custom object type id works too. IMPORTANT: v3 returns only a default subset of properties, so anything beyond the basics must be named in hubspotProperties or it silently comes back missing rather than erroring. Property keys are HubSpot internal names (firstname, not First Name) — call list_properties to discover them. delete_object archives rather than destroys, so it is recoverable in HubSpot. Associations are on API v4 while objects are on v3. A 409 means a unique property already exists, usually a duplicate email, so search then update.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "front", "label": "Front", "category": "Integrations",
			"description": "Front: conversations with search and inbox or tag scoping, customer-facing messages and replies, internal comments, drafts, tags, contacts and their handles, inboxes, channels, teammates, teams, accounts, links and the event stream.",
			"dataFields":  map[string]any{"integrationOp": "'list_conversations'|'search_conversations'|'get_conversation'|'update_conversation'|'assign_conversation'|'list_conversation_messages'|'send_message'|'reply_to_conversation'|'create_draft'|'add_comment'|'list_comments'|'list_tags'|'add_tags'|'remove_tags'|'create_tag'|'list_contacts'|'get_contact'|'create_contact'|'update_contact'|'delete_contact'|'add_contact_handle'|'list_inboxes'|'list_channels'|'list_teammates'|'get_teammate'|'list_teams'|'list_accounts'|'list_events'|'list_links'|'create_link'|'link_conversation'", "frontAssigneeId": "string", "frontAuthorId": "teammate id the message or comment is sent as", "frontBcc": "string", "frontBody": "string", "frontCc": "string", "frontChannelId": "cha_… — required to start a new conversation", "frontContactId": "string", "frontConversationId": "cnv_…", "frontDescription": "string", "frontHandle": "an address on a channel, e.g. an email address", "frontHandleSource": "email | phone | twitter | intercom | custom", "frontInboxId": "string", "frontLimit": "number", "frontLinkId": "string", "frontName": "string", "frontPageToken": "string", "frontQuery": "search terms, or event types for list_events", "frontStatus": "archived | open | deleted | spam", "frontSubject": "string", "frontTagId": "comma-separated tag ids", "frontTeammateId": "string", "frontTo": "string", "frontUrl": "string"},
			"auth":        "OAuth connection used automatically — never set integrationToken. CRITICAL: a message is customer-facing and a comment is internal. Use add_comment for a note only teammates should see, and reply_to_conversation to actually answer the customer — confusing them sends private remarks to the customer. Starting a new conversation uses send_message with a channel; answering an existing one uses reply_to_conversation with the conversation. A 422 usually means the channel cannot send or the author is not one of its teammates.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "granola", "label": "Granola", "category": "Integrations",
			"description": "Granola meeting notes: list notes, read a note's AI summary, and pull the full transcript as readable text for summarising or extracting action items.",
			"dataFields":  map[string]any{"integrationOp": "'list_notes'|'get_note'|'get_transcript'", "granolaNoteId": "string – note id from list_notes", "granolaCreatedAfter": "string – RFC3339; use it to scope a digest to notes since the last run", "granolaCursor": "string – pagination cursor from a previous list_notes", "granolaLimit": "number"},
			"auth":        "API key, stored per user — never set integrationToken. Granola API access needs a Business or Enterprise workspace plan. Read-only: there is no way to create or edit notes.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "stripe", "label": "Stripe", "category": "Integrations",
			"description": "Stripe API: customers (create/get/list), subscriptions (list/get/cancel), products and prices (create/list), invoices, payment intents, refunds (create/list), payment links, balance, account events.",
			"dataFields":  map[string]any{"integrationOp": "'list_customers'|'create_customer'|'get_customer'|'list_payments'|'get_payment_intent'|'list_invoices'|'get_invoice'|'get_balance'|'create_payment_link'|'list_subscriptions'|'get_subscription'|'cancel_subscription'|'list_products'|'create_product'|'create_price'|'create_refund'|'list_refunds'|'list_events'", "stripeCustomerEmail": "string – filter for list_customers / create_customer email", "stripeCustomerName": "string – create_customer", "stripeCustomerId": "string – get_customer / list_subscriptions filter", "stripeSubscriptionId": "string – get/cancel_subscription", "stripeProductId": "string – create_price target", "stripeProductName": "string – create_product", "stripeAmount": "number – cents (create_price; optional partial amount for create_refund)", "stripeCurrency": "string – e.g. usd (default)", "stripeInterval": "'one-time'|'month'|'year' for create_price", "stripeInvoiceId": "string", "stripePaymentIntentId": "string – get_payment_intent / create_refund", "stripeRefundReason": "'duplicate'|'fraudulent'|'requested_by_customer'", "stripePriceId": "string – payment link price from list_integration_resources", "stripeQuantity": "number (default 1)", "stripeLimit": "number (default 10)"},
			"auth":        "OAuth connection used automatically — never set integrationToken; call list_integration_resources for real price IDs",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "shopify", "label": "Shopify", "category": "Integrations",
			"description": "Shopify Admin API: list/get orders, list/create products, list customers.",
			"dataFields":  map[string]any{"integrationOp": "'list_orders'|'get_order'|'cancel_order'|'close_order'|'list_products'|'get_product'|'create_product'|'update_product'|'delete_product'|'list_customers'|'get_customer'|'search_customers'|'create_customer'|'create_draft_order'|'list_draft_orders'|'list_locations'|'adjust_inventory'|'create_discount_code'", "shopifyOrderId": "string", "shopifyProductId": "string", "shopifyTitle": "string – product title / draft order line item (templates ok)", "shopifyDescription": "string (templates ok)", "shopifyPrice": "string – e.g. 19.99", "shopifyCustomerId": "string", "shopifyCustomerEmail": "string", "shopifyCustomerName": "string – 'First Last'", "shopifyQuery": "string – search_customers", "shopifyQuantity": "number (default 1)", "shopifyInventoryItemId": "string", "shopifyLocationId": "string – from list_locations", "shopifyDelta": "number – inventory adjustment, ±", "shopifyDiscountCode": "string – code text", "shopifyDiscountType": "'percentage'|'fixed_amount'", "shopifyDiscountValue": "string – e.g. '10'", "shopifyStatus": "'any'|'open'|'closed'", "shopifyLimit": "number (default 10)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. The connected shop domain is used automatically.",
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{"source (right)"}},
		},
		{
			"type": "textOutput", "label": "Text Output", "category": "Outputs",
			"description": "Displays the final result of the pipeline.",
			"dataFields":  map[string]any{"label": "string – display name"},
			"handles":     map[string]any{"inputs": []string{"target (left)"}, "outputs": []string{}},
		},
	}
}

func getAvailableNodesResult() string {
	result := map[string]any{
		"nodes": nodeCatalog(),
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

// catalogEntry returns the catalog doc for one node type (nil if unknown).
func catalogEntry(nodeType string) map[string]any {
	for _, n := range nodeCatalog() {
		if n["type"] == nodeType {
			return n
		}
	}
	return nil
}

// ── System prompt ───────────────────────────────────────────────

const workflowSystemPrompt = `You are a workflow builder AI. The user describes what they want and you build or edit it using your tools.

Decision rules:
- If the canvas might already have a workflow, call get_current_workflow first to see what's there.
- If the user asks to ADD, REMOVE, or CHANGE something specific → call update_workflow with targeted operations.
- If the user asks to build something from scratch, or the canvas is empty → call get_available_nodes then create_workflow.
- Never tear down and rebuild a workflow just to make a small change.

Tool order for NEW workflows:
1. get_available_nodes — learn node schemas
2. create_workflow — place all nodes and edges

Tool order for EDITS:
1. get_current_workflow — see what's already on the canvas
2. update_workflow — apply only the necessary changes

update_workflow operations:
- add_node: { op, node: { id, type, position: {x,y}, data: { nodeType, label, ...fields } } }
- remove_node: { op, node_id } — also removes connected edges automatically
- add_edge: { op, edge: { id, source, target, sourceHandle? } }
- remove_edge: { op, edge_id }
- update_node: { op, node_id, data: { ...only the fields to change } }

Rules:
- Every node's data MUST include nodeType (matching the node type) and label.
- For branch nodes, edges need sourceHandle "true" or "false".
- Space new nodes ~250px apart from existing ones.
- After calling create_workflow or update_workflow, explain what you did and what the user needs to configure.

Integrations (including notion, linear, github, gitlab, monday, asana, gmail, stripe, shopify, jira, confluence, bitbucket and the Google suite):
- Auth is handled by OAuth connections — NEVER set integrationToken and never ask the user for API keys.
- Before placing or editing an integration node, call list_integration_resources and use the REAL resource IDs (notionDatabaseId, notionPageId, linearTeamId, linearProjectId, githubRepo, gitlabProjectId, confluenceSpaceKey, confluencePageId, stripePriceId) from the response. Mention the resource by name when you explain the workflow.
- If the provider is not connected, still build the node but leave the resource ID empty and tell the user to click Connect in the node's settings panel, then ask you to fill in the target resource.
- Prefer the gmail node over emailSend when the user wants mail sent from their own address or wants to read/search their inbox.

App Triggers (integrationTrigger):
- Use integrationTrigger — not an action node — when the user says the workflow should start when something happens in a provider listed in its live eventCatalog.
- Call get_available_nodes for the live eventCatalog, then list_integration_resources for the chosen provider. Set triggerProvider, an EXACT supported triggerEvent, the REAL triggerResourceId, its triggerResourceLabel, and only supported triggerFilters. Never invent event ids or repository/project ids.
- Resource ids use the provider's real format (for example GitHub owner/repo; GitLab, monday.com and Asana numeric ids). If the provider is disconnected, leave the resource fields empty and explain that it must be connected before the target can be selected.
- The builder configures the node on the canvas but does not register external webhooks. After building, tell the user to save the workflow, open the App Trigger, click Start listening, and Publish. Registration validates provider access and creates the required remote subscription.
- The normalized payload is the trigger node output. Downstream nodes access event fields as {{nodeId.output.data.title}}, {{nodeId.output.data.body}}, and so on, using the selected event's sample payload as the shape.

Persistence (Data stores):
- Trigger outputs carry NO memory — a scheduled run knows nothing about previous runs. For anything that must survive across runs (counters like "email #3 of 10", dedup like "skip orders already handled", cursors, accumulating digests), use a data node backed by a Data store. Do not fake state with LLM prompts.
- Call list_data_stores first and set a REAL dataStoreId. If no suitable store exists, call create_data_store BEFORE building the nodes that need it — it pauses and waits for the user's answer, then returns either the approved store's real store_id (use it as dataStoreId and carry on) or a rejection (don't use the store; build what you can and say which part needs persistence).
- Scope guide: workflow (default — persists across this workflow's runs), account (shared across the user's workflows), run (scratch state inside a single run).

Schedules:
- A scheduledTrigger node carries no cadence. Whenever you place or keep one and the user has stated any timing ("every 2 minutes", "each morning", "Mondays"), call set_schedule to set it. Never end a turn telling the user to open the node and set the cadence themselves — that is your job.
- Times are UTC; if the user names a local time, convert it and say what you set.
- Schedules only fire when the workflow is Published, so if set_schedule reports published:false, tell them to hit Publish.

Debugging with run history:
- When the user says something is broken, didn't run, or produced the wrong result, call list_runs and then get_run_logs on the relevant run. Diagnose from the recorded error, quote it back to them, and fix the node it points at — never speculate about a cause you have not read.
- You cannot execute workflows yourself. After a fix, tell the user to hit Run (or wait for the schedule); next turn you can read that run's logs to confirm it worked.
- If a run failed because a value is missing that only the user has (a recipient address, a resource id, a connected account), name the exact field they need to fill instead of inventing a value.
- No runs recorded yet means you have nothing to diagnose: say so and ask them to run it once, rather than guessing.`

// ── Handler ─────────────────────────────────────────────────────

func (h *WorkflowHandler) AIGenerate(c *gin.Context) {
	var req aiGenerateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	// Per-user cap: each call spends paid LLM tokens, so throttle abuse.
	if !auth.Allow(c.Request.Context(), h.redis, "rl:ai:"+auth.UserID(c), 30, time.Minute) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests — try again in a minute"})
		return
	}

	// The AI builder spends real tokens before anyone has paid anything, so it is
	// metered like any other surface. Left free it would be both the largest
	// zero-revenue cost and an open invitation to farm our provider quota through
	// the free tier. The free grant is sized to allow genuine evaluation instead.
	plan, err := h.bill.CheckBalance(currentOrgID(c), auth.UserID(c))
	if err != nil {
		if errors.Is(err, billing.ErrOverCap) {
			c.JSON(http.StatusPaymentRequired, gin.H{"error": err.Error(), "limit": billing.KindOf(err)})
			return
		}
		slog.ErrorContext(c.Request.Context(), "builder balance check failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not start"})
		return
	}

	// Tag the surface once, here, so every LLM call this request makes — including
	// the ones inside the tool loop — bills as builder usage rather than "unknown".
	ctx := telemetry.WithSurface(c.Request.Context(), telemetry.SurfaceBuilder)
	ctx = telemetry.WithBilling(ctx, billing.BillingContextFor(currentOrgID(c), auth.UserID(c), plan))
	c.Request = c.Request.WithContext(ctx)

	model := resolveChatModel(req.Model)
	prov := chatProviders[model.Provider]
	apiKey := os.Getenv(prov.KeyEnv)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": prov.KeyEnv + " not configured on server"})
		return
	}

	start := time.Now()
	slog.InfoContext(c.Request.Context(), "ai generate requested",
		"model", model.ID, "provider", model.Provider, "prompt_chars", len(req.Prompt))
	telemetry.SpanAttrs(c.Request.Context(), attribute.String("fernary.ai.model", model.ID))

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

	var chatOK bool
	if model.Provider == "anthropic" {
		chatOK = h.runAnthropicChat(c, flusher, &req, model, apiKey)
	} else {
		chatOK = h.runOpenAIChat(c, flusher, &req, model, apiKey, prov.URL)
	}

	sendSSE(c.Writer, flusher, "done", "")
	slog.InfoContext(c.Request.Context(), "ai generate finished",
		"model", model.ID, "duration_ms", time.Since(start).Milliseconds(), "ok", chatOK)
}

// toolActivityLabel is what the chat UI shows for each builder tool call —
// plain description of the work, not the tool's wire name.
func toolActivityLabel(name string, input any) string {
	m, _ := input.(map[string]any)
	switch name {
	case "get_available_nodes":
		return "Reading the node catalog"
	case "get_current_workflow":
		return "Reading the canvas"
	case "create_workflow":
		if nodes, ok := m["nodes"].([]any); ok {
			return fmt.Sprintf("Building the workflow · %d nodes", len(nodes))
		}
		return "Building the workflow"
	case "update_workflow":
		if ops, ok := m["operations"].([]any); ok {
			return fmt.Sprintf("Updating the workflow · %d change(s)", len(ops))
		}
		return "Updating the workflow"
	case "list_integration_resources":
		if p, _ := m["provider"].(string); p != "" {
			return "Checking your " + p + " connection"
		}
		return "Checking connected integrations"
	case "list_data_stores":
		return "Checking your data stores"
	case "create_data_store":
		if n, _ := m["name"].(string); n != "" {
			return "Waiting for your approval · " + n
		}
		return "Waiting for your approval"
	case "set_schedule":
		return "Setting the schedule"
	case "list_runs":
		return "Checking recent runs"
	case "get_run_logs":
		return "Reading the run logs"
	case executor.ClockToolName:
		if tz, _ := m["timezone"].(string); tz != "" {
			return "Checking the time in " + tz
		}
		return "Checking the current time"
	default:
		return name
	}
}

// execChatToolWithActivity wraps a tool call in tool_start/tool_result SSE
// events so the chat UI can show what the builder is doing, step by step.
func (h *WorkflowHandler) execChatToolWithActivity(c *gin.Context, flusher http.Flusher, req *aiGenerateRequest, name string, input any) string {
	label := toolActivityLabel(name, input)
	chip, _ := json.Marshal(map[string]string{"tool": name, "label": label})
	sendSSE(c.Writer, flusher, "tool_start", string(chip))

	out := h.execChatTool(c, flusher, req, name, input)

	status := "ok"
	doneLabel := label
	// Surface a tool-level error (and a rejected proposal) as a failed step.
	var probe map[string]any
	if json.Unmarshal([]byte(out), &probe) == nil {
		if _, bad := probe["error"]; bad {
			status = "error"
		} else if s, _ := probe["status"].(string); s == "rejected" || s == "timeout" {
			status = "error"
		}
		// "Waiting for your approval" is only true while it's waiting — restate
		// the finished step as what actually happened.
		if name == "create_data_store" {
			m, _ := input.(map[string]any)
			suffix := ""
			if n, _ := m["name"].(string); n != "" {
				suffix = " · " + n
			}
			switch s, _ := probe["status"].(string); s {
			case "approved":
				doneLabel = "Created the data store" + suffix
			case "rejected":
				doneLabel = "Data store rejected" + suffix
			case "timeout":
				doneLabel = "Approval timed out" + suffix
			case "cancelled":
				doneLabel = "Approval cancelled" + suffix
			}
		}
	}
	res, _ := json.Marshal(map[string]string{"tool": name, "label": doneLabel, "status": status})
	sendSSE(c.Writer, flusher, "tool_result", string(res))
	return out
}

// execChatTool runs one builder tool call and returns its result JSON.
// create_workflow / update_workflow additionally stream their input to the
// client so the canvas updates immediately.
func (h *WorkflowHandler) execChatTool(c *gin.Context, flusher http.Flusher, req *aiGenerateRequest, name string, input any) string {
	switch name {
	case "get_available_nodes":
		return getAvailableNodesResult()

	case executor.ClockToolName:
		tz := ""
		if m, ok := input.(map[string]any); ok {
			tz, _ = m["timezone"].(string)
		}
		return executor.CurrentTime(tz)

	case "create_workflow":
		inputJSON, _ := json.Marshal(input)
		sendSSE(c.Writer, flusher, "workflow", string(inputJSON))
		// Keep the server's working copy in step with the canvas.
		if m, ok := input.(map[string]any); ok {
			if n, ok := m["nodes"].([]any); ok {
				req.CurrentNodes = n
			}
			if e, ok := m["edges"].([]any); ok {
				req.CurrentEdges = e
			}
		}
		return `{"status": "success", "message": "Workflow created on the canvas."}`

	case "get_current_workflow":
		workflowJSON, _ := json.Marshal(map[string]any{
			"nodes": req.CurrentNodes,
			"edges": req.CurrentEdges,
		})
		return string(workflowJSON)

	case "update_workflow":
		inputJSON, _ := json.Marshal(input)
		sendSSE(c.Writer, flusher, "patch", string(inputJSON))
		if m, ok := input.(map[string]any); ok {
			if ops, ok := m["operations"].([]any); ok {
				req.CurrentNodes, req.CurrentEdges = applyOpsToCanvas(req.CurrentNodes, req.CurrentEdges, ops)
			}
		}
		return `{"status":"success","message":"Patch applied to canvas."}`

	case "list_integration_resources":
		m, _ := input.(map[string]any)
		provider, _ := m["provider"].(string)
		return h.integrationResourcesForAI(currentOrgID(c), currentUserID(c), provider)

	case "list_data_stores":
		return h.dataStoresForAI(currentOrgID(c), req.WorkflowID)

	case "set_schedule":
		return h.setScheduleForAI(c, req.WorkflowID, input)

	case "list_runs":
		return h.listRunsForAI(c, req.WorkflowID)

	case "get_run_logs":
		return h.runLogsForAI(c, input)

	case "create_data_store":
		// Blocks this turn until the user decides. Nothing is created here —
		// accepting creates the store in ResolveDataStoreProposal and hands the
		// real id back below, so the model can wire it in the same turn.
		var spec storeProposalSpec
		if b, err := json.Marshal(input); err == nil {
			_ = json.Unmarshal(b, &spec)
		}
		if spec.Name == "" || !validKinds[spec.Kind] || !validScopes[spec.Scope] {
			return `{"error":"a data store proposal needs a name, a kind (kv|collection|text) and a scope (run|workflow|account)"}`
		}

		proposalID, ch := registerProposal(currentUserID(c), currentOrgID(c), req.WorkflowID, spec)
		defer clearProposal(proposalID)

		card, _ := json.Marshal(map[string]any{
			"proposalId": proposalID,
			"name":       spec.Name,
			"kind":       spec.Kind,
			"scope":      spec.Scope,
			"schema":     spec.Schema,
			"reason":     spec.Reason,
		})
		sendSSE(c.Writer, flusher, "data_store_proposal", string(card))
		slog.InfoContext(c.Request.Context(), "data store proposed, builder paused",
			"proposal_id", proposalID, "name", spec.Name, "kind", spec.Kind, "scope", spec.Scope)

		select {
		case out := <-ch:
			if out.Action == "approved" {
				slog.InfoContext(c.Request.Context(), "data store proposal approved",
					"proposal_id", proposalID, "store_id", out.StoreID)
				return fmt.Sprintf(`{"status":"approved","store_id":%q,"name":%q,"kind":%q,"scope":%q,"message":"The user approved it and the store now EXISTS. Use this store_id as dataStoreId on the data node and finish the workflow now."}`,
					out.StoreID, spec.Name, spec.Kind, spec.Scope)
			}
			slog.InfoContext(c.Request.Context(), "data store proposal rejected", "proposal_id", proposalID)
			note := out.Note
			if note == "" {
				note = "no reason given"
			}
			return fmt.Sprintf(`{"status":"rejected","note":%q,"message":"The user REJECTED this store — it was not created. Do not use it or propose it again. Build what you can without it and tell them plainly which part needs persistence."}`, note)

		case <-time.After(proposalWait):
			sendSSE(c.Writer, flusher, "data_store_timeout", proposalID)
			slog.WarnContext(c.Request.Context(), "data store proposal timed out", "proposal_id", proposalID)
			return `{"status":"timeout","message":"The user did not respond in time and the store was NOT created. Wrap up: build what you can without it and tell them they can ask again to set it up."}`

		case <-c.Request.Context().Done():
			slog.InfoContext(c.Request.Context(), "data store proposal abandoned (client gone)", "proposal_id", proposalID)
			return `{"status":"cancelled","message":"The user cancelled. Stop here."}`
		}

	default:
		return fmt.Sprintf(`{"error": "unknown tool: %s"}`, name)
	}
}

// cachedSystem renders a system prompt as two blocks: the static instructions,
// marked cacheable, then the clock.
//
// Anthropic hashes the prompt in the order tools → system → messages and
// caches everything up to the breakpoint, so marking the static block also
// covers the tool schemas sitting in front of it. The clock goes after the
// breakpoint: it changes every second, and anything past the breakpoint is
// re-read without invalidating what precedes it. Marked the other way round —
// or concatenated, as WithClockAndTool does — the prefix is unique per request
// and nothing caches at all.
//
// The saving is the whole builder prompt plus the integration catalogue, re-sent
// on every turn of a five-round tool loop.
func cachedSystem(static string) []map[string]any {
	return []map[string]any{
		{"type": "text", "text": static, "cache_control": map[string]any{"type": "ephemeral"}},
		{"type": "text", "text": executor.ClockBlock(true)},
	}
}

// cachedSystemMessages is cachedSystem for OpenAI-compatible providers, which
// have no cache_control: they cache the longest common prefix on their own, so
// the only thing that matters is that the first message stays byte-identical
// from turn to turn. Same two pieces, same order, one message each.
func cachedSystemMessages(static string) []map[string]any {
	return []map[string]any{
		{"role": "system", "content": static},
		{"role": "system", "content": executor.ClockBlock(true)},
	}
}

// runAnthropicChat drives the tool loop against the Anthropic Messages API.
// The returned bool is false when the loop ended on a request/stream error
// (instrumentation only — the client already got the SSE error event).
func (h *WorkflowHandler) runAnthropicChat(c *gin.Context, flusher http.Flusher, req *aiGenerateRequest, model chatModelSpec, apiKey string) bool {
	allTools := builderTools()

	// Build message history — prior turns as plain text role/content pairs
	var messages []map[string]any
	for _, h := range req.History {
		role, _ := h["role"].(string)
		content, _ := h["content"].(string)
		if (role == "user" || role == "assistant") && content != "" {
			messages = append(messages, map[string]any{"role": role, "content": content})
		}
	}
	messages = append(messages, map[string]any{"role": "user", "content": req.Prompt})

	// Multi-turn tool loop: keep going until the model stops calling tools
	for round := 0; round < 5; round++ {
		sendSSE(c.Writer, flusher, "thinking", statusForRound(round))

		body, _ := json.Marshal(map[string]any{
			"model":      model.ID,
			"max_tokens": 16000,
			"thinking":   model.Thinking,
			"stream":     true,
			"system":     cachedSystem(workflowSystemPrompt),
			"tools":      allTools,
			"messages":   messages,
		})

		resp, err := doAnthropicRequest(c, apiKey, body)
		if err != nil {
			sendSSE(c.Writer, flusher, "error", fmt.Sprintf("Request failed: %v", err))
			return false
		}

		stopReason, assistantContent, err := consumeStream(c, resp, flusher, model.ID)
		resp.Body.Close()
		if err != nil {
			sendSSE(c.Writer, flusher, "error", fmt.Sprintf("Stream error: %v", err))
			return false
		}

		// Append assistant message with thinking blocks intact — the API requires
		// them (with their signatures) to be passed back unchanged on tool-use turns.
		messages = append(messages, map[string]any{"role": "assistant", "content": assistantContent})

		if stopReason != "tool_use" {
			// Model is done — no more tool calls
			break
		}

		// Process tool calls and build results
		var toolResults []any
		for _, block := range assistantContent {
			bm, ok := block.(map[string]any)
			if !ok || bm["type"] != "tool_use" {
				continue
			}
			toolName, _ := bm["name"].(string)
			toolID, _ := bm["id"].(string)

			toolResults = append(toolResults, map[string]any{
				"type":        "tool_result",
				"tool_use_id": toolID,
				"content":     h.execChatToolWithActivity(c, flusher, req, toolName, bm["input"]),
			})
		}

		messages = append(messages, map[string]any{"role": "user", "content": toolResults})
	}
	return true
}

// runOpenAIChat drives the same tool loop against an OpenAI-compatible
// chat-completions endpoint (OpenAI, Gemini, xAI). The returned bool is false
// when the loop ended on a request/stream error (instrumentation only).
func (h *WorkflowHandler) runOpenAIChat(c *gin.Context, flusher http.Flusher, req *aiGenerateRequest, model chatModelSpec, apiKey, url string) bool {
	messages := cachedSystemMessages(workflowSystemPrompt)
	for _, h := range req.History {
		role, _ := h["role"].(string)
		content, _ := h["content"].(string)
		if (role == "user" || role == "assistant") && content != "" {
			messages = append(messages, map[string]any{"role": role, "content": content})
		}
	}
	messages = append(messages, map[string]any{"role": "user", "content": req.Prompt})

	for round := 0; round < 5; round++ {
		sendSSE(c.Writer, flusher, "thinking", statusForRound(round))

		body, _ := json.Marshal(map[string]any{
			"model":  model.ID,
			"stream": true,
			// Without include_usage a streamed response carries no token counts
			// at all, which would leave the whole builder surface unbillable.
			"stream_options": map[string]any{"include_usage": true},
			"messages":       messages,
			"tools":          openAIToolDefs(),
		})

		resp, err := doOpenAIRequest(c, url, apiKey, body)
		if err != nil {
			sendSSE(c.Writer, flusher, "error", fmt.Sprintf("Request failed: %v", err))
			return false
		}

		content, toolCalls, err := consumeOpenAIStream(c, resp, flusher, model.Provider, model.ID)
		resp.Body.Close()
		if err != nil {
			sendSSE(c.Writer, flusher, "error", fmt.Sprintf("Stream error: %v", err))
			return false
		}

		assistantMsg := map[string]any{"role": "assistant", "content": content}
		if len(toolCalls) > 0 {
			assistantMsg["tool_calls"] = toolCalls
		}
		messages = append(messages, assistantMsg)

		if len(toolCalls) == 0 {
			// Model is done — no more tool calls
			break
		}

		for _, tc := range toolCalls {
			fn, _ := tc["function"].(map[string]any)
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)

			var input any
			if err := json.Unmarshal([]byte(args), &input); err != nil {
				input = map[string]any{}
			}

			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": tc["id"],
				"content":      h.execChatToolWithActivity(c, flusher, req, name, input),
			})
		}
	}
	return true
}

// openAIToolDefs converts the Anthropic-format tool definitions to the
// OpenAI function-calling format.
func openAIToolDefs() []map[string]any {
	anthropicTools := builderTools()
	out := make([]map[string]any, 0, len(anthropicTools))
	for _, t := range anthropicTools {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t["name"],
				"description": t["description"],
				"parameters":  t["input_schema"],
			},
		})
	}
	return out
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

type streamEmitter func(eventType, data string)

// consumeStream is the HTTP/SSE adapter used by the builder chat.
func consumeStream(c *gin.Context, resp *http.Response, flusher http.Flusher, model string) (string, []any, error) {
	return consumeAnthropicStream(c.Request.Context(), resp, model, func(eventType, data string) {
		sendSSE(c.Writer, flusher, eventType, data)
	})
}

// consumeAnthropicStream reads the provider stream without depending on Gin
// or SSE. Callers decide how streamed thinking/text events are delivered.
func consumeAnthropicStream(ctx context.Context, resp *http.Response, model string, emit streamEmitter) (string, []any, error) {
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
		usage         telemetry.Usage
	)
	// Recorded on the way out so a stream that errors mid-flight still bills for
	// what it consumed.
	defer func() { telemetry.LLMTokens(ctx, "anthropic", model, usage) }()

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

		// message_start carries input counts, message_delta the output ones.
		if event.Message != nil {
			usage.InputTokens += event.Message.Usage.InputTokens
			usage.OutputTokens += event.Message.Usage.OutputTokens
			usage.CacheReadTokens += event.Message.Usage.CacheReadInputTokens
			usage.CacheWriteTokens += event.Message.Usage.CacheCreationInputTokens
		}
		if event.Usage != nil {
			usage.InputTokens += event.Usage.InputTokens
			usage.OutputTokens += event.Usage.OutputTokens
			usage.CacheReadTokens += event.Usage.CacheReadInputTokens
			usage.CacheWriteTokens += event.Usage.CacheCreationInputTokens
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
					currentBlock["signature"] = ""
				}
				if event.ContentBlock.Type == "redacted_thinking" {
					currentBlock["data"] = event.ContentBlock.Data
				}
				if event.ContentBlock.Type == "text" {
					currentBlock["text"] = ""
				}
			}

		case "content_block_delta":
			if event.Delta != nil && currentBlock != nil {
				switch event.Delta.Type {
				case "thinking_delta":
					if emit != nil {
						emit("thinking", event.Delta.Thinking)
					}
					currentBlock["thinking"] = currentBlock["thinking"].(string) + event.Delta.Thinking
				case "text_delta":
					if emit != nil {
						emit("text", event.Delta.Text)
					}
					currentBlock["text"] = currentBlock["text"].(string) + event.Delta.Text
				case "input_json_delta":
					toolInputBuf.WriteString(event.Delta.PartialJSON)
				case "signature_delta":
					currentBlock["signature"] = currentBlock["signature"].(string) + event.Delta.Signature
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

// consumeOpenAIStream is the HTTP/SSE adapter used by the builder chat.
func consumeOpenAIStream(c *gin.Context, resp *http.Response, flusher http.Flusher, provider, model string) (string, []map[string]any, error) {
	return consumeOpenAIProviderStream(c.Request.Context(), resp, provider, model, func(eventType, data string) {
		sendSSE(c.Writer, flusher, eventType, data)
	})
}

// consumeOpenAIProviderStream reads an OpenAI-compatible provider stream
// without depending on Gin or SSE.
func consumeOpenAIProviderStream(ctx context.Context, resp *http.Response, provider, model string, emit streamEmitter) (string, []map[string]any, error) {
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("provider %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		content string
		calls   []map[string]any // ordered tool calls
		byIndex = map[int]map[string]any{}
		usage   telemetry.Usage
	)
	defer func() { telemetry.LLMTokens(ctx, provider, model, usage) }()

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk openAIStreamChunk
		if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Usage != nil {
			// OpenAI reports prompt_tokens inclusive of cached ones, so the
			// cached count is subtracted out rather than billed at both rates.
			cached := chunk.Usage.PromptTokensDetails.CachedTokens
			input := chunk.Usage.PromptTokens - cached
			if input < 0 {
				input = 0
			}
			usage.InputTokens += input
			usage.OutputTokens += chunk.Usage.CompletionTokens
			usage.CacheReadTokens += cached
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil || len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		if delta.Reasoning != "" {
			if emit != nil {
				emit("thinking", delta.Reasoning)
			}
		}
		if delta.Content != "" {
			if emit != nil {
				emit("text", delta.Content)
			}
			content += delta.Content
		}
		for _, tc := range delta.ToolCalls {
			// Match an existing call by ID when given (Gemini repeats complete
			// entries without an index); otherwise by index (OpenAI streams
			// continuation deltas with an index but no ID).
			var call map[string]any
			if tc.ID != "" {
				for _, existing := range calls {
					if existing["id"] == tc.ID {
						call = existing
						break
					}
				}
			} else {
				call = byIndex[tc.Index]
			}
			if call == nil {
				call = map[string]any{
					"id":       tc.ID,
					"type":     "function",
					"function": map[string]any{"name": "", "arguments": ""},
				}
				byIndex[tc.Index] = call
				calls = append(calls, call)
			}
			fn := call["function"].(map[string]any)
			if tc.Function.Name != "" {
				fn["name"] = fn["name"].(string) + tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				fn["arguments"] = fn["arguments"].(string) + tc.Function.Arguments
			}
			// Gemini attaches thought signatures here and rejects follow-up
			// requests that don't echo them back on the assistant message.
			if len(tc.ExtraContent) > 0 {
				var ec any
				if json.Unmarshal(tc.ExtraContent, &ec) == nil {
					call["extra_content"] = ec
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return content, calls, fmt.Errorf("scanner: %w", err)
	}
	return content, calls, nil
}

func doOpenAIRequest(c *gin.Context, url, apiKey string, body []byte) (*http.Response, error) {
	return doOpenAIRequestContext(c.Request.Context(), url, apiKey, body)
}

func doOpenAIRequestContext(ctx context.Context, url, apiKey string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := anthropicClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	return resp, nil
}

func doAnthropicRequest(c *gin.Context, apiKey string, body []byte) (*http.Response, error) {
	return doAnthropicRequestContext(c.Request.Context(), apiKey, body)
}

func doAnthropicRequestContext(ctx context.Context, apiKey string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
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
	if max <= 0 {
		return "..."
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "..."
}

// ── OpenAI-compatible streaming types ───────────────────────────

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"` // xAI-style; absent elsewhere
			ToolCalls []struct {
				Index        int             `json:"index"`
				ID           string          `json:"id"`
				ExtraContent json.RawMessage `json:"extra_content"` // Gemini thought signatures
				Function     struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// The terminal chunk carries usage and NO choices, so it must be handled
	// before any guard that skips choice-less chunks. Note the field names differ
	// from Anthropic's: Chat Completions says prompt/completion, and nests the
	// cached count one level deeper.
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage,omitempty"`
}

// ── Anthropic streaming types ───────────────────────────────────

type streamEvent struct {
	Type         string       `json:"type"`
	ContentBlock *streamBlock `json:"content_block,omitempty"`
	Delta        *streamDelta `json:"delta,omitempty"`
	// Anthropic splits usage across two events: input counts arrive on
	// message_start, output counts on the terminal message_delta. Both have to
	// be read or the call looks free.
	Message *struct {
		Usage streamUsage `json:"usage"`
	} `json:"message,omitempty"`
	Usage *streamUsage `json:"usage,omitempty"`
}

type streamUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type streamBlock struct {
	Type string `json:"type"` // "thinking", "redacted_thinking", "text", "tool_use"
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Data string `json:"data,omitempty"` // redacted_thinking payload
}

type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}
