package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type sentryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f sentryRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// sentryStub points the Sentry client at a fake and records what it asked for.
func sentryStub(t *testing.T, status int, body string) *http.Request {
	t.Helper()
	previousBase, previousClient := sentryAPIBaseOverride, integrationHTTP
	t.Cleanup(func() { sentryAPIBaseOverride, integrationHTTP = previousBase, previousClient })
	sentryAPIBaseOverride = "https://sentry.test/api/0"

	var seen http.Request
	integrationHTTP = &http.Client{Transport: sentryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = *r
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			seen.Body = io.NopCloser(strings.NewReader(string(raw)))
		}
		return &http.Response{StatusCode: status, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return &seen
}

func TestSentryListIssuesDefaultsToUnresolved(t *testing.T) {
	seen := sentryStub(t, http.StatusOK, `[{"id":"1","title":"Boom"}]`)

	if _, err := runSentry(context.Background(), "sentry-token", "acme", FlowNodeData{
		IntegrationOp: "list_issues", SentryProject: "backend", SentryStatsPeriod: "24h", SentryLimit: 10,
	}, nil); err != nil {
		t.Fatalf("runSentry: %v", err)
	}

	if seen.Header.Get("Authorization") != "Bearer sentry-token" {
		t.Errorf("authorization = %q", seen.Header.Get("Authorization"))
	}
	if seen.URL.Path != "/api/0/organizations/acme/issues/" {
		t.Errorf("path = %q", seen.URL.Path)
	}
	q := seen.URL.Query()
	// An empty search means "every issue ever" to Sentry, which is not what a
	// person means by "the issues".
	if q.Get("query") != "is:unresolved" {
		t.Errorf("query = %q, want the unresolved default", q.Get("query"))
	}
	if q.Get("project") != "backend" || q.Get("statsPeriod") != "24h" || q.Get("per_page") != "10" {
		t.Errorf("params = %v", q)
	}
}

func TestSentryTriageOpsSendTheRightStatus(t *testing.T) {
	cases := []struct {
		op         string
		data       FlowNodeData
		wantStatus string
		wantDetail map[string]any
	}{
		{op: "resolve_issue", wantStatus: "resolved"},
		{op: "unresolve_issue", wantStatus: "unresolved"},
		{op: "ignore_issue", wantStatus: "ignored"},
		{
			// The explicit field wins, so "resolve in the next release" is reachable.
			op: "resolve_issue", data: FlowNodeData{SentryStatus: "resolvedInNextRelease"},
			wantStatus: "resolvedInNextRelease",
		},
		{
			op: "ignore_issue", data: FlowNodeData{SentryIgnoreMinutes: 60},
			wantStatus: "ignored", wantDetail: map[string]any{"ignoreDuration": float64(60)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.op+"/"+tc.wantStatus, func(t *testing.T) {
			seen := sentryStub(t, http.StatusOK, `{"status":"ok"}`)
			data := tc.data
			data.IntegrationOp = tc.op
			data.SentryIssueId = "1234567890"

			if _, err := runSentry(context.Background(), "sentry-token", "acme", data, nil); err != nil {
				t.Fatalf("runSentry: %v", err)
			}
			if seen.Method != http.MethodPut {
				t.Errorf("method = %s, want PUT", seen.Method)
			}
			if seen.URL.Path != "/api/0/organizations/acme/issues/1234567890/" {
				t.Errorf("path = %q", seen.URL.Path)
			}
			var body map[string]any
			raw, _ := io.ReadAll(seen.Body)
			_ = json.Unmarshal(raw, &body)
			if body["status"] != tc.wantStatus {
				t.Errorf("status = %#v, want %q", body["status"], tc.wantStatus)
			}
			if tc.wantDetail != nil {
				details, _ := body["statusDetails"].(map[string]any)
				for key, want := range tc.wantDetail {
					if details[key] != want {
						t.Errorf("statusDetails[%q] = %#v, want %#v", key, details[key], want)
					}
				}
			}
		})
	}
}

func TestSentryCreateDeployPostsToTheRelease(t *testing.T) {
	seen := sentryStub(t, http.StatusCreated, `{"id":"7","environment":"production"}`)

	if _, err := runSentry(context.Background(), "sentry-token", "acme", FlowNodeData{
		IntegrationOp: "create_deploy", SentryVersion: "1.4.2", SentryEnvironment: "production",
		SentryProjects: "backend, frontend", SentryDeployName: "deploy-2419",
	}, nil); err != nil {
		t.Fatalf("runSentry: %v", err)
	}
	if seen.URL.Path != "/api/0/organizations/acme/releases/1.4.2/deploys/" {
		t.Errorf("path = %q", seen.URL.Path)
	}
	var body map[string]any
	raw, _ := io.ReadAll(seen.Body)
	_ = json.Unmarshal(raw, &body)
	if body["environment"] != "production" || body["name"] != "deploy-2419" {
		t.Errorf("body = %#v", body)
	}
	projects, _ := body["projects"].([]any)
	if len(projects) != 2 || projects[0] != "backend" || projects[1] != "frontend" {
		t.Errorf("projects = %#v", body["projects"])
	}
}

func TestSentryRequiresItsInputs(t *testing.T) {
	sentryStub(t, http.StatusOK, `{}`)
	cases := []struct {
		name string
		data FlowNodeData
	}{
		{"issue op without an issue", FlowNodeData{IntegrationOp: "get_issue"}},
		{"deploy without an environment", FlowNodeData{IntegrationOp: "create_deploy", SentryVersion: "1.0"}},
		{"release without a project", FlowNodeData{IntegrationOp: "create_release", SentryVersion: "1.0"}},
		{"comment with no text", FlowNodeData{IntegrationOp: "add_comment", SentryIssueId: "1"}},
		{"an operation nobody implements", FlowNodeData{IntegrationOp: "delete_organization"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := runSentry(context.Background(), "sentry-token", "acme", tc.data, nil); err == nil {
				t.Fatal("the call went out without its required input")
			}
		})
	}
}

// The organization comes from the connection. Without it every path would be
// malformed, so it fails here rather than as a confusing 404 from Sentry.
func TestSentryWithoutAnOrganizationFailsClearly(t *testing.T) {
	sentryStub(t, http.StatusOK, `[]`)
	_, err := runSentry(context.Background(), "sentry-token", "", FlowNodeData{IntegrationOp: "list_projects"}, nil)
	if err == nil || !strings.Contains(err.Error(), "reconnect Sentry") {
		t.Fatalf("error = %v, want a reconnect instruction", err)
	}
}

func TestSentryErrorsSayWhatSentrySaid(t *testing.T) {
	t.Run("detail", func(t *testing.T) {
		sentryStub(t, http.StatusNotFound, `{"detail":"The requested resource does not exist"}`)
		_, err := runSentry(context.Background(), "t", "acme", FlowNodeData{
			IntegrationOp: "get_issue", SentryIssueId: "1"}, nil)
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("field validation", func(t *testing.T) {
		sentryStub(t, http.StatusBadRequest, `{"version":["This field is required."]}`)
		_, err := runSentry(context.Background(), "t", "acme", FlowNodeData{
			IntegrationOp: "create_release", SentryVersion: "1.0", SentryProject: "backend"}, nil)
		if err == nil || !strings.Contains(err.Error(), "version: This field is required.") {
			t.Fatalf("error = %v", err)
		}
	})

	// A 401 is a dead installation token, and saying so beats "unauthorized".
	t.Run("expired installation", func(t *testing.T) {
		sentryStub(t, http.StatusUnauthorized, `{"detail":"Invalid token"}`)
		_, err := runSentry(context.Background(), "t", "acme", FlowNodeData{IntegrationOp: "list_projects"}, nil)
		if err == nil || !strings.Contains(err.Error(), "reconnect Sentry") {
			t.Fatalf("error = %v", err)
		}
	})
}

// The comments endpoint moved under /organizations/; older installs answer on
// the bare path. A 404 on the first shape retries the second.
func TestSentryCommentsFallBackToTheOlderPath(t *testing.T) {
	previousBase, previousClient := sentryAPIBaseOverride, integrationHTTP
	t.Cleanup(func() { sentryAPIBaseOverride, integrationHTTP = previousBase, previousClient })
	sentryAPIBaseOverride = "https://sentry.test/api/0"

	var paths []string
	integrationHTTP = &http.Client{Transport: sentryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/organizations/") {
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"detail":"not found"}`))}, nil
		}
		return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"id":"9"}`))}, nil
	})}

	out, err := runSentry(context.Background(), "t", "acme", FlowNodeData{
		IntegrationOp: "add_comment", SentryIssueId: "100", SentryComment: "on it"}, nil)
	if err != nil {
		t.Fatalf("runSentry: %v", err)
	}
	if !strings.Contains(out, `"id":"9"`) {
		t.Errorf("output = %s", out)
	}
	if len(paths) != 2 || paths[0] != "/api/0/organizations/acme/issues/100/comments/" ||
		paths[1] != "/api/0/issues/100/comments/" {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestSentryTemplatesResolveBeforeTheCallGoesOut(t *testing.T) {
	seen := sentryStub(t, http.StatusOK, `{}`)
	_, err := runSentry(context.Background(), "t", "acme", FlowNodeData{
		IntegrationOp: "get_issue", SentryIssueId: "{{trigger.output.id}}",
	}, map[string]string{"trigger": `{"id":"555"}`})
	if err != nil {
		t.Fatalf("runSentry: %v", err)
	}
	if !strings.Contains(seen.URL.Path, "/issues/555/") {
		t.Fatalf("path = %q, want the templated issue id", seen.URL.Path)
	}
}
