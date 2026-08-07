package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
)

const (
	mondayTokenURL = "https://auth.monday.com/oauth_ms/oauth/token"
	asanaTokenURL  = "https://app.asana.com/-/oauth_token"
)

var (
	mondayGraphQLURL = "https://api.monday.com/v2"
	asanaAPIBase     = "https://app.asana.com/api/1.0"
)

// exchangeMondayCode implements monday.com's current OAuth 2.1 flow. New-flow
// access tokens are JWTs and the token response does not consistently include
// expires_in, so the signed token's exp claim becomes the stored refresh time.
func exchangeMondayCode(code, verifier string) (*models.IntegrationConnection, error) {
	if verifier == "" {
		return nil, fmt.Errorf("this monday.com connection attempt is missing its PKCE verifier — start the connection again")
	}
	body, _ := json.Marshal(map[string]string{
		"grant_type": "authorization_code", "code": code,
		"client_id": os.Getenv("MONDAY_CLIENT_ID"), "client_secret": os.Getenv("MONDAY_CLIENT_SECRET"),
		"redirect_uri": oauthRedirectURI("monday"), "code_verifier": verifier,
	})
	req, _ := http.NewRequest(http.MethodPost, mondayTokenURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		msg := firstNonBlank(tok.Description, tok.Error, "token exchange returned no access token")
		return nil, fmt.Errorf("monday.com %s", msg)
	}
	conn := &models.IntegrationConnection{
		Provider: "monday", AccessToken: tok.AccessToken,
		RefreshToken: tok.RefreshToken, Scope: tok.Scope,
	}
	setOAuthExpiry(conn, tok.AccessToken, tok.ExpiresIn)
	if identity, err := mondayIdentity(tok.AccessToken); err == nil {
		conn.WorkspaceID = identity.AccountID
		conn.WorkspaceName = identity.AccountName
		if conn.WorkspaceName == "" {
			conn.WorkspaceName = identity.UserName
		}
	}
	return conn, nil
}

type mondayAccountIdentity struct {
	UserName    string
	AccountID   string
	AccountName string
}

func mondayIdentity(token string) (mondayAccountIdentity, error) {
	var response struct {
		Me struct {
			Name    string `json:"name"`
			Account struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"account"`
		} `json:"me"`
	}
	if err := mondayGraphQL(token, `query { me { name account { id name } } }`, nil, &response); err != nil {
		return mondayAccountIdentity{}, err
	}
	return mondayAccountIdentity{
		UserName: response.Me.Name, AccountID: response.Me.Account.ID, AccountName: response.Me.Account.Name,
	}, nil
}

func setOAuthExpiry(conn *models.IntegrationConnection, accessToken string, expiresIn int64) {
	if expiresIn > 0 {
		exp := time.Now().Add(time.Duration(expiresIn) * time.Second)
		conn.ExpiresAt = &exp
		return
	}
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) == nil && claims.ExpiresAt > 0 {
		exp := time.Unix(claims.ExpiresAt, 0)
		conn.ExpiresAt = &exp
	}
}

func mondayGraphQL(token, query string, variables map[string]any, target any) error {
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	req, _ := http.NewRequest(http.MethodPost, mondayGraphQLURL, bytes.NewReader(payload))
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("API-Version", "2026-04")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return err
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("parse monday.com response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("monday.com API error: %s", envelope.Errors[0].Message)
	}
	if target != nil && json.Unmarshal(envelope.Data, target) != nil {
		return fmt.Errorf("parse monday.com response data")
	}
	return nil
}

func mondayResources(token string) ([]integrationResource, error) {
	var response struct {
		Workspaces []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"workspaces"`
		Boards []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"boards"`
		Users []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"users"`
	}
	query := `query { workspaces { id name } boards(limit: 100, state: active) { id name workspace_id } users(limit: 100) { id name } }`
	if err := mondayGraphQL(token, query, nil, &response); err != nil {
		return nil, err
	}
	out := make([]integrationResource, 0, len(response.Workspaces)+len(response.Boards)+len(response.Users))
	for _, workspace := range response.Workspaces {
		out = append(out, integrationResource{ID: workspace.ID, Name: workspace.Name, Type: "workspace"})
	}
	for _, board := range response.Boards {
		out = append(out, integrationResource{ID: board.ID, Name: board.Name, Type: "board"})
	}
	for _, user := range response.Users {
		out = append(out, integrationResource{ID: user.ID, Name: user.Name, Type: "user"})
	}
	return out, nil
}

func mondayBoardResources(token, boardID string) ([]integrationResource, error) {
	if _, err := positiveProviderID("monday.com board", boardID); err != nil {
		return nil, err
	}
	var response struct {
		Boards []struct {
			Groups []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"groups"`
			Columns []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"columns"`
		} `json:"boards"`
	}
	if err := mondayGraphQL(token, `query ($board: [ID!]) { boards(ids: $board) { groups { id title } columns { id title } } }`,
		map[string]any{"board": []string{boardID}}, &response); err != nil {
		return nil, err
	}
	if len(response.Boards) != 1 {
		return nil, fmt.Errorf("monday.com board %s is unavailable to this connection", boardID)
	}
	out := make([]integrationResource, 0, len(response.Boards[0].Groups)+len(response.Boards[0].Columns))
	for _, group := range response.Boards[0].Groups {
		out = append(out, integrationResource{ID: group.ID, Name: group.Title, Type: "group"})
	}
	for _, column := range response.Boards[0].Columns {
		out = append(out, integrationResource{ID: column.ID, Name: column.Title, Type: "column"})
	}
	return out, nil
}

func exchangeAsanaCode(code, verifier string) (*models.IntegrationConnection, error) {
	if verifier == "" {
		return nil, fmt.Errorf("this Asana connection attempt is missing its PKCE verifier — start the connection again")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", os.Getenv("ASANA_CLIENT_ID"))
	form.Set("client_secret", os.Getenv("ASANA_CLIENT_SECRET"))
	form.Set("redirect_uri", oauthRedirectURI("asana"))
	form.Set("code_verifier", verifier)
	req, _ := http.NewRequest(http.MethodPost, asanaTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Data         struct {
			GID  string `json:"gid"`
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		msg := firstNonBlank(tok.Description, tok.Error, "token exchange returned no access token")
		return nil, fmt.Errorf("Asana %s", msg)
	}
	workspaceID := tok.Data.GID
	if workspaceID == "" && tok.Data.ID > 0 {
		workspaceID = strconv.FormatInt(tok.Data.ID, 10)
	}
	conn := &models.IntegrationConnection{
		Provider: "asana", AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
		Scope: tok.Scope, WorkspaceID: workspaceID, WorkspaceName: tok.Data.Name,
	}
	setOAuthExpiry(conn, tok.AccessToken, tok.ExpiresIn)
	return conn, nil
}

func asanaGET(token, path string, target any) error {
	req, _ := http.NewRequest(http.MethodGet, strings.TrimRight(asanaAPIBase, "/")+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("parse Asana response: %w", err)
	}
	return nil
}

type asanaCompactList struct {
	Data []struct {
		GID  string `json:"gid"`
		Name string `json:"name"`
	} `json:"data"`
}

func asanaResources(token string) ([]integrationResource, error) {
	var workspaces asanaCompactList
	if err := asanaGET(token, "/workspaces?limit=100", &workspaces); err != nil {
		return nil, err
	}
	out := make([]integrationResource, 0, len(workspaces.Data)*2)
	seenProjects := map[string]bool{}
	for _, workspace := range workspaces.Data {
		out = append(out, integrationResource{ID: workspace.GID, Name: workspace.Name, Type: "workspace"})
		var projects asanaCompactList
		path := "/workspaces/" + url.PathEscape(workspace.GID) + "/projects?archived=false&limit=100"
		if err := asanaGET(token, path, &projects); err != nil {
			return nil, err
		}
		for _, project := range projects.Data {
			if seenProjects[project.GID] {
				continue
			}
			seenProjects[project.GID] = true
			out = append(out, integrationResource{ID: project.GID, Name: project.Name, Type: "project"})
		}
	}
	return out, nil
}

func asanaProjectResources(token, projectID string) ([]integrationResource, error) {
	if _, err := positiveProviderID("Asana project", projectID); err != nil {
		return nil, err
	}
	var sections asanaCompactList
	if err := asanaGET(token, "/projects/"+url.PathEscape(projectID)+"/sections?limit=100", &sections); err != nil {
		return nil, err
	}
	var tasks asanaCompactList
	q := url.Values{"project": {projectID}, "limit": {"100"}, "completed_since": {"now"}}
	if err := asanaGET(token, "/tasks?"+q.Encode(), &tasks); err != nil {
		return nil, err
	}
	out := make([]integrationResource, 0, len(sections.Data)+len(tasks.Data))
	for _, section := range sections.Data {
		out = append(out, integrationResource{ID: section.GID, Name: section.Name, Type: "section"})
	}
	for _, task := range tasks.Data {
		out = append(out, integrationResource{ID: task.GID, Name: task.Name, Type: "task"})
	}
	return out, nil
}

func positiveProviderID(label, value string) (string, error) {
	value = strings.TrimSpace(value)
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != value {
		return "", fmt.Errorf("%s id must be a positive decimal number", label)
	}
	return value, nil
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
