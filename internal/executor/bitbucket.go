package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

// Bitbucket Cloud REST API 2.0. Unlike Jira and Confluence this is not behind
// the Atlassian gateway — it is api.bitbucket.org with a plain bearer token, and
// paths are built from a workspace slug plus a repo slug.
//
// The workspace defaults to the one captured at connect time, so most nodes only
// need to name a repository.

const bitbucketAPI = "https://api.bitbucket.org/2.0"

func bitbucketCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, bitbucketAPI+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return bitbucketDo(req)
}

func bitbucketDo(req *http.Request) (string, error) {
	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("bitbucket request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Detail  string `json:"detail"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg := e.Error.Message
			if e.Error.Detail != "" {
				msg += ": " + e.Error.Detail
			}
			if resp.StatusCode == http.StatusForbidden {
				// Bitbucket fixes scopes on the OAuth consumer, so a 403 usually
				// means the consumer was created without the needed permission —
				// reconnecting alone will not fix it.
				msg += " — the Bitbucket OAuth consumer may be missing a permission; " +
					"add it in Bitbucket workspace settings, then reconnect"
			}
			return "", fmt.Errorf("Bitbucket API error (%d): %s", resp.StatusCode, msg)
		}
		return "", fmt.Errorf("Bitbucket API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

func runBitbucket(ctx context.Context, token, workspace string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.BitbucketLimit, 25)

	// The node may override the connected workspace, e.g. for a personal repo
	// under a different account.
	ws := firstNonEmpty(sub(d.BitbucketWorkspace), workspace)
	if ws == "" && d.IntegrationOp != "get_current_user" && d.IntegrationOp != "list_workspaces" {
		return "", fmt.Errorf("no Bitbucket workspace is set — reconnect Bitbucket or name a workspace on this node")
	}

	// repo returns the {workspace}/{repo} path segment every repository-scoped
	// endpoint starts with.
	repo := func() (string, error) {
		r := sub(d.BitbucketRepo)
		if r == "" {
			return "", fmt.Errorf("this operation needs a repository slug")
		}
		return "/repositories/" + ws + "/" + r, nil
	}

	switch d.IntegrationOp {
	// ---- repositories ----
	case "list_repositories":
		q := url.Values{"pagelen": {fmt.Sprint(limit)}, "sort": {"-updated_on"}}
		if v := sub(d.BitbucketQuery); v != "" {
			q.Set("q", v)
		}
		raw, err := bitbucketCall(ctx, token, http.MethodGet, "/repositories/"+ws+"?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_repository":
		base, err := repo()
		if err != nil {
			return "", err
		}
		return bitbucketCall(ctx, token, http.MethodGet, base, nil)

	case "create_repository":
		base, err := repo()
		if err != nil {
			return "", err
		}
		payload := map[string]any{
			"scm":         "git",
			"is_private":  !strings.EqualFold(sub(d.BitbucketPrivate), "false"),
			"description": sub(d.BitbucketBody),
		}
		return bitbucketCall(ctx, token, http.MethodPut, base, payload)

	// ---- pull requests ----
	case "list_pull_requests":
		base, err := repo()
		if err != nil {
			return "", err
		}
		q := url.Values{"pagelen": {fmt.Sprint(limit)}}
		// Bitbucket defaults to OPEN only; an explicit state widens or narrows it.
		if s := sub(d.BitbucketState); s != "" {
			q.Set("state", strings.ToUpper(s))
		}
		raw, err := bitbucketCall(ctx, token, http.MethodGet, base+"/pullrequests?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_pull_request":
		base, err := repo()
		if err != nil {
			return "", err
		}
		return bitbucketCall(ctx, token, http.MethodGet, base+"/pullrequests/"+sub(d.BitbucketPrId), nil)

	case "create_pull_request":
		base, err := repo()
		if err != nil {
			return "", err
		}
		if sub(d.BitbucketSource) == "" {
			return "", fmt.Errorf("create_pull_request needs a source branch")
		}
		payload := map[string]any{
			"title":       firstNonEmpty(sub(d.BitbucketTitle), sub(d.BitbucketSource)),
			"description": sub(d.BitbucketBody),
			"source":      map[string]any{"branch": map[string]string{"name": sub(d.BitbucketSource)}},
		}
		if dest := sub(d.BitbucketDest); dest != "" {
			payload["destination"] = map[string]any{"branch": map[string]string{"name": dest}}
		}
		return bitbucketCall(ctx, token, http.MethodPost, base+"/pullrequests", payload)

	case "merge_pull_request":
		base, err := repo()
		if err != nil {
			return "", err
		}
		payload := map[string]any{
			"merge_strategy": firstNonEmpty(sub(d.BitbucketMergeStrategy), "merge_commit"),
		}
		if m := sub(d.BitbucketMessage); m != "" {
			payload["message"] = m
		}
		return bitbucketCall(ctx, token, http.MethodPost,
			base+"/pullrequests/"+sub(d.BitbucketPrId)+"/merge", payload)

	case "decline_pull_request":
		base, err := repo()
		if err != nil {
			return "", err
		}
		return bitbucketCall(ctx, token, http.MethodPost,
			base+"/pullrequests/"+sub(d.BitbucketPrId)+"/decline", nil)

	case "approve_pull_request":
		base, err := repo()
		if err != nil {
			return "", err
		}
		return bitbucketCall(ctx, token, http.MethodPost,
			base+"/pullrequests/"+sub(d.BitbucketPrId)+"/approve", nil)

	case "comment_on_pull_request":
		base, err := repo()
		if err != nil {
			return "", err
		}
		if sub(d.BitbucketBody) == "" {
			return "", fmt.Errorf("comment_on_pull_request needs comment text")
		}
		return bitbucketCall(ctx, token, http.MethodPost,
			base+"/pullrequests/"+sub(d.BitbucketPrId)+"/comments",
			map[string]any{"content": map[string]string{"raw": sub(d.BitbucketBody)}})

	case "list_pr_comments":
		base, err := repo()
		if err != nil {
			return "", err
		}
		raw, err := bitbucketCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"%s/pullrequests/%s/comments?pagelen=%d", base, sub(d.BitbucketPrId), limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_pr_commits":
		base, err := repo()
		if err != nil {
			return "", err
		}
		raw, err := bitbucketCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"%s/pullrequests/%s/commits?pagelen=%d", base, sub(d.BitbucketPrId), limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_pr_diff":
		base, err := repo()
		if err != nil {
			return "", err
		}
		// The diff endpoint answers text/plain, not JSON, and follows a redirect.
		return bitbucketRaw(ctx, token, base+"/pullrequests/"+sub(d.BitbucketPrId)+"/diff", 12000)

	// ---- branches & commits ----
	case "list_branches":
		base, err := repo()
		if err != nil {
			return "", err
		}
		raw, err := bitbucketCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"%s/refs/branches?pagelen=%d&sort=-target.date", base, limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "create_branch":
		base, err := repo()
		if err != nil {
			return "", err
		}
		if sub(d.BitbucketBranch) == "" {
			return "", fmt.Errorf("create_branch needs a branch name")
		}
		// The target may be a branch name or a commit hash; Bitbucket resolves both.
		return bitbucketCall(ctx, token, http.MethodPost, base+"/refs/branches", map[string]any{
			"name":   sub(d.BitbucketBranch),
			"target": map[string]string{"hash": firstNonEmpty(sub(d.BitbucketRef), "main")},
		})

	case "delete_branch":
		base, err := repo()
		if err != nil {
			return "", err
		}
		if sub(d.BitbucketBranch) == "" {
			return "", fmt.Errorf("delete_branch needs a branch name")
		}
		return bitbucketCall(ctx, token, http.MethodDelete,
			base+"/refs/branches/"+url.PathEscape(sub(d.BitbucketBranch)), nil)

	case "list_commits":
		base, err := repo()
		if err != nil {
			return "", err
		}
		path := base + "/commits"
		if ref := sub(d.BitbucketRef); ref != "" {
			path += "/" + url.PathEscape(ref)
		}
		raw, err := bitbucketCall(ctx, token, http.MethodGet,
			fmt.Sprintf("%s?pagelen=%d", path, limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_commit":
		base, err := repo()
		if err != nil {
			return "", err
		}
		return bitbucketCall(ctx, token, http.MethodGet, base+"/commit/"+sub(d.BitbucketRef), nil)

	// ---- files ----
	case "get_file":
		base, err := repo()
		if err != nil {
			return "", err
		}
		if sub(d.BitbucketPath) == "" {
			return "", fmt.Errorf("get_file needs a file path")
		}
		// /src returns the file's own bytes, not JSON.
		return bitbucketRaw(ctx, token, fmt.Sprintf("%s/src/%s/%s", base,
			url.PathEscape(firstNonEmpty(sub(d.BitbucketRef), "main")),
			pathEscapeSegments(sub(d.BitbucketPath))), 12000)

	case "commit_file":
		return bitbucketCommitFile(ctx, token, ws, d, sub)

	// ---- issue tracker ----
	case "list_issues":
		base, err := repo()
		if err != nil {
			return "", err
		}
		q := url.Values{"pagelen": {fmt.Sprint(limit)}, "sort": {"-updated_on"}}
		if s := sub(d.BitbucketState); s != "" {
			q.Set("q", fmt.Sprintf("state=%q", s))
		}
		raw, err := bitbucketCall(ctx, token, http.MethodGet, base+"/issues?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_issue":
		base, err := repo()
		if err != nil {
			return "", err
		}
		return bitbucketCall(ctx, token, http.MethodGet, base+"/issues/"+sub(d.BitbucketIssueId), nil)

	case "create_issue":
		base, err := repo()
		if err != nil {
			return "", err
		}
		if sub(d.BitbucketTitle) == "" {
			return "", fmt.Errorf("create_issue needs a title")
		}
		payload := map[string]any{
			"title":    sub(d.BitbucketTitle),
			"kind":     firstNonEmpty(sub(d.BitbucketKind), "bug"),
			"priority": firstNonEmpty(sub(d.BitbucketPriority), "major"),
		}
		if b := sub(d.BitbucketBody); b != "" {
			payload["content"] = map[string]string{"raw": b}
		}
		return bitbucketCall(ctx, token, http.MethodPost, base+"/issues", payload)

	case "comment_on_issue":
		base, err := repo()
		if err != nil {
			return "", err
		}
		if sub(d.BitbucketBody) == "" {
			return "", fmt.Errorf("comment_on_issue needs comment text")
		}
		return bitbucketCall(ctx, token, http.MethodPost,
			base+"/issues/"+sub(d.BitbucketIssueId)+"/comments",
			map[string]any{"content": map[string]string{"raw": sub(d.BitbucketBody)}})

	// ---- pipelines ----
	case "list_pipelines":
		base, err := repo()
		if err != nil {
			return "", err
		}
		raw, err := bitbucketCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"%s/pipelines?pagelen=%d&sort=-created_on", base, limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "trigger_pipeline":
		base, err := repo()
		if err != nil {
			return "", err
		}
		return bitbucketCall(ctx, token, http.MethodPost, base+"/pipelines", map[string]any{
			"target": map[string]any{
				"type":     "pipeline_ref_target",
				"ref_type": "branch",
				"ref_name": firstNonEmpty(sub(d.BitbucketBranch), sub(d.BitbucketRef), "main"),
			},
		})

	// ---- account ----
	case "list_workspaces":
		return bitbucketCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/workspaces?pagelen=%d", limit), nil)

	case "get_current_user":
		return bitbucketCall(ctx, token, http.MethodGet, "/user", nil)

	case "":
		return "", fmt.Errorf("no Bitbucket operation selected")
	}
	return "", fmt.Errorf("unsupported Bitbucket operation: %s", d.IntegrationOp)
}

// pathEscapeSegments escapes each segment of a repository path but keeps the
// slashes, so "docs/readme.md" stays two segments rather than one escaped blob.
func pathEscapeSegments(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// bitbucketRaw fetches an endpoint that answers with bytes rather than JSON
// (file contents, diffs).
func bitbucketRaw(ctx context.Context, token, path string, maxBytes int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bitbucketAPI+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("bitbucket request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)*4))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Bitbucket API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return truncateStr(string(raw), maxBytes), nil
}

// bitbucketCommitFile writes a file and commits it in one request. The /src
// endpoint is form-encoded rather than JSON: each form field name is a repo path
// and its value is the new content.
func bitbucketCommitFile(ctx context.Context, token, ws string, d FlowNodeData, sub func(string) string) (string, error) {
	repoSlug, path := sub(d.BitbucketRepo), sub(d.BitbucketPath)
	if repoSlug == "" || path == "" {
		return "", fmt.Errorf("commit_file needs a repository slug and a file path")
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField(strings.TrimPrefix(path, "/"), sub(d.BitbucketContent))
	_ = w.WriteField("message", firstNonEmpty(sub(d.BitbucketMessage), "Update "+path))
	if b := firstNonEmpty(sub(d.BitbucketBranch), sub(d.BitbucketRef)); b != "" {
		_ = w.WriteField("branch", b)
	}
	w.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		bitbucketAPI+"/repositories/"+ws+"/"+repoSlug+"/src", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if _, err := bitbucketDo(req); err != nil {
		return "", err
	}
	// A successful write answers 201 with no body.
	return fmt.Sprintf(`{"ok":true,"repository":%q,"path":%q}`, repoSlug, path), nil
}
