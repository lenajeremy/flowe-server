package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"workflow-ai/server/internal/tenancy"

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
			"scope":      {"app_mentions:read,chat:write,chat:write.customize,chat:write.public,channels:read,channels:history,channels:manage,channels:join,groups:read,groups:history,groups:write,users:read,users:read.email,reactions:write,pins:write,files:write"},
			"user_scope": {"chat:write,im:write,im:read,im:history,mpim:read,mpim:history,search:read"},
		},
	},
	"googlemeet": {
		name:         "googlemeet",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/meetings.space.created https://www.googleapis.com/auth/meetings.space.readonly https://www.googleapis.com/auth/meetings.space.settings"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	"googleslides": {
		name:         "googleslides",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/presentations https://www.googleapis.com/auth/drive"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	"googleforms": {
		name:         "googleforms",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/forms.body https://www.googleapis.com/auth/forms.responses.readonly https://www.googleapis.com/auth/drive.file"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	"googletasks": {
		name:         "googletasks",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/tasks"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	"googlechat": {
		name:         "googlechat",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/chat.spaces https://www.googleapis.com/auth/chat.messages https://www.googleapis.com/auth/chat.memberships https://www.googleapis.com/auth/chat.delete"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	"googlekeep": {
		name:         "googlekeep",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/keep"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	// HubSpot access tokens last 30 minutes and its refresh tokens do not expire.
	// Every scope ticked on the app's Auth settings is treated as REQUIRED and must
	// appear here, or HubSpot shows an error instead of a consent screen — so this
	// list has to match the app's configuration exactly.
	"hubspot": {
		name:         "hubspot",
		authorizeURL: "https://app.hubspot.com/oauth/authorize",
		clientIDEnv:  "HUBSPOT_CLIENT_ID",
		secretEnv:    "HUBSPOT_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope": {"oauth crm.objects.contacts.read crm.objects.contacts.write " +
				"crm.objects.companies.read crm.objects.companies.write " +
				"crm.objects.deals.read crm.objects.deals.write " +
				"crm.objects.owners.read crm.schemas.contacts.read crm.schemas.companies.read " +
				"crm.schemas.deals.read crm.lists.read crm.lists.write tickets"},
		},
	},
	// Front access tokens last 60 minutes. Its refresh token is valid for six
	// months and is only replaced in the final 24 hours, so rotation is rare but
	// still has to be persisted — which the shared refresh path does.
	"front": {
		name:         "front",
		authorizeURL: "https://app.frontapp.com/oauth/authorize",
		clientIDEnv:  "FRONT_CLIENT_ID",
		secretEnv:    "FRONT_CLIENT_SECRET",
	},
	// Search Console's webmasters scope covers both reading reports and adding or
	// removing properties and sitemaps; there is no narrower write scope.
	"googlesearchconsole": {
		name:         "googlesearchconsole",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/webmasters"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	// contacts covers read and write; contacts.other.readonly is what exposes the
	// "other contacts" people have corresponded with but never saved.
	"googlecontacts": {
		name:         "googlecontacts",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		clientIDEnv:  "GOOGLE_CLIENT_ID",
		secretEnv:    "GOOGLE_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":       {"https://www.googleapis.com/auth/contacts https://www.googleapis.com/auth/contacts.other.readonly"},
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	},
	// Gumroad's OAuth scopes are coarse: edit_products covers the whole catalogue
	// and view_sales the whole ledger. There is nothing narrower to ask for.
	"gumroad": {
		name:         "gumroad",
		authorizeURL: "https://gumroad.com/oauth/authorize",
		clientIDEnv:  "GUMROAD_CLIENT_ID",
		secretEnv:    "GUMROAD_CLIENT_SECRET",
		extraAuthQ:   url.Values{"scope": {"edit_products view_sales"}},
	},
	// Supabase Connect: PKCE is required and the token endpoint wants the client
	// credentials as Basic auth. Scopes below are taken from the per-endpoint
	// x-oauth-scope values in Supabase's own OpenAPI document, covering exactly the
	// operations this node ships — a 403 here means a scope, not a permission.
	"supabase": {
		name:         "supabase",
		authorizeURL: "https://api.supabase.com/v1/oauth/authorize",
		clientIDEnv:  "SUPABASE_CLIENT_ID",
		secretEnv:    "SUPABASE_CLIENT_SECRET",
		extraAuthQ:   url.Values{"scope": {"analytics:read auth:read auth:write database:read database:write domains:read domains:write edge_functions:read edge_functions:write environment:read environment:write organizations:read projects:read projects:write rest:read rest:write secrets:read secrets:write storage:read"}},
	},
	// Netlify has no scope system and issues no refresh token: an OAuth token
	// lasts until it is revoked, so there is no refreshTokenEndpoints entry.
	// Its documented flow is implicit; the code-exchange token URL below is not in
	// Netlify's OpenAPI spec, so treat a failure here as the first thing to check.
	"netlify": {
		name:         "netlify",
		authorizeURL: "https://app.netlify.com/authorize",
		clientIDEnv:  "NETLIFY_CLIENT_ID",
		secretEnv:    "NETLIFY_CLIENT_SECRET",
	},
	// Dropbox issues short-lived tokens, so token_access_type=offline is what
	// earns a refresh token; without it the connection dies in four hours.
	"dropbox": {
		name:         "dropbox",
		authorizeURL: "https://www.dropbox.com/oauth2/authorize",
		clientIDEnv:  "DROPBOX_CLIENT_ID",
		secretEnv:    "DROPBOX_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"token_access_type": {"offline"},
			"scope": {"account_info.read files.metadata.read files.metadata.write " +
				"files.content.read files.content.write sharing.read sharing.write " +
				"file_requests.read file_requests.write"},
		},
	},
	// Typeform access tokens expire after a week, and it only returns a refresh
	// token when the "offline" scope is asked for — without it the connection dies
	// after seven days with nothing to renew from. Its refresh tokens also rotate,
	// which the shared refresh path already persists.
	"typeform": {
		name:         "typeform",
		authorizeURL: "https://api.typeform.com/oauth/authorize",
		clientIDEnv:  "TYPEFORM_CLIENT_ID",
		secretEnv:    "TYPEFORM_CLIENT_SECRET",
		extraAuthQ: url.Values{
			// themes:write is here for delete_theme; Typeform's scopes are not
			// hierarchical, so a write scope does not imply its read counterpart.
			"scope": {"offline forms:read forms:write responses:read responses:write " +
				"workspaces:read workspaces:write themes:read themes:write " +
				"images:read webhooks:read webhooks:write accounts:read"},
		},
	},
	// Calendly's booking endpoints need a paid plan; the grant itself does not say
	// so, and a 403 at run time is the first sign.
	//
	// Scopes are requested here even though Calendly's own OAuth walkthrough omits
	// them from its example URL — the scope reference does define them, and a write
	// scope implies its matching read, which is why the read halves of
	// scheduled_events, organizations and webhooks are absent rather than missing.
	//
	// Only one redirect URI is allowed per Calendly app, so each deployment
	// environment needs its own app and its own CALENDLY_CLIENT_ID.
	"calendly": {
		name:         "calendly",
		authorizeURL: "https://auth.calendly.com/oauth/authorize",
		clientIDEnv:  "CALENDLY_CLIENT_ID",
		secretEnv:    "CALENDLY_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope": {"users:read event_types:read availability:read " +
				"scheduled_events:write scheduling_links:write " +
				"organizations:write routing_forms:read webhooks:write " +
				"data_compliance:write"},
		},
	},
	// ClickUp authorizes on the app domain and takes no scope — see
	// integrations_clickup.go.
	"clickup": {
		name:         "clickup",
		authorizeURL: clickupAuthorizeURL,
		clientIDEnv:  "CLICKUP_CLIENT_ID",
		secretEnv:    "CLICKUP_CLIENT_SECRET",
	},
	// monday.com's current OAuth flow requires PKCE and returns an expiring JWT
	// access token plus a rotating refresh token. The scopes match the actions,
	// resource pickers, and signed board webhooks implemented by this connector.
	"monday": {
		name:         "monday",
		authorizeURL: "https://auth.monday.com/oauth2/authorize",
		clientIDEnv:  "MONDAY_CLIENT_ID",
		secretEnv:    "MONDAY_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope":                   {"me:read account:read boards:read boards:write updates:read updates:write users:read webhooks:read webhooks:write"},
			"force_install_if_needed": {"true"},
		},
	},
	// Asana scopes are granular and non-inheriting: write does not imply read,
	// and delete is separate again. Keep this list in step with the concrete
	// operations below instead of relying on the app's legacy full-access mode.
	"asana": {
		name:         "asana",
		authorizeURL: "https://app.asana.com/-/oauth_authorize",
		clientIDEnv:  "ASANA_CLIENT_ID",
		secretEnv:    "ASANA_CLIENT_SECRET",
		extraAuthQ: url.Values{
			"scope": {"workspaces:read projects:read project_sections:read tasks:read tasks:write tasks:delete stories:read stories:write webhooks:write webhooks:delete"},
		},
	},
	// Airtable requires PKCE and Basic auth on the token endpoint — see
	// integrations_airtable.go.
	"airtable": {
		name:         "airtable",
		authorizeURL: airtableAuthorizeURL,
		clientIDEnv:  "AIRTABLE_CLIENT_ID",
		secretEnv:    "AIRTABLE_CLIENT_SECRET",
		extraAuthQ:   airtableAuthorizeQuery(),
	},
	// Jira and Confluence share one Atlassian OAuth app, differing only in
	// scope — see integrations_atlassian.go.
	"jira": {
		name:         "jira",
		authorizeURL: atlassianAuthorizeURL,
		clientIDEnv:  "ATLASSIAN_CLIENT_ID",
		secretEnv:    "ATLASSIAN_CLIENT_SECRET",
		extraAuthQ:   atlassianAuthorizeQuery("jira"),
	},
	"confluence": {
		name:         "confluence",
		authorizeURL: atlassianAuthorizeURL,
		clientIDEnv:  "ATLASSIAN_CLIENT_ID",
		secretEnv:    "ATLASSIAN_CLIENT_SECRET",
		extraAuthQ:   atlassianAuthorizeQuery("confluence"),
	},
	// Bitbucket has its own OAuth server, not Atlassian's, and does not accept a
	// scope parameter — scopes are fixed on the consumer in Bitbucket settings.
	"bitbucket": {
		name:         "bitbucket",
		authorizeURL: "https://bitbucket.org/site/oauth2/authorize",
		clientIDEnv:  "BITBUCKET_CLIENT_ID",
		secretEnv:    "BITBUCKET_CLIENT_SECRET",
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
	"gmail", "googlecalendar", "googledrive", "googledocs", "googlesheets",
	"googleslides", "googleforms", "googlemeet", "googlechat", "googletasks",
	"googlekeep", "outlook", "slack", "notion", "linear",
	"github", "gitlab", "jira", "confluence", "bitbucket",
	"stripe", "shopify", "granola", "resend", "sendgrid", "kit", "airtable", "clickup", "monday", "asana", "typeform", "calendly", "dropbox", "netlify", "supabase", "gumroad",
	"googlesearchconsole", "googlecontacts", "hubspot", "front",
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
	userID string
	// orgID is captured when the flow STARTS, because the provider's redirect back
	// carries no session — there is no other way to know which tenant the
	// resulting connection belongs to.
	orgID  string
	origin string // opener origin for the popup's postMessage target
	shop   string // shopify shop domain (empty for other providers)
	// verifier is the PKCE code verifier for providers that require it. It is
	// generated with the state and never leaves the server: only its SHA-256
	// challenge goes to the provider, and the verifier is replayed at token
	// exchange to prove the same client started the flow.
	verifier string
	// githubInstall marks state minted for /apps/{slug}/installations/new.
	// githubInstallationID is filled by the setup callback before it chains
	// into OAuth, then verified against the resulting GitHub App user token.
	githubInstall        bool
	githubInstallationID string
	// agentHost distinguishes an org-level team-chat installation from an
	// ordinary user-owned Slack action credential, even though both use the same
	// Slack OAuth application and callback.
	agentHost bool
	expires   time.Time
}

var (
	oauthStatesMu sync.Mutex
	oauthStates   = map[string]oauthStateEntry{}
)

func newOAuthState(userID, orgID, origin string) string {
	return newOAuthStateFull(userID, orgID, origin, "", "")
}

func newOAuthStateShop(userID, orgID, origin, shop string) string {
	return newOAuthStateFull(userID, orgID, origin, shop, "")
}

func newOAuthStateFull(userID, orgID, origin, shop, verifier string) string {
	return newOAuthStateEntry(oauthStateEntry{
		userID: userID, orgID: orgID, origin: origin, shop: shop, verifier: verifier,
	})
}

func newAgentHostOAuthState(userID, orgID, origin, verifier string) string {
	return newOAuthStateEntry(oauthStateEntry{
		userID: userID, orgID: orgID, origin: origin, verifier: verifier, agentHost: true,
	})
}

func newGitHubInstallState(userID, orgID, origin string) string {
	return newOAuthStateEntry(oauthStateEntry{
		userID: userID, orgID: orgID, origin: origin, githubInstall: true,
	})
}

func newGitHubInstalledOAuthState(userID, orgID, origin, installationID string) string {
	return newOAuthStateEntry(oauthStateEntry{
		userID: userID, orgID: orgID, origin: origin, githubInstallationID: installationID,
	})
}

func newOAuthStateEntry(entry oauthStateEntry) string {
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
	entry.expires = time.Now().Add(10 * time.Minute)
	oauthStates[s] = entry
	return s
}

// consumeOAuthState validates the state and returns the entry that started the
// flow. The state is single-use: it is deleted whether or not it was still valid.
func consumeOAuthState(s string) (oauthStateEntry, bool) {
	oauthStatesMu.Lock()
	defer oauthStatesMu.Unlock()
	e, found := oauthStates[s]
	delete(oauthStates, s)
	if !found || time.Now().After(e.expires) {
		return oauthStateEntry{}, false
	}
	return e, true
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
// scoped to the current organization and user. A person can connect the same
// provider separately in two organizations; showing the other tenant's account
// here would both leak metadata and send the resource picker down the wrong
// authorization path.
func (h *WorkflowHandler) ListIntegrations(c *gin.Context) {
	var conns []models.IntegrationConnection
	h.db.DB.Where("organization_id = ? AND user_id = ?", currentOrgID(c), currentUserID(c)).Find(&conns)
	byProvider := map[string]models.IntegrationConnection{}
	for _, conn := range conns {
		byProvider[conn.Provider] = conn
	}

	// How many of the user's workflows reference each provider. The connections
	// screen shows this so "disconnect" can say what it will break rather than
	// leaving the user to guess.
	usage := h.providerWorkflowCounts(currentOrgID(c))

	out := []gin.H{}
	for _, p := range allProviders {
		conn, connected := byProvider[p]
		// An API-key provider is always available: the user supplies the
		// credential, so there is nothing for the server to have configured.
		keyProv, isKeyAuth := apiKeyProviders[p]
		available := isKeyAuth
		if !isKeyAuth {
			prov := oauthProviders[p]
			available = os.Getenv(prov.clientIDEnv) != "" && os.Getenv(prov.secretEnv) != ""
		}
		entry := gin.H{
			"provider":  p,
			"connected": connected,
			"available": available,
			"workflows": usage[p],
		}
		if isKeyAuth {
			entry["auth_style"] = "api_key"
			entry["key_hint"] = keyProv.hint
		}
		if connected {
			entry["workspace_name"] = conn.WorkspaceName
			entry["connected_at"] = conn.CreatedAt
			entry["updated_at"] = conn.UpdatedAt
			// Presence of an expiry, and whether it has passed. The token may still
			// refresh — this is for display, not for gating a call.
			if conn.ExpiresAt != nil {
				entry["expires_at"] = conn.ExpiresAt
				entry["expired"] = conn.ExpiresAt.Before(time.Now())
			}
		}
		out = append(out, entry)
	}
	c.JSON(http.StatusOK, out)
}

// providerWorkflowCounts counts, per provider, how many of the org's workflows
// contain either an action node of that provider's type or an App Trigger whose
// triggerProvider names it. JSONB containment finds both without loading every
// graph; omitting the second shape would let disconnect claim that no workflow
// uses GitLab immediately before removing its project hooks.
//
// Counted per ORG because workflows are org-owned: disconnecting a credential
// should warn about everything it would break, including a teammate's workflow.
func (h *WorkflowHandler) providerWorkflowCounts(orgID string) map[string]int {
	counts := map[string]int{}
	for _, p := range allProviders {
		var n int64
		h.db.DB.Model(&models.Workflow{}).
			Where("organization_id = ? AND deleted_at IS NULL", orgID).
			Where(`(nodes @> ? OR nodes @> ?)`,
				fmt.Sprintf(`[{"data":{"nodeType":%q}}]`, p),
				fmt.Sprintf(`[{"data":{"nodeType":"integrationTrigger","triggerProvider":%q}}]`, p)).
			Count(&n)
		counts[p] = int(n)
	}
	return counts
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
		q.Set("state", newOAuthStateShop(currentUserID(c), currentOrgID(c), openerOrigin(c), shop))
		c.JSON(http.StatusOK, gin.H{"url": "https://" + shop + "/admin/oauth/authorize?" + q.Encode()})
		return
	}

	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", oauthRedirectURI(prov.name))
	q.Set("response_type", "code")
	verifier := ""
	if pkceProviders[prov.name] {
		verifier = newPKCEVerifier()
		q.Set("code_challenge", pkceChallenge(verifier))
		q.Set("code_challenge_method", "S256")
	}
	agentHost, _ := c.Get("agent-host-connect")
	if agentHost == true {
		q.Set("state", newAgentHostOAuthState(currentUserID(c), currentOrgID(c), openerOrigin(c), verifier))
	} else {
		q.Set("state", newOAuthStateFull(currentUserID(c), currentOrgID(c), openerOrigin(c), "", verifier))
	}
	for k, vs := range prov.extraAuthQ {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	if agentHost == true && prov.name == "slack" {
		q.Set("scope", strings.Join(slackAgentHostRequestedScopes, ","))
		q.Del("user_scope")
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
	st, stateOK := consumeOAuthState(c.Query("state"))
	userID, orgID, openerOrig, shop := st.userID, st.orgID, st.origin, st.shop
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
	if st.agentHost && !tenancy.CanManageMembers(h.db.DB, orgID, userID) {
		slog.WarnContext(ctx, "slack agent host connect rejected", "reason", "host_manager_authority_ended")
		oauthResultPage(c, provider, openerOrig, false, "your organization role no longer allows connecting the shared Slack host")
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
	case "googlecalendar", "googledrive", "googledocs", "googlesheets",
		"googlemeet", "googleslides", "googleforms", "googletasks", "googlechat", "googlekeep",
		"googlesearchconsole", "googlecontacts":
		conn, err = exchangeGoogleServiceCode(provider, code)
	case "airtable":
		conn, err = exchangeAirtableCode(code, st.verifier)
	case "clickup":
		conn, err = exchangeClickUpCode(code)
	case "monday":
		conn, err = exchangeMondayCode(code, st.verifier)
	case "asana":
		conn, err = exchangeAsanaCode(code, st.verifier)
	case "dropbox":
		conn, err = exchangeFormPostCode("dropbox", code, "https://api.dropboxapi.com/oauth2/token")
	case "netlify":
		conn, err = exchangeFormPostCode("netlify", code, "https://api.netlify.com/oauth/token")
	case "supabase":
		conn, err = exchangeSupabaseCode(code, st.verifier)
	case "gumroad":
		conn, err = exchangeFormPostCode("gumroad", code, "https://api.gumroad.com/oauth/token")
	case "hubspot":
		conn, err = exchangeFormPostCode("hubspot", code, "https://api.hubapi.com/oauth/v1/token")
	case "front":
		// Front wants the client credentials as Basic auth on its OAuth endpoints.
		conn, err = exchangeBasicAuthCode("front", code, "https://app.frontapp.com/oauth/token")
	case "typeform":
		conn, err = exchangeFormPostCode("typeform", code, "https://api.typeform.com/oauth/token")
	case "calendly":
		conn, err = exchangeFormPostCode("calendly", code, "https://auth.calendly.com/oauth/token")
	case "jira", "confluence":
		conn, err = exchangeAtlassianCode(provider, code)
	case "bitbucket":
		conn, err = exchangeBitbucketCode(code)
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
	if provider == "github" && (st.githubInstall || st.githubInstallationID != "") {
		installationID := st.githubInstallationID
		if st.githubInstall {
			installationID, err = positiveGitHubID(c.Query("installation_id"))
		}
		if err == nil {
			err = verifyGitHubInstallation(ctx, conn.AccessToken, installationID)
		}
		if err != nil {
			slog.WarnContext(ctx, "github installation verification failed", "reason", truncate(err.Error(), 200))
			telemetry.AuthEvent(ctx, "integration_oauth", "error")
			oauthResultPage(c, provider, openerOrig, false, err.Error())
			return
		}
	}
	conn.UserID = userID
	conn.OrganizationID = orgID
	if provider == "slack" && st.agentHost {
		// A host installation is bot-only transport shared by the organization.
		// Never overwrite the deployer's separate Slack action credential with it.
		if err := h.syncSlackAgentHost(orgID, userID, conn); err != nil {
			slog.WarnContext(ctx, "slack agent host sync failed", "reason", truncate(err.Error(), 200))
			telemetry.AuthEvent(ctx, "integration_oauth", "error")
			message := "failed to connect the shared Slack workspace"
			if strings.Contains(err.Error(), "already connected to another Fernary organization") {
				message = "this Slack workspace is already connected to another Fernary organization"
			}
			oauthResultPage(c, provider, openerOrig, false, message)
			return
		}
		slog.InfoContext(ctx, "slack agent host connected", "user_id", userID, "org_id", orgID)
		telemetry.AuthEvent(ctx, "integration_oauth", "ok")
		oauthResultPage(c, provider, openerOrig, true, "")
		return
	}

	// Upsert: one connection per user per provider. Hard delete — a soft-deleted
	// row would still occupy the (organization_id, user_id, provider) unique index
	// and block the insert, and dead tokens shouldn't linger in the table anyway.
	err = h.withHostedAuthorityLock(ctx, orgID, userID, func(connection *gorm.DB) error {
		return connection.Transaction(func(tx *gorm.DB) error {
			if err := tx.Unscoped().Where("organization_id = ? AND user_id = ? AND provider = ?",
				orgID, userID, provider).Delete(&models.IntegrationConnection{}).Error; err != nil {
				return err
			}
			return tx.Create(conn).Error
		})
	})
	if err != nil {
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
	if !knownProvider(provider) {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider"})
		return
	}
	orgID, userID := currentOrgID(c), currentUserID(c)
	err := h.withHostedAuthorityLock(c.Request.Context(), orgID, userID, func(connection *gorm.DB) error {
		// GitLab creates real project hooks with this OAuth token. Remove them
		// while the credential still exists; the authority lock also prevents a
		// hosted turn from starting halfway through credential revocation.
		if provider == "gitlab" || provider == "monday" || provider == "asana" {
			query := connection.Where("organization_id = ? AND user_id = ? AND provider = ? AND deleted_at IS NULL",
				orgID, userID, provider)
			if err := h.retireIntegrationTriggers(c.Request.Context(), query); err != nil {
				return fmt.Errorf("retire provider webhooks: %w", err)
			}
		}
		return connection.Unscoped().Where("organization_id = ? AND user_id = ? AND provider = ?",
			orgID, userID, provider).Delete(&models.IntegrationConnection{}).Error
	})
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "could not disconnect integration", "provider", provider, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not disconnect " + provider})
		return
	}
	slog.InfoContext(c.Request.Context(), "integration disconnected", "provider", provider, "user_id", currentUserID(c))
	c.JSON(http.StatusOK, gin.H{"disconnected": provider})
}

// ── Resource listing — lets users pick what to use ────────────

type integrationResource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// errNotConnected separates "you haven't connected this yet" from a genuine
// upstream failure, so the picker can stay quiet instead of raising an error
// toast for a provider the user simply hasn't set up.
var errNotConnected = errors.New("is not connected")

// listProviderResources resolves fresh credentials and returns the concrete
// resources a connected provider exposes. Every fetch goes through
// FreshAccessToken so expiring tokens (gmail, gitlab) refresh transparently.
func (h *WorkflowHandler) listProviderResources(orgID, userID, provider string) ([]integrationResource, error) {
	token, workspace := FreshAccessTokenForOrg(h.db.DB, orgID, userID, provider)
	if token == "" {
		return nil, fmt.Errorf("%s: %w", provider, errNotConnected)
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
		return slackResources(token, UserGrantTokenForOrg(h.db.DB, orgID, userID, "slack"))
	case "jira":
		return jiraResources(token, workspace)
	case "confluence":
		return confluenceResources(token, workspace)
	case "bitbucket":
		return bitbucketResources(token, workspace)
	case "airtable":
		return airtableResources(token)
	case "supabase":
		return supabaseResources(token)
	case "googlesearchconsole":
		return searchConsoleResources(token)
	case "clickup":
		return clickupResources(token)
	case "monday":
		return mondayResources(token)
	case "asana":
		return asanaResources(token)
	case "googletasks":
		return googleTasksResources(token)
	case "googlechat":
		return googleChatResources(token)
		// meet/slides/forms/keep expose no listable resource: Meet spaces have no
		// index, and Slides/Forms live in Drive rather than their own API.
		// googledocs / googlesheets expose no pickable resource list (drive.file
		// scope only sees app-created files) — they fall through to empty.
	}
	return []integrationResource{}, nil
}

// listChildResources lists resources that live inside another resource —
// branches and collaborators of a repository, and later channels of a
// workspace. Separate from listProviderResources because the parent is not
// optional here: without it there is nothing to enumerate.
//
// Returns an empty list rather than an error for a provider/parent combination
// nobody has taught it yet, so the picker degrades to its manual-entry field
// instead of raising a toast at someone who did nothing wrong.
func (h *WorkflowHandler) listChildResources(orgID, userID, provider, parent string) ([]integrationResource, error) {
	token, _ := FreshAccessTokenForOrg(h.db.DB, orgID, userID, provider)
	if token == "" {
		return nil, fmt.Errorf("%s: %w", provider, errNotConnected)
	}
	switch provider {
	case "github":
		branches, err := githubBranchResources(token, parent)
		if err != nil {
			return nil, err
		}
		people, err := githubCollaboratorResources(token, parent)
		if err != nil {
			// Collaborators need push access to read; a repo the user can only
			// read still has usable branches, so return those rather than nothing.
			return branches, nil
		}
		return append(branches, people...), nil
	case "gitlab":
		branches, err := gitlabBranchResources(token, parent)
		if err != nil {
			return nil, err
		}
		people, err := gitlabMemberResources(token, parent)
		if err != nil {
			// Branch filters remain useful when the connected user cannot enumerate
			// every inherited project member.
			return branches, nil
		}
		return append(branches, people...), nil
	case "monday":
		return mondayBoardResources(token, parent)
	case "asana":
		return asanaProjectResources(token, parent)
	}
	return []integrationResource{}, nil
}

// IntegrationResources lists what the connected account exposes (databases,
// pages, repos, projects, labels, prices, products, …) for the resource picker.
func (h *WorkflowHandler) IntegrationResources(c *gin.Context) {
	provider := c.Param("provider")
	if !knownProvider(provider) {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider"})
		return
	}
	// Some resources only exist inside another one: a branch belongs to a
	// repository, a collaborator to a repository. `parent` names that container.
	// Without it the picker would have to ask the user to type a branch name,
	// which is exactly the guesswork the picker exists to remove.
	var resources []integrationResource
	var err error
	if parent := c.Query("parent"); parent != "" {
		resources, err = h.listChildResources(currentOrgID(c), currentUserID(c), provider, parent)
	} else {
		resources, err = h.listProviderResources(currentOrgID(c), currentUserID(c), provider)
	}
	if errors.Is(err, errNotConnected) {
		// 404, not 502 — the resource picker treats this as its quiet
		// "connect this first" state rather than a failure.
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
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
func (h *WorkflowHandler) integrationResourcesForAI(orgID, userID, provider string) string {
	providers := allProviders
	if _, ok := oauthProviders[provider]; ok {
		providers = []string{provider}
	}
	out := map[string]any{}
	for _, p := range providers {
		var conn models.IntegrationConnection
		if err := h.db.DB.Where("organization_id = ? AND user_id = ? AND provider = ?", orgID, userID, p).
			First(&conn).Error; err != nil {
			out[p] = map[string]any{
				"connected": false,
				"hint":      "Not connected. The user must click Connect " + p + " in the node settings panel.",
			}
			continue
		}
		entry := map[string]any{"connected": true, "workspace": conn.WorkspaceName}
		if resources, err := h.listProviderResources(orgID, userID, p); err != nil {
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
// identifier for a user's provider connection, transparently refreshing
// expiring tokens (including GitHub, Gmail, GitLab, Google, Outlook, monday.com
// and Asana) and persisting the
// rotated credentials. Returns empty strings when no connection exists.
func FreshAccessToken(db *gorm.DB, userID, provider string) (token, workspace string) {
	var conn models.IntegrationConnection
	if err := db.Where("user_id = ? AND provider = ?", userID, provider).First(&conn).Error; err != nil {
		return "", ""
	}
	return freshAccessTokenForConnection(db, &conn)
}

// FreshAccessTokenForOrg is the tenant-safe form for requests that already
// know their organization. A user can belong to more than one organization and
// connect the same provider in each; selecting by user/provider alone could
// refresh and use the credential from the wrong tenant.
func FreshAccessTokenForOrg(db *gorm.DB, orgID, userID, provider string) (token, workspace string) {
	var conn models.IntegrationConnection
	if err := db.Where("organization_id = ? AND user_id = ? AND provider = ?",
		orgID, userID, provider).First(&conn).Error; err != nil {
		return "", ""
	}
	return freshAccessTokenForConnection(db, &conn)
}

func freshAccessTokenForConnection(db *gorm.DB, conn *models.IntegrationConnection) (token, workspace string) {
	// Nothing to do for a token with no recorded expiry (classic OAuth Apps,
	// Slack, Notion…) or one that is still comfortably valid.
	if conn.ExpiresAt == nil || time.Until(*conn.ExpiresAt) > 2*time.Minute {
		return conn.AccessToken, conn.WorkspaceID
	}
	// Expired/expiring with nothing to refresh from: the call is going to 401,
	// so say why now — a stale token looks identical to a wrong one in the logs.
	if conn.RefreshToken == "" {
		slog.Warn("integration token expired and cannot be refreshed — reconnect required",
			"provider", conn.Provider, "user_id", conn.UserID, "expired_at", conn.ExpiresAt)
		return conn.AccessToken, conn.WorkspaceID
	}
	if refreshed, err := refreshConnection(db, conn); err == nil {
		slog.Info("integration token refreshed", "provider", conn.Provider, "user_id", conn.UserID,
			"expires_at", refreshed.ExpiresAt)
		return refreshed.AccessToken, refreshed.WorkspaceID
	} else {
		// The refresh token itself is spent or revoked — only reconnecting fixes
		// this, and the user needs to be told rather than left with silent 401s.
		slog.Error("integration token refresh failed — reconnect required",
			"provider", conn.Provider, "user_id", conn.UserID, "error", err.Error())
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

// UserGrantTokenForOrg is the tenant-safe equivalent used by authenticated
// resource pickers. Slack can be connected to a different workspace in each
// Fernary organization, so user/provider alone is ambiguous.
func UserGrantTokenForOrg(db *gorm.DB, orgID, userID, provider string) string {
	var conn models.IntegrationConnection
	if err := db.Where("organization_id = ? AND user_id = ? AND provider = ?", orgID, userID, provider).
		First(&conn).Error; err != nil {
		return ""
	}
	return conn.UserAccessToken
}

// refreshConnection exchanges the stored refresh token for a new access token
// with its provider. It persists the rotated access token, expiry,
// and (when the provider returns one) refresh token, then returns the updated
// connection. Providers without refresh flows return the connection unchanged.
// refreshTokenEndpoints maps a provider to its refresh exchange. Package-level
// so tests can point a provider at a stub server; production values only.
// GitHub App user tokens live 8 hours and their refresh token (~6 months)
// rotates on every use. Classic OAuth Apps never reach here — they return no
// refresh token, so ExpiresAt stays nil and the token is used as-is.
// jsonBody sends the grant as a JSON object rather than a form (Atlassian and monday.com).
// basicAuth puts the client credentials in an Authorization header rather than
// the body (Bitbucket rejects them in the body).
type refreshEndpoint struct {
	tokenURL    string
	clientIDEnv string
	secretEnv   string
	jsonBody    bool
	basicAuth   bool
}

var refreshTokenEndpoints = map[string]refreshEndpoint{
	"github":              {tokenURL: "https://github.com/login/oauth/access_token", clientIDEnv: "GITHUB_CLIENT_ID", secretEnv: "GITHUB_CLIENT_SECRET"},
	"gmail":               {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"gitlab":              {tokenURL: "https://gitlab.com/oauth/token", clientIDEnv: "GITLAB_CLIENT_ID", secretEnv: "GITLAB_CLIENT_SECRET"},
	"googlecalendar":      {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"googledrive":         {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"googledocs":          {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"googlesheets":        {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"googlemeet":          {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"googleslides":        {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"googleforms":         {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"googletasks":         {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"googlechat":          {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"googlekeep":          {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"googlesearchconsole": {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"googlecontacts":      {tokenURL: "https://oauth2.googleapis.com/token", clientIDEnv: "GOOGLE_CLIENT_ID", secretEnv: "GOOGLE_CLIENT_SECRET"},
	"outlook":             {tokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token", clientIDEnv: "MICROSOFT_CLIENT_ID", secretEnv: "MICROSOFT_CLIENT_SECRET"},
	"jira":                {tokenURL: atlassianTokenURL, clientIDEnv: "ATLASSIAN_CLIENT_ID", secretEnv: "ATLASSIAN_CLIENT_SECRET", jsonBody: true},
	"confluence":          {tokenURL: atlassianTokenURL, clientIDEnv: "ATLASSIAN_CLIENT_ID", secretEnv: "ATLASSIAN_CLIENT_SECRET", jsonBody: true},
	"airtable":            {tokenURL: airtableTokenURL, clientIDEnv: "AIRTABLE_CLIENT_ID", secretEnv: "AIRTABLE_CLIENT_SECRET", basicAuth: true},
	"supabase":            {tokenURL: supabaseTokenURL, clientIDEnv: "SUPABASE_CLIENT_ID", secretEnv: "SUPABASE_CLIENT_SECRET", basicAuth: true},
	"dropbox":             {tokenURL: "https://api.dropboxapi.com/oauth2/token", clientIDEnv: "DROPBOX_CLIENT_ID", secretEnv: "DROPBOX_CLIENT_SECRET"},
	"typeform":            {tokenURL: "https://api.typeform.com/oauth/token", clientIDEnv: "TYPEFORM_CLIENT_ID", secretEnv: "TYPEFORM_CLIENT_SECRET"},
	"calendly":            {tokenURL: "https://auth.calendly.com/oauth/token", clientIDEnv: "CALENDLY_CLIENT_ID", secretEnv: "CALENDLY_CLIENT_SECRET"},
	"bitbucket":           {tokenURL: bitbucketTokenURL, clientIDEnv: "BITBUCKET_CLIENT_ID", secretEnv: "BITBUCKET_CLIENT_SECRET", basicAuth: true},
	"monday":              {tokenURL: mondayTokenURL, clientIDEnv: "MONDAY_CLIENT_ID", secretEnv: "MONDAY_CLIENT_SECRET", jsonBody: true},
	"asana":               {tokenURL: asanaTokenURL, clientIDEnv: "ASANA_CLIENT_ID", secretEnv: "ASANA_CLIENT_SECRET"},
}

// refreshedToken is one provider's answer to a refresh-token grant.
type refreshedToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ErrorDesc    string `json:"error_description"`
}

// exchangeRefreshToken performs the refresh grant for a provider. Split from
// persistence so the wire format can be tested without a database; returns
// ok=false for providers that have no refresh flow.
func exchangeRefreshToken(conn *models.IntegrationConnection) (tok refreshedToken, ok bool, err error) {
	ep, supported := refreshTokenEndpoints[conn.Provider]
	if !supported {
		return tok, false, nil
	}

	clientID, secret := os.Getenv(ep.clientIDEnv), os.Getenv(ep.secretEnv)

	var req *http.Request
	if ep.jsonBody {
		body, _ := json.Marshal(map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": conn.RefreshToken,
			"client_id":     clientID,
			"client_secret": secret,
		})
		req, _ = http.NewRequest(http.MethodPost, ep.tokenURL, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
	} else {
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", conn.RefreshToken)
		if !ep.basicAuth {
			form.Set("client_id", clientID)
			form.Set("client_secret", secret)
		}
		req, _ = http.NewRequest(http.MethodPost, ep.tokenURL, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if ep.basicAuth {
		req.SetBasicAuth(clientID, secret)
	}
	// GitHub answers form-encoded unless JSON is requested explicitly, and it
	// reports refresh failures with a 200 — so ask for JSON and check the body.
	req.Header.Set("Accept", "application/json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return tok, true, err
	}
	if json.Unmarshal(raw, &tok) != nil || tok.AccessToken == "" {
		if tok.ErrorDesc != "" {
			return tok, true, fmt.Errorf("%s token refresh failed: %s", conn.Provider, tok.ErrorDesc)
		}
		return tok, true, fmt.Errorf("%s token refresh returned no access token", conn.Provider)
	}
	return tok, true, nil
}

func refreshConnection(db *gorm.DB, conn *models.IntegrationConnection) (*models.IntegrationConnection, error) {
	tok, supported, err := exchangeRefreshToken(conn)
	if !supported {
		return conn, nil
	}
	if err != nil {
		return nil, err
	}

	// A map update bypasses the model's BeforeSave hook, so encrypt the token
	// values explicitly here. The in-memory conn keeps plaintext for the caller.
	updates := map[string]any{"access_token": cryptobox.Encrypt(tok.AccessToken)}
	conn.AccessToken = tok.AccessToken
	setOAuthExpiry(conn, tok.AccessToken, tok.ExpiresIn)
	if conn.ExpiresAt != nil {
		updates["expires_at"] = *conn.ExpiresAt
	}
	// Providers that rotate refresh tokens return the replacement here; Google
	// commonly omits it and keeps the existing token.
	if tok.RefreshToken != "" {
		conn.RefreshToken = tok.RefreshToken
		updates["refresh_token"] = cryptobox.Encrypt(tok.RefreshToken)
	}
	if db != nil {
		db.Model(&models.IntegrationConnection{}).Where("id = ?", conn.ID).Updates(updates)
	}
	return conn, nil
}
