package handlers

import (
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

// Atlassian OAuth 2.0 (3LO) — shared by Jira and Confluence.
//
// Two things make this different from every other provider here:
//
//  1. One OAuth app covers both products. They differ only in scope, so
//     ATLASSIAN_CLIENT_ID/_SECRET back both "jira" and "confluence".
//  2. Requests do not go to the site's own domain. Every call is
//     https://api.atlassian.com/ex/{product}/{cloudId}/... where cloudId
//     identifies the site, and is only discoverable *after* the token exchange
//     via /oauth/token/accessible-resources. We store it in WorkspaceID so the
//     executor can build URLs without a lookup per call.
//
// Refresh tokens require the offline_access scope and are rotating, which the
// shared refresh path already handles (see refreshTokenEndpoints).

const (
	atlassianAuthorizeURL = "https://auth.atlassian.com/authorize"
	atlassianTokenURL     = "https://auth.atlassian.com/oauth/token"
	atlassianResourcesURL = "https://api.atlassian.com/oauth/token/accessible-resources"
)

// atlassianScopes lists exactly what the shipped operations need.
//
// Jira spans two APIs with two different scope models, which is not obvious and
// is easy to get wrong:
//
//   - The platform API (/rest/api/3) uses CLASSIC scopes. read:jira-work,
//     write:jira-work and read:jira-user cover all 19 platform operations here.
//   - The Jira Software API (/rest/agile/1.0), which serves boards and sprints,
//     does not support classic scopes at all — Atlassian's own reference says to
//     use granular ones. So the five Agile operations need the three granular
//     *-software scopes, and the app must have the Jira Software API added
//     alongside the Jira API in the developer console.
//
// Nothing broader is requested. Notably absent: manage:jira-project and
// manage:jira-configuration (no operation administers a project),
// write:board-scope:jira-software (boards are only read), and
// read:issue-details:jira (a granular platform scope that read:jira-work already
// covers, and mixing granular and classic scopes for the same API is asking for
// trouble).
//
// offline_access is what earns a refresh token; without it a connection dies in
// about an hour.
var atlassianScopes = map[string][]string{
	"jira": {
		"offline_access",
		// Platform API — classic.
		"read:jira-work", "write:jira-work", "read:jira-user",
		// Jira Software API — granular only; classic scopes do not work here.
		"read:board-scope:jira-software",
		"read:sprint:jira-software", "write:sprint:jira-software",
	},
	"confluence": {
		"offline_access",
		"read:confluence-space.summary", "read:confluence-content.all",
		"write:confluence-content", "read:confluence-content.summary",
		"read:confluence-props", "write:confluence-props",
		"read:confluence-user", "search:confluence",
	},
}

func atlassianAuthorizeQuery(provider string) url.Values {
	return url.Values{
		"audience": {"api.atlassian.com"},
		"scope":    {strings.Join(atlassianScopes[provider], " ")},
		// consent every time, so a scope change actually re-prompts rather than
		// silently reusing an older, narrower grant.
		"prompt": {"consent"},
	}
}

// atlassianSite is one site (a cloudId) the token can reach.
type atlassianSite struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	URL    string   `json:"url"`
	Scopes []string `json:"scopes"`
}

// exchangeAtlassianCode swaps the code for tokens and resolves which site the
// grant covers. provider is "jira" or "confluence" — same app, different scopes.
func exchangeAtlassianCode(provider, code string) (*models.IntegrationConnection, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     os.Getenv("ATLASSIAN_CLIENT_ID"),
		"client_secret": os.Getenv("ATLASSIAN_CLIENT_SECRET"),
		"code":          code,
		"redirect_uri":  oauthRedirectURI(provider),
	})
	req, _ := http.NewRequest(http.MethodPost, atlassianTokenURL, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

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
		return nil, fmt.Errorf("atlassian token exchange returned no access token")
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

	// The cloudId is not optional — without it there is no URL to call. Fail the
	// connection rather than storing a token that cannot be used.
	site, err := atlassianFirstSite(tok.AccessToken)
	if err != nil {
		return nil, err
	}
	conn.WorkspaceID = site.ID
	conn.WorkspaceName = site.Name
	if conn.WorkspaceName == "" {
		conn.WorkspaceName = strings.TrimPrefix(site.URL, "https://")
	}
	return conn, nil
}

// atlassianFirstSite returns the site the grant covers. A token can in principle
// reach several; we take the first, which is the only one the consent screen
// offers to pick. If the user has multiple sites they reconnect to switch.
func atlassianFirstSite(token string) (*atlassianSite, error) {
	req, _ := http.NewRequest(http.MethodGet, atlassianResourcesURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, fmt.Errorf("could not list Atlassian sites for this grant: %w", err)
	}
	return parseSites(raw)
}

// parseSites picks the first site out of an accessible-resources response.
func parseSites(raw []byte) (*atlassianSite, error) {
	var sites []atlassianSite
	if err := json.Unmarshal(raw, &sites); err != nil {
		return nil, fmt.Errorf("unexpected accessible-resources response")
	}
	if len(sites) == 0 {
		return nil, fmt.Errorf("this Atlassian account granted access to no sites — " +
			"check the app has the right scopes and the account can see a site")
	}
	return &sites[0], nil
}

// jiraResources lists what a Jira node can be pointed at: projects (whose keys
// go in jiraProjectKey) and Agile boards (jiraBoardId).
func jiraResources(token, cloudID string) ([]integrationResource, error) {
	if cloudID == "" {
		return nil, fmt.Errorf("jira connection is missing its site — reconnect Jira")
	}
	base := "https://api.atlassian.com/ex/jira/" + cloudID
	out := []integrationResource{}

	raw, err := atlassianGet(token, base+"/rest/api/3/project/search?maxResults=100")
	if err != nil {
		return nil, err
	}
	var projects struct {
		Values []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"values"`
	}
	if json.Unmarshal(raw, &projects) != nil {
		return nil, fmt.Errorf("parse jira projects")
	}
	for _, p := range projects.Values {
		out = append(out, integrationResource{ID: p.Key, Name: p.Name, Type: "project"})
	}

	// Boards need the Jira Software scopes, which a Jira Work Management-only
	// site won't have — a failure here shouldn't lose the projects above.
	if raw, err := atlassianGet(token, base+"/rest/agile/1.0/board?maxResults=100"); err == nil {
		var boards struct {
			Values []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"values"`
		}
		if json.Unmarshal(raw, &boards) == nil {
			for _, b := range boards.Values {
				out = append(out, integrationResource{
					ID: strconv.Itoa(b.ID), Name: b.Name, Type: "board",
				})
			}
		}
	}
	return out, nil
}

// confluenceResources lists spaces, whose keys go in confluenceSpaceKey.
func confluenceResources(token, cloudID string) ([]integrationResource, error) {
	if cloudID == "" {
		return nil, fmt.Errorf("confluence connection is missing its site — reconnect Confluence")
	}
	raw, err := atlassianGet(token,
		"https://api.atlassian.com/ex/confluence/"+cloudID+"/wiki/api/v2/spaces?limit=100")
	if err != nil {
		return nil, err
	}
	var spaces struct {
		Results []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &spaces) != nil {
		return nil, fmt.Errorf("parse confluence spaces")
	}
	out := make([]integrationResource, 0, len(spaces.Results))
	for _, s := range spaces.Results {
		out = append(out, integrationResource{ID: s.Key, Name: s.Name, Type: "space"})
	}
	return out, nil
}

// bitbucketResources lists repositories in the connected workspace; the slug is
// what bitbucketRepo wants.
func bitbucketResources(token, workspace string) ([]integrationResource, error) {
	if workspace == "" {
		return nil, fmt.Errorf("bitbucket connection is missing its workspace — reconnect Bitbucket")
	}
	raw, err := atlassianGet(token,
		bitbucketAPIBase+"/repositories/"+workspace+"?pagelen=100&sort=-updated_on")
	if err != nil {
		return nil, err
	}
	var repos struct {
		Values []struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"values"`
	}
	if json.Unmarshal(raw, &repos) != nil {
		return nil, fmt.Errorf("parse bitbucket repositories")
	}
	out := make([]integrationResource, 0, len(repos.Values))
	for _, r := range repos.Values {
		out = append(out, integrationResource{
			ID: r.Slug, Name: firstNonEmptyStr(r.Name, r.Slug), Type: "repo",
		})
	}
	return out, nil
}

func atlassianGet(token, url string) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return doOAuthRequest(req)
}
