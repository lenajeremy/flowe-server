package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
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
	repoValue := strings.TrimSpace(substituteTemplates(d.GithubRepo, outputs))
	repo := ""
	if repoValue != "" {
		var repoErr error
		repo, repoErr = githubRepositoryAPIPath(repoValue)
		if repoErr != nil {
			return "", repoErr
		}
	}
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
		issueNumber, err := githubPositiveNumber(sub(d.GithubIssueNumber), "issue number")
		if err != nil {
			return "", err
		}
		if _, err := githubCall(ctx, token, http.MethodPost,
			fmt.Sprintf("/repos/%s/issues/%s/comments", repo, issueNumber),
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
		prNumber, err := githubPositiveNumber(sub(d.GithubPrNumber), "pull request number")
		if err != nil {
			return "", err
		}
		return githubCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/repos/%s/pulls/%s", repo, prNumber), nil)

	case "get_issue":
		issueNumber, err := githubPositiveNumber(sub(d.GithubIssueNumber), "issue number")
		if err != nil {
			return "", err
		}
		return githubCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/repos/%s/issues/%s", repo, issueNumber), nil)

	case "update_issue":
		issueNumber, err := githubPositiveNumber(sub(d.GithubIssueNumber), "issue number")
		if err != nil {
			return "", err
		}
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
			fmt.Sprintf("/repos/%s/issues/%s", repo, issueNumber), payload)
		if err != nil {
			return "", err
		}
		return githubIssueResult(raw), nil

	case "create_branch":
		branch := strings.TrimPrefix(sub(d.GithubBranch), "refs/heads/")
		if _, err := githubRefAPIPath(branch); err != nil {
			return "", fmt.Errorf("create_branch needs a valid branch name: %w", err)
		}
		base := firstNonEmpty(sub(d.GithubBase), "main")
		if _, err := githubRefAPIPath(base); err != nil {
			return "", fmt.Errorf("create_branch base is invalid: %w", err)
		}
		baseSHA, err := githubRefSHA(ctx, token, repo, base)
		if err != nil {
			return "", fmt.Errorf("create_branch could not read base %q: %w", base, err)
		}
		if _, err := githubCall(ctx, token, http.MethodPost, "/repos/"+repo+"/git/refs", map[string]any{
			"ref": "refs/heads/" + branch,
			"sha": baseSHA,
		}); err != nil {
			// An agent that retries a step must not wedge on a branch it
			// created on the previous attempt. If the ref exists now, the
			// caller's intent is satisfied whatever the POST objected to, so
			// report the branch rather than the error.
			if existing, refErr := githubRefSHA(ctx, token, repo, branch); refErr == nil {
				return fmt.Sprintf(`{"branch":%q,"sha":%q,"base":%q,"created":false}`, branch, existing, base), nil
			}
			return "", err
		}
		return fmt.Sprintf(`{"branch":%q,"sha":%q,"base":%q,"created":true}`, branch, baseSHA, base), nil

	case "commit_files":
		branch := strings.TrimPrefix(sub(d.GithubBranch), "refs/heads/")
		branchPath, branchErr := githubRefAPIPath(branch)
		if branchErr != nil {
			return "", fmt.Errorf("commit_files needs a valid branch — create it first with create_branch: %w", branchErr)
		}
		files, err := parseGithubFiles(sub(d.GithubFiles))
		if err != nil {
			return "", err
		}

		// One commit for the whole change set, via the Git Data API. Repeating
		// create_or_update_file per file would cost two calls each AND leave a
		// separate commit per file, which reads as a commit spray rather than
		// a change.
		headSHA, err := githubRefSHA(ctx, token, repo, branch)
		if err != nil {
			return "", fmt.Errorf("commit_files could not read branch %q: %w", branch, err)
		}
		headRaw, err := githubCall(ctx, token, http.MethodGet, "/repos/"+repo+"/git/commits/"+headSHA, nil)
		if err != nil {
			return "", err
		}
		var head struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		}
		if json.Unmarshal([]byte(headRaw), &head) != nil || head.Tree.SHA == "" {
			return "", fmt.Errorf("commit_files could not read the tree of %s", headSHA)
		}

		entries := make([]map[string]any, 0, len(files))
		for _, f := range files {
			mode := "100644"
			if f.Executable {
				mode = "100755"
			}
			entry := map[string]any{"path": f.Path, "mode": mode, "type": "blob"}
			if f.Deleted {
				// A null sha is how the tree API spells "remove this path".
				entry["sha"] = nil
			} else {
				entry["content"] = f.Content
			}
			entries = append(entries, entry)
		}

		treeRaw, err := githubCall(ctx, token, http.MethodPost, "/repos/"+repo+"/git/trees", map[string]any{
			"base_tree": head.Tree.SHA,
			"tree":      entries,
		})
		if err != nil {
			return "", err
		}
		var tree struct {
			SHA string `json:"sha"`
		}
		if json.Unmarshal([]byte(treeRaw), &tree) != nil || tree.SHA == "" {
			return "", fmt.Errorf("commit_files could not build a tree")
		}

		commitRaw, err := githubCall(ctx, token, http.MethodPost, "/repos/"+repo+"/git/commits", map[string]any{
			"message": firstNonEmpty(sub(d.GithubCommitMsg), fmt.Sprintf("Update %d file(s)", len(files))),
			"tree":    tree.SHA,
			"parents": []string{headSHA},
		})
		if err != nil {
			return "", err
		}
		var commit struct {
			SHA string `json:"sha"`
			URL string `json:"html_url"`
		}
		if json.Unmarshal([]byte(commitRaw), &commit) != nil || commit.SHA == "" {
			return "", fmt.Errorf("commit_files could not create a commit")
		}

		// Only now does the branch move. Everything above is unreferenced
		// object creation, so a failure partway leaves the branch untouched
		// rather than half-updated.
		if _, err := githubCall(ctx, token, http.MethodPatch,
			"/repos/"+repo+"/git/refs/heads/"+branchPath, map[string]any{"sha": commit.SHA}); err != nil {
			return "", fmt.Errorf("commit_files built commit %s but could not move %q: %w", commit.SHA, branch, err)
		}
		return fmt.Sprintf(`{"branch":%q,"commit":%q,"files":%d,"url":%q}`,
			branch, commit.SHA, len(files), commit.URL), nil

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
		prNumber, err := githubPositiveNumber(sub(d.GithubPrNumber), "pull request number")
		if err != nil {
			return "", err
		}
		method := firstNonEmpty(d.GithubMergeMethod, "merge")
		raw, err := githubCall(ctx, token, http.MethodPut,
			fmt.Sprintf("/repos/%s/pulls/%s/merge", repo, prNumber),
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
		prNumber, err := githubPositiveNumber(sub(d.GithubPrNumber), "pull request number")
		if err != nil {
			return "", err
		}
		raw, err := githubCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/repos/%s/pulls/%s/files?per_page=%d", repo, prNumber, intOr(d.GithubLimit, 30)), nil)
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

	case "list_repo_tree":
		if repo == "" {
			return "", fmt.Errorf("githubRepo is required (owner/name)")
		}
		ref := sub(d.GithubRef)
		if ref == "" {
			details, err := githubCall(ctx, token, http.MethodGet, "/repos/"+repo, nil)
			if err != nil {
				return "", err
			}
			var metadata struct {
				DefaultBranch string `json:"default_branch"`
			}
			if err := json.Unmarshal([]byte(details), &metadata); err != nil || metadata.DefaultBranch == "" {
				return "", fmt.Errorf("GitHub: repository response did not include a default branch")
			}
			ref = metadata.DefaultBranch
		}
		raw, err := githubCall(ctx, token, http.MethodGet,
			"/repos/"+repo+"/git/trees/"+url.PathEscape(ref)+"?recursive=1", nil)
		if err != nil {
			return "", err
		}
		return githubRepositoryTree(raw, repo, ref, sub(d.GithubPath), d.GithubTreeLimit)

	case "get_repo_details":
		if repo == "" {
			return "", fmt.Errorf("githubRepo is required (owner/name)")
		}
		details, err := githubCall(ctx, token, http.MethodGet, "/repos/"+repo, nil)
		if err != nil {
			return "", err
		}
		languages, err := githubCall(ctx, token, http.MethodGet, "/repos/"+repo+"/languages", nil)
		if err != nil {
			return "", err
		}
		return githubRepositoryDetails(details, languages)

	case "get_file":
		contentPath, err := githubContentAPIPath(sub(d.GithubPath))
		if err != nil {
			return "", err
		}
		q := url.Values{}
		if ref := sub(d.GithubRef); ref != "" {
			q.Set("ref", ref)
		}
		raw, err := githubCall(ctx, token, http.MethodGet,
			"/repos/"+repo+"/contents/"+contentPath+"?"+q.Encode(), nil)
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
		contentPath, err := githubContentAPIPath(path)
		if err != nil {
			return "", err
		}
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
			"/repos/"+repo+"/contents/"+contentPath+"?"+q.Encode(), nil); err == nil {
			var f struct {
				SHA string `json:"sha"`
			}
			if json.Unmarshal([]byte(existing), &f) == nil && f.SHA != "" {
				payload["sha"] = f.SHA
			}
		}
		raw, err := githubCall(ctx, token, http.MethodPut, "/repos/"+repo+"/contents/"+contentPath, payload)
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
		workflowID, err := githubWorkflowAPIID(sub(d.GithubWorkflowId))
		if err != nil {
			return "", err
		}
		payload := map[string]any{"ref": firstNonEmpty(sub(d.GithubRef), "main")}
		if inputs := sub(d.GithubBody); inputs != "" {
			var m map[string]any
			if err := json.Unmarshal([]byte(inputs), &m); err != nil {
				return "", fmt.Errorf("GitHub: workflow inputs must be a JSON object: %w", err)
			}
			payload["inputs"] = m
		}
		if _, err := githubCall(ctx, token, http.MethodPost,
			fmt.Sprintf("/repos/%s/actions/workflows/%s/dispatches", repo, workflowID), payload); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": "dispatched", "workflow": workflowID, "ref": payload["ref"]})
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

const (
	githubTreeDefaultLimit = 1000
	githubTreeMaximumLimit = 5000
)

// githubRepositoryTree keeps recursive tree responses useful for an agent without
// allowing a large repository to consume the workflow's entire context window.
func githubRepositoryTree(raw, repo, ref, pathPrefix string, requestedLimit int) (string, error) {
	var tree struct {
		SHA       string `json:"sha"`
		Truncated bool   `json:"truncated"`
		Entries   []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	if err := json.Unmarshal([]byte(raw), &tree); err != nil {
		return "", fmt.Errorf("GitHub: could not decode repository tree: %w", err)
	}

	limit := requestedLimit
	if limit <= 0 {
		limit = githubTreeDefaultLimit
	}
	if limit > githubTreeMaximumLimit {
		limit = githubTreeMaximumLimit
	}
	prefix := strings.Trim(strings.TrimSpace(pathPrefix), "/")
	entries := make([]map[string]any, 0, min(limit, len(tree.Entries)))
	matching := 0
	for _, entry := range tree.Entries {
		if prefix != "" && entry.Path != prefix && !strings.HasPrefix(entry.Path, prefix+"/") {
			continue
		}
		matching++
		if len(entries) >= limit {
			continue
		}
		item := map[string]any{"path": entry.Path, "type": entry.Type}
		if entry.Type == "blob" {
			item["size_bytes"] = entry.Size
			if entry.Mode == "100755" {
				item["executable"] = true
			}
		}
		if entry.Type == "commit" {
			item["submodule"] = true
		}
		entries = append(entries, item)
	}

	locallyTruncated := matching > len(entries)
	result := map[string]any{
		"repository":         repo,
		"ref":                ref,
		"tree_sha":           tree.SHA,
		"path_prefix":        prefix,
		"total_entries":      len(tree.Entries),
		"matching_entries":   matching,
		"returned_entries":   len(entries),
		"truncated":          tree.Truncated || locallyTruncated,
		"provider_truncated": tree.Truncated,
		"entries":            entries,
	}
	if tree.Truncated || locallyTruncated {
		result["note"] = "The tree response is incomplete. Use githubPath to narrow the directory or increase githubTreeLimit (maximum 5000)."
	}
	b, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("GitHub: could not encode repository tree: %w", err)
	}
	return string(b), nil
}

// githubRepositoryDetails combines the repository and languages endpoints into a
// compact overview suitable for deciding which files an agent should inspect next.
func githubRepositoryDetails(detailsRaw, languagesRaw string) (string, error) {
	var repo struct {
		FullName        string   `json:"full_name"`
		Description     string   `json:"description"`
		Homepage        string   `json:"homepage"`
		HTMLURL         string   `json:"html_url"`
		Visibility      string   `json:"visibility"`
		Private         bool     `json:"private"`
		Fork            bool     `json:"fork"`
		Archived        bool     `json:"archived"`
		Disabled        bool     `json:"disabled"`
		DefaultBranch   string   `json:"default_branch"`
		Language        string   `json:"language"`
		Topics          []string `json:"topics"`
		Size            int64    `json:"size"`
		StargazersCount int      `json:"stargazers_count"`
		ForksCount      int      `json:"forks_count"`
		OpenIssuesCount int      `json:"open_issues_count"`
		Subscribers     int      `json:"subscribers_count"`
		CreatedAt       string   `json:"created_at"`
		UpdatedAt       string   `json:"updated_at"`
		PushedAt        string   `json:"pushed_at"`
		HasIssues       bool     `json:"has_issues"`
		HasWiki         bool     `json:"has_wiki"`
		HasPages        bool     `json:"has_pages"`
		HasDiscussions  bool     `json:"has_discussions"`
		Owner           struct {
			Login string `json:"login"`
		} `json:"owner"`
		License *struct {
			Key  string `json:"key"`
			Name string `json:"name"`
			SPDX string `json:"spdx_id"`
		} `json:"license"`
		Parent *struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		} `json:"parent"`
	}
	if err := json.Unmarshal([]byte(detailsRaw), &repo); err != nil {
		return "", fmt.Errorf("GitHub: could not decode repository details: %w", err)
	}
	var languages map[string]int64
	if err := json.Unmarshal([]byte(languagesRaw), &languages); err != nil {
		return "", fmt.Errorf("GitHub: could not decode repository languages: %w", err)
	}

	result := map[string]any{
		"repository":       repo.FullName,
		"owner":            repo.Owner.Login,
		"description":      repo.Description,
		"homepage":         repo.Homepage,
		"url":              repo.HTMLURL,
		"visibility":       repo.Visibility,
		"private":          repo.Private,
		"fork":             repo.Fork,
		"archived":         repo.Archived,
		"disabled":         repo.Disabled,
		"default_branch":   repo.DefaultBranch,
		"primary_language": repo.Language,
		"languages_bytes":  languages,
		"topics":           repo.Topics,
		"size_kb":          repo.Size,
		"stars":            repo.StargazersCount,
		"forks":            repo.ForksCount,
		"open_issues":      repo.OpenIssuesCount,
		"subscribers":      repo.Subscribers,
		"created_at":       repo.CreatedAt,
		"updated_at":       repo.UpdatedAt,
		"pushed_at":        repo.PushedAt,
		"features":         map[string]bool{"issues": repo.HasIssues, "wiki": repo.HasWiki, "pages": repo.HasPages, "discussions": repo.HasDiscussions},
	}
	if repo.License != nil {
		result["license"] = map[string]string{"key": repo.License.Key, "name": repo.License.Name, "spdx_id": repo.License.SPDX}
	}
	if repo.Parent != nil {
		result["parent_repository"] = map[string]string{"repository": repo.Parent.FullName, "url": repo.Parent.HTMLURL}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("GitHub: could not encode repository details: %w", err)
	}
	return string(b), nil
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

var (
	githubRepositorySegment = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	githubPositiveNumberRE  = regexp.MustCompile(`^[1-9][0-9]{0,11}$`)
	githubWorkflowIDRE      = regexp.MustCompile(`^(?:[1-9][0-9]{0,19}|[A-Za-z0-9_.-]{1,200})$`)
)

func githubRepositoryAPIPath(repository string) (string, error) {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 {
		return "", errors.New("GitHub repository must be owner/name")
	}
	for _, part := range parts {
		if !githubRepositorySegment.MatchString(part) || part == "." || part == ".." {
			return "", errors.New("GitHub repository contains an invalid owner or name")
		}
	}
	return url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]), nil
}

func githubPositiveNumber(value, label string) (string, error) {
	value = strings.TrimSpace(value)
	if !githubPositiveNumberRE.MatchString(value) {
		return "", fmt.Errorf("GitHub %s must be a positive integer", label)
	}
	return value, nil
}

func githubRefAPIPath(ref string) (string, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	if ref == "" || len(ref) > 240 || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return "", errors.New("GitHub branch or ref is invalid")
	}
	parts := strings.Split(ref, "/")
	for index, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `~^:?*[\`) {
			return "", errors.New("GitHub branch or ref contains unsupported characters")
		}
		for _, r := range part {
			if r < 0x20 || r == 0x7f {
				return "", errors.New("GitHub branch or ref contains control characters")
			}
		}
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/"), nil
}

func githubContentAPIPath(path string) (string, error) {
	parts, err := validateGithubContentPath(path)
	if err != nil {
		return "", err
	}
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/"), nil
}

func validateGithubContentPath(path string) ([]string, error) {
	if strings.HasPrefix(path, "/") {
		return nil, errors.New("GitHub file path must be relative to the repository root")
	}
	if path == "" || len(path) > 1000 || strings.Contains(path, "\\") || strings.HasSuffix(path, "/") {
		return nil, errors.New("GitHub file path is invalid")
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("GitHub file path cannot traverse directories")
		}
		for _, r := range part {
			if r < 0x20 || r == 0x7f {
				return nil, errors.New("GitHub file path contains control characters")
			}
		}
	}
	return parts, nil
}

func githubWorkflowAPIID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !githubWorkflowIDRE.MatchString(value) || value == "." || value == ".." {
		return "", errors.New("GitHub workflow ID must be a numeric ID or workflow filename")
	}
	return url.PathEscape(value), nil
}

// githubRefSHA resolves a branch to the commit it points at.
//
// The ref is used unescaped: GitHub addresses nested branches as
// git/ref/heads/feature/foo, so percent-encoding the separator would turn a
// valid branch name into a 404.
func githubRefSHA(ctx context.Context, token, repo, ref string) (string, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	refPath, err := githubRefAPIPath(ref)
	if err != nil {
		return "", err
	}
	raw, err := githubCall(ctx, token, http.MethodGet, "/repos/"+repo+"/git/ref/heads/"+refPath, nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if json.Unmarshal([]byte(raw), &out) != nil || out.Object.SHA == "" {
		return "", fmt.Errorf("branch %q not found in %s", ref, repo)
	}
	return out.Object.SHA, nil
}

// githubFileEntry is one path in a commit_files change set. A deleted entry
// carries no content; everything else is written as a UTF-8 text blob.
type githubFileEntry struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Deleted    bool   `json:"deleted"`
	Executable bool   `json:"executable"`
}

// Bounds on one change set. The tree call inlines every file's content into a
// single request, so an unbounded set fails as an opaque 4xx from GitHub. These
// turn that into a sentence naming what was too big.
const (
	githubMaxCommitFiles = 200
	githubMaxCommitBytes = 5 << 20
)

func parseGithubFiles(raw string) ([]githubFileEntry, error) {
	raw = strings.TrimSpace(stripCodeFences(raw))
	if raw == "" {
		return nil, fmt.Errorf("commit_files needs githubFiles: a JSON array of {path, content}")
	}
	var files []githubFileEntry
	if err := json.Unmarshal([]byte(raw), &files); err != nil {
		return nil, fmt.Errorf("githubFiles must be a JSON array of {path, content}: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("githubFiles is empty — nothing to commit")
	}
	if len(files) > githubMaxCommitFiles {
		return nil, fmt.Errorf("commit_files takes at most %d files, got %d", githubMaxCommitFiles, len(files))
	}
	total := 0
	seenPaths := make(map[string]bool, len(files))
	for i, f := range files {
		if strings.TrimSpace(f.Path) == "" {
			return nil, fmt.Errorf("githubFiles[%d] has no path", i)
		}
		if _, err := validateGithubContentPath(f.Path); err != nil {
			return nil, fmt.Errorf("githubFiles[%d] path %q is invalid: %w", i, f.Path, err)
		}
		if seenPaths[f.Path] {
			return nil, fmt.Errorf("githubFiles contains duplicate path %q", f.Path)
		}
		seenPaths[f.Path] = true
		total += len(f.Content)
	}
	if total > githubMaxCommitBytes {
		return nil, fmt.Errorf("commit_files content is %d bytes, over the %d byte limit", total, githubMaxCommitBytes)
	}
	return files, nil
}
