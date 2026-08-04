package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"workflow-ai/server/internal/database/models"
)

// ClickUp OAuth 2.0, which departs from the usual shape in three ways:
//
//   - The authorize URL is on the app domain (app.clickup.com/api), not the API
//     host, and it takes no scope parameter — consent is per workspace, chosen by
//     the user on ClickUp's screen.
//   - The token exchange passes its parameters as a query string, not a body.
//   - Access tokens do not expire and no refresh token is issued, so there is no
//     entry in refreshTokenEndpoints. A connection lasts until the user revokes
//     it in ClickUp.

const (
	clickupAuthorizeURL = "https://app.clickup.com/api"
	clickupTokenURL     = "https://api.clickup.com/api/v2/oauth/token"
)

func exchangeClickUpCode(code string) (*models.IntegrationConnection, error) {
	q := url.Values{}
	q.Set("client_id", os.Getenv("CLICKUP_CLIENT_ID"))
	q.Set("client_secret", os.Getenv("CLICKUP_CLIENT_SECRET"))
	q.Set("code", code)

	req, _ := http.NewRequest(http.MethodPost, clickupTokenURL+"?"+q.Encode(), nil)
	req.Header.Set("Accept", "application/json")

	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("clickup token exchange returned no access token")
	}

	conn := &models.IntegrationConnection{
		Provider:    "clickup",
		AccessToken: tok.AccessToken,
		// No scope string: ClickUp grants workspace-level access with no scopes.
		Scope: "workspace",
	}
	// Label the connection with the workspaces it can actually reach, since that
	// is what the grant is scoped to.
	if name := clickupWorkspaceName(tok.AccessToken); name != "" {
		conn.WorkspaceName = name
	}
	return conn, nil
}

func clickupWorkspaceName(token string) string {
	teams, err := clickupTeams(token)
	if err != nil || len(teams) == 0 {
		return ""
	}
	if len(teams) == 1 {
		return teams[0].Name
	}
	return fmt.Sprintf("%s and %d more", teams[0].Name, len(teams)-1)
}

type clickupTeam struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func clickupTeams(token string) ([]clickupTeam, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.clickup.com/api/v2/team", nil)
	// An OAuth token needs the Bearer prefix here; a personal token would not.
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Teams []clickupTeam `json:"teams"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return nil, fmt.Errorf("parse clickup workspaces")
	}
	return out.Teams, nil
}

// clickupResources lists the workspaces a connection can reach, whose ids go in
// clickupWorkspaceId — the starting point for most other ClickUp operations.
func clickupResources(token string) ([]integrationResource, error) {
	teams, err := clickupTeams(token)
	if err != nil {
		return nil, err
	}
	res := make([]integrationResource, 0, len(teams))
	for _, t := range teams {
		res = append(res, integrationResource{ID: t.ID, Name: t.Name, Type: "workspace"})
	}
	return res, nil
}
