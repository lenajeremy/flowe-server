package triggers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
)

// Sentry, delivered through the Integration Platform's app-level webhook.
//
// Sentry has no "add a webhook to this project" API worth using — the legacy
// project plugin posts unsigned JSON, which is not something to hang a workflow
// run on. The signed path is a Sentry App: one webhook URL configured on the
// app, one client secret signing every delivery, and every organization that
// installs it posting to that single endpoint. So this takes the same shape as
// GitHub and Slack: Register creates nothing, and the payload is the routing
// information.
//
// The app subscribes at the level of a resource — "issue", not "issue
// resolved" — so the app hears every action on every issue in every project the
// organization owns, and Parse drops what this trigger did not ask for. That
// filtering happens before a run is admitted; a branch node would reach the
// same verdict after paying for the run.
//
// Sentry wants a response inside a second, which the shared handler already
// gives it by acknowledging before any workflow starts.

func init() { Register(sentryAdapter{}) }

type sentryAdapter struct{}

func (sentryAdapter) Provider() string   { return "sentry" }
func (sentryAdapter) Delivery() Delivery { return Push }

func sentryAPIBase() string {
	if base := strings.TrimSpace(os.Getenv("SENTRY_API_BASE")); base != "" {
		return strings.TrimRight(base, "/")
	}
	return "https://sentry.io/api/0"
}

// sentryHookResources are the resource subscriptions this adapter needs the
// Sentry App to have enabled. Named here so Register can say which one is
// missing rather than leaving a trigger that quietly never fires.
var sentryHookResources = map[string]string{
	"issue":        "Issue",
	"error":        "Error",
	"comment":      "Comment",
	"event_alert":  "Issue alert",
	"metric_alert": "Metric alert",
}

func (sentryAdapter) Events() []EventSpec {
	issueFilters := []FilterSpec{
		{Key: "level", Label: "Level", Placeholder: "error"},
	}
	issueSample := map[string]any{
		"id": "1234567890", "shortId": "BACKEND-4F", "title": "TypeError: cannot read property 'id' of undefined",
		"culprit": "app/routes/checkout.ts in submit", "level": "error", "status": "unresolved",
		"project": "backend", "count": "128", "userCount": 41,
		"url": "https://sentry.io/organizations/acme/issues/1234567890/",
	}

	return []EventSpec{
		{
			ID: "issue.created", Label: "Issue created", ResourceKind: "project",
			Filters: issueFilters, Sample: issueSample,
		},
		{
			ID: "issue.resolved", Label: "Issue resolved", ResourceKind: "project",
			Filters: issueFilters, Sample: issueSample,
		},
		{
			ID: "issue.unresolved", Label: "Issue regressed", ResourceKind: "project",
			Filters: issueFilters, Sample: issueSample,
		},
		{
			ID: "issue.assigned", Label: "Issue assigned", ResourceKind: "project",
			Filters: issueFilters, Sample: issueSample,
		},
		{
			ID: "issue.archived", Label: "Issue archived", ResourceKind: "project",
			Filters: issueFilters, Sample: issueSample,
		},
		{
			// Every captured error, not one per issue. On a busy project this is
			// thousands of events an hour, and Sentry only offers it on Business
			// and Enterprise plans — both facts belong in the label, because the
			// alternative is a user discovering them through their credit balance.
			ID: "error.created", Label: "Error captured — every event (Business plan)", ResourceKind: "project",
			Filters: []FilterSpec{
				{Key: "level", Label: "Level", Placeholder: "error"},
				{Key: "environment", Label: "Environment", Placeholder: "production"},
				{Key: "release", Label: "Release", Placeholder: "1.4.2"},
			},
			Sample: map[string]any{
				"eventId": "c3f2d1e0", "issueId": "1234567890", "title": "TypeError: undefined is not a function",
				"level": "error", "platform": "javascript", "environment": "production",
				"release": "1.4.2", "project": "frontend",
				"webUrl": "https://sentry.io/organizations/acme/issues/1234567890/events/c3f2d1e0/",
			},
		},
		{
			ID: "event_alert.triggered", Label: "Issue alert fired", ResourceKind: "project",
			Filters: []FilterSpec{
				{Key: "rule", Label: "Alert rule", Placeholder: "Production pager"},
				{Key: "environment", Label: "Environment", Placeholder: "production"},
			},
			Sample: map[string]any{
				"rule": "Production pager", "title": "TypeError: undefined is not a function",
				"issueId": "1234567890", "eventId": "c3f2d1e0", "level": "error", "project": "frontend",
				"webUrl": "https://sentry.io/organizations/acme/issues/1234567890/",
			},
		},
		{
			ID: "metric_alert.critical", Label: "Metric alert critical", ResourceKind: "project",
			Filters: sentryMetricFilters(), Sample: sentryMetricSample("critical"),
		},
		{
			ID: "metric_alert.warning", Label: "Metric alert warning", ResourceKind: "project",
			Filters: sentryMetricFilters(), Sample: sentryMetricSample("warning"),
		},
		{
			ID: "metric_alert.resolved", Label: "Metric alert resolved", ResourceKind: "project",
			Filters: sentryMetricFilters(), Sample: sentryMetricSample("resolved"),
		},
		{
			ID: "comment.created", Label: "Comment added to an issue", ResourceKind: "project",
			Sample: sentryCommentSample(),
		},
		{
			ID: "comment.updated", Label: "Comment edited", ResourceKind: "project",
			Sample: sentryCommentSample(),
		},
		{
			ID: "comment.deleted", Label: "Comment deleted", ResourceKind: "project",
			Sample: sentryCommentSample(),
		},
	}
}

func sentryMetricFilters() []FilterSpec {
	return []FilterSpec{{Key: "rule", Label: "Alert rule", Placeholder: "Checkout latency"}}
}

func sentryMetricSample(status string) map[string]any {
	return map[string]any{
		"rule": "Checkout latency", "status": status, "project": "backend",
		"title": "Critical: Checkout latency", "description": "Latency is above 500ms in the last 5 minutes",
		"webUrl": "https://sentry.io/organizations/acme/alerts/rules/details/12/",
	}
}

func sentryCommentSample() map[string]any {
	return map[string]any{
		"comment": "Looks like the retry loop, I'll pick this up", "commentId": "1234",
		"issueId": "1234567890", "project": "backend", "actor": "colleen",
	}
}

// Register creates nothing at Sentry — the subscription lives on the app — but
// it does resolve which installation this trigger's events will arrive under,
// and refuses the trigger if it cannot.
//
// That resolution is not bookkeeping. One URL receives every customer's
// events, so the installation uuid is the only thing that keeps one
// organization's issues from waking another organization's workflow. A trigger
// stored without it would be matched on project slug alone, and project slugs
// are not unique across Sentry.
func (a sentryAdapter) Register(ctx context.Context, conn Conn, t *models.IntegrationTrigger) (Registration, error) {
	if !Supports("sentry", t.Event) {
		return Registration{}, fmt.Errorf("sentry: unknown event %q", t.Event)
	}
	if strings.TrimSpace(t.ResourceID) == "" {
		return Registration{}, fmt.Errorf("sentry: no project selected")
	}
	if os.Getenv("SENTRY_CLIENT_SECRET") == "" {
		return Registration{}, fmt.Errorf("sentry: SENTRY_CLIENT_SECRET is not configured, so deliveries could not be verified")
	}
	installation, err := a.installationFor(ctx, conn)
	if err != nil {
		return Registration{}, err
	}
	if resource := strings.SplitN(t.Event, ".", 2)[0]; !installation.subscribes(resource) {
		return Registration{}, fmt.Errorf(
			"sentry: the Fernary integration is not subscribed to %s webhooks — enable them in the integration's settings",
			sentryHookResources[resource])
	}
	return Registration{ScopeID: installation.UUID}, nil
}

type sentryInstallation struct {
	UUID string `json:"uuid"`
	App  struct {
		Slug   string   `json:"slug"`
		UUID   string   `json:"uuid"`
		Events []string `json:"events"`
	} `json:"app"`
}

// subscribes reports whether the installed app asked Sentry for this resource.
// Sentry omits the list on some responses; an absent list is treated as "yes"
// rather than blocking a trigger over missing metadata.
func (i sentryInstallation) subscribes(resource string) bool {
	if len(i.App.Events) == 0 {
		return true
	}
	for _, e := range i.App.Events {
		if strings.EqualFold(e, resource) {
			return true
		}
	}
	return false
}

// installationFor finds this deployment's app among the organization's
// installations. An organization can have several custom integrations; only
// ours delivers to our endpoint.
func (sentryAdapter) installationFor(ctx context.Context, conn Conn) (sentryInstallation, error) {
	var zero sentryInstallation
	org := strings.TrimSpace(conn.WorkspaceID)
	if org == "" {
		return zero, fmt.Errorf("sentry: this connection names no organization — reconnect Sentry")
	}
	slug := strings.TrimSpace(os.Getenv("SENTRY_APP_SLUG"))
	if slug == "" {
		return zero, fmt.Errorf("sentry: SENTRY_APP_SLUG is not configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		sentryAPIBase()+"/organizations/"+org+"/sentry-app-installations/", nil)
	if err != nil {
		return zero, err
	}
	req.Header.Set("Authorization", "Bearer "+conn.AccessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return zero, fmt.Errorf("sentry: could not read the organization's installations: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return zero, fmt.Errorf("sentry: could not read the organization's installations (%d)", resp.StatusCode)
	}
	var installations []sentryInstallation
	if err := json.NewDecoder(resp.Body).Decode(&installations); err != nil {
		return zero, fmt.Errorf("sentry: could not read the organization's installations")
	}
	for _, installation := range installations {
		if strings.EqualFold(installation.App.Slug, slug) && installation.UUID != "" {
			return installation, nil
		}
	}
	return zero, fmt.Errorf("sentry: the Fernary integration is not installed on %s — reconnect Sentry", org)
}

// Unregister is a no-op: nothing was created, and the app's webhook stays up
// for every other trigger. The installation itself is removed on disconnect.
func (sentryAdapter) Unregister(context.Context, Conn, *models.IntegrationTrigger) error {
	return nil
}

// Renew is a no-op: a Sentry App's webhook subscription does not expire. The
// installation's API token does, and that is refreshed on the connection.
func (sentryAdapter) Renew(context.Context, Conn, *models.IntegrationTrigger) (*time.Time, error) {
	return nil, nil
}

// Handshake: Sentry has none. It verifies an install by calling the app's own
// endpoints, not by challenging the webhook URL.
func (sentryAdapter) Handshake(*http.Request, []byte) (int, []byte, http.Header, bool) {
	return 0, nil, nil, false
}

// Verify checks the app's client secret against Sentry-Hook-Signature, a hex
// HMAC-SHA256 over the raw body.
//
// The trigger argument is unused: deliveries arrive at one app-level URL before
// we know which trigger they belong to, so authentication cannot depend on a
// per-trigger value.
func (sentryAdapter) Verify(r *http.Request, body []byte, _ *models.IntegrationTrigger) error {
	secret := os.Getenv("SENTRY_CLIENT_SECRET")
	if secret == "" {
		// Fail closed. An unset secret must never mean "accept anything" — that
		// would turn one missing variable into an open door to every workflow.
		return fmt.Errorf("sentry: SENTRY_CLIENT_SECRET is not configured")
	}
	got := r.Header.Get("Sentry-Hook-Signature")
	if got == "" {
		return fmt.Errorf("sentry: request is not signed")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	// Constant time: a byte-by-byte compare leaks how much of a forged
	// signature was right, which is enough to construct one.
	if !hmac.Equal([]byte(got), []byte(want)) {
		return fmt.Errorf("sentry: signature does not match")
	}
	return nil
}

// sentryDelivery is the envelope every Sentry webhook shares. The resource is
// in a header rather than the body, and the action is in the body, so the event
// id this system dispatches on is assembled from both.
type sentryDelivery struct {
	Action       string `json:"action"`
	Installation struct {
		UUID string `json:"uuid"`
	} `json:"installation"`
	Actor struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"actor"`
	Data json.RawMessage `json:"data"`
}

func (sentryAdapter) Parse(r *http.Request, body []byte) ([]Event, error) {
	resource := strings.TrimSpace(r.Header.Get("Sentry-Hook-Resource"))
	if resource == "" {
		return nil, fmt.Errorf("sentry: delivery has no Sentry-Hook-Resource header")
	}

	var d sentryDelivery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("sentry: unreadable payload")
	}
	scopeID := d.Installation.UUID

	// installation.* is Sentry telling us about our own app rather than about
	// anyone's errors. It is never a workflow input.
	if resource == "installation" {
		if d.Action != "deleted" {
			return nil, nil
		}
		return []Event{{
			Key: sentryDeliveryKey(resource, d.Action, body), Type: "installation.deleted",
			ScopeID: scopeID, OccurredAt: time.Now().UTC(),
			Lifecycle: &LifecycleEvent{Action: LifecycleScopeRemoved},
		}}, nil
	}

	eventType := resource + "." + d.Action
	if !Supports("sentry", eventType) {
		// Not a failure: the app is subscribed per resource, so it hears actions
		// nobody asked for. Acknowledged and dropped.
		return nil, nil
	}

	projects, data, occurred := sentryEventData(resource, d)
	if data == nil {
		return nil, nil
	}
	if d.Actor.Name != "" {
		data["actor"] = d.Actor.Name
	}
	data["action"] = d.Action

	// A metric alert can span several projects, and a trigger is set up against
	// one. Emitting a single event carrying only the first project silently
	// ignores every trigger watching the others, so one event is produced per
	// project named. The dedupe key is per project for the same reason: two
	// projects are two separate things to react to, not a redelivery.
	if len(projects) == 0 {
		projects = []string{""}
	}
	key := sentryDeliveryKey(resource, d.Action, body)
	events := make([]Event, 0, len(projects))
	for _, project := range projects {
		perProject := make(map[string]any, len(data)+1)
		for k, v := range data {
			perProject[k] = v
		}
		perProject["project"] = project
		eventKey := key
		if len(projects) > 1 {
			eventKey = key + ":" + project
		}
		events = append(events, Event{
			Key:        eventKey,
			Type:       eventType,
			ResourceID: project,
			ScopeID:    scopeID,
			OccurredAt: occurred,
			Data:       perProject,
		})
	}
	return events, nil
}

// sentryDeliveryKey deduplicates on the delivery's own bytes.
//
// Sentry sends a Request-ID header, but it identifies the HTTP attempt rather
// than the event, so a retry would arrive with a new one and buy a second
// workflow run. Two genuinely different Sentry events always differ somewhere
// in the body — an id, a timestamp, an occurrence count — so hashing the body
// collapses retries and nothing else.
func sentryDeliveryKey(resource, action string, body []byte) string {
	sum := sha256.Sum256(body)
	return resource + "." + action + ":" + hex.EncodeToString(sum[:])[:32]
}

// sentryEventData flattens one resource's payload into the fields a template
// reads, and reports which projects it belongs to. A metric alert can name
// several; every other resource names exactly one.
//
// Sentry identifies the project four different ways depending on the resource —
// a nested object, a slug field, a list of slugs, or nothing but a URL — so
// this is where that is reconciled rather than in each caller.
func sentryEventData(resource string, d sentryDelivery) (projects []string, data map[string]any, occurred time.Time) {
	occurred = time.Now().UTC()

	switch resource {
	case "issue":
		var payload struct {
			Issue struct {
				ID         string          `json:"id"`
				ShortID    string          `json:"shortId"`
				Title      string          `json:"title"`
				Culprit    string          `json:"culprit"`
				Level      string          `json:"level"`
				Status     string          `json:"status"`
				Substatus  string          `json:"substatus"`
				Count      json.RawMessage `json:"count"`
				UserCount  int             `json:"userCount"`
				FirstSeen  string          `json:"firstSeen"`
				LastSeen   string          `json:"lastSeen"`
				WebURL     string          `json:"web_url"`
				URL        string          `json:"url"`
				ProjectURL string          `json:"project_url"`
				Type       string          `json:"issueType"`
				Category   string          `json:"issueCategory"`
				Project    struct {
					Slug string `json:"slug"`
					Name string `json:"name"`
				} `json:"project"`
				AssignedTo struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"assignedTo"`
			} `json:"issue"`
		}
		if json.Unmarshal(d.Data, &payload) != nil {
			return nil, nil, occurred
		}
		issue := payload.Issue
		project := firstNonEmpty(issue.Project.Slug,
			sentryProjectFromURL(issue.ProjectURL), sentryProjectFromURL(issue.URL))
		if t, err := time.Parse(time.RFC3339, issue.LastSeen); err == nil {
			occurred = t
		}
		data = map[string]any{
			"id": issue.ID, "shortId": issue.ShortID, "title": issue.Title,
			"culprit": issue.Culprit, "level": issue.Level, "status": issue.Status,
			"substatus": issue.Substatus, "userCount": issue.UserCount,
			"firstSeen": issue.FirstSeen, "lastSeen": issue.LastSeen,
			"issueType": issue.Type, "issueCategory": issue.Category,
			"assignee": issue.AssignedTo.Name,
			"url":      firstNonEmpty(issue.WebURL, issue.URL),
		}
		// count arrives as a string on some payloads and a number on others.
		if len(issue.Count) > 0 {
			data["count"] = strings.Trim(string(issue.Count), `"`)
		}
		return []string{project}, data, occurred

	case "error", "event_alert":
		var payload struct {
			Error         *sentryErrorEvent `json:"error"`
			Event         *sentryErrorEvent `json:"event"`
			TriggeredRule string            `json:"triggered_rule"`
			IssueAlert    struct {
				Title string `json:"title"`
			} `json:"issue_alert"`
		}
		if json.Unmarshal(d.Data, &payload) != nil {
			return nil, nil, occurred
		}
		event := payload.Error
		if event == nil {
			event = payload.Event
		}
		if event == nil {
			return nil, nil, occurred
		}
		project := firstNonEmpty(event.ProjectSlug,
			sentryProjectFromURL(event.IssueURL), sentryProjectFromURL(event.WebURL))
		if event.Timestamp > 0 {
			occurred = time.Unix(int64(event.Timestamp), 0).UTC()
		}
		tags := sentryTagMap(event.Tags)
		data = map[string]any{
			"eventId": event.EventID, "issueId": event.IssueID, "title": event.Title,
			"culprit": event.Culprit, "level": firstNonEmpty(event.Level, tags["level"]),
			"platform": event.Platform, "release": firstNonEmpty(event.Release, tags["release"]),
			"environment": firstNonEmpty(event.Environment, tags["environment"]),
			"message":     event.Message,
			"webUrl":      firstNonEmpty(event.WebURL, event.IssueURL),
		}
		if resource == "event_alert" {
			data["rule"] = firstNonEmpty(payload.TriggeredRule, payload.IssueAlert.Title)
		}
		return []string{project}, data, occurred

	case "metric_alert":
		var payload struct {
			MetricAlert struct {
				ID        json.RawMessage `json:"id"`
				Title     string          `json:"title"`
				Status    json.RawMessage `json:"status"`
				Projects  []string        `json:"projects"`
				AlertRule struct {
					Name string `json:"name"`
				} `json:"alert_rule"`
			} `json:"metric_alert"`
			DescriptionText  string `json:"description_text"`
			DescriptionTitle string `json:"description_title"`
			WebURL           string `json:"web_url"`
		}
		if json.Unmarshal(d.Data, &payload) != nil {
			return nil, nil, occurred
		}
		alert := payload.MetricAlert
		data = map[string]any{
			"incidentId":  strings.Trim(string(alert.ID), `"`),
			"rule":        firstNonEmpty(alert.AlertRule.Name, alert.Title),
			"status":      d.Action,
			"title":       firstNonEmpty(payload.DescriptionTitle, alert.Title),
			"description": payload.DescriptionText,
			"webUrl":      payload.WebURL,
		}
		return alert.Projects, data, occurred

	case "comment":
		var payload struct {
			Comment     string          `json:"comment"`
			ProjectSlug string          `json:"project_slug"`
			CommentID   json.RawMessage `json:"comment_id"`
			IssueID     json.RawMessage `json:"issue_id"`
			Timestamp   string          `json:"timestamp"`
		}
		if json.Unmarshal(d.Data, &payload) != nil {
			return nil, nil, occurred
		}
		if t, err := time.Parse(time.RFC3339, payload.Timestamp); err == nil {
			occurred = t
		}
		data = map[string]any{
			"comment":   payload.Comment,
			"commentId": strings.Trim(string(payload.CommentID), `"`),
			"issueId":   strings.Trim(string(payload.IssueID), `"`),
		}
		return []string{payload.ProjectSlug}, data, occurred
	}

	return nil, nil, occurred
}

// sentryErrorEvent is the event body shared by the error and issue-alert
// webhooks — the same serializer under two different keys.
type sentryErrorEvent struct {
	EventID     string     `json:"event_id"`
	IssueID     string     `json:"issue_id"`
	IssueURL    string     `json:"issue_url"`
	WebURL      string     `json:"web_url"`
	ProjectSlug string     `json:"project_slug"`
	Title       string     `json:"title"`
	Message     string     `json:"message"`
	Culprit     string     `json:"culprit"`
	Level       string     `json:"level"`
	Platform    string     `json:"platform"`
	Release     string     `json:"release"`
	Environment string     `json:"environment"`
	Timestamp   float64    `json:"timestamp"`
	Tags        [][]string `json:"tags"`
}

// sentryTagMap flattens Sentry's [["key","value"], …] tag list. Environment,
// release and level often live only there.
func sentryTagMap(tags [][]string) map[string]string {
	out := make(map[string]string, len(tags))
	for _, pair := range tags {
		if len(pair) == 2 && pair[0] != "" {
			out[pair[0]] = pair[1]
		}
	}
	return out
}

// sentryProjectFromURL recovers the project slug from the URLs Sentry embeds
// when it does not name the project outright:
//
//	https://sentry.io/api/0/projects/{org}/{project}/issues/{id}/
//	https://sentry.io/organizations/{org}/projects/{project}/
func sentryProjectFromURL(raw string) string {
	segments := strings.Split(strings.Trim(raw, "/"), "/")
	for i, segment := range segments {
		if segment != "projects" {
			continue
		}
		// {org}/{project} follows; anything shorter is a listing URL.
		if i+2 < len(segments) {
			return segments[i+2]
		}
		if i+1 < len(segments) {
			return segments[i+1]
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
