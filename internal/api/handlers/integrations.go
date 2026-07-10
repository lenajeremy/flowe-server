package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"workflow-ai/server/internal/database/models"

	"github.com/gin-gonic/gin"
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
}

func oauthRedirectURI(provider string) string {
	base := os.Getenv("OAUTH_REDIRECT_BASE")
	if base == "" {
		base = "http://localhost:8080"
	}
	return strings.TrimRight(base, "/") + "/api/integrations/" + provider + "/callback"
}

// ── Current user ──────────────────────────────────────────────

// currentUserID resolves the user owning integration connections.
// Single seam for multi-user support: when auth lands, read the user from
// the session/token here — the schema and flows are already per-user.
func currentUserID(_ *gin.Context) string {
	return models.DefaultUserID
}

// ── CSRF state (in-memory, single instance) ───────────────────

type oauthStateEntry struct {
	userID  string
	expires time.Time
}

var (
	oauthStatesMu sync.Mutex
	oauthStates   = map[string]oauthStateEntry{}
)

func newOAuthState(userID string) string {
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
	oauthStates[s] = oauthStateEntry{userID: userID, expires: time.Now().Add(10 * time.Minute)}
	return s
}

// consumeOAuthState validates the state and returns the user who started the flow.
func consumeOAuthState(s string) (string, bool) {
	oauthStatesMu.Lock()
	defer oauthStatesMu.Unlock()
	e, ok := oauthStates[s]
	delete(oauthStates, s)
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	return e.userID, true
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
	for _, p := range []string{"notion", "linear"} {
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

	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", oauthRedirectURI(prov.name))
	q.Set("response_type", "code")
	q.Set("state", newOAuthState(currentUserID(c)))
	for k, vs := range prov.extraAuthQ {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	c.Redirect(http.StatusFound, prov.authorizeURL+"?"+q.Encode())
}

// CallbackIntegration exchanges the authorization code, stores the token, and
// returns an HTML page that notifies the opener window and closes itself.
func (h *WorkflowHandler) CallbackIntegration(c *gin.Context) {
	provider := c.Param("provider")
	if _, ok := oauthProviders[provider]; !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider"})
		return
	}
	if errParam := c.Query("error"); errParam != "" {
		oauthResultPage(c, provider, false, errParam)
		return
	}
	userID, stateOK := consumeOAuthState(c.Query("state"))
	if !stateOK {
		oauthResultPage(c, provider, false, "invalid or expired state — try connecting again")
		return
	}
	code := c.Query("code")
	if code == "" {
		oauthResultPage(c, provider, false, "provider returned no code")
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
	}
	if err != nil {
		oauthResultPage(c, provider, false, err.Error())
		return
	}
	conn.UserID = userID

	// Upsert: one connection per user per provider. Hard delete — a soft-deleted
	// row would still occupy the (user_id, provider) unique index and block the
	// insert, and dead tokens shouldn't linger in the table anyway.
	h.db.DB.Unscoped().Where("user_id = ? AND provider = ?", userID, provider).Delete(&models.IntegrationConnection{})
	if err := h.db.DB.Create(conn).Error; err != nil {
		oauthResultPage(c, provider, false, "failed to store connection")
		return
	}
	oauthResultPage(c, provider, true, "")
}

// DisconnectIntegration removes the current user's connection for a provider.
func (h *WorkflowHandler) DisconnectIntegration(c *gin.Context) {
	provider := c.Param("provider")
	if _, ok := oauthProviders[provider]; !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider"})
		return
	}
	h.db.DB.Unscoped().Where("user_id = ? AND provider = ?", currentUserID(c), provider).Delete(&models.IntegrationConnection{})
	c.JSON(http.StatusOK, gin.H{"disconnected": provider})
}

// ── Resource listing — lets users pick what to use ────────────

type integrationResource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// IntegrationResources lists what the connected account exposes:
// Notion → databases and pages the user shared with the integration;
// Linear → teams in the authorized workspace.
func (h *WorkflowHandler) IntegrationResources(c *gin.Context) {
	provider := c.Param("provider")
	if _, ok := oauthProviders[provider]; !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider"})
		return
	}
	var conn models.IntegrationConnection
	if err := h.db.DB.Where("user_id = ? AND provider = ?", currentUserID(c), provider).
		First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": provider + " is not connected"})
		return
	}

	var (
		resources []integrationResource
		err       error
	)
	switch provider {
	case "notion":
		resources, err = notionResources(conn.AccessToken)
	case "linear":
		resources, err = linearResources(conn.AccessToken)
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
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
	body := `{"query":"{ teams { nodes { id name key } } }"}`
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
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse linear teams: %w", err)
	}
	out := make([]integrationResource, 0, len(res.Data.Teams.Nodes))
	for _, t := range res.Data.Teams.Nodes {
		name := t.Name
		if t.Key != "" {
			name = fmt.Sprintf("%s (%s)", t.Name, t.Key)
		}
		out = append(out, integrationResource{ID: t.ID, Name: name, Type: "team"})
	}
	return out, nil
}

// integrationResourcesForAI returns connection status + resources as JSON for
// the AI builder's list_integration_resources tool.
func (h *WorkflowHandler) integrationResourcesForAI(userID, provider string) string {
	providers := []string{"notion", "linear"}
	if provider == "notion" || provider == "linear" {
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
		var (
			resources []integrationResource
			err       error
		)
		switch p {
		case "notion":
			resources, err = notionResources(conn.AccessToken)
		case "linear":
			resources, err = linearResources(conn.AccessToken)
		}
		if err != nil {
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
func oauthResultPage(c *gin.Context, provider string, ok bool, errMsg string) {
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
	html := `<!doctype html><html><body style="font-family:system-ui;background:#0D0D11;color:#fff;display:flex;align-items:center;justify-content:center;height:100vh;margin:0">
<div style="text-align:center"><p style="font-size:15px;text-transform:capitalize">` + provider + ` ` + status + `</p>
<p style="font-size:12px;color:#667179;max-width:420px">` + detail + `</p></div>
<script>
if (window.opener) { window.opener.postMessage(` + string(payload) + `, '*'); setTimeout(() => window.close(), 800); }
</script></body></html>`
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
}
