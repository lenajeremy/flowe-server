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

// Jira Cloud. Requests go through Atlassian's gateway rather than the site's own
// domain: https://api.atlassian.com/ex/jira/{cloudId}/rest/... The cloudId is
// captured at connect time and arrives here as the workspace value.
//
// Paths carry their own /rest prefix because Jira has two APIs at different
// roots — the platform API at /rest/api/3 and the Agile API (boards, sprints)
// at /rest/agile/1.0.

const (
	jiraAPI   = "/rest/api/3"
	jiraAgile = "/rest/agile/1.0"
)

// atlassianGateway fronts both Jira and Confluence. A var, not a const, so
// tests can point it at a stub server.
var atlassianGateway = "https://api.atlassian.com"

func jiraCall(ctx context.Context, token, cloudID, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, atlassianGateway+"/ex/jira/"+cloudID+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return jiraDo(req, "Jira")
}

// jiraDo runs the request and turns Atlassian's error shapes into one message.
func jiraDo(req *http.Request, product string) (string, error) {
	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s request failed: %w", strings.ToLower(product), err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", atlassianError(product, resp.StatusCode, raw)
	}
	// 204 on updates, transitions, deletes — report something useful instead of "".
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

// atlassianError unpacks the two error envelopes Atlassian uses: Jira's
// {errorMessages, errors} and Confluence's {message} / {errors:[{title,detail}]}.
func atlassianError(product string, status int, raw []byte) error {
	// Jira's "errors" is an object and Confluence's is an array of objects. Two
	// fields with the same JSON tag would make encoding/json ignore both, so
	// these have to be separate decodes.
	var jira struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
		Message       string            `json:"message"`
	}
	_ = json.Unmarshal(raw, &jira)
	var parts []string
	parts = append(parts, jira.ErrorMessages...)
	for field, msg := range jira.Errors {
		parts = append(parts, field+": "+msg)
	}
	if jira.Message != "" {
		parts = append(parts, jira.Message)
	}
	if len(parts) == 0 {
		var conf struct {
			Errors []struct {
				Title  string `json:"title"`
				Detail string `json:"detail"`
			} `json:"errors"`
		}
		if json.Unmarshal(raw, &conf) == nil {
			for _, x := range conf.Errors {
				if m := firstNonEmpty(x.Detail, x.Title); m != "" {
					parts = append(parts, m)
				}
			}
		}
	}
	if len(parts) > 0 {
		msg := strings.Join(parts, "; ")
		if product == "Confluence" && strings.Contains(strings.ToLower(msg), "scope does not match") {
			return fmt.Errorf("this Confluence connection was authorized with older permissions — " +
				"disconnect and reconnect to grant the ones this operation needs")
		}
		if status == http.StatusForbidden || status == http.StatusUnauthorized {
			msg += " — the connected account may lack permission, or the connection needs reauthorizing"
		}
		return fmt.Errorf("%s API error (%d): %s", product, status, msg)
	}
	return fmt.Errorf("%s API returned %d: %s", product, status, truncateStr(string(raw), 300))
}

// adf wraps plain text in an Atlassian Document Format doc. API v3 rejects a
// bare string for description and comment bodies, and blank paragraphs are
// invalid, so empty lines are dropped rather than emitted.
func adf(text string) map[string]any {
	content := []any{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		content = append(content, map[string]any{
			"type":    "paragraph",
			"content": []any{map[string]any{"type": "text", "text": line}},
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "paragraph"})
	}
	return map[string]any{"type": "doc", "version": 1, "content": content}
}

// jiraAccountID resolves what a user typed in an assignee field to an accountId.
// "me" means the connected account; an email is looked up; anything else is
// assumed to already be an accountId (which is what the API wants).
func jiraAccountID(ctx context.Context, token, cloudID, who string) (string, error) {
	who = strings.TrimSpace(who)
	switch {
	case who == "":
		return "", nil
	case strings.EqualFold(who, "me"):
		raw, err := jiraCall(ctx, token, cloudID, http.MethodGet, jiraAPI+"/myself", nil)
		if err != nil {
			return "", err
		}
		var me struct {
			AccountID string `json:"accountId"`
		}
		if json.Unmarshal([]byte(raw), &me) != nil || me.AccountID == "" {
			return "", fmt.Errorf("could not resolve the connected Jira account")
		}
		return me.AccountID, nil
	case strings.Contains(who, "@"):
		raw, err := jiraCall(ctx, token, cloudID, http.MethodGet,
			jiraAPI+"/user/search?query="+url.QueryEscape(who), nil)
		if err != nil {
			return "", err
		}
		var users []struct {
			AccountID string `json:"accountId"`
		}
		if json.Unmarshal([]byte(raw), &users) != nil || len(users) == 0 {
			return "", fmt.Errorf("no Jira user found for %q", who)
		}
		return users[0].AccountID, nil
	default:
		return who, nil
	}
}

// jiraIssueFields builds the create/update fields object, setting only what the
// node actually provides so an update never blanks an untouched field.
func jiraIssueFields(ctx context.Context, token, cloudID string, d FlowNodeData, sub func(string) string, forCreate bool) (map[string]any, error) {
	fields := map[string]any{}
	if forCreate {
		fields["project"] = map[string]string{"key": sub(d.JiraProjectKey)}
		fields["issuetype"] = map[string]string{"name": firstNonEmpty(sub(d.JiraIssueType), "Task")}
	}
	if v := sub(d.JiraSummary); v != "" {
		fields["summary"] = v
	}
	if v := sub(d.JiraDescription); v != "" {
		fields["description"] = adf(v)
	}
	if v := sub(d.JiraPriority); v != "" {
		fields["priority"] = map[string]string{"name": v}
	}
	if v := sub(d.JiraDueDate); v != "" {
		fields["duedate"] = v
	}
	if v := sub(d.JiraParentKey); v != "" {
		fields["parent"] = map[string]string{"key": v}
	}
	if v := sub(d.JiraLabels); v != "" {
		// Jira rejects labels containing spaces, so join words with hyphens
		// rather than failing the whole call on a stray space.
		var labels []string
		for _, l := range strings.Split(v, ",") {
			if l = strings.Join(strings.Fields(l), "-"); l != "" {
				labels = append(labels, l)
			}
		}
		fields["labels"] = labels
	}
	if v := sub(d.JiraAssignee); v != "" {
		id, err := jiraAccountID(ctx, token, cloudID, v)
		if err != nil {
			return nil, err
		}
		fields["assignee"] = map[string]string{"accountId": id}
	}
	return fields, nil
}

func runJira(ctx context.Context, token, cloudID string, d FlowNodeData, outputs map[string]string) (string, error) {
	if cloudID == "" {
		return "", fmt.Errorf("no Jira site is linked to this connection — reconnect Jira to select a site")
	}
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.JiraLimit, 25)
	issue := func() string { return sub(d.JiraIssueKey) }

	switch d.IntegrationOp {
	// ---- issues ----
	case "search_issues":
		// The v3 GET /search endpoint is gone; /search/jql is its replacement and
		// takes the query in a POST body.
		body := map[string]any{
			"jql":        firstNonEmpty(sub(d.JiraJql), "order by created DESC"),
			"maxResults": limit,
		}
		if f := sub(d.JiraFields); f != "" {
			body["fields"] = splitCSV(f)
		} else {
			body["fields"] = []string{"summary", "status", "assignee", "priority", "issuetype", "updated"}
		}
		raw, err := jiraCall(ctx, token, cloudID, http.MethodPost, jiraAPI+"/search/jql", body)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_issue":
		raw, err := jiraCall(ctx, token, cloudID, http.MethodGet, jiraAPI+"/issue/"+issue(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "create_issue":
		if sub(d.JiraProjectKey) == "" {
			return "", fmt.Errorf("create_issue needs a project key")
		}
		fields, err := jiraIssueFields(ctx, token, cloudID, d, sub, true)
		if err != nil {
			return "", err
		}
		return jiraCall(ctx, token, cloudID, http.MethodPost, jiraAPI+"/issue", map[string]any{"fields": fields})

	case "update_issue":
		fields, err := jiraIssueFields(ctx, token, cloudID, d, sub, false)
		if err != nil {
			return "", err
		}
		if len(fields) == 0 {
			return "", fmt.Errorf("update_issue needs at least one field to change")
		}
		return jiraCall(ctx, token, cloudID, http.MethodPut, jiraAPI+"/issue/"+issue(),
			map[string]any{"fields": fields})

	case "delete_issue":
		return jiraCall(ctx, token, cloudID, http.MethodDelete, jiraAPI+"/issue/"+issue(), nil)

	case "assign_issue":
		id, err := jiraAccountID(ctx, token, cloudID, sub(d.JiraAssignee))
		if err != nil {
			return "", err
		}
		// An explicit null unassigns; the field being absent would be a no-op.
		payload := map[string]any{"accountId": nil}
		if id != "" {
			payload["accountId"] = id
		}
		return jiraCall(ctx, token, cloudID, http.MethodPut, jiraAPI+"/issue/"+issue()+"/assignee", payload)

	case "list_transitions":
		return jiraCall(ctx, token, cloudID, http.MethodGet, jiraAPI+"/issue/"+issue()+"/transitions", nil)

	case "transition_issue":
		// Transitions are per-workflow numeric ids, but people think in status
		// names ("Done"), so resolve the name against what's available now.
		want := sub(d.JiraTransition)
		if want == "" {
			return "", fmt.Errorf("transition_issue needs a target status")
		}
		raw, err := jiraCall(ctx, token, cloudID, http.MethodGet, jiraAPI+"/issue/"+issue()+"/transitions", nil)
		if err != nil {
			return "", err
		}
		var list struct {
			Transitions []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				To   struct {
					Name string `json:"name"`
				} `json:"to"`
			} `json:"transitions"`
		}
		if json.Unmarshal([]byte(raw), &list) != nil {
			return "", fmt.Errorf("could not read the available transitions for %s", issue())
		}
		id := ""
		var available []string
		for _, t := range list.Transitions {
			available = append(available, t.To.Name)
			if strings.EqualFold(t.To.Name, want) || strings.EqualFold(t.Name, want) || t.ID == want {
				id = t.ID
				break
			}
		}
		if id == "" {
			return "", fmt.Errorf("%s cannot move to %q from its current status; available: %s",
				issue(), want, strings.Join(available, ", "))
		}
		payload := map[string]any{"transition": map[string]string{"id": id}}
		if c := sub(d.JiraComment); c != "" {
			payload["update"] = map[string]any{
				"comment": []any{map[string]any{"add": map[string]any{"body": adf(c)}}},
			}
		}
		if _, err := jiraCall(ctx, token, cloudID, http.MethodPost, jiraAPI+"/issue/"+issue()+"/transitions", payload); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"issue":%q,"status":%q}`, issue(), want), nil

	case "link_issues":
		return jiraCall(ctx, token, cloudID, http.MethodPost, jiraAPI+"/issueLink", map[string]any{
			"type":         map[string]string{"name": firstNonEmpty(sub(d.JiraLinkType), "Relates")},
			"inwardIssue":  map[string]string{"key": issue()},
			"outwardIssue": map[string]string{"key": sub(d.JiraLinkedIssue)},
		})

	// ---- comments ----
	case "add_comment":
		if sub(d.JiraComment) == "" {
			return "", fmt.Errorf("add_comment needs comment text")
		}
		return jiraCall(ctx, token, cloudID, http.MethodPost, jiraAPI+"/issue/"+issue()+"/comment",
			map[string]any{"body": adf(sub(d.JiraComment))})

	case "list_comments":
		raw, err := jiraCall(ctx, token, cloudID, http.MethodGet,
			fmt.Sprintf("%s/issue/%s/comment?maxResults=%d&orderBy=-created", jiraAPI, issue(), limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	// ---- worklogs ----
	case "add_worklog":
		payload := map[string]any{"timeSpent": firstNonEmpty(sub(d.JiraTimeSpent), "1h")}
		if c := sub(d.JiraComment); c != "" {
			payload["comment"] = adf(c)
		}
		if s := sub(d.JiraStarted); s != "" {
			// Jira wants milliseconds and a +0000-style offset, not RFC3339's "Z".
			payload["started"] = jiraTimestamp(s)
		}
		return jiraCall(ctx, token, cloudID, http.MethodPost, jiraAPI+"/issue/"+issue()+"/worklog", payload)

	case "list_worklogs":
		return jiraCall(ctx, token, cloudID, http.MethodGet, jiraAPI+"/issue/"+issue()+"/worklog", nil)

	// ---- attachments ----
	case "add_attachment":
		return jiraAttach(ctx, token, cloudID, issue(),
			firstNonEmpty(sub(d.JiraAttachName), "attachment.txt"), sub(d.JiraAttachBody))

	// ---- projects & metadata ----
	case "list_projects":
		raw, err := jiraCall(ctx, token, cloudID, http.MethodGet,
			fmt.Sprintf("%s/project/search?maxResults=%d", jiraAPI, limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_project":
		return jiraCall(ctx, token, cloudID, http.MethodGet, jiraAPI+"/project/"+sub(d.JiraProjectKey), nil)

	case "list_issue_types":
		if p := sub(d.JiraProjectKey); p != "" {
			return jiraCall(ctx, token, cloudID, http.MethodGet,
				jiraAPI+"/issue/createmeta/"+p+"/issuetypes", nil)
		}
		raw, err := jiraCall(ctx, token, cloudID, http.MethodGet, jiraAPI+"/issuetype", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 6000), nil

	case "search_users":
		return jiraCall(ctx, token, cloudID, http.MethodGet, fmt.Sprintf(
			"%s/user/search?query=%s&maxResults=%d", jiraAPI, url.QueryEscape(sub(d.JiraQuery)), limit), nil)

	case "get_current_user":
		return jiraCall(ctx, token, cloudID, http.MethodGet, jiraAPI+"/myself", nil)

	// ---- agile ----
	case "list_boards":
		q := url.Values{"maxResults": {fmt.Sprint(limit)}}
		if p := sub(d.JiraProjectKey); p != "" {
			q.Set("projectKeyOrId", p)
		}
		return jiraCall(ctx, token, cloudID, http.MethodGet, jiraAgile+"/board?"+q.Encode(), nil)

	case "list_sprints":
		if sub(d.JiraBoardId) == "" {
			return "", fmt.Errorf("list_sprints needs a board id")
		}
		return jiraCall(ctx, token, cloudID, http.MethodGet, fmt.Sprintf(
			"%s/board/%s/sprint?maxResults=%d", jiraAgile, sub(d.JiraBoardId), limit), nil)

	case "get_sprint_issues":
		raw, err := jiraCall(ctx, token, cloudID, http.MethodGet, fmt.Sprintf(
			"%s/sprint/%s/issue?maxResults=%d&fields=summary,status,assignee,priority",
			jiraAgile, sub(d.JiraSprintId), limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "create_sprint":
		if sub(d.JiraBoardId) == "" {
			return "", fmt.Errorf("create_sprint needs a board id")
		}
		payload := map[string]any{
			"name":          firstNonEmpty(sub(d.JiraSprintName), "New sprint"),
			"originBoardId": sub(d.JiraBoardId),
		}
		if v := sub(d.JiraStartDate); v != "" {
			payload["startDate"] = v
		}
		if v := sub(d.JiraEndDate); v != "" {
			payload["endDate"] = v
		}
		return jiraCall(ctx, token, cloudID, http.MethodPost, jiraAgile+"/sprint", payload)

	case "move_issues_to_sprint":
		keys := splitCSV(sub(d.JiraIssueKey))
		if len(keys) == 0 {
			return "", fmt.Errorf("move_issues_to_sprint needs at least one issue key")
		}
		target := sub(d.JiraSprintId)
		path := jiraAgile + "/sprint/" + target + "/issue"
		// "backlog" is a distinct endpoint, not a sprint id.
		if strings.EqualFold(target, "backlog") {
			path = jiraAgile + "/backlog/issue"
		}
		if _, err := jiraCall(ctx, token, cloudID, http.MethodPost, path,
			map[string]any{"issues": keys}); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"moved":%d,"sprint":%q}`, len(keys), target), nil

	case "":
		return "", fmt.Errorf("no Jira operation selected")
	}
	return "", fmt.Errorf("unsupported Jira operation: %s", d.IntegrationOp)
}

// jiraTimestamp converts RFC3339 to the offset format Jira's worklog needs
// (2026-08-04T09:00:00.000+0000). Anything unrecognized is passed through so
// the API's own error surfaces rather than a silently wrong time.
func jiraTimestamp(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "Z") {
		return strings.TrimSuffix(s, "Z") + ".000+0000"
	}
	return s
}

// jiraAttach uploads text content as an issue attachment. Multipart, and the
// X-Atlassian-Token header is mandatory — without it Jira rejects the upload as
// a possible XSRF attempt.
func jiraAttach(ctx context.Context, token, cloudID, issueKey, name, content string) (string, error) {
	if issueKey == "" {
		return "", fmt.Errorf("add_attachment needs an issue key")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(part, content); err != nil {
		return "", err
	}
	w.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		atlassianGateway+"/ex/jira/"+cloudID+jiraAPI+"/issue/"+issueKey+"/attachments", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Atlassian-Token", "no-check")
	return jiraDo(req, "Jira")
}
