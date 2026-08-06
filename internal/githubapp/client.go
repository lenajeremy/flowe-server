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
	ID                  int64      `json:"id"`
	Account             Account    `json:"account"`
	RepositorySelection string     `json:"repository_selection"`
	SuspendedAt         *time.Time `json:"suspended_at"`
}

type Repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
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

func (c *Client) get(ctx context.Context, path string, out any) error {
	base := strings.TrimRight(c.BaseURL, "/")
	if _, err := url.ParseRequestURI(base + path); err != nil {
		return fmt.Errorf("github: invalid API path: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
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
