package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/cryptobox"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// OAuth connections for third-party integrations (Notion, Linear).
// Flow: GET /connect redirects to the provider's consent page; the provider
// redirects back to GET /callback, which exchanges the code, stores the
// token, and returns a small HTML page that notifies the opener and closes.

type oauthProvider struct {
	name         string
	authorizeURL string
	clientIDEnv  string
	secretEnv    string
	extraAuthQ   url.Values
}

var oauthProviders = map[string]oauthProvider{
	"notion": {
		name:         "notion",
		authorizeURL: "https://api.notion.com/v1/oauth/authorize",
		clientIDEnv:  "NOTION_CLIENT_ID",
		secretEnv:    "NOTION_CLIENT_SECRET",
		extraAuthQ:   url.Values{"owner": {"user"}},
	},
	"linear": {
		name:         "linear",
		authorizeURL: "https://linear.app/oauth/authorize",
		clientIDEnv:  "LINEAR_CLIENT_ID",
		secretEnv:    "LINEAR_CLIENT_SECRET",
		extraAuthQ:   url.Values{"scope": {"read,write"}, "prompt": {"consent"}},
	},
	"github": {
		name:         "github",
		authorizeURL: "https://github.com/login/oauth/authorize",
		clientIDEnv:  "GITHUB_CLIENT_ID",
		secretEnv:    "GITHUB_CLIENT_SECRET",
		extraAuthQ:   url.Values{"scope": {"repo"}},
	},
	"gitlab": {
		name:         "gitlab",
		authorizeURL: "https://gitlab.com/oauth/authorize",
		clientIDEnv:  "GITLAB_CLIENT_ID",
		secretEnv:    "GITLAB_CLIENT_SECRET",
		extraAuthQ:   url.Values{"scope": {"api"}},
	},
	"gmail": {
		name:         "gmail",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID", // Gmail reuses the Google sign-in app
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		// access_type=offline + prompt=consent are required to receive a
		// refresh token (Google only returns one on first consent otherwise).
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/gmail.modify"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	"stripe": {
		name:         "stripe",
		authorizeURL: "https://connect.stripe.com/oauth/authorize",
		clientIDEnv:  "STRIPE_CLIENT_ID", // ca_… Connect client id
		secretEnv:    "STRIPE_CLIENT_SECRET",
		extraAuthQ:   url.Values{"scope": {"read_write"}},
	},
	// Google Calendar/Drive/Docs/Sheets all reuse the Google sign-in app,
	// differing only in scope. access_type=offline + prompt=consent are needed
	// to receive a refresh token on first consent.
	"googlecalendar": {
		name:         "googlecalendar",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/calendar"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	"googledrive": {
		name:         "googledrive",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/drive"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	"googledocs": {
		name:         "googledocs",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/documents https://www.googleapis.com/auth/drive"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	"googlesheets": {
		name:         "googlesheets",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/spreadsheets https://www.googleapis.com/auth/drive"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	"outlook": {
		name:         "outlook",
		authorizeURL: "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
		clientIDEnv:  "MICROSOFT_CLIENT_ID",
		secretEnv:    "MICROSOFT_CLIENT_SECRET",
		// offline_access yields a refresh token; the rest cover mail + calendar.
		extraAuthQ: url.Values{
			"scope": {"offline_access Mail.ReadWrite Mail.Send Calendars.ReadWrite Contacts.ReadWrite User.Read"},
		},
	},
	"slack": {
		name:         "slack",
		authorizeURL: "https://slack.com/oauth/v2/authorize",
		clientIDEnv:  "SLACK_CLIENT_ID",
		secretEnv:    "SLACK_CLIENT_SECRET",
		// scope = bot token grants; user_scope = a second token acting as the
		// human who connected, so sends can run "as me" (users:read powers the
		// DM recipient picker).
		// chat:write.customize lets bot sends override the display name/icon;
		// the im/mpim user scopes let workflows list and read the connecting
		// user's DMs and group chats (bots are never members of those).
		extraAuthQ: url.Values{
			"scope":      {"chat:write,chat:write.customize,chat:write.public,channels:read,channels:history,channels:manage,channels:join,groups:read,groups:write,users:read,users:read.email,reactions:write,pins:write,files:write"},
			"user_scope": {"chat:write,im:write,im:read,im:history,mpim:read,mpim:history,search:read"},
		},
	},
	// Shopify's authorize URL is per-shop, so ConnectIntegration/Callback
	// handle it specially; this entry exists for availability + resource routing.
	"shopify": {
		name:        "shopify",
		clientIDEnv: "SHOPIFY_CLIENT_ID",
		secretEnv:   "SHOPIFY_CLIENT_SECRET",
	},
}

// allProviders is the stable iteration order for status/resource listings.
var allProviders = []string{
	"notion", "linear", "github", "gitlab", "gmail",
	"googlecalendar", "googledrive", "googledocs", "googlesheets",
	"outlook", "slack", "stripe", "shopify",
}

func oauthRedirectURI(provider string) string {
	base := os.Getenv("OAUTH_REDIRECT_BASE")
	if base == "" {
		base = "http://localhost:8080"
	}
	return strings.TrimRight(base, "/") + "/api/integrations/" + provider + "/callback"
}

// ── Current user ──────────────────────────────────────────────

// currentUserID resolves the session user set by the RequireAuth middleware.
func currentUserID(c *gin.Context) string {
	return auth.UserID(c)
}

// ── CSRF state (in-memory, single instance) ───────────────────

type oauthStateEntry struct {
	userID  string
	origin  string // opener origin for the popup's postMessage target
	shop    string // shopify shop domain (empty for other providers)
	expires time.Time
}

var (
	oauthStatesMu sync.Mutex
	oauthStates   = map[string]oauthStateEntry{}
)

func newOAuthState(userID, origin string) string {
	return newOAuthStateShop(userID, origin, "")
}

func newOAuthStateShop(userID, origin, shop string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	s := hex.EncodeToString(b)
	oauthStatesMu.Lock()
	defer oauthStatesMu.Unlock()
	for k, e := range oauthStates {
		if time.Now().After(e.expires) {
			delete(oauthStates, k)
		}
	}
	oauthStates[s] = oauthStateEntry{userID: userID, origin: origin, shop: shop, expires: time.Now().Add(10 * time.Minute)}
	return s
}

// consumeOAuthState validates the state and returns the user, opener origin,
// and shop domain (shopify only) that started the flow.
func consumeOAuthState(s string) (userID, origin, shop string, ok bool) {
	oauthStatesMu.Lock()
	defer oauthStatesMu.Unlock()
	e, found := oauthStates[s]
	delete(oauthStates, s)
	if !found || time.Now().After(e.expires) {
		return "", "", "", false
	}
	return e.userID, e.origin, e.shop, true
}

// openerOrigin extracts the validated ?origin= param the frontend appends
// when opening an OAuth popup ("" when absent or not allowed).
func openerOrigin(c *gin.Context) string {
	origin := strings.TrimRight(c.Query("origin"), "/")
	if origin != "" && auth.OriginAllowed(origin) {
		return origin
	}
	return ""
}

// ── Handlers ──────────────────────────────────────────────────

// ListIntegrations returns connection status for every supported provider,
// scoped to the current user.
func (h *WorkflowHandler) ListIntegrations(c *gin.Context) {
	var conns []models.IntegrationConnection
	h.db.DB.Where("user_id = ?", currentUserID(c)).Find(&conns)
	byProvider := map[string]models.IntegrationConnection{}
	for _, conn := range conns {
		byProvider[conn.Provider] = conn
	}

	out := []gin.H{}
	for _, p := range allProviders {
		prov := oauthProviders[p]
		conn, connected := byProvider[p]
		entry := gin.H{
			"provider":  p,
			"connected": connected,
			"available": os.Getenv(prov.clientIDEnv) != "" && os.Getenv(prov.secretEnv) != "",
		}
		if connected {
			entry["workspace_name"] = conn.WorkspaceName
		}
		out = append(out, entry)
	}
	c.JSON(http.StatusOK, out)
}

// ConnectIntegration redirects the browser to the provider's consent page.
func (h *WorkflowHandler) ConnectIntegration(c *gin.Context) {
	prov, ok := oauthProviders[c.Param("provider")]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider"})
		return
	}
	clientID := os.Getenv(prov.clientIDEnv)
	if clientID == "" || os.Getenv(prov.secretEnv) == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": fmt.Sprintf("%s OAuth is not configured — set %s and %s", prov.name, prov.clientIDEnv, prov.secretEnv),
		})
		return
	}
	slog.InfoContext(c.Request.Context(), "integration connect started", "provider", prov.name)

	// Shopify authorizes against the shop's own domain, which the frontend
	// supplies as ?shop=. The domain is validated and carried in the state so
	// the callback can exchange the code against the same shop.
	if provider := prov.name; provider == "shopify" {
		shop, err := normalizeShopDomain(c.Query("shop"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		q := url.Values{}
		q.Set("client_id", clientID)
		q.Set("redirect_uri", oauthRedirectURI(provider))
		q.Set("scope", "read_orders,write_orders,read_products,write_products,read_customers,write_customers,read_draft_orders,write_draft_orders,read_inventory,write_inventory,read_locations,read_price_rules,write_price_rules")
		q.Set("state", newOAuthStateShop(currentUserID(c), openerOrigin(c), shop))
		c.JSON(http.StatusOK, gin.H{"url": "https://" + shop + "/admin/oauth/authorize?" + q.Encode()})
		return
	}

	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", oauthRedirectURI(prov.name))
	q.Set("response_type", "code")
	q.Set("state", newOAuthState(currentUserID(c), openerOrigin(c)))
	for k, vs := range prov.extraAuthQ {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	// Return the authorize URL (not a 302) so the SPA can call this with an
	// Authorization header, then open the URL in the popup it already spawned.
	c.JSON(http.StatusOK, gin.H{"url": prov.authorizeURL + "?" + q.Encode()})
}

// CallbackIntegration exchanges the authorization code, stores the token, and
// returns an HTML page that notifies the opener window and closes itself.
func (h *WorkflowHandler) CallbackIntegration(c *gin.Context) {
	provider := c.Param("provider")
	if _, ok := oauthProviders[provider]; !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider"})
		return
	}
	ctx := c.Request.Context()
	userID, openerOrig, shop, stateOK := consumeOAuthState(c.Query("state"))
	if errParam := c.Query("error"); errParam != "" {
		slog.WarnContext(ctx, "integration connect failed", "provider", provider, "reason", truncate(errParam, 200))
		telemetry.AuthEvent(ctx, "integration_oauth", "error")
		oauthResultPage(c, provider, openerOrig, false, errParam)
		return
	}
	if !stateOK {
		slog.WarnContext(ctx, "integration connect failed", "provider", provider, "reason", "invalid_or_expired_state")
		telemetry.AuthEvent(ctx, "integration_oauth", "error")
		oauthResultPage(c, provider, openerOrig, false, "invalid or expired state — try connecting again")
		return
	}
	code := c.Query("code")
	if code == "" {
		slog.WarnContext(ctx, "integration connect failed", "provider", provider, "reason", "no_code")
		telemetry.AuthEvent(ctx, "integration_oauth", "error")
		oauthResultPage(c, provider, openerOrig, false, "provider returned no code")
		return
	}

	var (
		conn *models.IntegrationConnection
		err  error
	)
	switch provider {
	case "notion":
		conn, err = exchangeNotionCode(code)
	case "linear":
		conn, err = exchangeLinearCode(code)
	case "github":
		conn, err = exchangeGithubCode(code)
	case "gitlab":
		conn, err = exchangeGitlabCode(code)
	case "gmail":
		conn, err = exchangeGmailCode(code)
	case "stripe":
		conn, err = exchangeStripeCode(code)
	case "shopify":
		conn, err = exchangeShopifyCode(code, shop)
	case "googlecalendar", "googledrive", "googledocs", "googlesheets":
		conn, err = exchangeGoogleServiceCode(provider, code)
	case "outlook":
		conn, err = exchangeOutlookCode(code)
	case "slack":
		conn, err = exchangeSlackCode(code)
	}
	if err != nil {
		slog.WarnContext(ctx, "integration connect failed", "provider", provider, "reason", truncate(err.Error(), 200))
		telemetry.AuthEvent(ctx, "integration_oauth", "error")
		oauthResultPage(c, provider, openerOrig, false, err.Error())
		return
	}
	conn.UserID = userID

	// Upsert: one connection per user per provider. Hard delete — a soft-deleted
	// row would still occupy the (user_id, provider) unique index and block the
	// insert, and dead tokens shouldn't linger in the table anyway.
	h.db.DB.Unscoped().Where("user_id = ? AND provider = ?", userID, provider).Delete(&models.IntegrationConnection{})
	if err := h.db.DB.Create(conn).Error; err != nil {
		slog.WarnContext(ctx, "integration connect failed", "provider", provider, "reason", "store_failed")
		telemetry.AuthEvent(ctx, "integration_oauth", "error")
		oauthResultPage(c, provider, openerOrig, false, "failed to store connection")
		return
	}
	slog.InfoContext(ctx, "integration connected", "provider", provider, "user_id", userID)
	telemetry.AuthEvent(ctx, "integration_oauth", "ok")
	oauthResultPage(c, provider, openerOrig, true, "")
}

// DisconnectIntegration removes the current user's connection for a provider.
func (h *WorkflowHandler) DisconnectIntegration(c *gin.Context) {
	provider := c.Param("provider")
	if _, ok := oauthProviders[provider]; !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider"})
		return
	}
	h.db.DB.Unscoped().Where("user_id = ? AND provider = ?", currentUserID(c), provider).Delete(&models.IntegrationConnection{})
	slog.InfoContext(c.Request.Context(), "integration disconnected", "provider", provider, "user_id", currentUserID(c))
	c.JSON(http.StatusOK, gin.H{"disconnected": provider})
}

// ── Resource listing — lets users pick what to use ────────────

type integrationResource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// listProviderResources resolves fresh credentials and returns the concrete
// resources a connected provider exposes. Every fetch goes through
// FreshAccessToken so expiring tokens (gmail, gitlab) refresh transparently.
func (h *WorkflowHandler) listProviderResources(userID, provider string) ([]integrationResource, error) {
	token, workspace := FreshAccessToken(h.db.DB, userID, provider)
	if token == "" {
		return nil, fmt.Errorf("%s is not connected", provider)
	}
	switch provider {
	case "notion":
		return notionResources(token)
	case "linear":
		return linearResources(token)
	case "github":
		return githubResources(token)
	case "gitlab":
		return gitlabResources(token)
	case "gmail":
		return gmailResources(token)
	case "stripe":
		return stripeResources(token)
	case "shopify":
		return shopifyResources(token, workspace)
	case "googlecalendar":
		return googleCalendarResources(token)
	case "googledrive":
		return googleDriveResources(token)
	case "outlook":
		return outlookResources(token)
	case "slack":
		return slackResources(token, UserGrantToken(h.db.DB, userID, "slack"))
		// googledocs / googlesheets expose no pickable resource list (drive.file
		// scope only sees app-created files) — they fall through to empty.
	}
	return []integrationResource{}, nil
}

// IntegrationResources lists what the connected account exposes (databases,
// pages, repos, projects, labels, prices, products, …) for the resource picker.
func (h *WorkflowHandler) IntegrationResources(c *gin.Context) {
	provider := c.Param("provider")
	if _, ok := oauthProviders[provider]; !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider"})
		return
	}
	resources, err := h.listProviderResources(currentUserID(c), provider)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	kinds := map[string]bool{}
	for _, r := range resources {
		kinds[r.Type] = true
	}
	kindList := make([]string, 0, len(kinds))
	for k := range kinds {
		kindList = append(kindList, k)
	}
	slog.DebugContext(c.Request.Context(), "integration resources listed",
		"provider", provider, "resource_kinds", strings.Join(kindList, ","), "resource_count", len(resources))
	c.JSON(http.StatusOK, resources)
}

func notionResources(token string) ([]integrationResource, error) {
	out := []integrationResource{}
	for _, kind := range []string{"database", "page"} {
		body := fmt.Sprintf(`{"filter":{"value":"%s","property":"object"},"page_size":100}`, kind)
		req, _ := http.NewRequest(http.MethodPost, "https://api.notion.com/v1/search", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Notion-Version", "2022-06-28")
		req.Header.Set("Content-Type", "application/json")
		raw, err := doOAuthRequest(req)
		if err != nil {
			return nil, err
		}
		var res struct {
			Results []struct {
				ID     string `json:"id"`
				Object string `json:"object"`
				Title  []struct {
					PlainText string `json:"plain_text"`
				} `json:"title"`
				// For pages, a property value typed "title" holds the page name as
				// a rich-text array. For databases the same key is a schema object,
				// so it must stay raw and be parsed only when it is an array.
				Properties map[string]struct {
					Type  string          `json:"type"`
					Title json.RawMessage `json:"title"`
				} `json:"properties"`
			} `json:"results"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("parse notion search: %w", err)
		}
		for _, r := range res.Results {
			name := ""
			for _, t := range r.Title {
				name += t.PlainText
			}
			if name == "" {
				for _, p := range r.Properties {
					if p.Type != "title" || len(p.Title) == 0 {
						continue
					}
					var rich []struct {
						PlainText string `json:"plain_text"`
					}
					if json.Unmarshal(p.Title, &rich) == nil {
						for _, t := range rich {
							name += t.PlainText
						}
					}
					break
				}
			}
			if name == "" {
				name = "Untitled"
			}
			out = append(out, integrationResource{ID: r.ID, Name: name, Type: kind})
		}
	}
	return out, nil
}

func linearResources(token string) ([]integrationResource, error) {
	body := `{"query":"{ teams { nodes { id name key } } projects(first: 50) { nodes { id name } } }"}`
	req, _ := http.NewRequest(http.MethodPost, "https://api.linear.app/graphql", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data struct {
			Teams struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					Key  string `json:"key"`
				} `json:"nodes"`
			} `json:"teams"`
			Projects struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"projects"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse linear teams: %w", err)
	}
	out := make([]integrationResource, 0, len(res.Data.Teams.Nodes)+len(res.Data.Projects.Nodes))
	for _, t := range res.Data.Teams.Nodes {
		name := t.Name
		if t.Key != "" {
			name = fmt.Sprintf("%s (%s)", t.Name, t.Key)
		}
		out = append(out, integrationResource{ID: t.ID, Name: name, Type: "team"})
	}
	for _, p := range res.Data.Projects.Nodes {
		out = append(out, integrationResource{ID: p.ID, Name: p.Name, Type: "project"})
	}
	return out, nil
}

// integrationResourcesForAI returns connection status + resources as JSON for
// the AI builder's list_integration_resources tool.
func (h *WorkflowHandler) integrationResourcesForAI(userID, provider string) string {
	providers := allProviders
	if _, ok := oauthProviders[provider]; ok {
		providers = []string{provider}
	}
	out := map[string]any{}
	for _, p := range providers {
		var conn models.IntegrationConnection
		if err := h.db.DB.Where("user_id = ? AND provider = ?", userID, p).First(&conn).Error; err != nil {
			out[p] = map[string]any{
				"connected": false,
				"hint":      "Not connected. The user must click Connect " + p + " in the node settings panel.",
			}
			continue
		}
		entry := map[string]any{"connected": true, "workspace": conn.WorkspaceName}
		if resources, err := h.listProviderResources(userID, p); err != nil {
			entry["error"] = err.Error()
		} else {
			entry["resources"] = resources
		}
		out[p] = entry
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// ── Token exchange ────────────────────────────────────────────

func exchangeNotionCode(code string) (*models.IntegrationConnection, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":   "authorization_code",
		"code":         code,
		"redirect_uri": oauthRedirectURI("notion"),
	})
	req, _ := http.NewRequest(http.MethodPost, "https://api.notion.com/v1/oauth/token", strings.NewReader(string(body)))
	basic := base64.StdEncoding.EncodeToString([]byte(os.Getenv("NOTION_CLIENT_ID") + ":" + os.Getenv("NOTION_CLIENT_SECRET")))
	req.Header.Set("Authorization", "Basic "+basic)
	req.Header.Set("Content-Type", "application/json")

	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken   string `json:"access_token"`
		WorkspaceName string `json:"workspace_name"`
		WorkspaceID   string `json:"workspace_id"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("notion token exchange returned no access token")
	}
	return &models.IntegrationConnection{
		Provider:      "notion",
		AccessToken:   tok.AccessToken,
		WorkspaceName: tok.WorkspaceName,
		WorkspaceID:   tok.WorkspaceID,
	}, nil
}

func exchangeLinearCode(code string) (*models.IntegrationConnection, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", oauthRedirectURI("linear"))
	form.Set("client_id", os.Getenv("LINEAR_CLIENT_ID"))
	form.Set("client_secret", os.Getenv("LINEAR_CLIENT_SECRET"))

	req, _ := http.NewRequest(http.MethodPost, "https://api.linear.app/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("linear token exchange returned no access token")
	}

	conn := &models.IntegrationConnection{
		Provider:    "linear",
		AccessToken: tok.AccessToken,
		Scope:       tok.Scope,
	}
	// Best-effort: resolve the workspace name for display.
	if name, id := linearOrganization(tok.AccessToken); name != "" {
		conn.WorkspaceName = name
		conn.WorkspaceID = id
	}
	return conn, nil
}

func linearOrganization(token string) (name, id string) {
	body := `{"query":"{ organization { id name } }"}`
	req, _ := http.NewRequest(http.MethodPost, "https://api.linear.app/graphql", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return "", ""
	}
	var out struct {
		Data struct {
			Organization struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"organization"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return "", ""
	}
	return out.Data.Organization.Name, out.Data.Organization.ID
}

func doOAuthRequest(req *http.Request) ([]byte, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("provider returned %d: %s", resp.StatusCode, msg)
	}
	return raw, nil
}

// oauthResultPage notifies the opener window and closes the popup.
// targetOrigin is the opener origin captured at connect time; empty falls
// back to the configured frontend URL.
func oauthResultPage(c *gin.Context, provider, targetOrigin string, ok bool, errMsg string) {
	status := "connected"
	detail := "You can close this window."
	if !ok {
		status = "error"
		detail = errMsg
	}
	payload, _ := json.Marshal(map[string]string{
		"type":     "integration-oauth",
		"provider": provider,
		"status":   status,
		"error":    errMsg,
	})
	if targetOrigin == "" {
		targetOrigin = frontendURL()
	}
	target, _ := json.Marshal(targetOrigin)
	// Escape everything interpolated into HTML — detail derives from the
	// provider's `error` query param, so raw interpolation would be reflected XSS.
	safeHTML := `<!doctype html><html><body style="font-family:system-ui;background:#0D0D11;color:#fff;display:flex;align-items:center;justify-content:center;height:100vh;margin:0">
<div style="text-align:center"><p style="font-size:15px;text-transform:capitalize">` + html.EscapeString(provider) + ` ` + status + `</p>
<p style="font-size:12px;color:#667179;max-width:420px">` + html.EscapeString(detail) + `</p></div>
<script>
if (window.opener) { window.opener.postMessage(` + string(payload) + `, ` + string(target) + `); setTimeout(() => window.close(), 800); }
</script></body></html>`
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(safeHTML))
}

// ── Credential access with transparent refresh ────────────────

// FreshAccessToken returns a valid access token and workspace/tenant
// identifier for a user's provider connection, refreshing expiring tokens
// (gmail, gitlab) transparently and persisting the rotated credentials.
// Returns empty strings when no connection exists.
func FreshAccessToken(db *gorm.DB, userID, provider string) (token, workspace string) {
	var conn models.IntegrationConnection
	if err := db.Where("user_id = ? AND provider = ?", userID, provider).First(&conn).Error; err != nil {
		return "", ""
	}
	// Non-expiring token (or no expiry recorded): use as-is.
	if conn.ExpiresAt == nil || time.Until(*conn.ExpiresAt) > 2*time.Minute || conn.RefreshToken == "" {
		return conn.AccessToken, conn.WorkspaceID
	}
	// Expiring soon — refresh. (Backend-auth expansion implements the
	// per-provider refresh exchange; see refreshConnection.)
	if refreshed, err := refreshConnection(db, &conn); err == nil {
		return refreshed.AccessToken, refreshed.WorkspaceID
	}
	return conn.AccessToken, conn.WorkspaceID
}

// UserGrantToken returns the stored user-identity token (e.g. Slack xoxp-)
// for acting on the connecting human's behalf, or "" when the connection
// predates user grants. These tokens don't expire, so no refresh path.
func UserGrantToken(db *gorm.DB, userID, provider string) string {
	var conn models.IntegrationConnection
	if err := db.Where("user_id = ? AND provider = ?", userID, provider).First(&conn).Error; err != nil {
		return ""
	}
	return conn.UserAccessToken
}

// refreshConnection exchanges the stored refresh token for a new access token
// (gmail via Google, gitlab). It persists the rotated access token, expiry,
// and (when the provider returns one) refresh token, then returns the updated
// connection. Providers without refresh flows return the connection unchanged.
func refreshConnection(db *gorm.DB, conn *models.IntegrationConnection) (*models.IntegrationConnection, error) {
	var tokenURL, clientIDEnv, secretEnv string
	switch conn.Provider {
	case "gmail":
		tokenURL, clientIDEnv, secretEnv = "https://oauth2.googleapis.com/token", "GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"
	case "gitlab":
		tokenURL, clientIDEnv, secretEnv = "https://gitlab.com/oauth/token", "GITLAB_CLIENT_ID", "GITLAB_CLIENT_SECRET"
	case "googlecalendar", "googledrive", "googledocs", "googlesheets":
		tokenURL, clientIDEnv, secretEnv = "https://oauth2.googleapis.com/token", "GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"
	case "outlook":
		tokenURL, clientIDEnv, secretEnv = "https://login.microsoftonline.com/common/oauth2/v2.0/token", "MICROSOFT_CLIENT_ID", "MICROSOFT_CLIENT_SECRET"
	default:
		return conn, nil
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", conn.RefreshToken)
	form.Set("client_id", os.Getenv(clientIDEnv))
	form.Set("client_secret", os.Getenv(secretEnv))

	req, _ := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if json.Unmarshal(raw, &tok) != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("%s token refresh returned no access token", conn.Provider)
	}

	// A map update bypasses the model's BeforeSave hook, so encrypt the token
	// values explicitly here. The in-memory conn keeps plaintext for the caller.
	updates := map[string]any{"access_token": cryptobox.Encrypt(tok.AccessToken)}
	conn.AccessToken = tok.AccessToken
	if tok.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		conn.ExpiresAt = &exp
		updates["expires_at"] = exp
	}
	if tok.RefreshToken != "" { // gitlab rotates the refresh token; google does not
		conn.RefreshToken = tok.RefreshToken
		updates["refresh_token"] = cryptobox.Encrypt(tok.RefreshToken)
	}
	db.Model(&models.IntegrationConnection{}).Where("id = ?", conn.ID).Updates(updates)
	return conn, nil
}
