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

// Bitbucket Cloud OAuth 2.0. Its own authorization server, not Atlassian's, and
// it ignores a scope parameter entirely — the consumer's scopes are fixed when
// the consumer is created in Bitbucket workspace settings. So the connect URL
// carries no scope, and a missing permission surfaces as a 403 on the call
// rather than as a narrower grant.

const (
	bitbucketTokenURL = "https://bitbucket.org/site/oauth2/access_token"
	bitbucketAPIBase  = "https://api.bitbucket.org/2.0"
)

func exchangeBitbucketCode(code string) (*models.IntegrationConnection, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", oauthRedirectURI("bitbucket"))

	req, _ := http.NewRequest(http.MethodPost, bitbucketTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Bitbucket wants the client credentials as Basic auth on the token call.
	req.SetBasicAuth(os.Getenv("BITBUCKET_CLIENT_ID"), os.Getenv("BITBUCKET_CLIENT_SECRET"))

	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scopes       string `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("bitbucket token exchange returned no access token")
	}

	conn := &models.IntegrationConnection{
		Provider:     "bitbucket",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Scope:        tok.Scopes,
	}
	if tok.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		conn.ExpiresAt = &exp
	}

	// Best-effort display name, and the default workspace for ops that take one.
	if slug, name := bitbucketPrimaryWorkspace(tok.AccessToken); slug != "" {
		conn.WorkspaceID = slug
		conn.WorkspaceName = name
	}
	return conn, nil
}

// bitbucketPrimaryWorkspace returns (slug, display name) of the first workspace
// the token can see. The slug is what every repository path is built from.
func bitbucketPrimaryWorkspace(token string) (string, string) {
	req, _ := http.NewRequest(http.MethodGet, bitbucketAPIBase+"/workspaces?pagelen=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return "", ""
	}
	var out struct {
		Values []struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"values"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Values) == 0 {
		return "", ""
	}
	return out.Values[0].Slug, out.Values[0].Name
}
