package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGitHubInstallURLUsesOnlyAValidatedAppSlug(t *testing.T) {
	t.Setenv("GITHUB_APP_SLUG", "fernary-workflows")
	got, err := githubAppInstallURL("one-time-state")
	if err != nil {
		t.Fatalf("valid slug rejected: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse install URL: %v", err)
	}
	if u.Scheme != "https" || u.Host != "github.com" || u.Path != "/apps/fernary-workflows/installations/new" {
		t.Fatalf("unexpected install URL: %s", got)
	}
	if u.Query().Get("state") != "one-time-state" {
		t.Fatalf("install state was not preserved: %s", got)
	}

	for _, invalid := range []string{"", "two words", "app/../../evil", "-starts-with-dash", "ends-with-dash-"} {
		t.Setenv("GITHUB_APP_SLUG", invalid)
		if got, err := githubAppInstallURL("state"); err == nil || got != "" {
			t.Errorf("unsafe slug %q produced %q, %v", invalid, got, err)
		}
	}
}

func TestGitHubInstallationSettingsURLIsRestrictedToGitHub(t *testing.T) {
	for _, valid := range []string{
		"https://github.com/settings/installations/151704344",
		"https://github.com/organizations/acme-inc/settings/installations/42",
	} {
		if got := safeGitHubInstallationSettingsURL(valid); got != valid {
			t.Errorf("valid settings URL %q became %q", valid, got)
		}
	}
	for _, invalid := range []string{
		"http://github.com/settings/installations/42",
		"https://evil.example/settings/installations/42",
		"https://github.com/settings/installations/0",
		"https://github.com/settings/installations/42?next=https://evil.example",
		"https://github.com/apps/fernary-ai/installations/new",
	} {
		if got := safeGitHubInstallationSettingsURL(invalid); got != "" {
			t.Errorf("unsafe settings URL %q was accepted as %q", invalid, got)
		}
	}
}

func TestGitHubInstallStateIsBoundAndSingleUse(t *testing.T) {
	state := newGitHubInstallState("user-1", "org-1", "https://app.fernary.com")
	entry, ok := consumeOAuthState(state)
	if !ok {
		t.Fatal("fresh installation state was rejected")
	}
	if entry.userID != "user-1" || entry.orgID != "org-1" || entry.origin != "https://app.fernary.com" || !entry.githubInstall {
		t.Fatalf("installation state lost its binding: %#v", entry)
	}
	if _, ok := consumeOAuthState(state); ok {
		t.Fatal("installation state was accepted twice")
	}
}

func TestGitHubAuthorizeURLHasNoClassicOAuthScope(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "Iv1.fernary")
	t.Setenv("GITHUB_CLIENT_SECRET", "secret")
	t.Setenv("OAUTH_REDIRECT_BASE", "https://api.fernary.com")

	got, err := githubUserAuthorizeURL("state")
	if err != nil {
		t.Fatalf("authorize URL: %v", err)
	}
	u, _ := url.Parse(got)
	if scope := u.Query().Get("scope"); scope != "" {
		t.Fatalf("GitHub App authorization carried classic OAuth scope %q", scope)
	}
	if !strings.HasSuffix(u.Query().Get("redirect_uri"), "/api/integrations/github/callback") {
		t.Fatalf("wrong callback in %s", got)
	}
	if scope := oauthProviders["github"].extraAuthQ.Get("scope"); scope != "" {
		t.Fatalf("shared connect flow still adds classic OAuth scope %q", scope)
	}
}

func TestGitHubInstallationIDMustBePositiveNumeric(t *testing.T) {
	if got, err := positiveGitHubID("42"); err != nil || got != "42" {
		t.Fatalf("valid id: got %q, %v", got, err)
	}
	for _, invalid := range []string{"", "0", "-1", "1/../2", "not-a-number"} {
		if _, err := positiveGitHubID(invalid); err == nil {
			t.Errorf("invalid installation id %q was accepted", invalid)
		}
	}
}

func TestGitHubSetupCallbackCarriesTheExactInstallationIntoOAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("GITHUB_CLIENT_ID", "Iv1.fernary")
	t.Setenv("GITHUB_CLIENT_SECRET", "secret")
	t.Setenv("OAUTH_REDIRECT_BASE", "https://api.fernary.com")
	installState := newGitHubInstallState("user-1", "org-1", "https://app.fernary.com")

	router := gin.New()
	h := &WorkflowHandler{}
	router.GET("/api/integrations/github/setup/callback", h.GitHubSetupCallback)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/api/integrations/github/setup/callback?setup_action=install&installation_id=48273901&state="+installState, nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	if location.Host != "github.com" || location.Path != "/login/oauth/authorize" {
		t.Fatalf("unexpected OAuth redirect: %s", location)
	}
	oauthState, ok := consumeOAuthState(location.Query().Get("state"))
	if !ok {
		t.Fatal("setup callback did not mint a usable OAuth state")
	}
	if oauthState.userID != "user-1" || oauthState.orgID != "org-1" ||
		oauthState.origin != "https://app.fernary.com" || oauthState.githubInstallationID != "48273901" {
		t.Fatalf("OAuth state lost installation binding: %#v", oauthState)
	}
}

func TestGitHubSetupCallbackDoesNotAuthorizeAPendingRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	state := newGitHubInstallState("user-1", "org-1", "https://app.fernary.com")
	router := gin.New()
	h := &WorkflowHandler{}
	router.GET("/api/integrations/github/setup/callback", h.GitHubSetupCallback)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/api/integrations/github/setup/callback?setup_action=request&installation_id=48273901&state="+state, nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "has not been approved") {
		t.Fatalf("pending request response = %d, %s", recorder.Code, recorder.Body.String())
	}
}
