package handlers

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"crypto/rand"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Sentry connects through the Integration Platform rather than plain OAuth.
//
// The difference that shapes this whole file: Sentry's install flow accepts no
// state parameter. A user installs from a fixed URL and Sentry redirects back
// with only `code` and `installationId`, so the redirect alone cannot say which
// Fernary account it belongs to — the thing every other provider's state solves
// for free.
//
// So the flow is split. The public callback exchanges the grant, verifies the
// installation and parks the result under a one-time handoff id, which it posts
// to the opener window. The SPA — which does have a session — then claims that
// handoff over an authenticated request, and only then is a connection written.
// Nothing is stored against a user until an authenticated request asks for it.

const (
	sentryDefaultAPIBase = "https://sentry.io/api/0"
	// sentryTokenLifetime is what Sentry documents for installation tokens.
	// Used only when a response omits expiresAt; the real value is preferred.
	sentryTokenLifetime = 8 * time.Hour
)

// sentryAPIBase is the API origin. Sentry stores organizations in regions and
// publishes region-specific hosts (us.sentry.io, de.sentry.io); sentry.io
// serves every region, so it is the default and the variable exists for
// self-hosted installs and for pinning a region to cut latency.
func sentryAPIBase() string {
	if base := strings.TrimSpace(os.Getenv("SENTRY_API_BASE")); base != "" {
		return strings.TrimRight(base, "/")
	}
	return sentryDefaultAPIBase
}

// sentryWebBase is where a person's browser goes, derived from the API base so
// a self-hosted deployment only has to set one variable.
func sentryWebBase() string {
	if base := strings.TrimSpace(os.Getenv("SENTRY_WEB_BASE")); base != "" {
		return strings.TrimRight(base, "/")
	}
	return strings.TrimSuffix(sentryAPIBase(), "/api/0")
}

func sentryAppSlug() string { return strings.TrimSpace(os.Getenv("SENTRY_APP_SLUG")) }

// sentryConfigured reports whether this deployment has a Sentry integration
// registered. All three are needed: the slug to send people to, and the client
// credentials to exchange grants and to verify inbound webhook signatures.
func sentryConfigured() bool {
	return sentryAppSlug() != "" &&
		os.Getenv("SENTRY_CLIENT_ID") != "" &&
		os.Getenv("SENTRY_CLIENT_SECRET") != ""
}

// sentryInstallURL is the fixed address every public integration exposes.
func sentryInstallURL() string {
	return sentryWebBase() + "/sentry-apps/" + sentryAppSlug() + "/external-install/"
}

// ── Handoff between the anonymous callback and the authenticated claim ──

type sentryPendingInstall struct {
	installationID string
	token          string
	refreshToken   string
	expiresAt      *time.Time
	orgSlug        string
	orgName        string
	scopes         string
	expires        time.Time
}

var (
	sentryPendingMu sync.Mutex
	sentryPending   = map[string]sentryPendingInstall{}
)

func parkSentryInstall(p sentryPendingInstall) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	handoff := hex.EncodeToString(b)

	sentryPendingMu.Lock()
	defer sentryPendingMu.Unlock()
	for k, e := range sentryPending {
		if time.Now().After(e.expires) {
			delete(sentryPending, k)
		}
	}
	p.expires = time.Now().Add(10 * time.Minute)
	sentryPending[handoff] = p
	return handoff
}

// claimSentryInstall is single-use: the entry is removed whether or not it was
// still valid, so a replayed handoff cannot bind the same installation twice.
func claimSentryInstall(handoff string) (sentryPendingInstall, bool) {
	sentryPendingMu.Lock()
	defer sentryPendingMu.Unlock()
	p, found := sentryPending[handoff]
	delete(sentryPending, handoff)
	if !found || time.Now().After(p.expires) {
		return sentryPendingInstall{}, false
	}
	return p, true
}

// ── Connect ───────────────────────────────────────────────────

// connectSentry answers the connect call for Sentry. There is no authorize URL
// to build and no state to mint — the install address is fixed per app.
func (h *WorkflowHandler) connectSentry(c *gin.Context) {
	if !sentryConfigured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Sentry is not configured — set SENTRY_APP_SLUG, SENTRY_CLIENT_ID and SENTRY_CLIENT_SECRET",
		})
		return
	}
	slog.InfoContext(c.Request.Context(), "integration connect started", "provider", "sentry")
	c.JSON(http.StatusOK, gin.H{"url": sentryInstallURL()})
}

// ── Callback ──────────────────────────────────────────────────

// callbackSentry runs unauthenticated, as the provider redirect that it is.
func (h *WorkflowHandler) callbackSentry(c *gin.Context) {
	ctx := c.Request.Context()
	fail := func(reason, shown string) {
		slog.WarnContext(ctx, "integration connect failed", "provider", "sentry", "reason", reason)
		telemetry.AuthEvent(ctx, "integration_oauth", "error")
		oauthResultPageExtra(c, "sentry", "", false, shown, nil)
	}

	if !sentryConfigured() {
		fail("not_configured", "Sentry is not configured on this deployment")
		return
	}
	if errParam := c.Query("error"); errParam != "" {
		fail("provider_error", truncate(errParam, 200))
		return
	}
	code := strings.TrimSpace(c.Query("code"))
	installationID := strings.TrimSpace(c.Query("installationId"))
	if code == "" || installationID == "" {
		fail("missing_code_or_installation", "Sentry returned an incomplete installation")
		return
	}

	grant, err := exchangeSentryGrant(installationID, code)
	if err != nil {
		fail(truncate(err.Error(), 200), "could not finish the Sentry installation")
		return
	}

	orgSlug, orgName, err := sentryIdentity(grant.Token)
	if err != nil {
		fail(truncate(err.Error(), 200), "connected to Sentry but could not read the organization")
		return
	}

	// Telling Sentry the install is finished flips it out of "pending" in the
	// user's integration directory. A failure here is cosmetic — the grant is
	// already exchanged — so it is logged rather than shown.
	if err := verifySentryInstall(installationID, grant.Token); err != nil {
		slog.WarnContext(ctx, "could not mark sentry installation verified",
			"installation_id", installationID, "error", truncate(err.Error(), 200))
	}

	handoff := parkSentryInstall(sentryPendingInstall{
		installationID: installationID,
		token:          grant.Token,
		refreshToken:   grant.RefreshToken,
		expiresAt:      grant.expiry(),
		orgSlug:        orgSlug,
		orgName:        orgName,
		scopes:         strings.Join(grant.Scopes, " "),
	})
	slog.InfoContext(ctx, "sentry installation awaiting claim",
		"installation_id", installationID, "organization", orgSlug)
	oauthResultPageExtra(c, "sentry", "", true, "", map[string]string{"handoff": handoff})
}

// ── Claim ─────────────────────────────────────────────────────

// ClaimSentryInstall binds a parked installation to the caller's account. This
// is the first authenticated step in the Sentry flow and the only one that
// writes anything.
func (h *WorkflowHandler) ClaimSentryInstall(c *gin.Context) {
	var body struct {
		Handoff string `json:"handoff"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Handoff) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected a handoff field"})
		return
	}
	pending, ok := claimSentryInstall(strings.TrimSpace(body.Handoff))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "that Sentry installation has expired — start the connection again",
		})
		return
	}

	ctx := c.Request.Context()
	userID, orgID := currentUserID(c), currentOrgID(c)
	conn := &models.IntegrationConnection{
		UserID:         userID,
		OrganizationID: orgID,
		Provider:       "sentry",
		AccessToken:    pending.token,
		RefreshToken:   pending.refreshToken,
		ExpiresAt:      pending.expiresAt,
		WorkspaceID:    pending.orgSlug,
		WorkspaceName:  pending.orgName,
		InstallationID: pending.installationID,
		Scope:          pending.scopes,
	}
	err := h.withHostedAuthorityLock(ctx, orgID, userID, func(connection *gorm.DB) error {
		return connection.Transaction(func(tx *gorm.DB) error {
			if err := tx.Unscoped().Where("organization_id = ? AND user_id = ? AND provider = ?",
				orgID, userID, "sentry").Delete(&models.IntegrationConnection{}).Error; err != nil {
				return err
			}
			return tx.Create(conn).Error
		})
	})
	if err != nil {
		slog.ErrorContext(ctx, "integration connect failed", "provider", "sentry", "reason", "store_failed")
		telemetry.AuthEvent(ctx, "integration_oauth", "error")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store the Sentry connection"})
		return
	}
	slog.InfoContext(ctx, "integration connected", "provider", "sentry", "user_id", userID)
	telemetry.AuthEvent(ctx, "integration_oauth", "ok")
	c.JSON(http.StatusOK, gin.H{"connected": "sentry", "workspace_name": pending.orgName})
}

// ── Sentry API calls used by the connection flow ──────────────

// sentryGrant is Sentry's answer to both grant types. The field names differ
// from every OAuth provider we speak to (token, not access_token; an absolute
// expiresAt, not a duration), which is why this does not reuse refreshedToken.
type sentryGrant struct {
	Token        string   `json:"token"`
	RefreshToken string   `json:"refreshToken"`
	ExpiresAt    string   `json:"expiresAt"`
	Scopes       []string `json:"scopes"`
}

func (g sentryGrant) expiry() *time.Time {
	if t, err := time.Parse(time.RFC3339, g.ExpiresAt); err == nil {
		return &t
	}
	t := time.Now().Add(sentryTokenLifetime)
	return &t
}

func sentryAuthorizationURL(installationID string) string {
	return sentryAPIBase() + "/sentry-app-installations/" + installationID + "/authorizations/"
}

func exchangeSentryGrant(installationID, code string) (sentryGrant, error) {
	return postSentryAuthorization(installationID, map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"client_id":     os.Getenv("SENTRY_CLIENT_ID"),
		"client_secret": os.Getenv("SENTRY_CLIENT_SECRET"),
	})
}

func refreshSentryGrant(installationID, refreshToken string) (sentryGrant, error) {
	return postSentryAuthorization(installationID, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     os.Getenv("SENTRY_CLIENT_ID"),
		"client_secret": os.Getenv("SENTRY_CLIENT_SECRET"),
	})
}

func postSentryAuthorization(installationID string, payload map[string]string) (sentryGrant, error) {
	var grant sentryGrant
	body, err := json.Marshal(payload)
	if err != nil {
		return grant, err
	}
	req, err := http.NewRequest(http.MethodPost, sentryAuthorizationURL(installationID), strings.NewReader(string(body)))
	if err != nil {
		return grant, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	raw, err := doOAuthRequest(req)
	if err != nil {
		return grant, fmt.Errorf("sentry token exchange failed: %w", err)
	}
	if err := json.Unmarshal(raw, &grant); err != nil || grant.Token == "" {
		return grant, fmt.Errorf("sentry token exchange returned no token")
	}
	return grant, nil
}

// verifySentryInstall marks the installation finished so it stops showing as
// pending in the user's Sentry settings.
func verifySentryInstall(installationID, token string) error {
	req, err := http.NewRequest(http.MethodPut,
		sentryAPIBase()+"/sentry-app-installations/"+installationID+"/",
		strings.NewReader(`{"status":"installed"}`))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	_, err = doOAuthRequest(req)
	return err
}

// sentryIdentity resolves the organization an installation token belongs to.
// An installation token is scoped to exactly one organization, so listing
// organizations returns that one and nothing else.
func sentryIdentity(token string) (slug, name string, err error) {
	req, err := http.NewRequest(http.MethodGet, sentryAPIBase()+"/organizations/", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	raw, err := doOAuthRequest(req)
	if err != nil {
		return "", "", fmt.Errorf("could not read the Sentry organization: %w", err)
	}
	var orgs []struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &orgs); err != nil || len(orgs) == 0 {
		return "", "", fmt.Errorf("the Sentry installation names no organization")
	}
	name = orgs[0].Name
	if name == "" {
		name = orgs[0].Slug
	}
	return orgs[0].Slug, name, nil
}

// uninstallSentryApp removes the installation from the user's Sentry
// organization on disconnect. Leaving it behind would keep sending us webhooks
// for an account that no longer has a connection.
func uninstallSentryApp(conn *models.IntegrationConnection) error {
	if conn.InstallationID == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodDelete,
		sentryAPIBase()+"/sentry-app-installations/"+conn.InstallationID+"/", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+conn.AccessToken)
	_, err = doOAuthRequest(req)
	return err
}

// ── Resources ─────────────────────────────────────────────────

// sentryResources lists the organization's projects, which is what every
// Sentry node and trigger filter needs to point at.
func sentryResources(token, orgSlug string) ([]integrationResource, error) {
	if strings.TrimSpace(orgSlug) == "" {
		return nil, fmt.Errorf("this Sentry connection names no organization — reconnect Sentry")
	}
	req, err := http.NewRequest(http.MethodGet,
		sentryAPIBase()+"/organizations/"+orgSlug+"/projects/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var projects []struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &projects); err != nil {
		return nil, fmt.Errorf("could not read the Sentry project list")
	}
	out := make([]integrationResource, 0, len(projects))
	for _, p := range projects {
		if p.Slug == "" {
			continue
		}
		name := p.Name
		if name == "" {
			name = p.Slug
		}
		out = append(out, integrationResource{ID: p.Slug, Name: name, Type: "project"})
	}
	return out, nil
}
