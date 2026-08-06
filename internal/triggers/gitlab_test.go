package triggers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"workflow-ai/server/internal/database/models"

	"github.com/google/uuid"
)

var gitlabTestKey = []byte("0123456789abcdef0123456789abcdef")

func gitlabTestToken() string {
	return "whsec_" + base64.StdEncoding.EncodeToString(gitlabTestKey)
}

func gitlabSignedRequest(event, body, token string, at time.Time) *http.Request {
	messageID := "delivery-gitlab-1"
	timestamp := strconv.FormatInt(at.Unix(), 10)
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(token, "whsec_"))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(messageID + "." + timestamp + "." + body))
	signature := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest(http.MethodPost, "/api/hooks/gitlab/trigger", strings.NewReader(body))
	r.Header.Set("X-Gitlab-Event", event)
	r.Header.Set("webhook-id", messageID)
	r.Header.Set("webhook-timestamp", timestamp)
	r.Header.Set("webhook-signature", signature)
	return r
}

func gitlabTestTrigger(event, project string) *models.IntegrationTrigger {
	return &models.IntegrationTrigger{
		BaseModel:  models.BaseModel{ID: uuid.MustParse("d1424948-aed9-455f-a362-6cb3c1ec96d0")},
		Event:      event,
		ResourceID: project,
		Secret:     gitlabTestToken(),
	}
}

func redirectGitLabHooksTo(url string) func() {
	previous := gitlabHooksAPIBase
	gitlabHooksAPIBase = url
	return func() { gitlabHooksAPIBase = previous }
}

func TestGitLabRegisterCreatesAProjectScopedSignedHook(t *testing.T) {
	t.Setenv("PUBLIC_BASE_URL", "https://api.fernary.test")
	var created map[string]any
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Errorf("authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/projects/123/hooks":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Fatalf("decode registration: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":987,"project_id":123}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/projects/123/hooks/987":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	restore := redirectGitLabHooksTo(srv.URL)
	defer restore()

	trigger := gitlabTestTrigger("merge_request.opened", "123")
	trigger.Secret = ""
	reg, err := (gitlabAdapter{}).Register(context.Background(), Conn{AccessToken: "oauth-token"}, trigger)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.RemoteID != "987" || !strings.HasPrefix(reg.Secret, "whsec_") {
		t.Fatalf("registration = %#v", reg)
	}
	wantCallback := "https://api.fernary.test/api/hooks/gitlab/" + trigger.ID.String() + "?registration="
	if !strings.HasPrefix(fmt.Sprint(created["url"]), wantCallback) {
		t.Errorf("callback URL = %v", created["url"])
	}
	if created["merge_requests_events"] != true || created["push_events"] != false {
		t.Errorf("wrong event flags: %#v", created)
	}
	if _, usedLegacyToken := created["token"]; usedLegacyToken {
		t.Error("registered GitLab's weaker plaintext secret token instead of a signing token")
	}
	if created["signing_token"] != reg.Secret {
		t.Error("stored signing token differs from the one registered at GitLab")
	}

	trigger.RemoteID = reg.RemoteID
	if err := (gitlabAdapter{}).Unregister(context.Background(), Conn{AccessToken: "oauth-token"}, trigger); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if !deleted {
		t.Error("deleting the Fernary trigger did not remove the GitLab project hook")
	}
}

func TestEveryGitLabCatalogEventHasAProjectHookFlag(t *testing.T) {
	events := (gitlabAdapter{}).Events()
	if len(events) != len(gitlabHookFields) {
		t.Fatalf("catalog has %d events but hook mapping has %d", len(events), len(gitlabHookFields))
	}
	for _, event := range events {
		if len(gitlabHookFields[event.ID]) == 0 {
			t.Errorf("catalog event %q cannot be registered at GitLab", event.ID)
		}
		if event.ResourceKind != "project" {
			t.Errorf("catalog event %q is scoped to %q instead of a project", event.ID, event.ResourceKind)
		}
	}
}

func TestGitLabRegisterPushesBranchFilteringToGitLab(t *testing.T) {
	t.Setenv("PUBLIC_BASE_URL", "https://api.fernary.test")
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&created)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":9,"project_id":44}`))
	}))
	defer srv.Close()
	restore := redirectGitLabHooksTo(srv.URL)
	defer restore()

	trigger := gitlabTestTrigger("push", "44")
	trigger.Secret = ""
	trigger.Filters = models.JSONB(`{"branch":"release/*"}`)
	if _, err := (gitlabAdapter{}).Register(context.Background(), Conn{AccessToken: "token"}, trigger); err != nil {
		t.Fatal(err)
	}
	if created["push_events"] != true || created["push_events_branch_filter"] != "release/*" {
		t.Errorf("push filter was not registered at GitLab: %#v", created)
	}
}

func TestGitLabRegisterExplainsProjectRoleRequirement(t *testing.T) {
	t.Setenv("PUBLIC_BASE_URL", "https://api.fernary.test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"403 Forbidden"}`))
	}))
	defer srv.Close()
	restore := redirectGitLabHooksTo(srv.URL)
	defer restore()

	trigger := gitlabTestTrigger("issues.opened", "123")
	trigger.Secret = ""
	_, err := (gitlabAdapter{}).Register(context.Background(), Conn{AccessToken: "token"}, trigger)
	if err == nil || !strings.Contains(err.Error(), "Maintainer or Owner") {
		t.Fatalf("role failure was not actionable: %v", err)
	}
}

func TestGitLabRejectsInvalidOrUnreachableProjectHookTargets(t *testing.T) {
	a := gitlabAdapter{}
	if _, err := a.Register(context.Background(), Conn{}, gitlabTestTrigger("unknown", "123")); err == nil {
		t.Error("unknown event was accepted")
	}
	if _, err := a.Register(context.Background(), Conn{}, gitlabTestTrigger("issues.opened", "acme/widgets")); err == nil {
		t.Error("non-numeric project id was accepted even though payload routing uses numeric ids")
	}
	t.Setenv("PUBLIC_BASE_URL", "http://localhost:8080")
	if _, err := a.Register(context.Background(), Conn{}, gitlabTestTrigger("issues.opened", "123")); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("unreachable callback was accepted: %v", err)
	}
}

func TestGitLabStandardWebhookSignatureVerification(t *testing.T) {
	token := gitlabTestToken()
	trigger := gitlabTestTrigger("issues.opened", "123")
	body := `{"object_kind":"issue"}`
	r := gitlabSignedRequest("Issue Hook", body, token, time.Now())
	if err := (gitlabAdapter{}).Verify(r, []byte(body), trigger); err != nil {
		t.Fatalf("valid GitLab signature rejected: %v", err)
	}
	validSignature := r.Header.Get("webhook-signature")
	r.Header.Set("webhook-signature", "v1,not-the-right-signature "+validSignature)
	if err := (gitlabAdapter{}).Verify(r, []byte(body), trigger); err != nil {
		t.Fatalf("valid signature in GitLab's multi-signature header was rejected: %v", err)
	}

	if err := (gitlabAdapter{}).Verify(r, []byte(body+" "), trigger); err == nil {
		t.Error("tampered GitLab payload passed verification")
	}
	stale := gitlabSignedRequest("Issue Hook", body, token, time.Now().Add(-10*time.Minute))
	if err := (gitlabAdapter{}).Verify(stale, []byte(body), trigger); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Errorf("stale signed payload was accepted: %v", err)
	}
	if err := (gitlabAdapter{}).Verify(r, []byte(body), nil); err == nil {
		t.Error("per-project GitLab delivery passed without its trigger signing key")
	}
}

func TestGitLabOpenedMergeRequestBecomesOneEvent(t *testing.T) {
	body := `{
		"object_kind":"merge_request",
		"user":{"username":"alex"},
		"project":{"id":123,"path_with_namespace":"acme/widgets","web_url":"https://gitlab.com/acme/widgets"},
		"object_attributes":{"action":"open","iid":42,"title":"Fix retries","description":"Body",
			"url":"https://gitlab.com/acme/widgets/-/merge_requests/42","state":"opened",
			"source_branch":"fix-retries","target_branch":"main"},
		"labels":[{"title":"bug"}]
	}`
	r := gitlabSignedRequest("Merge Request Hook", body, gitlabTestToken(), time.Now())
	events, err := (gitlabAdapter{}).Parse(r, []byte(body))
	if err != nil || len(events) != 1 {
		t.Fatalf("parse: %v, events: %d", err, len(events))
	}
	ev := events[0]
	if ev.Type != "merge_request.opened" || ev.ResourceID != "123" || ev.Key != "delivery-gitlab-1" {
		t.Errorf("routing fields = %#v", ev)
	}
	if ev.Data["number"] != int64(42) || ev.Data["base"] != "main" || ev.Data["author"] != "alex" {
		t.Errorf("merge request payload not flattened: %#v", ev.Data)
	}
	if !valueMatches(ev.Data["label"], "bug") {
		t.Errorf("labels are not filterable: %#v", ev.Data["label"])
	}
}

func TestGitLabEditedIssueIncludesPreviousValues(t *testing.T) {
	body := `{
		"object_kind":"issue",
		"user":{"username":"alex"},
		"project":{"id":123,"path_with_namespace":"acme/widgets"},
		"object_attributes":{"action":"update","iid":17,"title":"New title","description":"New body",
			"url":"https://gitlab.com/acme/widgets/-/issues/17","state":"opened"},
		"labels":[{"title":"p1"}],
		"changes":{"title":{"previous":"Old title","current":"New title"},
			"description":{"previous":"Old body","current":"New body"}}
	}`
	r := gitlabSignedRequest("Issue Hook", body, gitlabTestToken(), time.Now())
	events, err := (gitlabAdapter{}).Parse(r, []byte(body))
	if err != nil || len(events) != 1 {
		t.Fatalf("parse: %v, events: %d", err, len(events))
	}
	ev := events[0]
	if ev.Type != "issues.edited" || ev.Data["previous_title"] != "Old title" || ev.Data["previous_body"] != "Old body" {
		t.Errorf("issue update context missing: %#v", ev.Data)
	}
	if got := fmt.Sprint(ev.Data["changed_fields"]); got != "[description title]" {
		t.Errorf("changed fields = %s", got)
	}
}

func TestGitLabIssueCommentIncludesIssueContext(t *testing.T) {
	body := `{
		"object_kind":"note",
		"user":{"username":"reviewer"},
		"project":{"id":123,"path_with_namespace":"acme/widgets"},
		"object_attributes":{"id":991,"action":"create","note":"I can reproduce it",
			"noteable_type":"Issue","url":"https://gitlab.com/acme/widgets/-/issues/17#note_991"},
		"issue":{"iid":17,"title":"Crash","description":"Steps","url":"https://gitlab.com/acme/widgets/-/issues/17",
			"labels":[{"title":"bug"}]}
	}`
	r := gitlabSignedRequest("Note Hook", body, gitlabTestToken(), time.Now())
	events, err := (gitlabAdapter{}).Parse(r, []byte(body))
	if err != nil || len(events) != 1 {
		t.Fatalf("parse: %v, events: %d", err, len(events))
	}
	ev := events[0]
	if ev.Type != "note.created" || ev.Data["body"] != "I can reproduce it" || ev.Data["number"] != int64(17) {
		t.Errorf("comment context missing: %#v", ev.Data)
	}
	if ev.Data["is_merge_request"] != false || !valueMatches(ev.Data["label"], "bug") {
		t.Errorf("issue note classified incorrectly: %#v", ev.Data)
	}
}

func TestGitLabDropsActionsTheTriggerCatalogDoesNotOffer(t *testing.T) {
	tests := []struct {
		event string
		body  string
	}{
		{"Merge Request Hook", `{"object_kind":"merge_request","project":{"id":123},"object_attributes":{"action":"update"}}`},
		{"Issue Hook", `{"object_kind":"issue","project":{"id":123},"object_attributes":{"action":"close"}}`},
		{"Note Hook", `{"object_kind":"note","project":{"id":123},"object_attributes":{"action":"update","noteable_type":"Issue"}}`},
		{"Release Hook", `{"object_kind":"release","project":{"id":123},"action":"delete"}`},
	}
	for _, test := range tests {
		r := gitlabSignedRequest(test.event, test.body, gitlabTestToken(), time.Now())
		events, err := (gitlabAdapter{}).Parse(r, []byte(test.body))
		if err != nil || len(events) != 0 {
			t.Errorf("%s: got %d events / %v", test.event, len(events), err)
		}
	}
}

func TestGitLabPushAndReleasePayloadsAreNormalized(t *testing.T) {
	push := `{"object_kind":"push","ref":"refs/heads/main","before":"aaa","after":"bbb",
		"user_username":"alex","project":{"id":123,"path_with_namespace":"acme/widgets"},
		"total_commits_count":2,"commits":[{"id":"c1","message":"one"},{"id":"c2","message":"two"}]}`
	r := gitlabSignedRequest("Push Hook", push, gitlabTestToken(), time.Now())
	events, err := (gitlabAdapter{}).Parse(r, []byte(push))
	if err != nil || len(events) != 1 || events[0].Type != "push" {
		t.Fatalf("push parse: %v / %#v", err, events)
	}
	if events[0].Data["branch"] != "main" || events[0].Data["commit_count"] != 2 {
		t.Errorf("push payload = %#v", events[0].Data)
	}

	release := `{"object_kind":"release","action":"create","id":7,"tag":"v1.2","name":"Version 1.2",
		"description":"Notes","url":"https://gitlab.com/acme/widgets/-/releases/v1.2",
		"project":{"id":123,"path_with_namespace":"acme/widgets"}}`
	r = gitlabSignedRequest("Release Hook", release, gitlabTestToken(), time.Now())
	events, err = (gitlabAdapter{}).Parse(r, []byte(release))
	if err != nil || len(events) != 1 || events[0].Type != "release.published" {
		t.Fatalf("release parse: %v / %#v", err, events)
	}
	if events[0].Data["tag"] != "v1.2" || events[0].Data["release_id"] != int64(7) {
		t.Errorf("release payload = %#v", events[0].Data)
	}
}
