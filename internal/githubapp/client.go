// Package githubapp contains the small part of GitHub's API concerned with a
// GitHub App user token: which installations that token can use, and which
// repositories each installation actually covers.
//
// Keeping this here (rather than in the HTTP handlers or trigger adapter) makes
// one rule authoritative everywhere: seeing a repository as a GitHub user is
// not enough; the Fernary GitHub App installation must include that exact repo.
package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com"
	perPage        = 100
)

// HTTPDoer is implemented by *http.Client and keeps the client deterministic
// in tests without changing production URLs or global transports.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Client struct {
	Token   string
	BaseURL string
	HTTP    HTTPDoer
}

func NewClient(token string, httpClient HTTPDoer) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{Token: token, BaseURL: defaultBaseURL, HTTP: httpClient}
}

type Account struct {
	Login     string `json:"login"`
	Type      string `json:"type"`
	AvatarURL string `json:"avatar_url"`
}

type Installation struct {
	ID                  int64             `json:"id"`
	Account             Account           `json:"account"`
	HTMLURL             string            `json:"html_url"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	Events              []string          `json:"events"`
	SuspendedAt         *time.Time        `json:"suspended_at"`
}

func (i Installation) MissingWebhookRequirements() []string {
	return missingWebhookRequirements(i.Permissions, i.Events)
}

type Repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

// AppRegistration is the public portion of a GitHub App registration that
// determines which webhook deliveries GitHub will send. A configured webhook
// URL and secret are insufficient when the App subscribes to no events.
type AppRegistration struct {
	Slug        string            `json:"slug"`
	Permissions map[string]string `json:"permissions"`
	Events      []string          `json:"events"`
}

// MissingWebhookRequirements reports the GitHub App settings required by the
// trigger catalog implemented by Fernary. Write permission also satisfies a
// read requirement, though the App should use read-only permissions in normal
// operation.
func (a AppRegistration) MissingWebhookRequirements() []string {
	return missingWebhookRequirements(a.Permissions, a.Events)
}

func missingWebhookRequirements(permissions map[string]string, events []string) []string {
	missing := make([]string, 0)
	for _, requirement := range []struct {
		key   string
		label string
	}{
		{key: "contents", label: "Contents permission (read-only)"},
		{key: "issues", label: "Issues permission (read-only)"},
		{key: "pull_requests", label: "Pull requests permission (read-only)"},
	} {
		level := strings.ToLower(strings.TrimSpace(permissions[requirement.key]))
		if level != "read" && level != "write" && level != "admin" {
			missing = append(missing, requirement.label)
		}
	}

	subscribed := make(map[string]bool, len(events))
	for _, event := range events {
		subscribed[strings.ToLower(strings.TrimSpace(event))] = true
	}
	for _, requirement := range []struct {
		key   string
		label string
	}{
		{key: "issues", label: "Issues event"},
		{key: "issue_comment", label: "Issue comment event"},
		{key: "pull_request", label: "Pull request event"},
		{key: "push", label: "Push event"},
		{key: "release", label: "Release event"},
	} {
		if !subscribed[requirement.key] {
			missing = append(missing, requirement.label)
		}
	}
	return missing
}

// APIError preserves the status because a 403 from /user/installations has a
// useful meaning: the stored credential is not a GitHub App user access token.
// Callers can turn that into a reconnect/install prompt instead of a generic
// upstream failure.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
}

func (c *Client) ListInstallations(ctx context.Context) ([]Installation, error) {
	var all []Installation
	for page := 1; ; page++ {
		var out struct {
			TotalCount    int            `json:"total_count"`
			Installations []Installation `json:"installations"`
		}
		path := "/user/installations?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
		if err := c.get(ctx, path, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Installations...)
		if len(out.Installations) == 0 || len(all) >= out.TotalCount || len(out.Installations) < perPage {
			return all, nil
		}
	}
}

func (c *Client) ListInstallationRepositories(ctx context.Context, installationID int64) ([]Repository, error) {
	var all []Repository
	for page := 1; ; page++ {
		var out struct {
			TotalCount   int          `json:"total_count"`
			Repositories []Repository `json:"repositories"`
		}
		path := "/user/installations/" + strconv.FormatInt(installationID, 10) +
			"/repositories?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
		if err := c.get(ctx, path, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Repositories...)
		if len(out.Repositories) == 0 || len(all) >= out.TotalCount || len(out.Repositories) < perPage {
			return all, nil
		}
	}
}

// GetAppRegistration reads the App's public permissions and event
// subscriptions. GitHub exposes this endpoint without App-owner credentials,
// which lets Fernary fail closed instead of claiming webhook readiness based
// only on the presence of a local signing secret.
func (c *Client) GetAppRegistration(ctx context.Context, slug string) (*AppRegistration, error) {
	if !validAppSlug(slug) {
		return nil, fmt.Errorf("github: invalid app slug")
	}
	var app AppRegistration
	if err := c.get(ctx, "/apps/"+slug, &app); err != nil {
		return nil, err
	}
	return &app, nil
}

// InstallationForRepository returns the installation that covers the exact
// owner/name repository. Owner matching alone is deliberately insufficient:
// an installation can be limited to selected repositories.
func (c *Client) InstallationForRepository(ctx context.Context, fullName string) (*Installation, error) {
	if !validRepositoryName(fullName) {
		return nil, fmt.Errorf("github: %q is not an owner/repository name", fullName)
	}
	owner, _, _ := strings.Cut(fullName, "/")
	installations, err := c.ListInstallations(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: could not list your app installations: %w", err)
	}
	for i := range installations {
		installation := &installations[i]
		if installation.SuspendedAt != nil || !strings.EqualFold(installation.Account.Login, owner) {
			continue
		}
		repositories, err := c.ListInstallationRepositories(ctx, installation.ID)
		if err != nil {
			return nil, fmt.Errorf("github: could not inspect installation %d repositories: %w", installation.ID, err)
		}
		for _, repository := range repositories {
			if strings.EqualFold(repository.FullName, fullName) {
				return installation, nil
			}
		}
	}
	return nil, fmt.Errorf("github: the Fernary app is not installed for repository %s", fullName)
}

func validRepositoryName(fullName string) bool {
	owner, name, ok := strings.Cut(fullName, "/")
	return ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

func validAppSlug(slug string) bool {
	if len(slug) == 0 || len(slug) > 100 || slug[0] == '-' || slug[len(slug)-1] == '-' {
		return false
	}
	for i := 0; i < len(slug); i++ {
		b := slug[i]
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '-' {
			return false
		}
	}
	return true
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	base := strings.TrimRight(c.BaseURL, "/")
	if _, err := url.ParseRequestURI(base + path); err != nil {
		return fmt.Errorf("github: invalid API path: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(c.Token) != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(raw))
		var payload struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &payload) == nil && payload.Message != "" {
			message = payload.Message
		}
		if len(message) > 300 {
			message = message[:300] + "…"
		}
		return &APIError{StatusCode: resp.StatusCode, Method: http.MethodGet, Path: path, Message: message}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("github: parse %s: %w", path, err)
	}
	return nil
}
