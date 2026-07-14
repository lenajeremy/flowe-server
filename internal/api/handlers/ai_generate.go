package handlers

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"workflow-ai/server/internal/auth"

	"github.com/gin-gonic/gin"
)

// ── Request type ────────────────────────────────────────────────

type aiGenerateRequest struct {
	Prompt       string `json:"prompt"`
	Model        string `json:"model,omitempty"`
	CurrentNodes []any  `json:"currentNodes,omitempty"`
	CurrentEdges []any  `json:"currentEdges,omitempty"`
	// History is the prior conversation as [{role, content}] pairs (user/assistant text only).
	History []map[string]any `json:"history,omitempty"`
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
	"description": "Lists the user's connected integrations and the concrete resources each exposes — Notion databases/pages, Linear teams/projects, GitHub repos, GitLab projects, Gmail labels, Stripe prices, Shopify products — with their IDs and names. ALWAYS call this before configuring an integration node so you can set real IDs (notionDatabaseId, linearTeamId, githubRepo, gitlabProjectId, stripePriceId, …) instead of placeholders. If a provider is not connected, leave the ID empty and tell the user to hit Connect in the node settings. Never ask the user to paste API tokens — OAuth connections are used automatically.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"enum":        []string{"notion", "linear", "github", "gitlab", "gmail", "googlecalendar", "googledrive", "googledocs", "googlesheets", "outlook", "slack", "stripe", "shopify"},
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
			"description": "Starts workflow on a recurring schedule.",
			"dataFields":  map[string]any{"interval": "string – '5m', '15m', '30m', '1h', '6h', '12h', '24h'"},
			"handles":     map[string]any{"inputs": []string{}, "outputs": []string{"source (right)"}},
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
			"description": "GitHub API: issues (create/get/update/list/search/comment), pull requests (create/merge/list/inspect/files), repo contents (read/commit files), branches, commits, releases, Actions workflows (trigger/list runs), list repos.",
			"dataFields":  map[string]any{"integrationOp": "'create_issue'|'get_issue'|'update_issue'|'list_issues'|'search_issues'|'create_comment'|'create_pull_request'|'merge_pull_request'|'list_pull_requests'|'get_pull_request'|'list_pr_files'|'list_commits'|'list_branches'|'get_file'|'create_or_update_file'|'list_releases'|'create_release'|'trigger_workflow'|'list_workflow_runs'|'list_repos'", "githubRepo": "string – 'owner/name', REAL value from list_integration_resources", "githubTitle": "string (templates ok; also PR/release title)", "githubBody": "string (templates ok; also trigger_workflow JSON inputs)", "githubIssueNumber": "string", "githubPrNumber": "string", "githubLabels": "string – comma-separated", "githubState": "'open'|'closed'|'all'", "githubBranch": "string – PR head / commit branch", "githubBase": "string – PR base (default main)", "githubMergeMethod": "'merge'|'squash'|'rebase'", "githubPath": "string – file path", "githubContent": "string – file content (templates ok)", "githubCommitMessage": "string", "githubRef": "string – branch/tag/sha for reads and workflow dispatch (default main)", "githubTag": "string – create_release tag", "githubWorkflowId": "string – workflow file name e.g. deploy.yml", "githubQuery": "string – search_issues query (GitHub search syntax)", "githubSince": "string – ISO 8601 time filter: list_commits since / list_issues updated after / list_workflow_runs created from", "githubUntil": "string – ISO 8601 time filter: list_commits until / list_workflow_runs created to", "githubLimit": "number (default 10)"},
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
			"type": "gmail", "label": "Gmail", "category": "Integrations",
			"description": "Gmail API: send/reply from the user's own address, search/list/read messages and threads, manage labels and read state, archive/trash, and work with drafts.",
			"dataFields":  map[string]any{"integrationOp": "'send_email'|'reply_to_message'|'list_messages'|'get_message'|'get_thread'|'create_draft'|'list_drafts'|'send_draft'|'list_labels'|'create_label'|'add_label'|'remove_label'|'mark_read'|'mark_unread'|'archive_message'|'trash_message'", "gmailTo": "string (templates ok; reply_to_message defaults to the original sender)", "gmailCc": "string", "gmailSubject": "string (templates ok)", "gmailBody": "string (templates ok)", "gmailQuery": "string – Gmail search syntax e.g. 'is:unread newer_than:1d'", "gmailMessageId": "string – target message (get/reply/label/read-state/archive/trash)", "gmailThreadId": "string – for get_thread", "gmailLabelId": "string – label id from list_labels (add_label/remove_label)", "gmailLabelName": "string – for create_label", "gmailDraftId": "string – for send_draft", "gmailLimit": "number (default 10)"},
			"auth":        "OAuth connection used automatically — never set integrationToken. Prefer gmail over the generic emailSend node when the user wants mail sent from their own Gmail address or wants to read their inbox.",
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
			"type": "googlecalendar", "label": "Google Calendar", "category": "Integrations",
			"description": "Google Calendar API: list/get/create/update/delete events, natural-language quick add, list calendars, find free/busy windows, respond to invitations.",
			"dataFields":  map[string]any{"integrationOp": "'list_events'|'get_event'|'create_event'|'update_event'|'delete_event'|'quick_add'|'list_calendars'|'find_free_time'|'respond_to_event'", "gcalCalendarId": "string – calendar id from list_integration_resources (default 'primary')", "gcalEventId": "string – target event (get/update/delete/respond)", "gcalSummary": "string – event title (templates ok)", "gcalDescription": "string (templates ok)", "gcalStart": "string – RFC3339 e.g. 2026-07-20T15:00:00Z (also find_free_time window start)", "gcalEnd": "string – RFC3339 (also find_free_time window end)", "gcalAttendees": "string – comma-separated emails", "gcalText": "string – natural language for quick_add e.g. 'Lunch with Sam Friday 1pm'", "gcalResponse": "'accepted'|'declined'|'tentative' for respond_to_event", "gcalLimit": "number (default 10)"},
			"auth":        "OAuth connection used automatically — never set integrationToken.",
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

Integrations (notion, linear, github, gitlab, gmail, stripe, shopify):
- Auth is handled by OAuth connections — NEVER set integrationToken and never ask the user for API keys.
- Before placing or editing an integration node, call list_integration_resources and use the REAL resource IDs (notionDatabaseId, notionPageId, linearTeamId, linearProjectId, githubRepo, gitlabProjectId, stripePriceId) from the response. Mention the resource by name when you explain the workflow.
- If the provider is not connected, still build the node but leave the resource ID empty and tell the user to click Connect in the node's settings panel, then ask you to fill in the target resource.
- Prefer the gmail node over emailSend when the user wants mail sent from their own address or wants to read/search their inbox.`

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

	model := resolveChatModel(req.Model)
	prov := chatProviders[model.Provider]
	apiKey := os.Getenv(prov.KeyEnv)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": prov.KeyEnv + " not configured on server"})
		return
	}

	slog.Info("ai generate", "requested", req.Model, "model", model.ID, "provider", model.Provider)

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

	if model.Provider == "anthropic" {
		h.runAnthropicChat(c, flusher, &req, model, apiKey)
	} else {
		h.runOpenAIChat(c, flusher, &req, model, apiKey, prov.URL)
	}

	sendSSE(c.Writer, flusher, "done", "")
}

// execChatTool runs one builder tool call and returns its result JSON.
// create_workflow / update_workflow additionally stream their input to the
// client so the canvas updates immediately.
func (h *WorkflowHandler) execChatTool(c *gin.Context, flusher http.Flusher, req *aiGenerateRequest, name string, input any) string {
	switch name {
	case "get_available_nodes":
		return getAvailableNodesResult()

	case "create_workflow":
		inputJSON, _ := json.Marshal(input)
		sendSSE(c.Writer, flusher, "workflow", string(inputJSON))
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
		return `{"status":"success","message":"Patch applied to canvas."}`

	case "list_integration_resources":
		m, _ := input.(map[string]any)
		provider, _ := m["provider"].(string)
		return h.integrationResourcesForAI(currentUserID(c), provider)

	default:
		return fmt.Sprintf(`{"error": "unknown tool: %s"}`, name)
	}
}

// runAnthropicChat drives the tool loop against the Anthropic Messages API.
func (h *WorkflowHandler) runAnthropicChat(c *gin.Context, flusher http.Flusher, req *aiGenerateRequest, model chatModelSpec, apiKey string) {
	allTools := []map[string]any{toolGetNodes, toolGetCurrentWorkflow, toolCreateWorkflow, toolUpdateWorkflow, toolListIntegrationResources}

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
			"system":     workflowSystemPrompt,
			"tools":      allTools,
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
				"content":     h.execChatTool(c, flusher, req, toolName, bm["input"]),
			})
		}

		messages = append(messages, map[string]any{"role": "user", "content": toolResults})
	}
}

// runOpenAIChat drives the same tool loop against an OpenAI-compatible
// chat-completions endpoint (OpenAI, Gemini, xAI).
func (h *WorkflowHandler) runOpenAIChat(c *gin.Context, flusher http.Flusher, req *aiGenerateRequest, model chatModelSpec, apiKey, url string) {
	messages := []map[string]any{{"role": "system", "content": workflowSystemPrompt}}
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
			"model":    model.ID,
			"stream":   true,
			"messages": messages,
			"tools":    openAIToolDefs(),
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
				"content":      h.execChatTool(c, flusher, req, name, input),
			})
		}
	}
}

// openAIToolDefs converts the Anthropic-format tool definitions to the
// OpenAI function-calling format.
func openAIToolDefs() []map[string]any {
	anthropicTools := []map[string]any{toolGetNodes, toolGetCurrentWorkflow, toolCreateWorkflow, toolUpdateWorkflow, toolListIntegrationResources}
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
					sendSSE(c.Writer, flusher, "thinking", event.Delta.Thinking)
					currentBlock["thinking"] = currentBlock["thinking"].(string) + event.Delta.Thinking
				case "text_delta":
					sendSSE(c.Writer, flusher, "text", event.Delta.Text)
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

// consumeOpenAIStream reads an OpenAI-compatible SSE stream, forwards text
// (and reasoning, when the provider exposes it) to the client, and returns
// the accumulated assistant text plus any tool calls.
func consumeOpenAIStream(c *gin.Context, resp *http.Response, flusher http.Flusher) (string, []map[string]any, error) {
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

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil || len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		if delta.Reasoning != "" {
			sendSSE(c.Writer, flusher, "thinking", delta.Reasoning)
		}
		if delta.Content != "" {
			sendSSE(c.Writer, flusher, "text", delta.Content)
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
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, bytes.NewReader(body))
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
}

// ── Anthropic streaming types ───────────────────────────────────

type streamEvent struct {
	Type         string       `json:"type"`
	ContentBlock *streamBlock `json:"content_block,omitempty"`
	Delta        *streamDelta `json:"delta,omitempty"`
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
