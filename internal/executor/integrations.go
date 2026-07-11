package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Native Notion / Linear API calls. These replaced an n8n sidecar that only
// wrapped the same HTTP requests in webhook->Code workflows.

var integrationHTTP = &http.Client{Timeout: 30 * time.Second}

// notionCall performs an authenticated Notion API request and surfaces
// Notion's {object:"error"} payloads as errors.
func notionCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("encode notion request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.notion.com/v1"+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Notion-Version", "2022-06-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("notion request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var errBody struct {
		Object  string `json:"object"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &errBody) == nil && errBody.Object == "error" {
		return "", fmt.Errorf("Notion API error (%s): %s", errBody.Code, errBody.Message)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Notion API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return string(raw), nil
}

func notionCreatePage(ctx context.Context, token, databaseID, title, content string) (string, error) {
	children := []any{}
	if content != "" {
		children = append(children, paragraphBlock(content))
	}
	return notionCall(ctx, token, http.MethodPost, "/pages", map[string]any{
		"parent": map[string]any{"database_id": databaseID},
		"properties": map[string]any{
			"title": map[string]any{
				"title": []any{map[string]any{"text": map[string]any{"content": title}}},
			},
		},
		"children": children,
	})
}

func notionQueryDatabase(ctx context.Context, token, databaseID, filter string) (string, error) {
	body := map[string]any{}
	if filter != "" {
		var filterObj any
		if err := json.Unmarshal([]byte(filter), &filterObj); err == nil {
			body["filter"] = filterObj
		}
	}
	return notionCall(ctx, token, http.MethodPost, "/databases/"+databaseID+"/query", body)
}

func notionAppendBlocks(ctx context.Context, token, pageID, content string) (string, error) {
	return notionCall(ctx, token, http.MethodPatch, "/blocks/"+pageID+"/children", map[string]any{
		"children": []any{paragraphBlock(content)},
	})
}

func notionUpdatePage(ctx context.Context, token, pageID, propertiesJSON string) (string, error) {
	var props any
	if err := json.Unmarshal([]byte(propertiesJSON), &props); err != nil {
		return "", fmt.Errorf("notionProperties must be a JSON object: %w", err)
	}
	return notionCall(ctx, token, http.MethodPatch, "/pages/"+pageID, map[string]any{"properties": props})
}

func notionSearch(ctx context.Context, token, query string) (string, error) {
	return notionCall(ctx, token, http.MethodPost, "/search", map[string]any{"query": query, "page_size": 25})
}

func notionAddComment(ctx context.Context, token, pageID, content string) (string, error) {
	return notionCall(ctx, token, http.MethodPost, "/comments", map[string]any{
		"parent":    map[string]any{"page_id": pageID},
		"rich_text": []any{map[string]any{"text": map[string]any{"content": content}}},
	})
}

// notionGetPageContent fetches a page's blocks and returns a readable text
// rendering of paragraph/heading/list/todo content alongside the raw JSON.
func notionGetPageContent(ctx context.Context, token, pageID string) (string, error) {
	raw, err := notionCall(ctx, token, http.MethodGet, "/blocks/"+pageID+"/children?page_size=100", nil)
	if err != nil {
		return "", err
	}
	var res struct {
		Results []struct {
			Type      string `json:"type"`
			Paragraph *struct {
				RichText []struct {
					PlainText string `json:"plain_text"`
				} `json:"rich_text"`
			} `json:"paragraph"`
			Heading1 *notionRichHolder `json:"heading_1"`
			Heading2 *notionRichHolder `json:"heading_2"`
			Heading3 *notionRichHolder `json:"heading_3"`
			Bulleted *notionRichHolder `json:"bulleted_list_item"`
			Numbered *notionRichHolder `json:"numbered_list_item"`
			ToDo     *notionRichHolder `json:"to_do"`
			Quote    *notionRichHolder `json:"quote"`
			Callout  *notionRichHolder `json:"callout"`
		} `json:"results"`
	}
	if json.Unmarshal([]byte(raw), &res) != nil {
		return raw, nil // fall back to raw JSON if the shape surprises us
	}
	var text strings.Builder
	appendRich := func(prefix string, h *notionRichHolder) {
		if h == nil {
			return
		}
		line := prefix
		for _, t := range h.RichText {
			line += t.PlainText
		}
		if strings.TrimSpace(line) != prefix {
			text.WriteString(line + "\n")
		}
	}
	for _, b := range res.Results {
		switch b.Type {
		case "paragraph":
			if b.Paragraph != nil {
				line := ""
				for _, t := range b.Paragraph.RichText {
					line += t.PlainText
				}
				if line != "" {
					text.WriteString(line + "\n")
				}
			}
		case "heading_1":
			appendRich("# ", b.Heading1)
		case "heading_2":
			appendRich("## ", b.Heading2)
		case "heading_3":
			appendRich("### ", b.Heading3)
		case "bulleted_list_item":
			appendRich("• ", b.Bulleted)
		case "numbered_list_item":
			appendRich("- ", b.Numbered)
		case "to_do":
			appendRich("[ ] ", b.ToDo)
		case "quote":
			appendRich("> ", b.Quote)
		case "callout":
			appendRich("", b.Callout)
		}
	}
	return fmt.Sprintf(`{"text":%s,"raw":%s}`, jsonString(text.String()), raw), nil
}

type notionRichHolder struct {
	RichText []struct {
		PlainText string `json:"plain_text"`
	} `json:"rich_text"`
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func paragraphBlock(content string) map[string]any {
	return map[string]any{
		"object": "block",
		"type":   "paragraph",
		"paragraph": map[string]any{
			"rich_text": []any{map[string]any{"text": map[string]any{"content": content}}},
		},
	}
}

// linearCall performs a Linear GraphQL request and surfaces GraphQL errors.
// Personal API keys (lin_api_*) are passed raw; OAuth tokens need Bearer.
func linearCall(ctx context.Context, token, query string, variables map[string]any) (string, error) {
	b, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return "", fmt.Errorf("encode linear request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.linear.app/graphql", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	auth := token
	if !strings.HasPrefix(token, "lin_api_") {
		auth = "Bearer " + token
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("linear request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var gql struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &gql) == nil && len(gql.Errors) > 0 {
		return "", fmt.Errorf("Linear API error: %s", gql.Errors[0].Message)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Linear API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return string(raw), nil
}

func linearCreateIssue(ctx context.Context, token, teamID, title, description string, priority any) (string, error) {
	const query = `mutation CreateIssue($teamId: String!, $title: String!, $description: String, $priority: Int) {
		issueCreate(input: { teamId: $teamId, title: $title, description: $description, priority: $priority }) {
			success issue { id title url }
		}
	}`
	return linearCall(ctx, token, query, map[string]any{
		"teamId": teamID, "title": title, "description": description, "priority": toInt(priority, 0),
	})
}

func linearGetIssues(ctx context.Context, token, teamID string, limit any) (string, error) {
	first := toInt(limit, 10)
	if teamID == "" {
		const query = `query GetIssues($first: Int) {
			issues(first: $first) { nodes { id title description state { name } priority url } }
		}`
		return linearCall(ctx, token, query, map[string]any{"first": first})
	}
	const query = `query GetIssues($teamId: String!, $first: Int) {
		team(id: $teamId) { issues(first: $first) { nodes { id title description state { name } priority url } } }
	}`
	return linearCall(ctx, token, query, map[string]any{"teamId": teamID, "first": first})
}

func linearCreateComment(ctx context.Context, token, issueID, body string) (string, error) {
	const query = `mutation CreateComment($issueId: String!, $body: String!) {
		commentCreate(input: { issueId: $issueId, body: $body }) { success comment { id body } }
	}`
	return linearCall(ctx, token, query, map[string]any{"issueId": issueID, "body": body})
}

type linearUpdateInput struct {
	Title       string
	Description string
	Priority    int
	StateID     string
	AssigneeID  string
	ProjectID   string
}

// linearUpdateIssue updates only the fields the caller provided (empty strings
// are omitted so a partial edit doesn't blank out other fields).
func linearUpdateIssue(ctx context.Context, token, issueID string, in linearUpdateInput) (string, error) {
	input := map[string]any{}
	if in.Title != "" {
		input["title"] = in.Title
	}
	if in.Description != "" {
		input["description"] = in.Description
	}
	if in.Priority > 0 {
		input["priority"] = in.Priority
	}
	if in.StateID != "" {
		input["stateId"] = in.StateID
	}
	if in.AssigneeID != "" {
		input["assigneeId"] = in.AssigneeID
	}
	if in.ProjectID != "" {
		input["projectId"] = in.ProjectID
	}
	const query = `mutation UpdateIssue($id: String!, $input: IssueUpdateInput!) {
		issueUpdate(id: $id, input: $input) { success issue { id identifier title url } }
	}`
	return linearCall(ctx, token, query, map[string]any{"id": issueID, "input": input})
}

func linearSearchIssues(ctx context.Context, token, query string, limit any) (string, error) {
	const q = `query SearchIssues($term: String!, $first: Int) {
		searchIssues(term: $term, first: $first) {
			nodes { id identifier title description state { name } priority url }
		}
	}`
	return linearCall(ctx, token, q, map[string]any{"term": query, "first": toInt(limit, 10)})
}

func linearListProjects(ctx context.Context, token string) (string, error) {
	const q = `query Projects { projects(first: 50) { nodes { id name description state } } }`
	return linearCall(ctx, token, q, map[string]any{})
}

func linearGetIssue(ctx context.Context, token, issueID string) (string, error) {
	const q = `query GetIssue($id: String!) {
		issue(id: $id) {
			id identifier title description priority url
			state { name }
			assignee { name }
			comments { nodes { body user { name } } }
		}
	}`
	return linearCall(ctx, token, q, map[string]any{"id": issueID})
}

func toInt(v any, fallback int) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case string:
		var out int
		if _, err := fmt.Sscanf(n, "%d", &out); err == nil {
			return out
		}
	}
	return fallback
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
