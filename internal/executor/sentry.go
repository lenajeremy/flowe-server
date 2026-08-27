package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// sentryAPIBase is read per call rather than captured at init so a self-hosted
// or region-pinned deployment can point every Sentry node at its own host with
// one variable. Tests override the variable directly.
var sentryAPIBaseOverride = ""

func sentryAPIURL() string {
	if sentryAPIBaseOverride != "" {
		return strings.TrimRight(sentryAPIBaseOverride, "/")
	}
	if base := strings.TrimSpace(os.Getenv("SENTRY_API_BASE")); base != "" {
		return strings.TrimRight(base, "/")
	}
	return "https://sentry.io/api/0"
}

func sentryCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("encode Sentry request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, sentryAPIURL()+path, reader)
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
		return "", fmt.Errorf("Sentry request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", sentryError(resp.StatusCode, raw)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

// sentryError turns Sentry's several error shapes into one sentence. Sentry
// answers with {"detail": …} most of the time, but validation failures come
// back as a field→messages map, which reads as raw JSON unless unpacked.
func sentryError(status int, raw []byte) error {
	message := truncateStr(string(raw), 300)

	var detail struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &detail) == nil && detail.Detail != "" {
		message = detail.Detail
	} else {
		var fields map[string][]string
		if json.Unmarshal(raw, &fields) == nil && len(fields) > 0 {
			parts := make([]string, 0, len(fields))
			for field, messages := range fields {
				parts = append(parts, field+": "+strings.Join(messages, ", "))
			}
			message = strings.Join(parts, "; ")
		}
	}

	switch status {
	case http.StatusUnauthorized:
		message += " — reconnect Sentry to refresh its installation"
	case http.StatusForbidden:
		message += " — the Sentry integration is missing a permission for this operation"
	}
	return fmt.Errorf("Sentry API error (%d): %s", status, message)
}

// sentryPaged appends the list parameters every Sentry collection understands.
func sentryPaged(q url.Values, d FlowNodeData, sub func(string) string) url.Values {
	limit := intOr(d.SentryLimit, 25)
	if limit < 1 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}
	q.Set("per_page", strconv.Itoa(limit))
	if v := sub(d.SentryStatsPeriod); v != "" {
		q.Set("statsPeriod", v)
	}
	if v := sub(d.SentryEnvironment); v != "" {
		q.Set("environment", v)
	}
	if v := sub(d.SentryCursor); v != "" {
		q.Set("cursor", v)
	}
	return q
}

func runSentry(ctx context.Context, token, org string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(value string) string { return substituteTemplates(value, outputs) }
	need := func(label, value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("this operation needs %s", label)
		}
		return nil
	}
	org = strings.TrimSpace(org)
	if org == "" {
		return "", fmt.Errorf("this Sentry connection names no organization — reconnect Sentry")
	}
	orgPath := "/organizations/" + url.PathEscape(org)

	project := sub(d.SentryProject)
	issue := sub(d.SentryIssueId)
	version := sub(d.SentryVersion)

	// Issue endpoints moved under the organization; the bare /issues/{id}/ form
	// still answers on sentry.io but is the older spelling, so everything here
	// uses the organization-scoped path.
	issuePath := func() string { return orgPath + "/issues/" + url.PathEscape(issue) + "/" }

	switch d.IntegrationOp {
	case "list_projects":
		return sentryCall(ctx, token, http.MethodGet, orgPath+"/projects/", nil)

	case "get_project":
		if err := need("a project", project); err != nil {
			return "", err
		}
		return sentryCall(ctx, token, http.MethodGet,
			"/projects/"+url.PathEscape(org)+"/"+url.PathEscape(project)+"/", nil)

	case "list_issues":
		q := sentryPaged(url.Values{}, d, sub)
		// An empty query means "everything" to Sentry, including issues someone
		// resolved months ago. Unresolved is what a person means by "the issues".
		query := sub(d.SentryQuery)
		if strings.TrimSpace(query) == "" {
			query = "is:unresolved"
		}
		q.Set("query", query)
		if project != "" {
			q.Set("project", project)
		}
		if v := sub(d.SentrySort); v != "" {
			q.Set("sort", v)
		}
		return sentryCall(ctx, token, http.MethodGet, orgPath+"/issues/?"+q.Encode(), nil)

	case "get_issue":
		if err := need("an issue ID", issue); err != nil {
			return "", err
		}
		return sentryCall(ctx, token, http.MethodGet, issuePath(), nil)

	case "get_latest_event":
		if err := need("an issue ID", issue); err != nil {
			return "", err
		}
		return sentryCall(ctx, token, http.MethodGet, issuePath()+"events/latest/", nil)

	case "list_issue_events":
		if err := need("an issue ID", issue); err != nil {
			return "", err
		}
		q := sentryPaged(url.Values{}, d, sub)
		return sentryCall(ctx, token, http.MethodGet, issuePath()+"events/?"+q.Encode(), nil)

	case "list_issue_tag_values":
		if err := need("an issue ID", issue); err != nil {
			return "", err
		}
		key := sub(d.SentryTagKey)
		if err := need("a tag key", key); err != nil {
			return "", err
		}
		return sentryCall(ctx, token, http.MethodGet,
			issuePath()+"tags/"+url.PathEscape(key)+"/values/", nil)

	case "resolve_issue", "ignore_issue", "unresolve_issue":
		if err := need("an issue ID", issue); err != nil {
			return "", err
		}
		body := map[string]any{"status": sentryTargetStatus(d.IntegrationOp, sub(d.SentryStatus))}
		if d.IntegrationOp == "ignore_issue" {
			if details := sentryIgnoreDetails(d); len(details) > 0 {
				body["statusDetails"] = details
			}
		}
		return sentryCall(ctx, token, http.MethodPut, issuePath(), body)

	case "assign_issue":
		if err := need("an issue ID", issue); err != nil {
			return "", err
		}
		assignee := sub(d.SentryAssignee)
		if err := need("an assignee (user:<id>, team:<id>, or an email address)", assignee); err != nil {
			return "", err
		}
		return sentryCall(ctx, token, http.MethodPut, issuePath(), map[string]any{"assignedTo": assignee})

	case "delete_issue":
		if err := need("an issue ID", issue); err != nil {
			return "", err
		}
		return sentryCall(ctx, token, http.MethodDelete, issuePath(), nil)

	case "list_comments":
		if err := need("an issue ID", issue); err != nil {
			return "", err
		}
		return sentryIssueComments(ctx, token, orgPath, issue, http.MethodGet, nil)

	case "add_comment":
		if err := need("an issue ID", issue); err != nil {
			return "", err
		}
		text := sub(d.SentryComment)
		if err := need("comment text", text); err != nil {
			return "", err
		}
		return sentryIssueComments(ctx, token, orgPath, issue, http.MethodPost, map[string]any{"text": text})

	case "list_releases":
		q := sentryPaged(url.Values{}, d, sub)
		if v := sub(d.SentryQuery); v != "" {
			q.Set("query", v)
		}
		if project != "" {
			q.Set("project", project)
		}
		return sentryCall(ctx, token, http.MethodGet, orgPath+"/releases/?"+q.Encode(), nil)

	case "create_release":
		if err := need("a version", version); err != nil {
			return "", err
		}
		projects := sentryProjectList(d, sub, project)
		if len(projects) == 0 {
			return "", fmt.Errorf("this operation needs at least one project")
		}
		body := map[string]any{"version": version, "projects": projects}
		if v := sub(d.SentryRef); v != "" {
			body["ref"] = v
		}
		if v := sub(d.SentryUrl); v != "" {
			body["url"] = v
		}
		return sentryCall(ctx, token, http.MethodPost, orgPath+"/releases/", body)

	case "create_deploy":
		if err := need("a version", version); err != nil {
			return "", err
		}
		environment := sub(d.SentryEnvironment)
		if err := need("an environment", environment); err != nil {
			return "", err
		}
		body := map[string]any{"environment": environment}
		if v := sub(d.SentryDeployName); v != "" {
			body["name"] = v
		}
		if v := sub(d.SentryUrl); v != "" {
			body["url"] = v
		}
		if projects := sentryProjectList(d, sub, project); len(projects) > 0 {
			body["projects"] = projects
		}
		return sentryCall(ctx, token, http.MethodPost,
			orgPath+"/releases/"+url.PathEscape(version)+"/deploys/", body)

	case "query_events":
		q := sentryPaged(url.Values{}, d, sub)
		fields := splitCSV(sub(d.SentryFields))
		if len(fields) == 0 {
			fields = []string{"title", "project", "timestamp", "count()"}
		}
		for _, field := range fields {
			q.Add("field", field)
		}
		if v := sub(d.SentryQuery); v != "" {
			q.Set("query", v)
		}
		if project != "" {
			q.Set("project", project)
		}
		if v := sub(d.SentrySort); v != "" {
			q.Set("sort", v)
		}
		if q.Get("statsPeriod") == "" {
			q.Set("statsPeriod", "24h")
		}
		return sentryCall(ctx, token, http.MethodGet, orgPath+"/events/?"+q.Encode(), nil)

	case "list_alert_rules":
		if err := need("a project", project); err != nil {
			return "", err
		}
		return sentryCall(ctx, token, http.MethodGet,
			"/projects/"+url.PathEscape(org)+"/"+url.PathEscape(project)+"/rules/", nil)
	}

	return "", fmt.Errorf("unsupported Sentry operation %q", d.IntegrationOp)
}

// sentryTargetStatus resolves the status an issue is being moved to. The
// explicit field wins so "resolve in the next release" is reachable, and the
// operation name is the fallback.
func sentryTargetStatus(op, explicit string) string {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return explicit
	}
	switch op {
	case "resolve_issue":
		return "resolved"
	case "ignore_issue":
		return "ignored"
	default:
		return "unresolved"
	}
}

// sentryIgnoreDetails builds the "archive until" conditions. Sentry treats a
// bare ignore as forever, which is rarely what someone automating triage wants.
func sentryIgnoreDetails(d FlowNodeData) map[string]any {
	details := map[string]any{}
	if d.SentryIgnoreMinutes > 0 {
		details["ignoreDuration"] = d.SentryIgnoreMinutes
	}
	if d.SentryIgnoreCount > 0 {
		details["ignoreCount"] = d.SentryIgnoreCount
	}
	return details
}

// sentryProjectList resolves the projects a release or deploy applies to,
// accepting either the comma-separated list or the single project field.
func sentryProjectList(d FlowNodeData, sub func(string) string, fallback string) []string {
	if projects := splitCSV(sub(d.SentryProjects)); len(projects) > 0 {
		return projects
	}
	if fallback = strings.TrimSpace(fallback); fallback != "" {
		return []string{fallback}
	}
	return nil
}

// sentryIssueComments reaches the issue's Activity notes.
//
// This is the one endpoint here with no entry in Sentry's published API
// reference: it is real and stable in the product, but its path moved under
// /organizations/ with the rest of the issue endpoints and older installs still
// answer on the bare form. A 404 on the first shape is retried on the second
// rather than surfaced, because "not found" here means the wrong spelling, not
// a missing issue — that would have failed the same way for every issue.
func sentryIssueComments(ctx context.Context, token, orgPath, issue, method string, body any) (string, error) {
	out, err := sentryCall(ctx, token, method,
		orgPath+"/issues/"+url.PathEscape(issue)+"/comments/", body)
	if err == nil || !strings.Contains(err.Error(), "(404)") {
		return out, err
	}
	return sentryCall(ctx, token, method, "/issues/"+url.PathEscape(issue)+"/comments/", body)
}
