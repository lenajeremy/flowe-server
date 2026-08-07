package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var asanaAPIURL = "https://app.asana.com/api/1.0"

func asanaCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(map[string]any{"data": body})
		if err != nil {
			return "", fmt.Errorf("encode Asana request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(asanaAPIURL, "/")+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("Asana request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope struct {
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		message := truncateStr(string(raw), 300)
		if json.Unmarshal(raw, &envelope) == nil && len(envelope.Errors) > 0 {
			message = envelope.Errors[0].Message
		}
		if resp.StatusCode == http.StatusUnauthorized {
			message += " — reconnect Asana to refresh its grant"
		}
		return "", fmt.Errorf("Asana API error (%d): %s", resp.StatusCode, message)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

func runAsana(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(value string) string { return substituteTemplates(value, outputs) }
	need := func(label, value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("this operation needs %s", label)
		}
		return nil
	}
	workspace := sub(d.AsanaWorkspaceId)
	project := sub(d.AsanaProjectId)
	section := sub(d.AsanaSectionId)
	task := sub(d.AsanaTaskId)
	parent := sub(d.AsanaParentTaskId)
	limit := intOr(d.AsanaLimit, 50)
	if limit < 1 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	compactFields := "gid,name,resource_type"
	taskFields := "gid,name,notes,completed,completed_at,due_on,permalink_url,assignee.gid,assignee.name,projects.gid,projects.name,memberships.project.gid,memberships.section.gid"

	switch d.IntegrationOp {
	case "list_workspaces":
		return asanaCall(ctx, token, http.MethodGet, "/workspaces?limit="+strconv.Itoa(limit)+"&opt_fields="+url.QueryEscape(compactFields), nil)

	case "list_projects":
		if err := need("a workspace ID", workspace); err != nil {
			return "", err
		}
		return asanaCall(ctx, token, http.MethodGet, "/workspaces/"+url.PathEscape(workspace)+"/projects?archived=false&limit="+strconv.Itoa(limit)+"&opt_fields="+url.QueryEscape(compactFields), nil)

	case "list_sections":
		if err := need("a project ID", project); err != nil {
			return "", err
		}
		return asanaCall(ctx, token, http.MethodGet, "/projects/"+url.PathEscape(project)+"/sections?limit="+strconv.Itoa(limit)+"&opt_fields="+url.QueryEscape(compactFields), nil)

	case "list_tasks":
		if err := need("a project ID", project); err != nil {
			return "", err
		}
		return asanaCall(ctx, token, http.MethodGet, "/projects/"+url.PathEscape(project)+"/tasks?limit="+strconv.Itoa(limit)+"&opt_fields="+url.QueryEscape(taskFields), nil)

	case "get_task":
		if err := need("a task ID", task); err != nil {
			return "", err
		}
		return asanaCall(ctx, token, http.MethodGet, "/tasks/"+url.PathEscape(task)+"?opt_fields="+url.QueryEscape(taskFields), nil)

	case "create_task":
		if err := need("a workspace ID", workspace); err != nil {
			return "", err
		}
		name := sub(d.AsanaName)
		if err := need("a task name", name); err != nil {
			return "", err
		}
		payload, err := asanaTaskPayload(d, sub, true)
		if err != nil {
			return "", err
		}
		payload["workspace"] = workspace
		if section != "" {
			if err := need("a project ID when a section is selected", project); err != nil {
				return "", err
			}
			payload["memberships"] = []map[string]string{{"project": project, "section": section}}
		} else if project != "" {
			payload["projects"] = []string{project}
		}
		return asanaCall(ctx, token, http.MethodPost, "/tasks?opt_fields="+url.QueryEscape(taskFields), payload)

	case "create_subtask":
		if err := need("a parent task ID", parent); err != nil {
			return "", err
		}
		if err := need("a task name", sub(d.AsanaName)); err != nil {
			return "", err
		}
		payload, err := asanaTaskPayload(d, sub, true)
		if err != nil {
			return "", err
		}
		return asanaCall(ctx, token, http.MethodPost, "/tasks/"+url.PathEscape(parent)+"/subtasks?opt_fields="+url.QueryEscape(taskFields), payload)

	case "update_task":
		if err := need("a task ID", task); err != nil {
			return "", err
		}
		payload, err := asanaTaskPayload(d, sub, false)
		if err != nil {
			return "", err
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("update_task needs at least one field to change")
		}
		return asanaCall(ctx, token, http.MethodPut, "/tasks/"+url.PathEscape(task)+"?opt_fields="+url.QueryEscape(taskFields), payload)

	case "delete_task":
		if err := need("a task ID", task); err != nil {
			return "", err
		}
		return asanaCall(ctx, token, http.MethodDelete, "/tasks/"+url.PathEscape(task), nil)

	case "add_comment":
		if err := need("a task ID", task); err != nil {
			return "", err
		}
		comment := sub(d.AsanaComment)
		if err := need("a comment", comment); err != nil {
			return "", err
		}
		return asanaCall(ctx, token, http.MethodPost, "/tasks/"+url.PathEscape(task)+"/stories", map[string]any{"text": comment})

	case "list_comments":
		if err := need("a task ID", task); err != nil {
			return "", err
		}
		fields := "gid,text,created_at,created_by.gid,created_by.name,resource_subtype"
		return asanaCall(ctx, token, http.MethodGet, "/tasks/"+url.PathEscape(task)+"/stories?limit="+strconv.Itoa(limit)+"&opt_fields="+url.QueryEscape(fields), nil)

	case "add_task_to_project":
		if err := need("a task ID", task); err != nil {
			return "", err
		}
		if err := need("a project ID", project); err != nil {
			return "", err
		}
		payload := map[string]any{"project": project}
		if section != "" {
			payload["section"] = section
		}
		return asanaCall(ctx, token, http.MethodPost, "/tasks/"+url.PathEscape(task)+"/addProject", payload)

	default:
		return "", fmt.Errorf("unknown Asana operation %q", d.IntegrationOp)
	}
}

func asanaTaskPayload(d FlowNodeData, sub func(string) string, creating bool) (map[string]any, error) {
	payload := map[string]any{}
	if value := sub(d.AsanaName); value != "" {
		payload["name"] = value
	}
	if value := sub(d.AsanaNotes); value != "" {
		payload["notes"] = value
	}
	if value := sub(d.AsanaAssignee); value != "" {
		payload["assignee"] = value
	}
	if value := sub(d.AsanaDueOn); value != "" {
		payload["due_on"] = value
	}
	if value := strings.TrimSpace(sub(d.AsanaCompleted)); value != "" {
		completed, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("asanaCompleted must be true or false")
		}
		payload["completed"] = completed
	}
	if creating {
		delete(payload, "completed")
	}
	return payload, nil
}
