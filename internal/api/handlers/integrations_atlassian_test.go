package handlers

import (
	"crypto/sha256"
	"encoding/base64"
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

// The six new Google services share the sign-in app but must each carry their
// own scope, and every one needs offline access or the connection dies in an hour.
func TestGoogleServiceScopes(t *testing.T) {
	want := map[string]string{
		"googlemeet":   "meetings.space.created",
		"googleslides": "presentations",
		"googleforms":  "forms.body",
		"googletasks":  "auth/tasks",
		"googlechat":   "chat.spaces",
		"googlekeep":   "auth/keep",
	}
	seen := map[string]string{}
	for provider, marker := range want {
		prov, ok := oauthProviders[provider]
		if !ok {
			t.Errorf("%s is not registered", provider)
			continue
		}
		if prov.clientIDEnv != "GOOGLE_CLIENT_ID" {
			t.Errorf("%s should reuse the Google sign-in app, got %q", provider, prov.clientIDEnv)
		}
		scope := prov.extraAuthQ.Get("scope")
		if !strings.Contains(scope, marker) {
			t.Errorf("%s is missing its own API scope (%s): %q", provider, marker, scope)
		}
		// Google only returns a refresh token when asked offline, with consent.
		if prov.extraAuthQ.Get("access_type") != "offline" {
			t.Errorf("%s must request offline access or it cannot refresh", provider)
		}
		if prov.extraAuthQ.Get("prompt") != "consent" {
			t.Errorf("%s must force the consent screen to receive a refresh token", provider)
		}
		if other, dup := seen[scope]; dup {
			t.Errorf("%s and %s request identical scopes — one of them is wrong", provider, other)
		}
		seen[scope] = provider
	}
}

// Google Chat and Keep are Workspace-only, which arrives as a 403. The executor
// annotates that so the user isn't left guessing; assert the wiring exists.
func TestWorkspaceOnlyProvidersAreRegistered(t *testing.T) {
	for _, p := range []string{"googlechat", "googlekeep"} {
		if _, ok := oauthProviders[p]; !ok {
			t.Errorf("%s is not registered", p)
		}
		if _, ok := refreshTokenEndpoints[p]; !ok {
			t.Errorf("%s cannot refresh", p)
		}
	}
}

// Every provider the UI can list must be registered, or /connect 404s.
func TestNewProvidersAreListedAndRegistered(t *testing.T) {
	for _, p := range []string{"jira", "confluence", "bitbucket",
		"googlemeet", "googleslides", "googleforms", "googletasks", "googlechat", "googlekeep"} {
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

// PKCE is not optional for Airtable: an authorize request without a challenge is
// rejected, and a verifier that does not survive to the token exchange fails it.
func TestPKCEChallengeIsS256OfTheVerifier(t *testing.T) {
	v := newPKCEVerifier()
	if len(v) < 43 {
		t.Fatalf("RFC 7636 requires a verifier of at least 43 characters, got %d", len(v))
	}
	// The verifier must be URL-safe, since it travels as a query/form value.
	if strings.ContainsAny(v, "+/=") {
		t.Errorf("verifier is not base64url-encoded: %q", v)
	}
	sum := sha256.Sum256([]byte(v))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := pkceChallenge(v); got != want {
		t.Errorf("challenge is not the S256 of the verifier:\n got %q\nwant %q", got, want)
	}
	// Two verifiers must never collide, or one flow could redeem another's code.
	if newPKCEVerifier() == v {
		t.Error("two verifiers came back identical")
	}
}

func TestOAuthStateCarriesTheVerifierAndIsSingleUse(t *testing.T) {
	state := newOAuthStateFull("user-1", "org-1", "https://example.com", "", "verifier-abc")
	got, ok := consumeOAuthState(state)
	if !ok {
		t.Fatal("state did not validate")
	}
	if got.verifier != "verifier-abc" {
		t.Errorf("verifier lost in transit: %q", got.verifier)
	}
	if got.userID != "user-1" || got.origin != "https://example.com" {
		t.Errorf("state lost its other fields: %+v", got)
	}
	// Replaying a state must fail, or a leaked code could be redeemed twice.
	if _, ok := consumeOAuthState(state); ok {
		t.Error("state was accepted a second time")
	}
}

func TestOnlyPKCEProvidersGetAChallenge(t *testing.T) {
	if !pkceProviders["airtable"] {
		t.Error("airtable must use PKCE — Airtable rejects the flow without it")
	}
	// Sending an unexpected code_challenge to a provider that does not support it
	// can fail the authorize request, so the set stays deliberately narrow.
	for _, p := range []string{"github", "jira", "googlemeet", "bitbucket"} {
		if pkceProviders[p] {
			t.Errorf("%s is not known to support PKCE; adding it needs verification first", p)
		}
	}
}

// A provider that expires its tokens needs both a refresh endpoint AND whatever
// scope earns it a refresh token. Having one without the other looks fine until
// the connection silently dies days later.
func TestExpiringProvidersCanActuallyRenew(t *testing.T) {
	// provider → the scope substring that earns a refresh token, if one is needed.
	needsOfflineScope := map[string]string{
		"jira":       "offline_access",
		"confluence": "offline_access",
		"typeform":   "offline",
		"dropbox":    "", // uses token_access_type=offline instead of a scope
	}
	for provider, marker := range needsOfflineScope {
		prov, ok := oauthProviders[provider]
		if !ok {
			t.Errorf("%s is not registered", provider)
			continue
		}
		if _, ok := refreshTokenEndpoints[provider]; !ok {
			t.Errorf("%s expires its tokens but has no refresh endpoint", provider)
		}
		if marker != "" && !strings.Contains(prov.extraAuthQ.Get("scope"), marker) {
			t.Errorf("%s must request %q or it never receives a refresh token: %q",
				provider, marker, prov.extraAuthQ.Get("scope"))
		}
	}
	// Dropbox earns its refresh token through a parameter rather than a scope.
	if oauthProviders["dropbox"].extraAuthQ.Get("token_access_type") != "offline" {
		t.Error("dropbox must request offline access or its short-lived token cannot be renewed")
	}
	// The reverse: a provider with no expiry must not claim a refresh endpoint,
	// which would only ever fail.
	for _, p := range []string{"clickup", "netlify"} {
		if _, ok := refreshTokenEndpoints[p]; ok {
			t.Errorf("%s issues no refresh token, so a refresh endpoint is misleading", p)
		}
	}
}
