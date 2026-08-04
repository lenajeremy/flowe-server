package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
)

// Airtable OAuth 2.0. Three things differ from every other provider here:
//
//   - PKCE is mandatory, not optional. An authorize request without a challenge
//     is rejected outright (see integrations_pkce.go).
//   - The token endpoint wants the client credentials as HTTP Basic auth
//     whenever the integration has a secret, not in the body.
//   - Access tokens last 60 minutes and refresh tokens rotate on every use, so
//     the shared refresh path has to persist the new refresh token — which it
//     already does for the providers that rotate.

const (
	airtableAuthorizeURL = "https://airtable.com/oauth2/v1/authorize"
	airtableTokenURL     = "https://airtable.com/oauth2/v1/token"
)

var airtableScopes = []string{
	"data.records:read", "data.records:write",
	"data.recordComments:read", "data.recordComments:write",
	"schema.bases:read", "schema.bases:write",
	"webhook:manage",
	"user.email:read",
}

func airtableAuthorizeQuery() url.Values {
	return url.Values{"scope": {strings.Join(airtableScopes, " ")}}
}

// exchangeAirtableCode redeems the code, replaying the PKCE verifier that was
// minted with the CSRF state.
func exchangeAirtableCode(code, verifier string) (*models.IntegrationConnection, error) {
	if verifier == "" {
		return nil, fmt.Errorf("this Airtable connection attempt is missing its PKCE verifier — " +
			"start the connection again")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", oauthRedirectURI("airtable"))
	form.Set("code_verifier", verifier)

	req, _ := http.NewRequest(http.MethodPost, airtableTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(os.Getenv("AIRTABLE_CLIENT_ID"), os.Getenv("AIRTABLE_CLIENT_SECRET"))

	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("airtable token exchange returned no access token")
	}

	conn := &models.IntegrationConnection{
		Provider:     "airtable",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Scope:        tok.Scope,
	}
	if tok.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		conn.ExpiresAt = &exp
	}
	if email := airtableUserEmail(tok.AccessToken); email != "" {
		conn.WorkspaceName = email
	}
	return conn, nil
}

// airtableUserEmail labels the connection with whose account it is. Best effort:
// the email scope may be absent, which is not a reason to fail the connection.
func airtableUserEmail(token string) string {
	req, _ := http.NewRequest(http.MethodGet, "https://api.airtable.com/v0/meta/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return ""
	}
	var me struct {
		Email string `json:"email"`
		ID    string `json:"id"`
	}
	if json.Unmarshal(raw, &me) != nil {
		return ""
	}
	return firstNonEmptyStr(me.Email, me.ID)
}

// airtableResources lists the bases a connection can reach, whose ids go in
// airtableBaseId.
func airtableResources(token string) ([]integrationResource, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.airtable.com/v0/meta/bases", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Bases []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"bases"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return nil, fmt.Errorf("parse airtable bases")
	}
	res := make([]integrationResource, 0, len(out.Bases))
	for _, b := range out.Bases {
		res = append(res, integrationResource{ID: b.ID, Name: b.Name, Type: "base"})
	}
	return res, nil
}
