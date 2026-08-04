package handlers

import (
	"strings"
	"testing"
)

// The authorize query is the one part of an OAuth flow with no useful error
// message when it's wrong — Atlassian just refuses the grant or silently issues
// a token that can't refresh. So assert its shape directly.
func TestAtlassianAuthorizeQuery(t *testing.T) {
	for _, provider := range []string{"jira", "confluence"} {
		q := atlassianAuthorizeQuery(provider)

		if q.Get("audience") != "api.atlassian.com" {
			t.Errorf("%s: without audience=api.atlassian.com Atlassian issues an opaque token: %q",
				provider, q.Get("audience"))
		}
		if q.Get("prompt") != "consent" {
			t.Errorf("%s: prompt=consent is what makes a scope change re-prompt, got %q",
				provider, q.Get("prompt"))
		}
		scopes := strings.Fields(q.Get("scope"))
		if len(scopes) < 2 {
			t.Fatalf("%s: expected several scopes, got %q", provider, q.Get("scope"))
		}
		if !contains(scopes, "offline_access") {
			t.Errorf("%s: without offline_access there is no refresh token and the "+
				"connection dies in an hour: %v", provider, scopes)
		}
	}

	// The two products must not be handed each other's scopes.
	jira := atlassianAuthorizeQuery("jira").Get("scope")
	conf := atlassianAuthorizeQuery("confluence").Get("scope")
	if jira == conf {
		t.Fatal("jira and confluence requested identical scopes")
	}
	if strings.Contains(jira, "confluence") {
		t.Errorf("jira asked for confluence scopes: %s", jira)
	}
	if strings.Contains(conf, "jira") {
		t.Errorf("confluence asked for jira scopes: %s", conf)
	}
}

// Jira and Confluence share one Atlassian app; Bitbucket has its own.
func TestAtlassianProvidersShareOneApp(t *testing.T) {
	j, ok := oauthProviders["jira"]
	if !ok {
		t.Fatal("jira is not registered as an OAuth provider")
	}
	c := oauthProviders["confluence"]
	if j.clientIDEnv != c.clientIDEnv || j.clientIDEnv != "ATLASSIAN_CLIENT_ID" {
		t.Errorf("expected both to read ATLASSIAN_CLIENT_ID, got %q and %q", j.clientIDEnv, c.clientIDEnv)
	}
	if j.authorizeURL != atlassianAuthorizeURL {
		t.Errorf("jira points at the wrong authorize URL: %s", j.authorizeURL)
	}

	b := oauthProviders["bitbucket"]
	if b.clientIDEnv != "BITBUCKET_CLIENT_ID" {
		t.Errorf("bitbucket should have its own credentials, got %q", b.clientIDEnv)
	}
	if !strings.HasPrefix(b.authorizeURL, "https://bitbucket.org/") {
		t.Errorf("bitbucket authorizes against its own server, not Atlassian's: %s", b.authorizeURL)
	}
	// Bitbucket fixes scopes on the consumer and rejects a scope parameter.
	if b.extraAuthQ.Get("scope") != "" {
		t.Errorf("bitbucket must not send a scope parameter, got %q", b.extraAuthQ.Get("scope"))
	}
}

// Every provider the UI can list must be registered, or /connect 404s.
func TestNewProvidersAreListedAndRegistered(t *testing.T) {
	for _, p := range []string{"jira", "confluence", "bitbucket"} {
		if _, ok := oauthProviders[p]; !ok {
			t.Errorf("%s missing from oauthProviders", p)
		}
		if !contains(allProviders, p) {
			t.Errorf("%s missing from allProviders, so it never appears in the connections list", p)
		}
	}
}

// Atlassian rotates refresh tokens and wants JSON; Bitbucket wants Basic auth.
func TestNewProvidersCanRefresh(t *testing.T) {
	for _, p := range []string{"jira", "confluence", "bitbucket"} {
		ep, ok := refreshTokenEndpoints[p]
		if !ok {
			t.Errorf("%s has no refresh endpoint, so its connection expires permanently", p)
			continue
		}
		if ep.tokenURL == "" {
			t.Errorf("%s refresh endpoint has no token URL", p)
		}
	}
	if !refreshTokenEndpoints["jira"].jsonBody {
		t.Error("atlassian's token endpoint is documented as JSON")
	}
	if !refreshTokenEndpoints["bitbucket"].basicAuth {
		t.Error("bitbucket rejects client credentials in the body")
	}
	if refreshTokenEndpoints["gmail"].jsonBody || refreshTokenEndpoints["gmail"].basicAuth {
		t.Error("the existing form-encoded providers must be unaffected")
	}
}

func TestAtlassianFirstSiteRejectsAGrantWithNoSites(t *testing.T) {
	// A token with no accessible site cannot build a single valid URL, so the
	// connection must fail rather than be stored.
	if _, err := parseSites([]byte(`[]`)); err == nil {
		t.Fatal("expected an error when the grant covers no sites")
	}
	sites, err := parseSites([]byte(`[{"id":"cloud-1","name":"Acme","url":"https://acme.atlassian.net"}]`))
	if err != nil || sites.ID != "cloud-1" || sites.Name != "Acme" {
		t.Fatalf("failed to read the site: %+v / %v", sites, err)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
