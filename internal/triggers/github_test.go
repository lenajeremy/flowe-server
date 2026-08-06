package triggers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workflow-ai/server/internal/database/models"
)

// The GitHub adapter, which is also the template every other push provider
// copies — so the things pinned here are the things that must hold for all of
// them: a forged payload never runs anything, and an event nobody subscribed to
// costs nothing.

const testSecret = "s3cret-signing-key"

// The App's shared secret, as configured in the GitHub App's Webhook settings.
// Set per test rather than globally so the "not configured" case can exist.
func withAppSecret(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_WEBHOOK_SECRET", testSecret)
}

func signed(t *testing.T, event, body string) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, "/api/hooks/github/abc", strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-GitHub-Delivery", "delivery-1")
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return r
}

// Deliveries arrive at one app-level URL, so nothing about verification depends
// on which trigger the event turns out to be for.
func anyTrigger() *models.IntegrationTrigger { return &models.IntegrationTrigger{} }

// ── verification ──────────────────────────────────────────────

func TestAGenuineSignaturePasses(t *testing.T) {
	withAppSecret(t)
	body := `{"action":"opened"}`
	if err := (githubAdapter{}).Verify(signed(t, "pull_request", body), []byte(body), anyTrigger()); err != nil {
		t.Fatalf("a correctly signed delivery was rejected: %v", err)
	}
}

func TestATamperedBodyIsRejected(t *testing.T) {
	// The signature is computed over the original body; the handler then verifies
	// a different one. This is the attack the HMAC exists for — a payload edited
	// in flight to point at another repository, or to fake an approval.
	withAppSecret(t)
	body := `{"action":"opened"}`
	r := signed(t, "pull_request", body)
	tampered := []byte(`{"action":"opened","injected":true}`)
	if err := (githubAdapter{}).Verify(r, tampered, anyTrigger()); err == nil {
		t.Fatal("a modified payload passed verification")
	}
}

func TestAWrongSecretIsRejected(t *testing.T) {
	body := `{"action":"opened"}`
	r := signed(t, "pull_request", body) // signed with testSecret
	t.Setenv("GITHUB_WEBHOOK_SECRET", "a-completely-different-secret")
	if err := (githubAdapter{}).Verify(r, []byte(body), anyTrigger()); err == nil {
		t.Fatal("a payload signed with a different secret passed verification")
	}
}

func TestAnUnsignedRequestIsRejected(t *testing.T) {
	withAppSecret(t)
	body := `{"action":"opened"}`
	r := httptest.NewRequest(http.MethodPost, "/api/hooks/github/abc", strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", "pull_request")
	if err := (githubAdapter{}).Verify(r, []byte(body), anyTrigger()); err == nil {
		t.Fatal("an unsigned request passed verification")
	}
}

func TestNoConfiguredSecretMeansNothingIsAccepted(t *testing.T) {
	// One unset environment variable must not become an open door. A deploy that
	// forgets GITHUB_WEBHOOK_SECRET should take the feature offline, loudly —
	// not accept anything anyone posts.
	t.Setenv("GITHUB_WEBHOOK_SECRET", "")
	body := `{"action":"opened"}`
	if err := (githubAdapter{}).Verify(signed(t, "pull_request", body), []byte(body), anyTrigger()); err == nil {
		t.Fatal("an unconfigured secret accepted a signed payload")
	}
	// Including from a caller that passes no trigger at all — the app-level path.
	if err := (githubAdapter{}).Verify(signed(t, "pull_request", body), []byte(body), nil); err == nil {
		t.Fatal("an unconfigured secret accepted a payload on the app-level path")
	}
}

func TestRegisterRefusesTriggersThatCouldNeverFire(t *testing.T) {
	// Registration no longer creates anything at GitHub, so its only job is to
	// reject what would silently never work. Neither case reaches the network.
	a := githubAdapter{}
	if _, err := a.Register(context.Background(), Conn{}, &models.IntegrationTrigger{
		Event: "pull_request.reopened", ResourceID: "acme/widgets"}); err == nil {
		t.Error("an event no adapter parses was accepted")
	}
	if _, err := a.Register(context.Background(), Conn{}, &models.IntegrationTrigger{
		Event: "pull_request.opened"}); err == nil {
		t.Error("a trigger with no repository was accepted")
	}
}

// ── parsing ───────────────────────────────────────────────────

func TestAnOpenedPullRequestBecomesOneEvent(t *testing.T) {
	body := `{
		"action":"opened",
		"repository":{"full_name":"acme/widgets"},
		"pull_request":{"number":42,"title":"Fix the retry loop","html_url":"https://x/pull/42",
			"user":{"login":"octocat"},"base":{"ref":"main"},"head":{"ref":"fix"},
			"labels":[{"name":"bug"}]}
	}`
	events, err := (githubAdapter{}).Parse(signed(t, "pull_request", body), []byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want one event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != "pull_request.opened" {
		t.Errorf("type = %q", ev.Type)
	}
	if ev.Key != "delivery-1" {
		t.Errorf("dedupe key must be GitHub's delivery id, got %q", ev.Key)
	}
	if ev.ResourceID != "acme/widgets" {
		t.Errorf("resource = %q", ev.ResourceID)
	}
	if ev.Data["title"] != "Fix the retry loop" || ev.Data["base"] != "main" {
		t.Errorf("payload not flattened for templates: %#v", ev.Data)
	}
}

func TestActionsNobodySubscribedToProduceNoEvents(t *testing.T) {
	// GitHub subscribes at "pull_request", not "pull_request opened", so a busy
	// repo delivers labeled/synchronize/assigned all day. Each one that reaches a
	// workflow is a run somebody pays for.
	for _, action := range []string{"synchronize", "labeled", "assigned", "edited"} {
		body := `{"action":"` + action + `","repository":{"full_name":"acme/widgets"},
			"pull_request":{"number":42,"base":{"ref":"main"},"head":{"ref":"f"},"user":{"login":"o"}}}`
		events, err := (githubAdapter{}).Parse(signed(t, "pull_request", body), []byte(body))
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if len(events) != 0 {
			t.Errorf("%s produced %d events, want 0", action, len(events))
		}
	}
}

func TestClosedIsOnlyMergedWhenItActuallyMerged(t *testing.T) {
	base := `{"action":"closed","repository":{"full_name":"acme/widgets"},
		"pull_request":{"number":42,"merged":%s,"base":{"ref":"main"},"head":{"ref":"f"},"user":{"login":"o"}}}`

	merged := strings.Replace(base, "%s", "true", 1)
	events, _ := (githubAdapter{}).Parse(signed(t, "pull_request", merged), []byte(merged))
	if len(events) != 1 || events[0].Type != "pull_request.merged" {
		t.Errorf("a merged PR should be one merged event, got %#v", events)
	}

	// Closed without merging is somebody abandoning a PR. Treating it as merged
	// would ship a changelog entry for work that never landed.
	abandoned := strings.Replace(base, "%s", "false", 1)
	events, _ = (githubAdapter{}).Parse(signed(t, "pull_request", abandoned), []byte(abandoned))
	if len(events) != 0 {
		t.Errorf("a closed-unmerged PR produced %#v, want nothing", events)
	}
}

func TestThePingGitHubSendsOnCreationRunsNothing(t *testing.T) {
	body := `{"zen":"Non-blocking is better than blocking.","hook_id":1}`
	events, err := (githubAdapter{}).Parse(signed(t, "ping", body), []byte(body))
	if err != nil {
		t.Fatalf("the creation ping must not be an error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("the ping produced %d events", len(events))
	}
}

func TestTheInstallationRidesOnEveryEvent(t *testing.T) {
	// Routing depends on this. Two accounts can each own a repository called
	// "acme/widgets", and one app-level webhook hears both — without the
	// installation id on the event there is nothing to tell them apart, and one
	// account's pull request would start the other account's workflow.
	body := `{
		"action":"opened",
		"installation":{"id":48273901},
		"repository":{"full_name":"acme/widgets"},
		"pull_request":{"number":1,"base":{"ref":"main"},"head":{"ref":"f"},"user":{"login":"o"}}
	}`
	events, err := (githubAdapter{}).Parse(signed(t, "pull_request", body), []byte(body))
	if err != nil || len(events) != 1 {
		t.Fatalf("parse: %v, %d events", err, len(events))
	}
	if events[0].ScopeID != "48273901" {
		t.Errorf("installation id = %q, want \"48273901\"", events[0].ScopeID)
	}
}

func TestADeliveryWithNoIdIsRefused(t *testing.T) {
	// Without GitHub's delivery id there is nothing to deduplicate on, and a
	// redelivery would run the workflow twice. Better to refuse it.
	body := `{"action":"opened","repository":{"full_name":"a/b"},"pull_request":{"number":1,
		"base":{"ref":"main"},"head":{"ref":"f"},"user":{"login":"o"}}}`
	r := signed(t, "pull_request", body)
	r.Header.Del("X-GitHub-Delivery")
	if _, err := (githubAdapter{}).Parse(r, []byte(body)); err == nil {
		t.Fatal("a delivery with no id was accepted")
	}
}

func TestInstallationLifecycleEventsNeverBecomeWorkflowEvents(t *testing.T) {
	tests := []struct {
		name        string
		hookEvent   string
		body        string
		action      LifecycleAction
		resources   []string
		accountName string
		accountID   string
		scopeID     string
	}{
		{
			name: "installation deleted", hookEvent: "installation",
			body:   `{"action":"deleted","installation":{"id":48273901}}`,
			action: LifecycleScopeRemoved, scopeID: "48273901",
		},
		{
			name: "installation suspended", hookEvent: "installation",
			body:   `{"action":"suspend","installation":{"id":48273901}}`,
			action: LifecycleScopeSuspended, scopeID: "48273901",
		},
		{
			name: "installation restored", hookEvent: "installation",
			body:   `{"action":"unsuspend","installation":{"id":48273901}}`,
			action: LifecycleScopeRestored, scopeID: "48273901",
		},
		{
			name: "repositories removed", hookEvent: "installation_repositories",
			body:   `{"action":"removed","installation":{"id":48273901},"repositories_removed":[{"full_name":"acme/widgets"},{"full_name":"acme/api"}]}`,
			action: LifecycleResourcesRemoved, resources: []string{"acme/widgets", "acme/api"}, scopeID: "48273901",
		},
		{
			name: "repositories added", hookEvent: "installation_repositories",
			body:   `{"action":"added","installation":{"id":48273901},"repositories_added":[{"full_name":"acme/widgets"}]}`,
			action: LifecycleResourcesAdded, resources: []string{"acme/widgets"}, scopeID: "48273901",
		},
		{
			name: "authorization revoked", hookEvent: "github_app_authorization",
			body:   `{"action":"revoked","sender":{"id":583231,"login":"octocat"}}`,
			action: LifecycleAuthorizationRevoked, accountName: "octocat", accountID: "583231",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, err := (githubAdapter{}).Parse(signed(t, tt.hookEvent, tt.body), []byte(tt.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("want one lifecycle event, got %d", len(events))
			}
			ev := events[0]
			if ev.Type != "" {
				t.Fatalf("lifecycle event became workflow event %q", ev.Type)
			}
			if ev.Lifecycle == nil || ev.Lifecycle.Action != tt.action {
				t.Fatalf("lifecycle = %#v, want action %q", ev.Lifecycle, tt.action)
			}
			if ev.ScopeID != tt.scopeID {
				t.Errorf("scope = %q, want %q", ev.ScopeID, tt.scopeID)
			}
			if got := strings.Join(ev.Lifecycle.ResourceIDs, ","); got != strings.Join(tt.resources, ",") {
				t.Errorf("resources = %q, want %q", got, strings.Join(tt.resources, ","))
			}
			if ev.Lifecycle.AccountName != tt.accountName {
				t.Errorf("account = %q, want %q", ev.Lifecycle.AccountName, tt.accountName)
			}
			if ev.Lifecycle.AccountID != tt.accountID {
				t.Errorf("account id = %q, want %q", ev.Lifecycle.AccountID, tt.accountID)
			}
		})
	}
}

func TestInstallationLifecycleWithoutItsRoutingKeyIsRefused(t *testing.T) {
	for _, hookEvent := range []string{"installation", "installation_repositories"} {
		body := `{"action":"deleted"}`
		if hookEvent == "installation_repositories" {
			body = `{"action":"removed","repositories_removed":[{"full_name":"acme/widgets"}]}`
		}
		if _, err := (githubAdapter{}).Parse(signed(t, hookEvent, body), []byte(body)); err == nil {
			t.Errorf("%s without installation id was accepted", hookEvent)
		}
	}
}

// ── filters ───────────────────────────────────────────────────

func filtered(t *testing.T, filters string, data map[string]any) bool {
	t.Helper()
	return Matches(&models.IntegrationTrigger{Filters: models.JSONB(filters)},
		Event{Type: "pull_request.opened", Data: data})
}

func TestFiltersKeepTheEventsTheyName(t *testing.T) {
	data := map[string]any{"base": "main", "author": "octocat", "labels": []string{"bug", "p1"}}

	if !filtered(t, `{"base":"main"}`, data) {
		t.Error("a matching filter dropped the event")
	}
	if filtered(t, `{"base":"develop"}`, data) {
		t.Error("a PR into develop passed a filter for main")
	}
	if !filtered(t, `{"base":"MAIN"}`, data) {
		t.Error("filters should not be case-sensitive — nobody types branch names twice the same way")
	}
	// A list field matches when any element does: "has label bug" is what a
	// person means, not "the only label is bug".
	if !filtered(t, `{"labels":"p1"}`, data) {
		t.Error("a label filter failed to match one of several labels")
	}
	if filtered(t, `{"labels":"wontfix"}`, data) {
		t.Error("a label the PR does not carry matched anyway")
	}
}

func TestAnEmptyFilterMeansEverything(t *testing.T) {
	data := map[string]any{"base": "main"}
	if !filtered(t, `{}`, data) {
		t.Error("no filters should match everything")
	}
	// The UI sends blank strings for fields the user left alone. Treating those
	// as "must equal empty string" would make a trigger that never fires.
	if !filtered(t, `{"base":"","author":"  "}`, data) {
		t.Error("blank filter values should be ignored, not matched literally")
	}
}

func TestAFilterOnAFieldTheEventLacksDoesNotMatch(t *testing.T) {
	// The alternative — passing when the field is missing — would silently widen
	// a filter the user believes is narrowing.
	if filtered(t, `{"branch":"main"}`, map[string]any{"base": "main"}) {
		t.Error("a filter naming an absent field matched")
	}
}

// ── registry ──────────────────────────────────────────────────

func TestTheRegistryOnlyAdmitsEventsAnAdapterImplements(t *testing.T) {
	if !Supports("github", "pull_request.opened") {
		t.Error("a real event was rejected")
	}
	if Supports("github", "pull_request.reopened") {
		t.Error("an event no adapter handles was accepted — it would register a hook that never fires")
	}
	if Supports("nosuchprovider", "anything") {
		t.Error("an unknown provider was accepted")
	}
}

func TestEveryAdvertisedEventCanBeRegistered(t *testing.T) {
	// The catalogue drives the UI dropdown and the AI builder's enum. An event
	// offered there but missing from the hook-event map would register nothing
	// and fail at creation time instead.
	for _, spec := range (githubAdapter{}).Events() {
		if _, ok := githubHookEvents[spec.ID]; !ok {
			t.Errorf("%q is offered in the catalogue but has no GitHub hook event", spec.ID)
		}
	}
}

func TestTheCatalogueIsSerializable(t *testing.T) {
	// It is sent to the browser and inlined into the AI prompt; a type that will
	// not marshal breaks both surfaces at once.
	if _, err := json.Marshal(Catalog()); err != nil {
		t.Fatalf("catalogue does not marshal: %v", err)
	}
}
