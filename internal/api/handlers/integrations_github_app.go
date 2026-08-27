package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/githubapp"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var (
	githubAppSlugPattern                  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,98}[A-Za-z0-9])?$`)
	githubInstallationSettingsPathPattern = regexp.MustCompile(
		`^/(?:settings/installations|organizations/[A-Za-z0-9-]+/settings/installations)/[1-9][0-9]*$`)
)

type githubSetupRepository struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	InstallationID string `json:"installation_id"`
	AccountLogin   string `json:"account_login"`
}

type githubInstallationRepository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

type githubSetupInstallation struct {
	ID                    string                         `json:"id"`
	AccountLogin          string                         `json:"account_login"`
	AccountType           string                         `json:"account_type"`
	AvatarURL             string                         `json:"avatar_url,omitempty"`
	SettingsURL           string                         `json:"settings_url,omitempty"`
	RepositorySelection   string                         `json:"repository_selection"`
	Suspended             bool                           `json:"suspended"`
	PermissionsConfigured bool                           `json:"permissions_configured"`
	PermissionsMissing    []string                       `json:"permissions_missing"`
	AgentWritesConfigured bool                           `json:"agent_writes_configured"`
	AgentWritesMissing    []string                       `json:"agent_writes_missing"`
	Repositories          []githubInstallationRepository `json:"repositories"`
}

type githubSetupResponse struct {
	Connected               bool                      `json:"connected"`
	Installed               bool                      `json:"installed"`
	AppConfigured           bool                      `json:"app_configured"`
	AppSlug                 string                    `json:"app_slug,omitempty"`
	WebhookConfigured       bool                      `json:"webhook_configured"`
	WebhookEventsConfigured bool                      `json:"webhook_events_configured"`
	WebhookEventsMissing    []string                  `json:"webhook_events_missing"`
	WebhookEventsError      string                    `json:"webhook_events_error,omitempty"`
	TokenKind               string                    `json:"token_kind,omitempty"`
	ReconnectRequired       bool                      `json:"reconnect_required,omitempty"`
	InstallURL              string                    `json:"install_url,omitempty"`
	Installations           []githubSetupInstallation `json:"installations"`
	Repositories            []githubSetupRepository   `json:"repositories"`
}

// GitHubIntegrationSetup is the frontend's single source of truth for GitHub
// App setup. It never returns credentials. Repository choices come only from
// the installation-repository endpoint, not /user/repos, so every option shown
// here is capable of producing an app webhook.
func (h *WorkflowHandler) GitHubIntegrationSetup(c *gin.Context) {
	configured := githubAppAuthorizationConfigured()
	installURL := ""
	if configured {
		state := newGitHubInstallState(currentUserID(c), currentOrgID(c), openerOrigin(c))
		installURL, _ = githubAppInstallURL(state)
	}
	response := githubSetupResponse{
		AppConfigured:        configured,
		AppSlug:              validGitHubAppSlug(),
		WebhookConfigured:    os.Getenv("GITHUB_WEBHOOK_SECRET") != "",
		WebhookEventsMissing: []string{},
		InstallURL:           installURL,
		Installations:        []githubSetupInstallation{},
		Repositories:         []githubSetupRepository{},
	}
	var connection models.IntegrationConnection
	err := h.db.DB.Where("organization_id = ? AND user_id = ? AND provider = ?",
		currentOrgID(c), currentUserID(c), "github").First(&connection).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusOK, response)
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load GitHub connection"})
		return
	}
	response.Connected = true

	token, _ := FreshAccessTokenForOrg(h.db.DB, currentOrgID(c), currentUserID(c), "github")
	if token == "" {
		response.ReconnectRequired = true
		c.JSON(http.StatusOK, response)
		return
	}

	client := githubapp.NewClient(token, &http.Client{Timeout: 30 * time.Second})
	// Authenticate this lookup with the GitHub App user token we already have.
	// The endpoint is public, but anonymous requests share GitHub's much smaller
	// IP-based rate limit. Treating a 403 from that limit as "events missing"
	// made a correctly configured App look broken in production.
	if configured {
		app, appErr := client.GetAppRegistration(c.Request.Context(), response.AppSlug)
		if appErr != nil {
			slog.WarnContext(c.Request.Context(), "could not verify GitHub App event subscriptions",
				"app_slug", response.AppSlug, "error", appErr)
			response.WebhookEventsError = "Fernary could not verify the GitHub App’s event subscriptions. Retry in a moment."
		} else {
			response.WebhookEventsMissing = app.MissingWebhookRequirements()
			response.WebhookEventsConfigured = len(response.WebhookEventsMissing) == 0
		}
	}

	installations, err := client.ListInstallations(c.Request.Context())
	if err != nil {
		var apiErr *githubapp.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusUnauthorized) {
			// /user/installations accepts GitHub App user tokens, not classic
			// OAuth App tokens. Keep this a usable 200 response so the settings UI
			// can offer "Reconnect" rather than render an upstream-error screen.
			if apiErr.StatusCode == http.StatusForbidden {
				response.TokenKind = "oauth_app"
			} else {
				response.TokenKind = "unknown"
			}
			response.ReconnectRequired = true
			c.JSON(http.StatusOK, response)
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "GitHub installations could not be listed"})
		return
	}
	response.TokenKind = "github_app"

	seen := make(map[string]bool)
	for _, installation := range installations {
		item := githubSetupInstallation{
			ID:                  strconv.FormatInt(installation.ID, 10),
			AccountLogin:        installation.Account.Login,
			AccountType:         installation.Account.Type,
			AvatarURL:           installation.Account.AvatarURL,
			SettingsURL:         safeGitHubInstallationSettingsURL(installation.HTMLURL),
			RepositorySelection: installation.RepositorySelection,
			Suspended:           installation.SuspendedAt != nil,
			PermissionsMissing:  installation.MissingWebhookRequirements(),
			AgentWritesMissing:  installation.MissingCodingAgentWriteRequirements(),
			Repositories:        []githubInstallationRepository{},
		}
		item.PermissionsConfigured = len(item.PermissionsMissing) == 0
		item.AgentWritesConfigured = len(item.AgentWritesMissing) == 0
		if installation.SuspendedAt == nil {
			response.Installed = true
			repositories, listErr := client.ListInstallationRepositories(c.Request.Context(), installation.ID)
			if listErr != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("GitHub repositories for %s could not be listed", installation.Account.Login)})
				return
			}
			for _, repository := range repositories {
				item.Repositories = append(item.Repositories, githubInstallationRepository{
					ID: repository.ID, FullName: repository.FullName, Private: repository.Private,
				})
				key := strings.ToLower(repository.FullName)
				if !seen[key] {
					seen[key] = true
					response.Repositories = append(response.Repositories, githubSetupRepository{
						ID: repository.FullName, Name: repository.FullName, Type: "repo",
						InstallationID: item.ID, AccountLogin: installation.Account.Login,
					})
				}
			}
		}
		response.Installations = append(response.Installations, item)
	}
	c.JSON(http.StatusOK, response)
}

func safeGitHubInstallationSettingsURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "github.com") ||
		u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		!githubInstallationSettingsPathPattern.MatchString(u.EscapedPath()) {
		return ""
	}
	return u.String()
}

// GitHubSetupCallback is used when the GitHub App's Setup URL points here. The
// installation callback has no user session, so it consumes the one-time state
// from GitHubIntegrationSetup, captures the installation id, and immediately
// sends the browser through GitHub's user-authorization flow. The ordinary
// OAuth callback then verifies that exact installation before storing a token.
func (h *WorkflowHandler) GitHubSetupCallback(c *gin.Context) {
	state, ok := consumeOAuthState(c.Query("state"))
	if !ok || !state.githubInstall {
		oauthResultPage(c, "github", state.origin, false, "invalid or expired installation state — try again")
		return
	}
	installationID, err := positiveGitHubID(c.Query("installation_id"))
	if err != nil {
		oauthResultPage(c, "github", state.origin, false, "GitHub did not return a valid installation")
		return
	}
	if c.Query("setup_action") == "request" {
		oauthResultPage(c, "github", state.origin, false, "GitHub App access was requested but has not been approved yet")
		return
	}

	oauthState := newGitHubInstalledOAuthState(state.userID, state.orgID, state.origin, installationID)
	authorizeURL, err := githubUserAuthorizeURL(oauthState)
	if err != nil {
		oauthResultPage(c, "github", state.origin, false, err.Error())
		return
	}
	http.Redirect(c.Writer, c.Request, authorizeURL, http.StatusFound)
}

func githubAppInstallURL(state string) (string, error) {
	slug := validGitHubAppSlug()
	if slug == "" {
		return "", fmt.Errorf("GITHUB_APP_SLUG is not configured")
	}
	u := url.URL{Scheme: "https", Host: "github.com", Path: "/apps/" + slug + "/installations/new"}
	q := u.Query()
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func validGitHubAppSlug() string {
	slug := strings.TrimSpace(os.Getenv("GITHUB_APP_SLUG"))
	if !githubAppSlugPattern.MatchString(slug) {
		return ""
	}
	return slug
}

func githubAppAuthorizationConfigured() bool {
	return validGitHubAppSlug() != "" && os.Getenv("GITHUB_CLIENT_ID") != "" && os.Getenv("GITHUB_CLIENT_SECRET") != ""
}

func githubUserAuthorizeURL(state string) (string, error) {
	clientID := os.Getenv("GITHUB_CLIENT_ID")
	if clientID == "" || os.Getenv("GITHUB_CLIENT_SECRET") == "" {
		return "", fmt.Errorf("GitHub App authorization is not configured")
	}
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", oauthRedirectURI("github"))
	q.Set("response_type", "code")
	q.Set("state", state)
	return "https://github.com/login/oauth/authorize?" + q.Encode(), nil
}

func positiveGitHubID(raw string) (string, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("invalid GitHub installation id")
	}
	return strconv.FormatInt(id, 10), nil
}

func verifyGitHubInstallation(ctx context.Context, token, installationID string) error {
	installations, err := githubapp.NewClient(token, &http.Client{Timeout: 30 * time.Second}).ListInstallations(
		ctx)
	if err != nil {
		return fmt.Errorf("could not verify the GitHub App installation: %w", err)
	}
	for _, installation := range installations {
		if strconv.FormatInt(installation.ID, 10) == installationID && installation.SuspendedAt == nil {
			return nil
		}
	}
	return fmt.Errorf("the GitHub App installation is not accessible to this account")
}
