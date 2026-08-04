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

// Supabase Connect (OAuth 2.0). Like Airtable it requires PKCE and wants the
// client credentials as Basic auth on the token endpoint, so it reuses the
// verifier that was minted alongside the CSRF state.

const supabaseTokenURL = "https://api.supabase.com/v1/oauth/token"

func exchangeSupabaseCode(code, verifier string) (*models.IntegrationConnection, error) {
	if verifier == "" {
		return nil, fmt.Errorf("this Supabase connection attempt is missing its PKCE verifier — " +
			"start the connection again")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", oauthRedirectURI("supabase"))
	form.Set("code_verifier", verifier)

	req, _ := http.NewRequest(http.MethodPost, supabaseTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(os.Getenv("SUPABASE_CLIENT_ID"), os.Getenv("SUPABASE_CLIENT_SECRET"))

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
		return nil, fmt.Errorf("supabase token exchange returned no access token")
	}
	conn := &models.IntegrationConnection{
		Provider:     "supabase",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Scope:        tok.Scope,
	}
	if tok.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		conn.ExpiresAt = &exp
	}
	if org := supabaseFirstOrg(tok.AccessToken); org != "" {
		conn.WorkspaceName = org
	}
	return conn, nil
}

// supabaseFirstOrg labels the connection with the organization it reaches.
// Best effort: the grant may not include organizations:read.
func supabaseFirstOrg(token string) string {
	req, _ := http.NewRequest(http.MethodGet, "https://api.supabase.com/v1/organizations", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return ""
	}
	var orgs []struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if json.Unmarshal(raw, &orgs) != nil || len(orgs) == 0 {
		return ""
	}
	return firstNonEmptyStr(orgs[0].Name, orgs[0].Slug)
}

// supabaseResources lists the projects a connection can reach; the ref is what
// every project-scoped operation needs.
func supabaseResources(token string) ([]integrationResource, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.supabase.com/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var projects []struct {
		ID     string `json:"id"`
		Ref    string `json:"ref"`
		Name   string `json:"name"`
		Region string `json:"region"`
	}
	if json.Unmarshal(raw, &projects) != nil {
		return nil, fmt.Errorf("parse supabase projects")
	}
	res := make([]integrationResource, 0, len(projects))
	for _, p := range projects {
		// The ref, not the id: project endpoints 404 on the UUID.
		res = append(res, integrationResource{
			ID: firstNonEmptyStr(p.Ref, p.ID), Name: p.Name, Type: "project",
		})
	}
	return res, nil
}
