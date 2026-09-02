package triggers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"workflow-ai/server/internal/database/models"
)

const sentryTestSecret = "sentry-client-secret"

// sentryRequest builds a signed delivery the way Sentry does: hex HMAC-SHA256
// of the raw body under the app's client secret.
func sentryRequest(t *testing.T, resource string, body []byte) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(sentryTestSecret))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/sentry", nil)
	req.Header.Set("Sentry-Hook-Resource", resource)
	req.Header.Set("Sentry-Hook-Signature", hex.EncodeToString(mac.Sum(nil)))
	return req
}

const sentryIssueBody = `{
  "action": "created",
  "installation": {"uuid": "install-abc"},
  "actor": {"type": "application", "id": "sentry", "name": "Sentry"},
  "data": {"issue": {
    "id": "1234567890",
    "shortId": "BACKEND-4F",
    "title": "TypeError: cannot read property 'id' of undefined",
    "culprit": "app/routes/checkout.ts in submit",
    "level": "error",
    "status": "unresolved",
    "count": "128",
    "userCount": 41,
    "lastSeen": "2026-08-27T10:15:00Z",
    "web_url": "https://sentry.io/organizations/acme/issues/1234567890/",
    "project": {"slug": "backend", "name": "Backend"}
  }}
}`

func TestSentryVerifyAcceptsAGenuineSignature(t *testing.T) {
	t.Setenv("SENTRY_CLIENT_SECRET", sentryTestSecret)
	body := []byte(sentryIssueBody)
	if err := (sentryAdapter{}).Verify(sentryRequest(t, "issue", body), body, nil); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestSentryVerifyRejectsTamperedBodyAndWrongSecret(t *testing.T) {
	body := []byte(sentryIssueBody)

	t.Run("tampered body", func(t *testing.T) {
		t.Setenv("SENTRY_CLIENT_SECRET", sentryTestSecret)
		req := sentryRequest(t, "issue", body)
		if err := (sentryAdapter{}).Verify(req, append(body, ' '), nil); err == nil {
			t.Fatal("a changed body passed verification")
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		req := sentryRequest(t, "issue", body)
		t.Setenv("SENTRY_CLIENT_SECRET", "someone-elses-secret")
		if err := (sentryAdapter{}).Verify(req, body, nil); err == nil {
			t.Fatal("a signature from another app passed verification")
		}
	})

	t.Run("unsigned", func(t *testing.T) {
		t.Setenv("SENTRY_CLIENT_SECRET", sentryTestSecret)
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Sentry-Hook-Resource", "issue")
		if err := (sentryAdapter{}).Verify(req, body, nil); err == nil {
			t.Fatal("an unsigned request passed verification")
		}
	})

	// An unset secret must fail closed, not wave everything through.
	t.Run("no secret configured", func(t *testing.T) {
		t.Setenv("SENTRY_CLIENT_SECRET", "")
		req := sentryRequest(t, "issue", body)
		if err := (sentryAdapter{}).Verify(req, body, nil); err == nil {
			t.Fatal("verification passed with no configured secret")
		}
	})
}

func TestSentryParseNormalizesEachResource(t *testing.T) {
	cases := []struct {
		name       string
		resource   string
		body       string
		wantType   string
		wantProj   string
		wantFields map[string]any
	}{
		{
			name: "issue", resource: "issue", body: sentryIssueBody,
			wantType: "issue.created", wantProj: "backend",
			wantFields: map[string]any{
				"id": "1234567890", "shortId": "BACKEND-4F", "level": "error",
				"count": "128", "actor": "Sentry",
			},
		},
		{
			// The error payload never names the project; only the issue URL does.
			name: "error", resource: "error", wantType: "error.created", wantProj: "frontend",
			body: `{"action":"created","installation":{"uuid":"install-abc"},"data":{"error":{
				"event_id":"c3f2d1e0","issue_id":"999","title":"ReferenceError: blooopy is not defined",
				"level":"error","platform":"javascript","timestamp":1786000000,
				"issue_url":"https://sentry.io/api/0/projects/acme/frontend/issues/999/",
				"web_url":"https://sentry.io/organizations/acme/issues/999/",
				"tags":[["environment","production"],["release","1.4.2"]]}}}`,
			wantFields: map[string]any{
				"eventId": "c3f2d1e0", "issueId": "999",
				"environment": "production", "release": "1.4.2",
			},
		},
		{
			name: "issue alert", resource: "event_alert", wantType: "event_alert.triggered", wantProj: "frontend",
			body: `{"action":"triggered","installation":{"uuid":"install-abc"},"data":{
				"event":{"event_id":"aa11","issue_id":"999","title":"Boom","level":"error",
				  "issue_url":"https://sentry.io/api/0/projects/acme/frontend/issues/999/"},
				"triggered_rule":"Production pager"}}`,
			wantFields: map[string]any{"rule": "Production pager", "eventId": "aa11"},
		},
		{
			name: "metric alert", resource: "metric_alert", wantType: "metric_alert.critical", wantProj: "backend",
			body: `{"action":"critical","installation":{"uuid":"install-abc"},"data":{
				"metric_alert":{"id":12,"title":"Checkout latency","projects":["backend"],
				  "alert_rule":{"name":"Checkout latency"}},
				"description_title":"Critical: Checkout latency",
				"description_text":"Latency is above 500ms",
				"web_url":"https://sentry.io/organizations/acme/alerts/rules/details/12/"}}`,
			wantFields: map[string]any{
				"rule": "Checkout latency", "status": "critical", "incidentId": "12",
			},
		},
		{
			name: "comment", resource: "comment", wantType: "comment.created", wantProj: "backend",
			body: `{"action":"created","installation":{"uuid":"install-abc"},
				"actor":{"type":"user","id":1,"name":"colleen"},
				"data":{"comment":"adding a comment","project_slug":"backend",
				  "comment_id":1234,"issue_id":100,"timestamp":"2026-08-27T21:51:44Z"}}`,
			wantFields: map[string]any{
				"comment": "adding a comment", "commentId": "1234", "issueId": "100", "actor": "colleen",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			events, err := (sentryAdapter{}).Parse(sentryRequest(t, tc.resource, body), body)
			if err != nil || len(events) != 1 {
				t.Fatalf("Parse = %#v, %v", events, err)
			}
			event := events[0]
			if event.Type != tc.wantType {
				t.Errorf("type = %q, want %q", event.Type, tc.wantType)
			}
			if event.ResourceID != tc.wantProj {
				t.Errorf("project = %q, want %q", event.ResourceID, tc.wantProj)
			}
			if event.ScopeID != "install-abc" {
				t.Errorf("scope = %q, want the installation uuid", event.ScopeID)
			}
			if event.Key == "" {
				t.Error("event has no dedupe key")
			}
			for field, want := range tc.wantFields {
				if got := event.Data[field]; got != want {
					t.Errorf("data[%q] = %#v, want %#v", field, got, want)
				}
			}
		})
	}
}

// The app subscribes per resource, so it hears actions nobody asked for. Those
// are acknowledged and dropped rather than treated as a failure.
func TestSentryParseIgnoresUnsubscribedActions(t *testing.T) {
	body := []byte(`{"action":"ignored_forever","installation":{"uuid":"install-abc"},"data":{"issue":{"id":"1"}}}`)
	events, err := (sentryAdapter{}).Parse(sentryRequest(t, "issue", body), body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %#v, want none", events)
	}
}

func TestSentryParseTurnsUninstallIntoLifecycleNotAWorkflowInput(t *testing.T) {
	body := []byte(`{"action":"deleted","installation":{"uuid":"install-abc","status":"deleted"}}`)
	events, err := (sentryAdapter{}).Parse(sentryRequest(t, "installation", body), body)
	if err != nil || len(events) != 1 {
		t.Fatalf("Parse = %#v, %v", events, err)
	}
	event := events[0]
	if event.Lifecycle == nil || event.Lifecycle.Action != LifecycleScopeRemoved {
		t.Fatalf("lifecycle = %#v", event.Lifecycle)
	}
	if event.Data != nil {
		t.Errorf("a lifecycle event carried workflow data: %#v", event.Data)
	}

	// An install is our own callback's business, not a workflow's.
	created := []byte(`{"action":"created","installation":{"uuid":"install-abc"}}`)
	events, err = (sentryAdapter{}).Parse(sentryRequest(t, "installation", created), created)
	if err != nil || len(events) != 0 {
		t.Fatalf("installation.created produced %#v, %v", events, err)
	}
}

// Retries replay the same bytes, so they must collapse to one key; two
// genuinely different events must not.
func TestSentryDeliveryKeyCollapsesRetriesOnly(t *testing.T) {
	body := []byte(sentryIssueBody)
	first := sentryDeliveryKey("issue", "created", body)
	if first != sentryDeliveryKey("issue", "created", body) {
		t.Fatal("the same delivery produced two keys")
	}
	other := sentryDeliveryKey("issue", "created", []byte(`{"action":"created","data":{"issue":{"id":"2"}}}`))
	if first == other {
		t.Fatal("two different events share a dedupe key")
	}
}

func TestSentryProjectFromURL(t *testing.T) {
	cases := map[string]string{
		"https://sentry.io/api/0/projects/acme/backend/issues/1/": "backend",
		"https://sentry.io/organizations/acme/projects/backend/":  "backend",
		"https://sentry.io/organizations/acme/issues/1/":          "",
		"": "",
	}
	for url, want := range cases {
		if got := sentryProjectFromURL(url); got != want {
			t.Errorf("sentryProjectFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}

// Registration is what pins a trigger to one installation. Without a project
// or a configured signing secret there is nothing safe to register.
func TestSentryRegisterRefusesIncompleteSetup(t *testing.T) {
	t.Setenv("SENTRY_CLIENT_SECRET", sentryTestSecret)
	t.Setenv("SENTRY_APP_SLUG", "fernary")

	if _, err := (sentryAdapter{}).Register(t.Context(), Conn{},
		&models.IntegrationTrigger{Provider: "sentry", Event: "issue.created"}); err == nil {
		t.Fatal("registered a trigger with no project")
	}
	if _, err := (sentryAdapter{}).Register(t.Context(), Conn{},
		&models.IntegrationTrigger{Provider: "sentry", Event: "nope.invented", ResourceID: "backend"}); err == nil {
		t.Fatal("registered a trigger for an event no adapter implements")
	}

	t.Setenv("SENTRY_CLIENT_SECRET", "")
	if _, err := (sentryAdapter{}).Register(t.Context(), Conn{},
		&models.IntegrationTrigger{Provider: "sentry", Event: "issue.created", ResourceID: "backend"}); err == nil {
		t.Fatal("registered a trigger whose deliveries could never be verified")
	}
}

// A metric alert can span several projects, and a trigger is set up against
// one. Emitting a single event carrying only the first project silently ignores
// every trigger watching the others.
func TestSentryMetricAlertReachesEveryProjectItNames(t *testing.T) {
	body := []byte(`{"action":"critical","installation":{"uuid":"install-abc"},"data":{
		"metric_alert":{"id":12,"title":"Checkout latency","projects":["backend","payments","web"],
		  "alert_rule":{"name":"Checkout latency"}},
		"description_title":"Critical: Checkout latency",
		"web_url":"https://sentry.io/organizations/acme/alerts/rules/details/12/"}}`)

	events, err := (sentryAdapter{}).Parse(sentryRequest(t, "metric_alert", body), body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want one per project", len(events))
	}

	seen := map[string]bool{}
	keys := map[string]bool{}
	for _, event := range events {
		seen[event.ResourceID] = true
		if event.Data["project"] != event.ResourceID {
			t.Errorf("data.project = %#v, want %q", event.Data["project"], event.ResourceID)
		}
		if event.Data["rule"] != "Checkout latency" {
			t.Errorf("event lost its shared fields: %#v", event.Data)
		}
		if keys[event.Key] {
			// Same key means the second and third are treated as retries of the
			// first and dropped by the dedupe table.
			t.Errorf("two projects share the dedupe key %q", event.Key)
		}
		keys[event.Key] = true
	}
	for _, project := range []string{"backend", "payments", "web"} {
		if !seen[project] {
			t.Errorf("no event for project %q", project)
		}
	}
}

// A single-project alert must keep the plain body-hash key, so a genuine
// redelivery still collapses to one run.
func TestSentrySingleProjectAlertKeepsTheRetryProofKey(t *testing.T) {
	body := []byte(`{"action":"critical","installation":{"uuid":"install-abc"},"data":{
		"metric_alert":{"id":12,"title":"Checkout latency","projects":["backend"],
		  "alert_rule":{"name":"Checkout latency"}}}}`)

	first, err := (sentryAdapter{}).Parse(sentryRequest(t, "metric_alert", body), body)
	if err != nil || len(first) != 1 {
		t.Fatalf("Parse = %#v, %v", first, err)
	}
	second, err := (sentryAdapter{}).Parse(sentryRequest(t, "metric_alert", body), body)
	if err != nil || len(second) != 1 {
		t.Fatalf("Parse = %#v, %v", second, err)
	}
	if first[0].Key != second[0].Key {
		t.Fatal("a redelivery produced a different key, so it would buy a second run")
	}
}
