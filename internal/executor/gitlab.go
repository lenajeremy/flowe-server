package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// GitLab REST v4. Project ids are URL-encoded numeric ids.

func gitlabCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://gitlab.com/api/v4"+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("gitlab request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Message any    `json:"message"`
			Error   string `json:"error_description"`
		}
		if json.Unmarshal(raw, &e) == nil {
			if e.Error != "" {
				return "", fmt.Errorf("GitLab API error (%d): %s", resp.StatusCode, e.Error)
			}
			if e.Message != nil {
				return "", fmt.Errorf("GitLab API error (%d): %v", resp.StatusCode, e.Message)
			}
		}
		return "", fmt.Errorf("GitLab API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return string(raw), nil
}

func runGitlab(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	proj := substituteTemplates(d.GitlabProjectId, outputs)
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	if proj == "" && d.IntegrationOp != "" {
		return "", fmt.Errorf("gitlabProjectId is required")
	}
	base := "/projects/" + url.PathEscape(proj)

	switch d.IntegrationOp {
	case "create_issue":
		payload := map[string]any{"title": sub(d.GitlabTitle), "description": sub(d.GitlabDescription)}
		if labels := sub(d.GitlabLabels); labels != "" {
			payload["labels"] = labels
		}
		raw, err := gitlabCall(ctx, token, http.MethodPost, base+"/issues", payload)
		if err != nil {
			return "", err
		}
		return gitlabIssueResult(raw), nil

	case "list_issues":
		q := url.Values{"per_page": {fmt.Sprint(intOr(d.GitlabLimit, 10))}}
		if st := d.GitlabState; st != "" && st != "all" {
			q.Set("state", st)
		}
		raw, err := gitlabCall(ctx, token, http.MethodGet, base+"/issues?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return gitlabProjectItems(raw), nil

	case "create_comment":
		if _, err := gitlabCall(ctx, token, http.MethodPost,
			fmt.Sprintf("%s/issues/%s/notes", base, sub(d.GitlabIssueIid)),
			map[string]any{"body": sub(d.GitlabDescription)}); err != nil {
			return "", err
		}
		return `{"status":"commented"}`, nil

	case "list_merge_requests":
		q := url.Values{"per_page": {fmt.Sprint(intOr(d.GitlabLimit, 10))}}
		if st := d.GitlabState; st != "" && st != "all" {
			q.Set("state", st)
		}
		raw, err := gitlabCall(ctx, token, http.MethodGet, base+"/merge_requests?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return gitlabProjectItems(raw), nil

	case "get_merge_request":
		return gitlabCall(ctx, token, http.MethodGet,
			fmt.Sprintf("%s/merge_requests/%s", base, sub(d.GitlabMrIid)), nil)

	default:
		return "", fmt.Errorf("unknown GitLab operation: %s", d.IntegrationOp)
	}
}

func gitlabIssueResult(raw string) string {
	var iss struct {
		IID   int    `json:"iid"`
		Title string `json:"title"`
		URL   string `json:"web_url"`
	}
	if json.Unmarshal([]byte(raw), &iss) != nil {
		return raw
	}
	b, _ := json.Marshal(map[string]any{"status": "created", "iid": iss.IID, "title": iss.Title, "url": iss.URL})
	return string(b)
}

func gitlabProjectItems(raw string) string {
	var items []struct {
		IID    int    `json:"iid"`
		Title  string `json:"title"`
		State  string `json:"state"`
		URL    string `json:"web_url"`
		Author struct {
			Username string `json:"username"`
		} `json:"author"`
	}
	if json.Unmarshal([]byte(raw), &items) != nil {
		return truncateStr(raw, 8000)
	}
	out := make([]map[string]any, 0, len(items))
	for _, i := range items {
		out = append(out, map[string]any{"iid": i.IID, "title": i.Title, "state": i.State, "url": i.URL, "author": i.Author.Username})
	}
	b, _ := json.Marshal(out)
	return string(b)
}
