package n8n

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client calls n8n webhook endpoints and manages workflow seeding.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// CallWebhook POSTs payload to n8n at /webhook/<path> and returns the response body.
func (c *Client) CallWebhook(ctx context.Context, path string, payload map[string]any) (string, error) {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/webhook/"+path, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("n8n webhook %s returned %d: %s", path, resp.StatusCode, raw)
	}
	return string(raw), nil
}

// ── Workflow seeding ──────────────────────────────────────────

type n8nWorkflowListResp struct {
	Data []struct {
		Name string `json:"name"`
	} `json:"data"`
}

type n8nCreateResp struct {
	ID string `json:"id"`
}

// SeedWorkflows ensures pre-built webhook workflows exist in n8n.
func (c *Client) SeedWorkflows(ctx context.Context) error {
	existing, err := c.listWorkflowNames(ctx)
	if err != nil {
		return fmt.Errorf("list n8n workflows: %w", err)
	}

	for _, wf := range workflows() {
		if existing[wf.name] {
			continue
		}
		id, err := c.createWorkflow(ctx, wf.body)
		if err != nil {
			return fmt.Errorf("create workflow %s: %w", wf.name, err)
		}
		if err := c.activateWorkflow(ctx, id); err != nil {
			return fmt.Errorf("activate workflow %s: %w", wf.name, err)
		}
	}
	return nil
}

func (c *Client) listWorkflowNames(ctx context.Context) (map[string]bool, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/workflows", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r n8nWorkflowListResp
	_ = json.NewDecoder(resp.Body).Decode(&r)
	out := make(map[string]bool, len(r.Data))
	for _, w := range r.Data {
		out[w.Name] = true
	}
	return out, nil
}

func (c *Client) createWorkflow(ctx context.Context, body map[string]any) (string, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/workflows", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r n8nCreateResp
	_ = json.NewDecoder(resp.Body).Decode(&r)
	if r.ID == "" {
		return "", fmt.Errorf("n8n did not return workflow ID (status %d)", resp.StatusCode)
	}
	return r.ID, nil
}

func (c *Client) activateWorkflow(ctx context.Context, id string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPatch,
		c.baseURL+"/api/v1/workflows/"+id+"/activate", nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ── Workflow definitions ──────────────────────────────────────

type workflowDef struct {
	name string
	body map[string]any
}

func webhookToCode(name, webhookPath, jsCode string) map[string]any {
	return map[string]any{
		"name":   name,
		"active": true,
		"nodes": []map[string]any{
			{
				"id":          "webhook",
				"name":        "Webhook",
				"type":        "n8n-nodes-base.webhook",
				"typeVersion": 2,
				"position":    []int{240, 300},
				"parameters": map[string]any{
					"path":         webhookPath,
					"httpMethod":   "POST",
					"responseMode": "responseNode",
				},
			},
			{
				"id":          "code",
				"name":        "Code",
				"type":        "n8n-nodes-base.code",
				"typeVersion": 2,
				"position":    []int{460, 300},
				"parameters": map[string]any{
					"jsCode": jsCode,
				},
			},
			{
				"id":          "respond",
				"name":        "Respond to Webhook",
				"type":        "n8n-nodes-base.respondToWebhook",
				"typeVersion": 1,
				"position":    []int{680, 300},
				"parameters": map[string]any{
					"respondWith":  "json",
					"responseBody": "={{ JSON.stringify($json) }}",
				},
			},
		},
		"connections": map[string]any{
			"Webhook": map[string]any{
				"main": [][]map[string]any{
					{{"node": "Code", "type": "main", "index": 0}},
				},
			},
			"Code": map[string]any{
				"main": [][]map[string]any{
					{{"node": "Respond to Webhook", "type": "main", "index": 0}},
				},
			},
		},
	}
}

func workflows() []workflowDef {
	return []workflowDef{
		{
			name: "notion-create-page",
			body: webhookToCode("notion-create-page", "notion-create-page",
				"const b = $input.first().json;\n"+
					"const res = await fetch('https://api.notion.com/v1/pages', {\n"+
					"  method: 'POST',\n"+
					"  headers: { 'Authorization': 'Bearer ' + b.token, 'Notion-Version': '2022-06-28', 'Content-Type': 'application/json' },\n"+
					"  body: JSON.stringify({\n"+
					"    parent: { database_id: b.databaseId },\n"+
					"    properties: { title: { title: [{ text: { content: b.title || '' } }] } },\n"+
					"    children: b.content ? [{ object: 'block', type: 'paragraph', paragraph: { rich_text: [{ text: { content: b.content } }] } }] : []\n"+
					"  })\n"+
					"});\n"+
					"const data = await res.json();\n"+
					"return [{ json: data }];",
			),
		},
		{
			name: "notion-query-database",
			body: webhookToCode("notion-query-database", "notion-query-database",
				"const b = $input.first().json;\n"+
					"let filterObj;\n"+
					"try { filterObj = b.filter ? JSON.parse(b.filter) : undefined; } catch { filterObj = undefined; }\n"+
					"const res = await fetch('https://api.notion.com/v1/databases/' + b.databaseId + '/query', {\n"+
					"  method: 'POST',\n"+
					"  headers: { 'Authorization': 'Bearer ' + b.token, 'Notion-Version': '2022-06-28', 'Content-Type': 'application/json' },\n"+
					"  body: JSON.stringify(filterObj ? { filter: filterObj } : {})\n"+
					"});\n"+
					"const data = await res.json();\n"+
					"return [{ json: data }];",
			),
		},
		{
			name: "notion-append-blocks",
			body: webhookToCode("notion-append-blocks", "notion-append-blocks",
				"const b = $input.first().json;\n"+
					"const res = await fetch('https://api.notion.com/v1/blocks/' + b.pageId + '/children', {\n"+
					"  method: 'PATCH',\n"+
					"  headers: { 'Authorization': 'Bearer ' + b.token, 'Notion-Version': '2022-06-28', 'Content-Type': 'application/json' },\n"+
					"  body: JSON.stringify({ children: [{ object: 'block', type: 'paragraph', paragraph: { rich_text: [{ text: { content: b.content || '' } }] } }] })\n"+
					"});\n"+
					"const data = await res.json();\n"+
					"return [{ json: data }];",
			),
		},
		{
			name: "linear-create-issue",
			body: webhookToCode("linear-create-issue", "linear-create-issue",
				"const b = $input.first().json;\n"+
					"const query = 'mutation CreateIssue($teamId: String!, $title: String!, $description: String, $priority: Int) { issueCreate(input: { teamId: $teamId, title: $title, description: $description, priority: $priority }) { success issue { id title url } } }';\n"+
					"const res = await fetch('https://api.linear.app/graphql', {\n"+
					"  method: 'POST',\n"+
					"  headers: { 'Authorization': b.token, 'Content-Type': 'application/json' },\n"+
					"  body: JSON.stringify({ query, variables: { teamId: b.teamId, title: b.title, description: b.description, priority: b.priority || 0 } })\n"+
					"});\n"+
					"const data = await res.json();\n"+
					"return [{ json: data }];",
			),
		},
		{
			name: "linear-get-issues",
			body: webhookToCode("linear-get-issues", "linear-get-issues",
				"const b = $input.first().json;\n"+
					"const query = 'query GetIssues($teamId: String!, $first: Int) { team(id: $teamId) { issues(first: $first) { nodes { id title description state { name } priority url } } } }';\n"+
					"const res = await fetch('https://api.linear.app/graphql', {\n"+
					"  method: 'POST',\n"+
					"  headers: { 'Authorization': b.token, 'Content-Type': 'application/json' },\n"+
					"  body: JSON.stringify({ query, variables: { teamId: b.teamId, first: b.limit || 10 } })\n"+
					"});\n"+
					"const data = await res.json();\n"+
					"return [{ json: data }];",
			),
		},
		{
			name: "linear-create-comment",
			body: webhookToCode("linear-create-comment", "linear-create-comment",
				"const b = $input.first().json;\n"+
					"const query = 'mutation CreateComment($issueId: String!, $body: String!) { commentCreate(input: { issueId: $issueId, body: $body }) { success comment { id body } } }';\n"+
					"const res = await fetch('https://api.linear.app/graphql', {\n"+
					"  method: 'POST',\n"+
					"  headers: { 'Authorization': b.token, 'Content-Type': 'application/json' },\n"+
					"  body: JSON.stringify({ query, variables: { issueId: b.issueId, body: b.body } })\n"+
					"});\n"+
					"const data = await res.json();\n"+
					"return [{ json: data }];",
			),
		},
	}
}
