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
		if s := sub(d.GitlabSince); s != "" {
			q.Set("created_after", s)
		}
		if u := sub(d.GitlabUntil); u != "" {
			q.Set("created_before", u)
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
		if s := sub(d.GitlabSince); s != "" {
			q.Set("created_after", s)
		}
		if u := sub(d.GitlabUntil); u != "" {
			q.Set("created_before", u)
		}
		raw, err := gitlabCall(ctx, token, http.MethodGet, base+"/merge_requests?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return gitlabProjectItems(raw), nil

	case "get_merge_request":
		return gitlabCall(ctx, token, http.MethodGet,
			fmt.Sprintf("%s/merge_requests/%s", base, sub(d.GitlabMrIid)), nil)

	case "get_issue":
		return gitlabCall(ctx, token, http.MethodGet,
			fmt.Sprintf("%s/issues/%s", base, sub(d.GitlabIssueIid)), nil)

	case "update_issue":
		payload := map[string]any{}
		if v := sub(d.GitlabTitle); v != "" {
			payload["title"] = v
		}
		if v := sub(d.GitlabDescription); v != "" {
			payload["description"] = v
		}
		if v := d.GitlabStateEvent; v == "close" || v == "reopen" {
			payload["state_event"] = v
		}
		if labels := sub(d.GitlabLabels); labels != "" {
			payload["labels"] = labels
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("GitLab: nothing to update — set a title, description, state, or labels")
		}
		raw, err := gitlabCall(ctx, token, http.MethodPut,
			fmt.Sprintf("%s/issues/%s", base, sub(d.GitlabIssueIid)), payload)
		if err != nil {
			return "", err
		}
		return gitlabIssueResult(raw), nil

	case "create_merge_request":
		raw, err := gitlabCall(ctx, token, http.MethodPost, base+"/merge_requests", map[string]any{
			"title":         sub(d.GitlabTitle),
			"description":   sub(d.GitlabDescription),
			"source_branch": sub(d.GitlabSourceBranch),
			"target_branch": firstNonEmpty(sub(d.GitlabTargetBranch), "main"),
		})
		if err != nil {
			return "", err
		}
		return gitlabIssueResult(raw), nil

	case "merge_mr":
		raw, err := gitlabCall(ctx, token, http.MethodPut,
			fmt.Sprintf("%s/merge_requests/%s/merge", base, sub(d.GitlabMrIid)), map[string]any{})
		if err != nil {
			return "", err
		}
		var res struct {
			State string `json:"state"`
			SHA   string `json:"merge_commit_sha"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		b, _ := json.Marshal(map[string]any{"status": res.State, "sha": res.SHA})
		return string(b), nil

	case "list_branches":
		raw, err := gitlabCall(ctx, token, http.MethodGet,
			fmt.Sprintf("%s/repository/branches?per_page=%d", base, intOr(d.GitlabLimit, 30)), nil)
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

	case "list_commits":
		q := url.Values{"per_page": {fmt.Sprint(intOr(d.GitlabLimit, 10))}}
		if ref := sub(d.GitlabRef); ref != "" {
			q.Set("ref_name", ref)
		}
		if s := sub(d.GitlabSince); s != "" {
			q.Set("since", s)
		}
		if u := sub(d.GitlabUntil); u != "" {
			q.Set("until", u)
		}
		raw, err := gitlabCall(ctx, token, http.MethodGet, base+"/repository/commits?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var commits []struct {
			ShortID    string `json:"short_id"`
			Title      string `json:"title"`
			AuthorName string `json:"author_name"`
			CreatedAt  string `json:"created_at"`
		}
		if json.Unmarshal([]byte(raw), &commits) != nil {
			return truncateStr(raw, 8000), nil
		}
		out := make([]map[string]any, 0, len(commits))
		for _, c := range commits {
			out = append(out, map[string]any{"sha": c.ShortID, "message": c.Title, "author": c.AuthorName, "date": c.CreatedAt})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "list_pipelines":
		q := url.Values{"per_page": {fmt.Sprint(intOr(d.GitlabLimit, 10))}}
		if s := sub(d.GitlabSince); s != "" {
			q.Set("updated_after", s)
		}
		if u := sub(d.GitlabUntil); u != "" {
			q.Set("updated_before", u)
		}
		raw, err := gitlabCall(ctx, token, http.MethodGet, base+"/pipelines?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var pipes []struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
			Ref    string `json:"ref"`
			URL    string `json:"web_url"`
		}
		if json.Unmarshal([]byte(raw), &pipes) != nil {
			return truncateStr(raw, 8000), nil
		}
		out := make([]map[string]any, 0, len(pipes))
		for _, pl := range pipes {
			out = append(out, map[string]any{"id": pl.ID, "status": pl.Status, "ref": pl.Ref, "url": pl.URL})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "trigger_pipeline":
		raw, err := gitlabCall(ctx, token, http.MethodPost,
			base+"/pipeline", map[string]any{"ref": firstNonEmpty(sub(d.GitlabRef), "main")})
		if err != nil {
			return "", err
		}
		var pl struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
			URL    string `json:"web_url"`
		}
		_ = json.Unmarshal([]byte(raw), &pl)
		b, _ := json.Marshal(map[string]any{"status": "triggered", "id": pl.ID, "state": pl.Status, "url": pl.URL})
		return string(b), nil

	case "get_file":
		ref := firstNonEmpty(sub(d.GitlabRef), "main")
		raw, err := gitlabCall(ctx, token, http.MethodGet,
			base+"/repository/files/"+url.PathEscape(sub(d.GitlabPath))+"?ref="+url.QueryEscape(ref), nil)
		if err != nil {
			return "", err
		}
		var f struct {
			FileName string `json:"file_name"`
			Content  string `json:"content"`
		}
		_ = json.Unmarshal([]byte(raw), &f)
		dec, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			return "", fmt.Errorf("GitLab: could not decode file content: %w", err)
		}
		b, _ := json.Marshal(map[string]any{"name": f.FileName, "ref": ref, "content": truncateStr(string(dec), 1<<20)})
		return string(b), nil

	case "commit_file":
		path := url.PathEscape(sub(d.GitlabPath))
		ref := firstNonEmpty(sub(d.GitlabRef), "main")
		payload := map[string]any{
			"branch":         ref,
			"content":        sub(d.GitlabContent),
			"commit_message": firstNonEmpty(sub(d.GitlabCommitMsg), "Update "+sub(d.GitlabPath)),
		}
		// Try create; if the file exists GitLab answers 400 — fall back to update.
		if _, err := gitlabCall(ctx, token, http.MethodPost, base+"/repository/files/"+path, payload); err != nil {
			if _, err2 := gitlabCall(ctx, token, http.MethodPut, base+"/repository/files/"+path, payload); err2 != nil {
				return "", err2
			}
		}
		b, _ := json.Marshal(map[string]any{"status": "committed", "path": sub(d.GitlabPath), "branch": ref})
		return string(b), nil

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
