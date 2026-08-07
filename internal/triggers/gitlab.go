package triggers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
)

// GitLab webhooks are project-level objects created with the connected user's
// OAuth token. That is materially different from the GitHub App: GitHub sends
// every installation to one app-level URL, while GitLab gives each trigger its
// own hook, callback URL and signing token.
//
// Creating or deleting a project hook requires Maintainer or Owner access to
// the exact project. Register therefore doubles as the permission check: a user
// being able to see a project is not enough to promise that Fernary can listen
// to it.

func init() { Register(gitlabAdapter{}) }

type gitlabAdapter struct{}

var gitlabHooksAPIBase = "https://gitlab.com/api/v4"

const gitlabSignatureMaxAge = 5 * time.Minute

func (gitlabAdapter) Provider() string   { return "gitlab" }
func (gitlabAdapter) Delivery() Delivery { return Push }

func (gitlabAdapter) Events() []EventSpec {
	return []EventSpec{
		{
			ID: "merge_request.opened", Label: "Merge request opened", ResourceKind: "project",
			Filters: []FilterSpec{
				{Key: "base", Label: "Target branch", Placeholder: "main", ResourceKind: "branch"},
				{Key: "author", Label: "Opened by", Placeholder: "username", ResourceKind: "user"},
			},
			Sample: map[string]any{
				"number": 42, "title": "Fix the retry loop", "author": "alex",
				"base": "main", "head": "fix-retries", "url": "https://gitlab.com/acme/widgets/-/merge_requests/42",
			},
		},
		{
			ID: "merge_request.merged", Label: "Merge request merged", ResourceKind: "project",
			Filters: []FilterSpec{{Key: "base", Label: "Target branch", Placeholder: "main", ResourceKind: "branch"}},
		},
		{
			ID: "issues.opened", Label: "Issue opened", ResourceKind: "project",
			Filters: []FilterSpec{{Key: "label", Label: "Has label", Placeholder: "bug"}},
			Sample: map[string]any{
				"number": 17, "title": "Crash on empty input", "author": "alex",
				"url": "https://gitlab.com/acme/widgets/-/issues/17",
			},
		},
		{
			ID: "issues.edited", Label: "Issue edited", ResourceKind: "project",
			Filters: []FilterSpec{{Key: "label", Label: "Has label", Placeholder: "bug"}},
			Sample: map[string]any{
				"number": 17, "title": "Crash when input is empty", "body": "Updated reproduction steps",
				"author": "alex", "labels": []string{"bug"},
				"changed_fields": []string{"description", "title"},
				"previous_title": "Crash on empty input", "previous_body": "Original reproduction steps",
			},
		},
		{
			ID: "note.created", Label: "Comment added to an issue or merge request", ResourceKind: "project",
			Filters: []FilterSpec{{Key: "author", Label: "Commented by", Placeholder: "username", ResourceKind: "user"}},
			Sample: map[string]any{
				"number": 17, "body": "I can reproduce this on v2.1.", "author": "alex",
				"noteable_type": "Issue", "is_merge_request": false,
			},
		},
		{
			ID: "push", Label: "Commits pushed", ResourceKind: "project",
			Filters: []FilterSpec{{Key: "branch", Label: "Branch", Placeholder: "main", ResourceKind: "branch"}},
		},
		{ID: "release.published", Label: "Release published", ResourceKind: "project"},
	}
}

// GitLab's project-hook flags are coarser than Fernary's events. For example,
// one issues_events subscription delivers opens, updates, closes and reopens;
// Parse narrows that stream to the actions represented in Events above.
var gitlabHookFields = map[string][]string{
	"merge_request.opened": {"merge_requests_events"},
	"merge_request.merged": {"merge_requests_events"},
	"issues.opened":        {"issues_events", "confidential_issues_events"},
	"issues.edited":        {"issues_events", "confidential_issues_events"},
	"note.created":         {"note_events", "confidential_note_events"},
	"push":                 {"push_events"},
	"release.published":    {"releases_events"},
}

var gitlabBooleanHookFields = []string{
	"confidential_issues_events", "confidential_note_events", "deployment_events",
	"feature_flag_events", "issues_events", "job_events", "merge_requests_events",
	"milestone_events", "note_events", "pipeline_events", "push_events",
	"releases_events", "resource_access_token_events", "resource_deploy_token_events",
	"tag_push_events", "wiki_page_events",
}

func (gitlabAdapter) Register(ctx context.Context, conn Conn, t *models.IntegrationTrigger) (Registration, error) {
	fields, ok := gitlabHookFields[t.Event]
	if !ok {
		return Registration{}, fmt.Errorf("gitlab: unknown event %q", t.Event)
	}
	projectID, err := gitlabProjectID(t.ResourceID)
	if err != nil {
		return Registration{}, err
	}
	callback := HookURL("gitlab", t.ID.String())
	callbackURL, err := url.Parse(callback)
	if err != nil || callbackURL.Scheme != "https" || callbackURL.Host == "" {
		return Registration{}, fmt.Errorf("gitlab: PUBLIC_BASE_URL must be a public HTTPS URL before webhooks can be registered")
	}

	signingToken, err := newGitLabSigningToken()
	if err != nil {
		return Registration{}, err
	}
	// GitLab can reject two project hooks with the same URL. Reconfiguring an
	// existing trigger intentionally creates its replacement before deleting the
	// old hook, so give each registration a non-secret URL nonce. Gin routes on
	// the path and ignores this query value; RemoteID remains the deletion key.
	callbackNonce := sha256.Sum256([]byte(signingToken))
	callback += "?registration=" + fmt.Sprintf("%x", callbackNonce[:8])
	payload := map[string]any{
		"url":                     callback,
		"name":                    "Fernary workflow trigger",
		"description":             "Fernary event: " + t.Event,
		"enable_ssl_verification": true,
		"signing_token":           signingToken,
	}
	// GitLab historically defaulted push_events on for new hooks. Explicitly set
	// every event flag so this hook receives only the coarse stream it needs.
	for _, field := range gitlabBooleanHookFields {
		payload[field] = false
	}
	for _, field := range fields {
		payload[field] = true
	}
	if t.Event == "push" {
		if branch := triggerFilter(t, "branch"); branch != "" {
			payload["push_events_branch_filter"] = branch
			payload["branch_filter_strategy"] = "wildcard"
		}
	}

	var hook struct {
		ID        int64 `json:"id"`
		ProjectID int64 `json:"project_id"`
	}
	if err := gitlabHookAPI(ctx, conn.AccessToken, http.MethodPost,
		"/projects/"+projectID+"/hooks", payload, &hook); err != nil {
		return Registration{}, gitlabRegistrationError(projectID, err)
	}
	if hook.ID <= 0 {
		return Registration{}, fmt.Errorf("gitlab: project %s created a webhook without an id", projectID)
	}
	if hook.ProjectID > 0 && strconv.FormatInt(hook.ProjectID, 10) != projectID {
		// Defensive compensation. The request path should make this impossible,
		// but never retain a hook if GitLab says it belongs to a different project.
		_ = gitlabHookAPI(ctx, conn.AccessToken, http.MethodDelete,
			"/projects/"+projectID+"/hooks/"+strconv.FormatInt(hook.ID, 10), nil, nil)
		return Registration{}, fmt.Errorf("gitlab: webhook was created on an unexpected project")
	}
	return Registration{RemoteID: strconv.FormatInt(hook.ID, 10), Secret: signingToken}, nil
}

func (gitlabAdapter) Unregister(ctx context.Context, conn Conn, t *models.IntegrationTrigger) error {
	projectID, err := gitlabProjectID(t.ResourceID)
	if err != nil {
		return err
	}
	hookID, err := positiveDecimal(t.RemoteID)
	if err != nil {
		return fmt.Errorf("gitlab: invalid webhook id: %w", err)
	}
	return gitlabHookAPI(ctx, conn.AccessToken, http.MethodDelete,
		"/projects/"+projectID+"/hooks/"+hookID, nil, nil)
}

func (gitlabAdapter) Renew(context.Context, Conn, *models.IntegrationTrigger) (*time.Time, error) {
	return nil, nil
}

func (gitlabAdapter) Handshake(*http.Request, []byte) (int, []byte, http.Header, bool) {
	return 0, nil, nil, false
}

// Verify implements GitLab's Standard Webhooks signature. The signing token is
// encrypted in IntegrationTrigger.Secret and never returned by GitLab's API.
func (gitlabAdapter) Verify(r *http.Request, body []byte, t *models.IntegrationTrigger) error {
	if t == nil || strings.TrimSpace(t.Secret) == "" {
		return fmt.Errorf("gitlab: webhook has no signing token")
	}
	messageID := strings.TrimSpace(r.Header.Get("webhook-id"))
	timestamp := strings.TrimSpace(r.Header.Get("webhook-timestamp"))
	received := strings.Fields(r.Header.Get("webhook-signature"))
	if messageID == "" || timestamp == "" || len(received) == 0 {
		return fmt.Errorf("gitlab: request is not signed")
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("gitlab: invalid webhook timestamp")
	}
	delta := time.Since(time.Unix(seconds, 0))
	if delta < -gitlabSignatureMaxAge || delta > gitlabSignatureMaxAge {
		return fmt.Errorf("gitlab: webhook timestamp is stale")
	}
	if !strings.HasPrefix(t.Secret, "whsec_") {
		return fmt.Errorf("gitlab: invalid signing token")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(t.Secret, "whsec_"))
	if err != nil || len(key) != 32 {
		return fmt.Errorf("gitlab: invalid signing token")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, messageID+"."+timestamp+".")
	_, _ = mac.Write(body)
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, signature := range received {
		if hmac.Equal([]byte(signature), []byte(want)) {
			return nil
		}
	}
	return fmt.Errorf("gitlab: signature does not match")
}

func (gitlabAdapter) Parse(r *http.Request, body []byte) ([]Event, error) {
	deliveryID := strings.TrimSpace(r.Header.Get("webhook-id"))
	if deliveryID == "" {
		// Legacy deliveries without a signing token used Idempotency-Key. Fernary
		// registers signed hooks, but retaining this fallback makes captured payload
		// tests and in-flight migrations deduplicate correctly.
		deliveryID = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	if deliveryID == "" {
		return nil, fmt.Errorf("gitlab: delivery has no webhook id")
	}

	var p gitlabWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("gitlab: unreadable payload: %w", err)
	}
	if p.Project.ID <= 0 {
		return nil, fmt.Errorf("gitlab: payload has no project id")
	}
	projectID := strconv.FormatInt(p.Project.ID, 10)
	ev := Event{
		Key: deliveryID, ResourceID: projectID, OccurredAt: time.Now().UTC(),
		Data: map[string]any{
			"project_id": projectID, "project": p.Project.PathWithNamespace,
			"project_url": p.Project.WebURL,
		},
	}
	eventName := r.Header.Get("X-Gitlab-Event")

	switch {
	case eventName == "Merge Request Hook" && p.ObjectKind == "merge_request":
		switch p.ObjectAttributes.Action {
		case "open":
			ev.Type = "merge_request.opened"
		case "merge":
			ev.Type = "merge_request.merged"
		default:
			return nil, nil
		}
		putAll(ev.Data, map[string]any{
			"number": p.ObjectAttributes.IID, "title": p.ObjectAttributes.Title,
			"body": p.ObjectAttributes.Description, "url": p.ObjectAttributes.URL,
			"author": p.User.Username, "base": p.ObjectAttributes.TargetBranch,
			"head": p.ObjectAttributes.SourceBranch, "state": p.ObjectAttributes.State,
			"labels": gitlabLabelNames(p.Labels), "label": gitlabLabelNames(p.Labels),
		})

	case eventName == "Issue Hook" && p.ObjectKind == "issue":
		switch p.ObjectAttributes.Action {
		case "open":
			ev.Type = "issues.opened"
		case "update":
			ev.Type = "issues.edited"
		default:
			return nil, nil
		}
		labels := gitlabLabelNames(p.Labels)
		putAll(ev.Data, map[string]any{
			"number": p.ObjectAttributes.IID, "title": p.ObjectAttributes.Title,
			"body": p.ObjectAttributes.Description, "url": p.ObjectAttributes.URL,
			"author": p.User.Username, "state": p.ObjectAttributes.State,
			"labels": labels, "label": labels,
		})
		if p.ObjectAttributes.Action == "update" {
			changed := make([]string, 0, len(p.Changes))
			for field := range p.Changes {
				changed = append(changed, field)
			}
			sort.Strings(changed)
			ev.Data["changed_fields"] = changed
			if previous, ok := gitlabPreviousValue(p.Changes["title"]); ok {
				ev.Data["previous_title"] = previous
			}
			if previous, ok := gitlabPreviousValue(p.Changes["description"]); ok {
				ev.Data["previous_body"] = previous
			}
		}

	case eventName == "Note Hook" && p.ObjectKind == "note":
		if p.ObjectAttributes.Action != "create" {
			return nil, nil
		}
		noteableType := strings.ToLower(strings.TrimSpace(p.ObjectAttributes.NoteableType))
		if noteableType != "issue" && noteableType != "mergerequest" && noteableType != "merge_request" {
			return nil, nil
		}
		ev.Type = "note.created"
		putAll(ev.Data, map[string]any{
			"comment_id": p.ObjectAttributes.ID, "body": p.ObjectAttributes.Note,
			"url": p.ObjectAttributes.URL, "author": p.User.Username,
			"noteable_type":    p.ObjectAttributes.NoteableType,
			"is_merge_request": noteableType != "issue",
		})
		if p.Issue != nil {
			labels := gitlabLabelNames(p.Issue.Labels)
			putAll(ev.Data, map[string]any{
				"number": p.Issue.IID, "title": p.Issue.Title,
				"issue_body": p.Issue.Description, "issue_url": p.Issue.URL,
				"labels": labels, "label": labels,
			})
		} else if p.MergeRequest != nil {
			putAll(ev.Data, map[string]any{
				"number": p.MergeRequest.IID, "title": p.MergeRequest.Title,
				"merge_request_body": p.MergeRequest.Description,
				"merge_request_url":  p.MergeRequest.URL,
				"base":               p.MergeRequest.TargetBranch, "head": p.MergeRequest.SourceBranch,
			})
		}

	case eventName == "Push Hook" && p.ObjectKind == "push":
		ev.Type = "push"
		messages := make([]string, 0, len(p.Commits))
		commitIDs := make([]string, 0, len(p.Commits))
		for _, commit := range p.Commits {
			messages = append(messages, commit.Message)
			commitIDs = append(commitIDs, commit.ID)
		}
		count := p.TotalCommitsCount
		if count == 0 {
			count = len(p.Commits)
		}
		author := p.UserUsername
		if author == "" {
			author = p.User.Username
		}
		putAll(ev.Data, map[string]any{
			"branch": strings.TrimPrefix(p.Ref, "refs/heads/"), "ref": p.Ref,
			"before": p.Before, "after": p.After, "author": author,
			"commit_count": count, "commit_ids": commitIDs, "messages": messages,
		})

	case eventName == "Release Hook" && p.ObjectKind == "release":
		if p.Action != "create" {
			return nil, nil
		}
		ev.Type = "release.published"
		putAll(ev.Data, map[string]any{
			"release_id": p.ID, "tag": p.Tag, "name": p.Name,
			"body": p.Description, "url": p.URL,
		})

	default:
		return nil, nil
	}

	return []Event{ev}, nil
}

type gitlabWebhookPayload struct {
	ID                int64  `json:"id"`
	ObjectKind        string `json:"object_kind"`
	EventType         string `json:"event_type"`
	Action            string `json:"action"`
	Ref               string `json:"ref"`
	Before            string `json:"before"`
	After             string `json:"after"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Tag               string `json:"tag"`
	URL               string `json:"url"`
	UserUsername      string `json:"user_username"`
	TotalCommitsCount int    `json:"total_commits_count"`
	User              struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Username string `json:"username"`
	} `json:"user"`
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
	} `json:"project"`
	ObjectAttributes struct {
		ID           int64  `json:"id"`
		IID          int64  `json:"iid"`
		Action       string `json:"action"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		URL          string `json:"url"`
		State        string `json:"state"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		Note         string `json:"note"`
		NoteableType string `json:"noteable_type"`
	} `json:"object_attributes"`
	Labels  []gitlabLabel              `json:"labels"`
	Changes map[string]json.RawMessage `json:"changes"`
	Commits []struct {
		ID      string `json:"id"`
		Message string `json:"message"`
		URL     string `json:"url"`
	} `json:"commits"`
	Issue *struct {
		IID         int64         `json:"iid"`
		Title       string        `json:"title"`
		Description string        `json:"description"`
		URL         string        `json:"url"`
		Labels      []gitlabLabel `json:"labels"`
	} `json:"issue"`
	MergeRequest *struct {
		IID          int64  `json:"iid"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		URL          string `json:"url"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
	} `json:"merge_request"`
}

type gitlabLabel struct {
	Title string `json:"title"`
}

func gitlabLabelNames(labels []gitlabLabel) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if title := strings.TrimSpace(label.Title); title != "" {
			out = append(out, title)
		}
	}
	return out
}

func gitlabPreviousValue(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var change struct {
		Previous any `json:"previous"`
	}
	if err := json.Unmarshal(raw, &change); err != nil {
		return nil, false
	}
	return change.Previous, true
}

func putAll(dst, src map[string]any) {
	for key, value := range src {
		dst[key] = value
	}
}

func triggerFilter(t *models.IntegrationTrigger, key string) string {
	if t == nil || len(t.Filters) == 0 {
		return ""
	}
	var filters map[string]string
	if json.Unmarshal(t.Filters, &filters) != nil {
		return ""
	}
	return strings.TrimSpace(filters[key])
}

func newGitLabSigningToken() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("gitlab: could not generate a signing token: %w", err)
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(key), nil
}

func gitlabProjectID(value string) (string, error) {
	id, err := positiveDecimal(value)
	if err != nil {
		return "", fmt.Errorf("gitlab: select a valid project: %w", err)
	}
	return id, nil
}

func positiveDecimal(value string) (string, error) {
	value = strings.TrimSpace(value)
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("%q is not a positive numeric id", value)
	}
	return strconv.FormatInt(id, 10), nil
}

type gitlabHookAPIError struct {
	Status int
	Body   string
}

func (e *gitlabHookAPIError) Error() string {
	return fmt.Sprintf("GitLab API returned %d: %s", e.Status, truncate(e.Body, 300))
}

func gitlabRegistrationError(projectID string, err error) error {
	apiErr, ok := err.(*gitlabHookAPIError)
	if !ok {
		return err
	}
	switch apiErr.Status {
	case http.StatusUnauthorized:
		return fmt.Errorf("gitlab: reconnect GitLab so Fernary can register a webhook on project %s", projectID)
	case http.StatusForbidden, http.StatusNotFound:
		return fmt.Errorf("gitlab: the connected user must be a Maintainer or Owner of project %s to create webhooks", projectID)
	default:
		return err
	}
}

func gitlabHookAPI(ctx context.Context, token, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("gitlab: encode webhook request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, gitlabHooksAPIBase+path, reader)
	if err != nil {
		return fmt.Errorf("gitlab: build webhook request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab: webhook request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &gitlabHookAPIError{Status: resp.StatusCode, Body: gitlabErrorMessage(raw)}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("gitlab: parse webhook response: %w", err)
		}
	}
	return nil
}

func gitlabErrorMessage(raw []byte) string {
	var envelope struct {
		Error   string `json:"error"`
		Message any    `json:"message"`
	}
	if json.Unmarshal(raw, &envelope) == nil {
		if envelope.Error != "" {
			return envelope.Error
		}
		if envelope.Message != nil {
			if text, ok := envelope.Message.(string); ok {
				return text
			}
			if encoded, err := json.Marshal(envelope.Message); err == nil {
				return string(encoded)
			}
		}
	}
	return string(bytes.TrimSpace(raw))
}
