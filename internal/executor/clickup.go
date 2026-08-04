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

// ClickUp API v2.
//
// Two things to know before reading the ops:
//
//   - The Authorization header differs by credential type. A personal token
//     (pk_…) is sent raw; an OAuth access token needs a Bearer prefix. Sending
//     the wrong form is a 401, so clickupAuth picks based on the prefix — which
//     also means a user can paste a personal token and have it just work.
//   - ClickUp's OAuth has no scopes. Consent is per workspace, so a connection
//     can reach everything in the workspaces the user selected and nothing
//     outside them. There is no way to request narrower access.
//
// The id hierarchy is workspace (called team in the API) → space → folder → list
// → task, and most endpoints hang off exactly one level, so the ops name which
// id they need.

const clickupAPI = "https://api.clickup.com/api/v2"

// clickupAuth formats the header for whichever credential type this is.
func clickupAuth(token string) string {
	if strings.HasPrefix(strings.TrimSpace(token), "pk_") {
		return token
	}
	return "Bearer " + token
}

func clickupCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, clickupAPI+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", clickupAuth(token))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("clickup request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Err   string `json:"err"`
			ECode string `json:"ECODE"`
		}
		msg := ""
		if json.Unmarshal(raw, &e) == nil && e.Err != "" {
			msg = e.Err
			if e.ECode != "" {
				msg += " (" + e.ECode + ")"
			}
		}
		if msg == "" {
			msg = truncateStr(string(raw), 300)
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			msg += " — reconnect ClickUp, or check the token is a workspace the connection can see"
		case http.StatusForbidden:
			msg += " — the connected account may not have access to this workspace"
		case http.StatusTooManyRequests:
			msg += " — ClickUp's rate limit depends on plan tier (100/min on Free Forever)"
		}
		return "", fmt.Errorf("ClickUp API error (%d): %s", resp.StatusCode, msg)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

func runClickUp(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	// A custom task id is only resolvable with the workspace it belongs to, so
	// those two parameters always travel together.
	taskQuery := func() string {
		if !strings.EqualFold(sub(d.ClickUpCustomTaskIds), "true") {
			return ""
		}
		if sub(d.ClickUpWorkspaceId) == "" {
			return ""
		}
		return "?custom_task_ids=true&team_id=" + url.QueryEscape(sub(d.ClickUpWorkspaceId))
	}
	need := func(label, v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("this operation needs %s", label)
		}
		return nil
	}

	switch d.IntegrationOp {
	// ---- hierarchy ----
	case "list_workspaces":
		return clickupCall(ctx, token, http.MethodGet, "/team", nil)

	case "list_spaces":
		if err := need("a workspace ID", sub(d.ClickUpWorkspaceId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet, "/team/"+sub(d.ClickUpWorkspaceId)+"/space", nil)

	case "get_space":
		if err := need("a space ID", sub(d.ClickUpSpaceId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet, "/space/"+sub(d.ClickUpSpaceId), nil)

	case "list_folders":
		if err := need("a space ID", sub(d.ClickUpSpaceId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet, "/space/"+sub(d.ClickUpSpaceId)+"/folder", nil)

	case "list_lists":
		// Lists can sit in a folder or directly in a space; both are common.
		if f := sub(d.ClickUpFolderId); f != "" {
			return clickupCall(ctx, token, http.MethodGet, "/folder/"+f+"/list", nil)
		}
		if err := need("a folder ID or a space ID", sub(d.ClickUpSpaceId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet, "/space/"+sub(d.ClickUpSpaceId)+"/list", nil)

	case "get_list":
		if err := need("a list ID", sub(d.ClickUpListId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet, "/list/"+sub(d.ClickUpListId), nil)

	case "create_list":
		if err := need("a name", sub(d.ClickUpName)); err != nil {
			return "", err
		}
		payload := map[string]any{"name": sub(d.ClickUpName)}
		if f := sub(d.ClickUpFolderId); f != "" {
			return clickupCall(ctx, token, http.MethodPost, "/folder/"+f+"/list", payload)
		}
		if err := need("a folder ID or a space ID", sub(d.ClickUpSpaceId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodPost, "/space/"+sub(d.ClickUpSpaceId)+"/list", payload)

	// ---- tasks ----
	case "list_tasks":
		if err := need("a list ID", sub(d.ClickUpListId)); err != nil {
			return "", err
		}
		q := url.Values{}
		if n := intOr(d.ClickUpLimit, 0); n > 0 {
			// ClickUp pages at 100; a limit smaller than that still needs the page.
			q.Set("page", "0")
		}
		if strings.EqualFold(sub(d.ClickUpSubtasks), "true") {
			q.Set("subtasks", "true")
		}
		if strings.EqualFold(sub(d.ClickUpIncludeClosed), "true") {
			q.Set("include_closed", "true")
		}
		for _, s := range splitCSV(sub(d.ClickUpStatuses)) {
			q.Add("statuses[]", s)
		}
		for _, a := range splitCSV(sub(d.ClickUpAssignees)) {
			q.Add("assignees[]", a)
		}
		if v := sub(d.ClickUpOrderBy); v != "" {
			q.Set("order_by", v)
		}
		path := "/list/" + sub(d.ClickUpListId) + "/task"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		raw, err := clickupCall(ctx, token, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "search_tasks":
		// The only way to query across lists; scoped to one workspace.
		if err := need("a workspace ID", sub(d.ClickUpWorkspaceId)); err != nil {
			return "", err
		}
		q := url.Values{}
		for _, s := range splitCSV(sub(d.ClickUpStatuses)) {
			q.Add("statuses[]", s)
		}
		for _, a := range splitCSV(sub(d.ClickUpAssignees)) {
			q.Add("assignees[]", a)
		}
		for _, l := range splitCSV(sub(d.ClickUpListId)) {
			q.Add("list_ids[]", l)
		}
		if strings.EqualFold(sub(d.ClickUpIncludeClosed), "true") {
			q.Set("include_closed", "true")
		}
		if v := sub(d.ClickUpOrderBy); v != "" {
			q.Set("order_by", v)
		}
		path := "/team/" + sub(d.ClickUpWorkspaceId) + "/task"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		raw, err := clickupCall(ctx, token, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "get_task":
		if err := need("a task ID", sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet,
			"/task/"+sub(d.ClickUpTaskId)+taskQuery(), nil)

	case "create_task":
		if err := need("a list ID", sub(d.ClickUpListId)); err != nil {
			return "", err
		}
		if err := need("a task name", sub(d.ClickUpName)); err != nil {
			return "", err
		}
		payload, err := clickupTaskPayload(d, sub, true)
		if err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/list/"+sub(d.ClickUpListId)+"/task", payload)

	case "update_task":
		if err := need("a task ID", sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		payload, err := clickupTaskPayload(d, sub, false)
		if err != nil {
			return "", err
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("update_task needs at least one field to change")
		}
		return clickupCall(ctx, token, http.MethodPut,
			"/task/"+sub(d.ClickUpTaskId)+taskQuery(), payload)

	case "delete_task":
		if err := need("a task ID", sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodDelete,
			"/task/"+sub(d.ClickUpTaskId)+taskQuery(), nil)

	// ---- comments ----
	case "list_comments":
		if err := need("a task ID", sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		raw, err := clickupCall(ctx, token, http.MethodGet,
			"/task/"+sub(d.ClickUpTaskId)+"/comment", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "create_comment":
		if err := need("a task ID", sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		if err := need("comment text", sub(d.ClickUpComment)); err != nil {
			return "", err
		}
		payload := map[string]any{"comment_text": sub(d.ClickUpComment), "notify_all": false}
		if v := sub(d.ClickUpAssignees); v != "" {
			// A single assignee turns the comment into an assigned comment.
			if ids := splitCSV(v); len(ids) == 1 {
				payload["assignee"] = ids[0]
			}
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/task/"+sub(d.ClickUpTaskId)+"/comment", payload)

	case "update_comment":
		if err := need("a comment ID", sub(d.ClickUpCommentId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodPut, "/comment/"+sub(d.ClickUpCommentId),
			map[string]any{"comment_text": sub(d.ClickUpComment)})

	case "delete_comment":
		if err := need("a comment ID", sub(d.ClickUpCommentId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodDelete, "/comment/"+sub(d.ClickUpCommentId), nil)

	// ---- checklists ----
	case "create_checklist":
		if err := need("a task ID", sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/task/"+sub(d.ClickUpTaskId)+"/checklist",
			map[string]any{"name": firstNonEmpty(sub(d.ClickUpName), "Checklist")})

	case "create_checklist_item":
		if err := need("a checklist ID", sub(d.ClickUpChecklistId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/checklist/"+sub(d.ClickUpChecklistId)+"/checklist_item",
			map[string]any{"name": sub(d.ClickUpName)})

	case "update_checklist_item":
		if err := need("a checklist ID and item ID", sub(d.ClickUpChecklistId)+sub(d.ClickUpChecklistItemId)); err != nil {
			return "", err
		}
		payload := map[string]any{}
		if v := sub(d.ClickUpName); v != "" {
			payload["name"] = v
		}
		if v := sub(d.ClickUpResolved); v != "" {
			payload["resolved"] = strings.EqualFold(v, "true")
		}
		return clickupCall(ctx, token, http.MethodPut,
			"/checklist/"+sub(d.ClickUpChecklistId)+"/checklist_item/"+sub(d.ClickUpChecklistItemId),
			payload)

	case "delete_checklist":
		if err := need("a checklist ID", sub(d.ClickUpChecklistId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodDelete,
			"/checklist/"+sub(d.ClickUpChecklistId), nil)

	// ---- tags ----
	case "list_space_tags":
		if err := need("a space ID", sub(d.ClickUpSpaceId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet, "/space/"+sub(d.ClickUpSpaceId)+"/tag", nil)

	case "add_tag_to_task":
		if err := need("a task ID and a tag name", sub(d.ClickUpTaskId)+sub(d.ClickUpTagName)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/task/"+sub(d.ClickUpTaskId)+"/tag/"+url.PathEscape(sub(d.ClickUpTagName))+taskQuery(), nil)

	case "remove_tag_from_task":
		if err := need("a task ID and a tag name", sub(d.ClickUpTaskId)+sub(d.ClickUpTagName)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodDelete,
			"/task/"+sub(d.ClickUpTaskId)+"/tag/"+url.PathEscape(sub(d.ClickUpTagName))+taskQuery(), nil)

	// ---- custom fields ----
	case "list_custom_fields":
		if err := need("a list ID", sub(d.ClickUpListId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet, "/list/"+sub(d.ClickUpListId)+"/field", nil)

	case "set_custom_field_value":
		if err := need("a task ID and a field ID", sub(d.ClickUpTaskId)+sub(d.ClickUpFieldId)); err != nil {
			return "", err
		}
		// The value's type has to match the field, so it is parsed as JSON when it
		// looks like JSON and passed as a string otherwise.
		raw := strings.TrimSpace(sub(d.ClickUpFieldValue))
		var value any = raw
		if raw != "" && strings.ContainsAny(raw[:1], "[{0123456789-tfn\"") {
			var parsed any
			if json.Unmarshal([]byte(raw), &parsed) == nil {
				value = parsed
			}
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/task/"+sub(d.ClickUpTaskId)+"/field/"+sub(d.ClickUpFieldId)+taskQuery(),
			map[string]any{"value": value})

	case "remove_custom_field_value":
		if err := need("a task ID and a field ID", sub(d.ClickUpTaskId)+sub(d.ClickUpFieldId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodDelete,
			"/task/"+sub(d.ClickUpTaskId)+"/field/"+sub(d.ClickUpFieldId)+taskQuery(), nil)

	// ---- dependencies & links ----
	case "add_dependency":
		if err := need("a task ID", sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		payload := map[string]any{}
		// depends_on means "this task waits for that one"; dependency_of is the
		// reverse. Exactly one must be set.
		switch {
		case sub(d.ClickUpDependsOn) != "":
			payload["depends_on"] = sub(d.ClickUpDependsOn)
		case sub(d.ClickUpDependencyOf) != "":
			payload["dependency_of"] = sub(d.ClickUpDependencyOf)
		default:
			return "", fmt.Errorf("add_dependency needs either a task this one waits for, " +
				"or a task that waits for this one")
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/task/"+sub(d.ClickUpTaskId)+"/dependency"+taskQuery(), payload)

	case "delete_dependency":
		if err := need("a task ID", sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		q := url.Values{}
		if v := sub(d.ClickUpDependsOn); v != "" {
			q.Set("depends_on", v)
		}
		if v := sub(d.ClickUpDependencyOf); v != "" {
			q.Set("dependency_of", v)
		}
		if len(q) == 0 {
			return "", fmt.Errorf("delete_dependency needs the other task in the relationship")
		}
		return clickupCall(ctx, token, http.MethodDelete,
			"/task/"+sub(d.ClickUpTaskId)+"/dependency?"+q.Encode(), nil)

	case "link_tasks":
		if err := need("both task IDs", sub(d.ClickUpTaskId)+sub(d.ClickUpLinksTo)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/task/"+sub(d.ClickUpTaskId)+"/link/"+sub(d.ClickUpLinksTo)+taskQuery(), nil)

	case "unlink_tasks":
		if err := need("both task IDs", sub(d.ClickUpTaskId)+sub(d.ClickUpLinksTo)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodDelete,
			"/task/"+sub(d.ClickUpTaskId)+"/link/"+sub(d.ClickUpLinksTo)+taskQuery(), nil)

	// ---- time tracking ----
	case "list_time_entries":
		if err := need("a workspace ID", sub(d.ClickUpWorkspaceId)); err != nil {
			return "", err
		}
		q := url.Values{}
		if v := sub(d.ClickUpStartDate); v != "" {
			q.Set("start_date", v)
		}
		if v := sub(d.ClickUpEndDate); v != "" {
			q.Set("end_date", v)
		}
		path := "/team/" + sub(d.ClickUpWorkspaceId) + "/time_entries"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		raw, err := clickupCall(ctx, token, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "create_time_entry":
		if err := need("a workspace ID and a task ID", sub(d.ClickUpWorkspaceId)+sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		if sub(d.ClickUpDuration) == "" {
			return "", fmt.Errorf("create_time_entry needs a duration in milliseconds")
		}
		ms, err := atoiSafe(sub(d.ClickUpDuration))
		if err != nil {
			return "", fmt.Errorf("duration must be a whole number of milliseconds")
		}
		payload := map[string]any{"tid": sub(d.ClickUpTaskId), "duration": ms}
		if v := sub(d.ClickUpStartDate); v != "" {
			start, err := atoiSafe(v)
			if err != nil {
				return "", fmt.Errorf("start must be a unix timestamp in milliseconds")
			}
			payload["start"] = start
		}
		if v := sub(d.ClickUpComment); v != "" {
			payload["description"] = v
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/team/"+sub(d.ClickUpWorkspaceId)+"/time_entries", payload)

	case "get_running_timer":
		if err := need("a workspace ID", sub(d.ClickUpWorkspaceId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet,
			"/team/"+sub(d.ClickUpWorkspaceId)+"/time_entries/current", nil)

	case "start_timer":
		if err := need("a workspace ID and a task ID", sub(d.ClickUpWorkspaceId)+sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/team/"+sub(d.ClickUpWorkspaceId)+"/time_entries/start",
			map[string]any{"tid": sub(d.ClickUpTaskId), "description": sub(d.ClickUpComment)})

	case "stop_timer":
		if err := need("a workspace ID", sub(d.ClickUpWorkspaceId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/team/"+sub(d.ClickUpWorkspaceId)+"/time_entries/stop", nil)

	// ---- attachments, goals, members, views ----
	case "list_attachments":
		// Attachments come back on the task itself; there is no list endpoint.
		if err := need("a task ID", sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet,
			"/task/"+sub(d.ClickUpTaskId)+taskQuery(), nil)

	case "list_goals":
		if err := need("a workspace ID", sub(d.ClickUpWorkspaceId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet,
			"/team/"+sub(d.ClickUpWorkspaceId)+"/goal", nil)

	case "create_goal":
		if err := need("a workspace ID and a name", sub(d.ClickUpWorkspaceId)+sub(d.ClickUpName)); err != nil {
			return "", err
		}
		payload := map[string]any{
			"name":            sub(d.ClickUpName),
			"multiple_owners": false,
			"description":     sub(d.ClickUpDescription),
		}
		if v := sub(d.ClickUpDueDate); v != "" {
			due, err := atoiSafe(v)
			if err != nil {
				return "", fmt.Errorf("goal due date must be a unix timestamp in milliseconds")
			}
			payload["due_date"] = due
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/team/"+sub(d.ClickUpWorkspaceId)+"/goal", payload)

	case "list_list_members":
		if err := need("a list ID", sub(d.ClickUpListId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet, "/list/"+sub(d.ClickUpListId)+"/member", nil)

	case "list_task_members":
		if err := need("a task ID", sub(d.ClickUpTaskId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet, "/task/"+sub(d.ClickUpTaskId)+"/member", nil)

	case "list_views":
		// Views hang off whichever level was given.
		switch {
		case sub(d.ClickUpListId) != "":
			return clickupCall(ctx, token, http.MethodGet, "/list/"+sub(d.ClickUpListId)+"/view", nil)
		case sub(d.ClickUpFolderId) != "":
			return clickupCall(ctx, token, http.MethodGet, "/folder/"+sub(d.ClickUpFolderId)+"/view", nil)
		case sub(d.ClickUpSpaceId) != "":
			return clickupCall(ctx, token, http.MethodGet, "/space/"+sub(d.ClickUpSpaceId)+"/view", nil)
		default:
			return "", fmt.Errorf("list_views needs a list, folder or space ID")
		}

	// ---- webhooks & account ----
	case "list_webhooks":
		if err := need("a workspace ID", sub(d.ClickUpWorkspaceId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodGet,
			"/team/"+sub(d.ClickUpWorkspaceId)+"/webhook", nil)

	case "create_webhook":
		if err := need("a workspace ID and an endpoint URL", sub(d.ClickUpWorkspaceId)+sub(d.ClickUpUrl)); err != nil {
			return "", err
		}
		events := splitCSV(sub(d.ClickUpEvents))
		if len(events) == 0 {
			events = []string{"taskCreated", "taskUpdated"}
		}
		payload := map[string]any{"endpoint": sub(d.ClickUpUrl), "events": events}
		if v := sub(d.ClickUpListId); v != "" {
			payload["list_id"] = v
		}
		return clickupCall(ctx, token, http.MethodPost,
			"/team/"+sub(d.ClickUpWorkspaceId)+"/webhook", payload)

	case "delete_webhook":
		if err := need("a webhook ID", sub(d.ClickUpWebhookId)); err != nil {
			return "", err
		}
		return clickupCall(ctx, token, http.MethodDelete, "/webhook/"+sub(d.ClickUpWebhookId), nil)

	case "get_authorized_user":
		return clickupCall(ctx, token, http.MethodGet, "/user", nil)

	case "":
		return "", fmt.Errorf("no ClickUp operation selected")
	}
	return "", fmt.Errorf("unsupported ClickUp operation: %s", d.IntegrationOp)
}

// clickupTaskPayload assembles a create or update body. On update, only provided
// fields are sent so an untouched field is not cleared.
func clickupTaskPayload(d FlowNodeData, sub func(string) string, forCreate bool) (map[string]any, error) {
	payload := map[string]any{}
	if v := sub(d.ClickUpName); v != "" {
		payload["name"] = v
	}
	if v := sub(d.ClickUpDescription); v != "" {
		payload["description"] = v
	}
	if v := sub(d.ClickUpStatus); v != "" {
		payload["status"] = v
	}
	if v := sub(d.ClickUpPriority); v != "" {
		// ClickUp encodes priority as 1 (urgent) to 4 (low), not a name.
		p, err := atoiSafe(v)
		if err != nil || p < 1 || p > 4 {
			return nil, fmt.Errorf("priority must be 1 (urgent), 2 (high), 3 (normal) or 4 (low)")
		}
		payload["priority"] = p
	}
	if v := sub(d.ClickUpDueDate); v != "" {
		due, err := atoiSafe(v)
		if err != nil {
			return nil, fmt.Errorf("due date must be a unix timestamp in milliseconds")
		}
		payload["due_date"] = due
	}
	if v := sub(d.ClickUpTimeEstimate); v != "" {
		est, err := atoiSafe(v)
		if err != nil {
			return nil, fmt.Errorf("time estimate must be a whole number of milliseconds")
		}
		payload["time_estimate"] = est
	}
	if v := sub(d.ClickUpParent); v != "" {
		payload["parent"] = v
	}
	if ids := splitCSV(sub(d.ClickUpAssignees)); len(ids) > 0 {
		nums := make([]any, 0, len(ids))
		for _, id := range ids {
			n, err := atoiSafe(id)
			if err != nil {
				return nil, fmt.Errorf("assignees must be numeric ClickUp user IDs, not names or emails")
			}
			nums = append(nums, n)
		}
		if forCreate {
			payload["assignees"] = nums
		} else {
			// Update takes an add/rem object rather than a flat list.
			payload["assignees"] = map[string]any{"add": nums}
		}
	}
	if tags := splitCSV(sub(d.ClickUpTagName)); len(tags) > 0 && forCreate {
		payload["tags"] = tags
	}
	return payload, nil
}
