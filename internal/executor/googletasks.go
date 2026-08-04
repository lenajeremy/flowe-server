package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Google Tasks API v1. The simplest of the Google services: two nested
// collections, and "@default" always resolves to the user's primary list, so a
// node that names no list still works.
//
// One quirk worth knowing: Tasks stores only the date part of a due value. A
// time is accepted and then silently discarded, so "due today" is as precise as
// this API gets.

const tasksAPI = "https://tasks.googleapis.com/tasks/v1"

func runGoogleTasks(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.TasksLimit, 25)
	list := func() string { return firstNonEmpty(sub(d.TasksListId), "@default") }
	isTrue := func(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "true") }

	switch d.IntegrationOp {
	// ---- task lists ----
	case "list_task_lists":
		return googleCall(ctx, token, http.MethodGet,
			fmt.Sprintf("%s/users/@me/lists?maxResults=%d", tasksAPI, limit), nil)

	case "get_task_list":
		return googleCall(ctx, token, http.MethodGet, tasksAPI+"/users/@me/lists/"+list(), nil)

	case "create_task_list":
		if sub(d.TasksTitle) == "" {
			return "", fmt.Errorf("create_task_list needs a title")
		}
		return googleCall(ctx, token, http.MethodPost, tasksAPI+"/users/@me/lists",
			map[string]any{"title": sub(d.TasksTitle)})

	case "update_task_list":
		if sub(d.TasksTitle) == "" {
			return "", fmt.Errorf("update_task_list needs a title")
		}
		return googleCall(ctx, token, http.MethodPatch, tasksAPI+"/users/@me/lists/"+list(),
			map[string]any{"title": sub(d.TasksTitle)})

	case "delete_task_list":
		if sub(d.TasksListId) == "" {
			return "", fmt.Errorf("delete_task_list needs a list ID — refusing to delete the default list implicitly")
		}
		if _, err := googleCall(ctx, token, http.MethodDelete,
			tasksAPI+"/users/@me/lists/"+sub(d.TasksListId), nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"deletedList":%q}`, sub(d.TasksListId)), nil

	// ---- tasks ----
	case "list_tasks":
		q := url.Values{"maxResults": {fmt.Sprint(limit)}}
		// Completed tasks are hidden by default, which is usually what a workflow
		// wants; showCompleted opts back in.
		if isTrue(sub(d.TasksShowCompleted)) {
			q.Set("showCompleted", "true")
			q.Set("showHidden", "true")
		}
		if v := sub(d.TasksDueMin); v != "" {
			q.Set("dueMin", v)
		}
		if v := sub(d.TasksDueMax); v != "" {
			q.Set("dueMax", v)
		}
		raw, err := googleCall(ctx, token, http.MethodGet,
			tasksAPI+"/lists/"+list()+"/tasks?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_task":
		if sub(d.TasksTaskId) == "" {
			return "", fmt.Errorf("get_task needs a task ID")
		}
		return googleCall(ctx, token, http.MethodGet,
			tasksAPI+"/lists/"+list()+"/tasks/"+sub(d.TasksTaskId), nil)

	case "create_task":
		if sub(d.TasksTitle) == "" {
			return "", fmt.Errorf("create_task needs a title")
		}
		body := map[string]any{"title": sub(d.TasksTitle)}
		if v := sub(d.TasksNotes); v != "" {
			body["notes"] = v
		}
		if v := sub(d.TasksDue); v != "" {
			body["due"] = v
		}
		q := url.Values{}
		// parent makes it a subtask; previous positions it after a sibling.
		if v := sub(d.TasksParent); v != "" {
			q.Set("parent", v)
		}
		if v := sub(d.TasksPrevious); v != "" {
			q.Set("previous", v)
		}
		path := tasksAPI + "/lists/" + list() + "/tasks"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		return googleCall(ctx, token, http.MethodPost, path, body)

	case "update_task":
		if sub(d.TasksTaskId) == "" {
			return "", fmt.Errorf("update_task needs a task ID")
		}
		body := map[string]any{}
		if v := sub(d.TasksTitle); v != "" {
			body["title"] = v
		}
		if v := sub(d.TasksNotes); v != "" {
			body["notes"] = v
		}
		if v := sub(d.TasksDue); v != "" {
			body["due"] = v
		}
		if v := sub(d.TasksStatus); v != "" {
			body["status"] = v
		}
		if len(body) == 0 {
			return "", fmt.Errorf("update_task needs at least one field to change")
		}
		return googleCall(ctx, token, http.MethodPatch,
			tasksAPI+"/lists/"+list()+"/tasks/"+sub(d.TasksTaskId), body)

	case "complete_task":
		if sub(d.TasksTaskId) == "" {
			return "", fmt.Errorf("complete_task needs a task ID")
		}
		return googleCall(ctx, token, http.MethodPatch,
			tasksAPI+"/lists/"+list()+"/tasks/"+sub(d.TasksTaskId),
			map[string]any{"status": "completed"})

	case "delete_task":
		if sub(d.TasksTaskId) == "" {
			return "", fmt.Errorf("delete_task needs a task ID")
		}
		if _, err := googleCall(ctx, token, http.MethodDelete,
			tasksAPI+"/lists/"+list()+"/tasks/"+sub(d.TasksTaskId), nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"deletedTask":%q}`, sub(d.TasksTaskId)), nil

	case "move_task":
		if sub(d.TasksTaskId) == "" {
			return "", fmt.Errorf("move_task needs a task ID")
		}
		q := url.Values{}
		if v := sub(d.TasksParent); v != "" {
			q.Set("parent", v)
		}
		if v := sub(d.TasksPrevious); v != "" {
			q.Set("previous", v)
		}
		if v := sub(d.TasksDestinationList); v != "" {
			q.Set("destinationTasklist", v)
		}
		if len(q) == 0 {
			return "", fmt.Errorf("move_task needs a new parent, a previous sibling, or a destination list")
		}
		return googleCall(ctx, token, http.MethodPost,
			tasksAPI+"/lists/"+list()+"/tasks/"+sub(d.TasksTaskId)+"/move?"+q.Encode(), nil)

	case "clear_completed":
		// Hides every completed task on the list; the tasks themselves survive.
		if _, err := googleCall(ctx, token, http.MethodPost,
			tasksAPI+"/lists/"+list()+"/clear", nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"cleared":%q}`, list()), nil

	case "":
		return "", fmt.Errorf("no Google Tasks operation selected")
	}
	return "", fmt.Errorf("unsupported Google Tasks operation: %s", d.IntegrationOp)
}
