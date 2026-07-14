package executor

import (
	"bytes"
	"context"
	"encoding/base64"
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
		if s := sub(d.GithubSince); s != "" {
			q.Set("since", s) // GitHub issues API: updated at or after
		}
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

	case "get_issue":
		return githubCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/repos/%s/issues/%s", repo, sub(d.GithubIssueNumber)), nil)

	case "update_issue":
		payload := map[string]any{}
		if v := sub(d.GithubTitle); v != "" {
			payload["title"] = v
		}
		if v := sub(d.GithubBody); v != "" {
			payload["body"] = v
		}
		if v := d.GithubState; v == "open" || v == "closed" {
			payload["state"] = v
		}
		if labels := splitCSV(sub(d.GithubLabels)); len(labels) > 0 {
			payload["labels"] = labels
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("GitHub: nothing to update — set a title, body, state, or labels")
		}
		raw, err := githubCall(ctx, token, http.MethodPatch,
			fmt.Sprintf("/repos/%s/issues/%s", repo, sub(d.GithubIssueNumber)), payload)
		if err != nil {
			return "", err
		}
		return githubIssueResult(raw), nil

	case "create_pull_request":
		raw, err := githubCall(ctx, token, http.MethodPost, "/repos/"+repo+"/pulls", map[string]any{
			"title": sub(d.GithubTitle),
			"body":  sub(d.GithubBody),
			"head":  sub(d.GithubBranch),
			"base":  firstNonEmpty(sub(d.GithubBase), "main"),
		})
		if err != nil {
			return "", err
		}
		return githubIssueResult(raw), nil

	case "merge_pull_request":
		method := firstNonEmpty(d.GithubMergeMethod, "merge")
		raw, err := githubCall(ctx, token, http.MethodPut,
			fmt.Sprintf("/repos/%s/pulls/%s/merge", repo, sub(d.GithubPrNumber)),
			map[string]any{"merge_method": method})
		if err != nil {
			return "", err
		}
		var res struct {
			Merged bool   `json:"merged"`
			SHA    string `json:"sha"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		b, _ := json.Marshal(map[string]any{"merged": res.Merged, "sha": res.SHA, "method": method})
		return string(b), nil

	case "list_pr_files":
		raw, err := githubCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/repos/%s/pulls/%s/files?per_page=%d", repo, sub(d.GithubPrNumber), intOr(d.GithubLimit, 30)), nil)
		if err != nil {
			return "", err
		}
		var files []struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		}
		if json.Unmarshal([]byte(raw), &files) != nil {
			return truncateStr(raw, 8000), nil
		}
		out := make([]map[string]any, 0, len(files))
		for _, f := range files {
			out = append(out, map[string]any{"file": f.Filename, "status": f.Status, "additions": f.Additions, "deletions": f.Deletions})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "list_commits":
		q := url.Values{"per_page": {fmt.Sprint(intOr(d.GithubLimit, 10))}}
		if ref := sub(d.GithubRef); ref != "" {
			q.Set("sha", ref)
		}
		if s := sub(d.GithubSince); s != "" {
			q.Set("since", s)
		}
		if u := sub(d.GithubUntil); u != "" {
			q.Set("until", u)
		}
		raw, err := githubCall(ctx, token, http.MethodGet, "/repos/"+repo+"/commits?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var commits []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Name string `json:"name"`
					Date string `json:"date"`
				} `json:"author"`
			} `json:"commit"`
		}
		if json.Unmarshal([]byte(raw), &commits) != nil {
			return truncateStr(raw, 8000), nil
		}
		out := make([]map[string]any, 0, len(commits))
		for _, c := range commits {
			out = append(out, map[string]any{"sha": c.SHA[:min(12, len(c.SHA))], "message": truncateStr(c.Commit.Message, 200), "author": c.Commit.Author.Name, "date": c.Commit.Author.Date})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "list_branches":
		raw, err := githubCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/repos/%s/branches?per_page=%d", repo, intOr(d.GithubLimit, 30)), nil)
		if err != nil {
			return "", err
		}
		var branches []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(raw), &branches) != nil {
			return truncateStr(raw, 8000), nil
		}
		names := make([]string, 0, len(branches))
		for _, br := range branches {
			names = append(names, br.Name)
		}
		b, _ := json.Marshal(names)
		return string(b), nil

	case "get_file":
		q := url.Values{}
		if ref := sub(d.GithubRef); ref != "" {
			q.Set("ref", ref)
		}
		raw, err := githubCall(ctx, token, http.MethodGet,
			"/repos/"+repo+"/contents/"+sub(d.GithubPath)+"?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var f struct {
			Name    string `json:"name"`
			Content string `json:"content"`
			SHA     string `json:"sha"`
		}
		_ = json.Unmarshal([]byte(raw), &f)
		dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
		if err != nil {
			return "", fmt.Errorf("GitHub: could not decode file content: %w", err)
		}
		b, _ := json.Marshal(map[string]any{"name": f.Name, "sha": f.SHA, "content": truncateStr(string(dec), 1<<20)})
		return string(b), nil

	case "create_or_update_file":
		path := sub(d.GithubPath)
		payload := map[string]any{
			"message": firstNonEmpty(sub(d.GithubCommitMsg), "Update "+path),
			"content": base64.StdEncoding.EncodeToString([]byte(sub(d.GithubContent))),
		}
		if branch := sub(d.GithubBranch); branch != "" {
			payload["branch"] = branch
		}
		// If the file already exists we must pass its blob sha.
		q := url.Values{}
		if branch := sub(d.GithubBranch); branch != "" {
			q.Set("ref", branch)
		}
		if existing, err := githubCall(ctx, token, http.MethodGet,
			"/repos/"+repo+"/contents/"+path+"?"+q.Encode(), nil); err == nil {
			var f struct {
				SHA string `json:"sha"`
			}
			if json.Unmarshal([]byte(existing), &f) == nil && f.SHA != "" {
				payload["sha"] = f.SHA
			}
		}
		raw, err := githubCall(ctx, token, http.MethodPut, "/repos/"+repo+"/contents/"+path, payload)
		if err != nil {
			return "", err
		}
		var res struct {
			Commit struct {
				SHA     string `json:"sha"`
				HTMLURL string `json:"html_url"`
			} `json:"commit"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		b, _ := json.Marshal(map[string]any{"status": "committed", "sha": res.Commit.SHA, "url": res.Commit.HTMLURL})
		return string(b), nil

	case "list_releases":
		raw, err := githubCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/repos/%s/releases?per_page=%d", repo, intOr(d.GithubLimit, 10)), nil)
		if err != nil {
			return "", err
		}
		var rels []struct {
			TagName string `json:"tag_name"`
			Name    string `json:"name"`
			URL     string `json:"html_url"`
			Draft   bool   `json:"draft"`
		}
		if json.Unmarshal([]byte(raw), &rels) != nil {
			return truncateStr(raw, 8000), nil
		}
		out := make([]map[string]any, 0, len(rels))
		for _, r := range rels {
			out = append(out, map[string]any{"tag": r.TagName, "name": r.Name, "url": r.URL, "draft": r.Draft})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "create_release":
		raw, err := githubCall(ctx, token, http.MethodPost, "/repos/"+repo+"/releases", map[string]any{
			"tag_name": sub(d.GithubTag),
			"name":     firstNonEmpty(sub(d.GithubTitle), sub(d.GithubTag)),
			"body":     sub(d.GithubBody),
		})
		if err != nil {
			return "", err
		}
		var rel struct {
			TagName string `json:"tag_name"`
			URL     string `json:"html_url"`
		}
		_ = json.Unmarshal([]byte(raw), &rel)
		b, _ := json.Marshal(map[string]any{"status": "released", "tag": rel.TagName, "url": rel.URL})
		return string(b), nil

	case "trigger_workflow":
		payload := map[string]any{"ref": firstNonEmpty(sub(d.GithubRef), "main")}
		if inputs := sub(d.GithubBody); inputs != "" {
			var m map[string]any
			if err := json.Unmarshal([]byte(inputs), &m); err != nil {
				return "", fmt.Errorf("GitHub: workflow inputs must be a JSON object: %w", err)
			}
			payload["inputs"] = m
		}
		if _, err := githubCall(ctx, token, http.MethodPost,
			fmt.Sprintf("/repos/%s/actions/workflows/%s/dispatches", repo, sub(d.GithubWorkflowId)), payload); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": "dispatched", "workflow": sub(d.GithubWorkflowId), "ref": payload["ref"]})
		return string(b), nil

	case "list_workflow_runs":
		q := url.Values{"per_page": {fmt.Sprint(intOr(d.GithubLimit, 10))}}
		// Actions API takes a date range: "from..to", ">=from", or "<=to"
		switch s, u := sub(d.GithubSince), sub(d.GithubUntil); {
		case s != "" && u != "":
			q.Set("created", s+".."+u)
		case s != "":
			q.Set("created", ">="+s)
		case u != "":
			q.Set("created", "<="+u)
		}
		raw, err := githubCall(ctx, token, http.MethodGet,
			"/repos/"+repo+"/actions/runs?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var res struct {
			WorkflowRuns []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				Branch     string `json:"head_branch"`
				URL        string `json:"html_url"`
			} `json:"workflow_runs"`
		}
		if json.Unmarshal([]byte(raw), &res) != nil {
			return truncateStr(raw, 8000), nil
		}
		out := make([]map[string]any, 0, len(res.WorkflowRuns))
		for _, r := range res.WorkflowRuns {
			out = append(out, map[string]any{"id": r.ID, "name": r.Name, "status": r.Status, "conclusion": r.Conclusion, "branch": r.Branch, "url": r.URL})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "search_issues":
		q := url.Values{"q": {sub(d.GithubQuery)}, "per_page": {fmt.Sprint(intOr(d.GithubLimit, 10))}}
		raw, err := githubCall(ctx, token, http.MethodGet, "/search/issues?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Items json.RawMessage `json:"items"`
		}
		if json.Unmarshal([]byte(raw), &res) != nil {
			return truncateStr(raw, 8000), nil
		}
		return githubProjectIssues(string(res.Items)), nil

	case "list_repos":
		q := url.Values{"per_page": {fmt.Sprint(intOr(d.GithubLimit, 30))}, "sort": {"pushed"}}
		raw, err := githubCall(ctx, token, http.MethodGet, "/user/repos?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var repos []struct {
			FullName string `json:"full_name"`
			Private  bool   `json:"private"`
			URL      string `json:"html_url"`
		}
		if json.Unmarshal([]byte(raw), &repos) != nil {
			return truncateStr(raw, 8000), nil
		}
		out := make([]map[string]any, 0, len(repos))
		for _, r := range repos {
			out = append(out, map[string]any{"repo": r.FullName, "private": r.Private, "url": r.URL})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

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
