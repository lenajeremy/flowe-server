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

// Token exchange + resource listing for the Google-suite (Calendar, Drive,
// Docs, Sheets), Microsoft Outlook (Graph), and Slack providers. The Google
// services share one OAuth app and token endpoint, differing only in scope.

// ── Google (Calendar / Drive / Docs / Sheets) ─────────────────

// exchangeGoogleServiceCode swaps the auth code for tokens using the shared
// Google sign-in app. The provider name is stored so each service keeps its own
// connection row (and scope) even though the credentials are shared. (Distinct
// from auth.exchangeGoogleCode, which handles sign-in and returns id-token claims.)
func exchangeGoogleServiceCode(provider, code string) (*models.IntegrationConnection, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", oauthRedirectURI(provider))
	form.Set("client_id", os.Getenv("GOOGLE_CLIENT_ID"))
	form.Set("client_secret", os.Getenv("GOOGLE_CLIENT_SECRET"))

	req, _ := http.NewRequest(http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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
		return nil, fmt.Errorf("%s token exchange returned no access token", provider)
	}
	conn := &models.IntegrationConnection{
		Provider:     provider,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Scope:        tok.Scope,
	}
	if tok.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		conn.ExpiresAt = &exp
	}
	if email := googleUserEmail(tok.AccessToken); email != "" {
		conn.WorkspaceName = email
	}
	return conn, nil
}

func googleUserEmail(token string) string {
	req, _ := http.NewRequest(http.MethodGet, "https://www.googleapis.com/oauth2/v2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return ""
	}
	var p struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return p.Email
}

func googleCalendarResources(token string) ([]integrationResource, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://www.googleapis.com/calendar/v3/users/me/calendarList?maxResults=100", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		Items []struct {
			ID      string `json:"id"`
			Summary string `json:"summary"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse calendar list: %w", err)
	}
	out := make([]integrationResource, 0, len(res.Items))
	for _, it := range res.Items {
		out = append(out, integrationResource{ID: it.ID, Name: it.Summary, Type: "calendar"})
	}
	return out, nil
}

func googleDriveResources(token string) ([]integrationResource, error) {
	q := url.Values{}
	q.Set("q", "mimeType='application/vnd.google-apps.folder' and trashed=false")
	q.Set("pageSize", "100")
	q.Set("fields", "files(id,name)")
	q.Set("orderBy", "name")
	req, _ := http.NewRequest(http.MethodGet, "https://www.googleapis.com/drive/v3/files?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		Files []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse drive files: %w", err)
	}
	out := make([]integrationResource, 0, len(res.Files))
	for _, f := range res.Files {
		out = append(out, integrationResource{ID: f.ID, Name: f.Name, Type: "folder"})
	}
	return out, nil
}

// ── Microsoft Outlook (Graph) ─────────────────────────────────

func exchangeOutlookCode(code string) (*models.IntegrationConnection, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", oauthRedirectURI("outlook"))
	form.Set("client_id", os.Getenv("MICROSOFT_CLIENT_ID"))
	form.Set("client_secret", os.Getenv("MICROSOFT_CLIENT_SECRET"))
	form.Set("scope", "offline_access Mail.ReadWrite Mail.Send Calendars.ReadWrite User.Read")

	req, _ := http.NewRequest(http.MethodPost, "https://login.microsoftonline.com/common/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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
		return nil, fmt.Errorf("outlook token exchange returned no access token")
	}
	conn := &models.IntegrationConnection{
		Provider:     "outlook",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Scope:        tok.Scope,
	}
	if tok.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		conn.ExpiresAt = &exp
	}
	if email := outlookUserEmail(tok.AccessToken); email != "" {
		conn.WorkspaceName = email
	}
	return conn, nil
}

func outlookUserEmail(token string) string {
	req, _ := http.NewRequest(http.MethodGet, "https://graph.microsoft.com/v1.0/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return ""
	}
	var p struct {
		Mail              string `json:"mail"`
		UserPrincipalName string `json:"userPrincipalName"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	if p.Mail != "" {
		return p.Mail
	}
	return p.UserPrincipalName
}

func outlookResources(token string) ([]integrationResource, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://graph.microsoft.com/v1.0/me/mailFolders?$top=60", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse outlook folders: %w", err)
	}
	out := make([]integrationResource, 0, len(res.Value))
	for _, f := range res.Value {
		out = append(out, integrationResource{ID: f.ID, Name: f.DisplayName, Type: "folder"})
	}
	return out, nil
}

// ── Slack ─────────────────────────────────────────────────────

func exchangeSlackCode(code string) (*models.IntegrationConnection, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("redirect_uri", oauthRedirectURI("slack"))
	form.Set("client_id", os.Getenv("SLACK_CLIENT_ID"))
	form.Set("client_secret", os.Getenv("SLACK_CLIENT_SECRET"))

	req, _ := http.NewRequest(http.MethodPost, "https://slack.com/api/oauth.v2.access", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Slack answers 200 even on failure, signalling errors via {ok:false}.
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		OK          bool   `json:"ok"`
		Error       string `json:"error"`
		AccessToken string `json:"access_token"` // bot token (xoxb-…)
		Scope       string `json:"scope"`
		Team        struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"team"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("parse slack token: %w", err)
	}
	if !tok.OK || tok.AccessToken == "" {
		return nil, fmt.Errorf("slack token exchange failed: %s", tok.Error)
	}
	return &models.IntegrationConnection{
		Provider:      "slack",
		AccessToken:   tok.AccessToken,
		Scope:         tok.Scope,
		WorkspaceName: tok.Team.Name,
		WorkspaceID:   tok.Team.ID,
	}, nil
}

func slackResources(token string) ([]integrationResource, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://slack.com/api/conversations.list?limit=200&exclude_archived=true&types=public_channel,private_channel", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Channels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse slack channels: %w", err)
	}
	if !res.OK {
		return nil, fmt.Errorf("slack channels error: %s", res.Error)
	}
	out := make([]integrationResource, 0, len(res.Channels))
	for _, ch := range res.Channels {
		out = append(out, integrationResource{ID: ch.ID, Name: "#" + ch.Name, Type: "channel"})
	}
	return out, nil
}
