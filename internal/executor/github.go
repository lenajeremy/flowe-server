package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GitHub REST v3. Auth is Bearer + the vnd.github+json Accept header.

func githubCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("github request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			return "", fmt.Errorf("GitHub API error (%d): %s", resp.StatusCode, e.Message)
		}
		return "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return string(raw), nil
}

func runGithub(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	repo := substituteTemplates(d.GithubRepo, outputs)
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	switch d.IntegrationOp {
	case "create_issue":
		if repo == "" {
			return "", fmt.Errorf("githubRepo is required (owner/name)")
		}
		payload := map[string]any{
			"title": sub(d.GithubTitle),
			"body":  sub(d.GithubBody),
		}
		if labels := splitCSV(sub(d.GithubLabels)); len(labels) > 0 {
			payload["labels"] = labels
		}
		raw, err := githubCall(ctx, token, http.MethodPost, "/repos/"+repo+"/issues", payload)
		if err != nil {
			return "", err
		}
		return githubIssueResult(raw), nil

	case "list_issues":
		state := firstNonEmpty(d.GithubState, "open")
		q := url.Values{"state": {state}, "per_page": {fmt.Sprint(intOr(d.GithubLimit, 10))}}
		raw, err := githubCall(ctx, token, http.MethodGet, "/repos/"+repo+"/issues?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return githubProjectIssues(raw), nil

	case "create_comment":
		if _, err := githubCall(ctx, token, http.MethodPost,
			fmt.Sprintf("/repos/%s/issues/%s/comments", repo, sub(d.GithubIssueNumber)),
			map[string]any{"body": sub(d.GithubBody)}); err != nil {
			return "", err
		}
		return `{"status":"commented"}`, nil

	case "list_pull_requests":
		state := firstNonEmpty(d.GithubState, "open")
		q := url.Values{"state": {state}, "per_page": {fmt.Sprint(intOr(d.GithubLimit, 10))}}
		raw, err := githubCall(ctx, token, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return githubProjectIssues(raw), nil

	case "get_pull_request":
		return githubCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/repos/%s/pulls/%s", repo, sub(d.GithubPrNumber)), nil)

	default:
		return "", fmt.Errorf("unknown GitHub operation: %s", d.IntegrationOp)
	}
}

// githubIssueResult projects a created issue down to the useful fields.
func githubIssueResult(raw string) string {
	var iss struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"html_url"`
	}
	if json.Unmarshal([]byte(raw), &iss) != nil {
		return raw
	}
	b, _ := json.Marshal(map[string]any{"status": "created", "number": iss.Number, "title": iss.Title, "url": iss.URL})
	return string(b)
}

// githubProjectIssues trims a list of issues/PRs to signal fields.
func githubProjectIssues(raw string) string {
	var items []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		URL    string `json:"html_url"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if json.Unmarshal([]byte(raw), &items) != nil {
		return truncateStr(raw, 8000)
	}
	out := make([]map[string]any, 0, len(items))
	for _, i := range items {
		out = append(out, map[string]any{"number": i.Number, "title": i.Title, "state": i.State, "url": i.URL, "author": i.User.Login})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// ── small shared helpers (used across provider files) ──────────

func splitCSV(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func intOr(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}
