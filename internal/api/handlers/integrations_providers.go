package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
)

// Provider-specific OAuth code exchange and resource listing for the
// integrations added on top of Notion/Linear: GitHub, GitLab, Gmail, Stripe,
// Shopify. The shared flow (connect/callback/state) lives in integrations.go.

// ── GitHub ────────────────────────────────────────────────────

func exchangeGithubCode(code string) (*models.IntegrationConnection, error) {
	form := url.Values{}
	form.Set("client_id", os.Getenv("GITHUB_CLIENT_ID"))
	form.Set("client_secret", os.Getenv("GITHUB_CLIENT_SECRET"))
	form.Set("code", code)
	form.Set("redirect_uri", oauthRedirectURI("github"))

	req, _ := http.NewRequest(http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	// GitHub reports exchange failures with a 200 status, so the error field
	// must be checked explicitly.
	var tok struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		if tok.ErrorDesc != "" {
			return nil, fmt.Errorf("github token exchange failed: %s", tok.ErrorDesc)
		}
		return nil, fmt.Errorf("github token exchange returned no access token")
	}

	conn := &models.IntegrationConnection{
		Provider:    "github",
		AccessToken: tok.AccessToken,
		Scope:       tok.Scope,
	}
	// Best-effort: resolve the account login for display.
	if login := githubLogin(tok.AccessToken); login != "" {
		conn.WorkspaceName = login
	}
	return conn, nil
}

func githubLogin(token string) string {
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return ""
	}
	var u struct {
		Login string `json:"login"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return ""
	}
	return u.Login
}

func githubResources(token string) ([]integrationResource, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/user/repos?per_page=100&sort=updated", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var repos []struct {
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(raw, &repos); err != nil {
		return nil, fmt.Errorf("parse github repos: %w", err)
	}
	out := make([]integrationResource, 0, len(repos))
	for _, r := range repos {
		out = append(out, integrationResource{ID: r.FullName, Name: r.FullName, Type: "repo"})
	}
	return out, nil
}

// ── GitLab ────────────────────────────────────────────────────

func exchangeGitlabCode(code string) (*models.IntegrationConnection, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", oauthRedirectURI("gitlab"))
	form.Set("client_id", os.Getenv("GITLAB_CLIENT_ID"))
	form.Set("client_secret", os.Getenv("GITLAB_CLIENT_SECRET"))

	req, _ := http.NewRequest(http.MethodPost, "https://gitlab.com/oauth/token", strings.NewReader(form.Encode()))
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
		return nil, fmt.Errorf("gitlab token exchange returned no access token")
	}

	conn := &models.IntegrationConnection{
		Provider:     "gitlab",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Scope:        tok.Scope,
	}
	if tok.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		conn.ExpiresAt = &exp
	}
	// Best-effort: resolve the account username for display.
	if username := gitlabUsername(tok.AccessToken); username != "" {
		conn.WorkspaceName = username
	}
	return conn, nil
}

func gitlabUsername(token string) string {
	req, _ := http.NewRequest(http.MethodGet, "https://gitlab.com/api/v4/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return ""
	}
	var u struct {
		Username string `json:"username"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return ""
	}
	return u.Username
}

func gitlabResources(token string) ([]integrationResource, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://gitlab.com/api/v4/projects?membership=true&per_page=100&order_by=last_activity_at", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var projects []struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	}
	if err := json.Unmarshal(raw, &projects); err != nil {
		return nil, fmt.Errorf("parse gitlab projects: %w", err)
	}
	out := make([]integrationResource, 0, len(projects))
	for _, p := range projects {
		out = append(out, integrationResource{ID: strconv.FormatInt(p.ID, 10), Name: p.PathWithNamespace, Type: "project"})
	}
	return out, nil
}

// ── Gmail (Google) ────────────────────────────────────────────

func exchangeGmailCode(code string) (*models.IntegrationConnection, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", oauthRedirectURI("gmail"))
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
		return nil, fmt.Errorf("gmail token exchange returned no access token")
	}

	conn := &models.IntegrationConnection{
		Provider:     "gmail",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Scope:        tok.Scope,
	}
	if tok.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		conn.ExpiresAt = &exp
	}
	// Best-effort: resolve the mailbox address for display.
	if email := gmailAddress(tok.AccessToken); email != "" {
		conn.WorkspaceName = email
	}
	return conn, nil
}

func gmailAddress(token string) string {
	req, _ := http.NewRequest(http.MethodGet, "https://gmail.googleapis.com/gmail/v1/users/me/profile", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return ""
	}
	var p struct {
		EmailAddress string `json:"emailAddress"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return p.EmailAddress
}

func gmailResources(token string) ([]integrationResource, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://gmail.googleapis.com/gmail/v1/users/me/labels", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		Labels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse gmail labels: %w", err)
	}
	out := make([]integrationResource, 0, len(res.Labels))
	for _, l := range res.Labels {
		out = append(out, integrationResource{ID: l.ID, Name: l.Name, Type: "label"})
	}
	return out, nil
}

// ── Stripe (Connect, standard accounts) ───────────────────────

func exchangeStripeCode(code string) (*models.IntegrationConnection, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	// Stripe Connect uses the platform's secret key (sk_…) as client_secret.
	form.Set("client_secret", os.Getenv("STRIPE_CLIENT_SECRET"))

	req, _ := http.NewRequest(http.MethodPost, "https://connect.stripe.com/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		StripeUserID string `json:"stripe_user_id"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("stripe token exchange returned no access token")
	}

	conn := &models.IntegrationConnection{
		Provider:    "stripe",
		AccessToken: tok.AccessToken,
		WorkspaceID: tok.StripeUserID,
	}
	if tok.Scope != "" {
		conn.Scope = tok.Scope
	}
	// Best-effort: resolve the account's display name; fall back to the id.
	conn.WorkspaceName = stripeAccountName(tok.AccessToken)
	if conn.WorkspaceName == "" {
		conn.WorkspaceName = tok.StripeUserID
	}
	return conn, nil
}

func stripeAccountName(token string) string {
	req, _ := http.NewRequest(http.MethodGet, "https://api.stripe.com/v1/account", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return ""
	}
	var acct struct {
		ID       string `json:"id"`
		Settings struct {
			Dashboard struct {
				DisplayName string `json:"display_name"`
			} `json:"dashboard"`
		} `json:"settings"`
		BusinessProfile struct {
			Name string `json:"name"`
		} `json:"business_profile"`
	}
	if json.Unmarshal(raw, &acct) != nil {
		return ""
	}
	if acct.Settings.Dashboard.DisplayName != "" {
		return acct.Settings.Dashboard.DisplayName
	}
	if acct.BusinessProfile.Name != "" {
		return acct.BusinessProfile.Name
	}
	return acct.ID
}

func stripeResources(token string) ([]integrationResource, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.stripe.com/v1/prices?limit=100&active=true&expand[]=data.product", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data []struct {
			ID         string `json:"id"`
			Currency   string `json:"currency"`
			UnitAmount *int64 `json:"unit_amount"`
			Product    struct {
				Name string `json:"name"`
			} `json:"product"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse stripe prices: %w", err)
	}
	out := make([]integrationResource, 0, len(res.Data))
	for _, p := range res.Data {
		name := p.Product.Name
		if name == "" {
			name = p.ID
		}
		if p.UnitAmount != nil {
			name = fmt.Sprintf("%s — %.2f %s", name, float64(*p.UnitAmount)/100, strings.ToUpper(p.Currency))
		}
		out = append(out, integrationResource{ID: p.ID, Name: name, Type: "price"})
	}
	return out, nil
}

// ── Shopify ───────────────────────────────────────────────────

var shopDomainRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\.myshopify\.com$`)

// normalizeShopDomain turns "mystore" or "mystore.myshopify.com" into the full
// *.myshopify.com domain and rejects anything else, so the authorize redirect
// and token exchange can never be pointed at an arbitrary host.
func normalizeShopDomain(shop string) (string, error) {
	shop = strings.ToLower(strings.TrimSpace(shop))
	shop = strings.TrimPrefix(shop, "https://")
	shop = strings.TrimPrefix(shop, "http://")
	shop = strings.TrimSuffix(shop, "/")
	if shop == "" {
		return "", fmt.Errorf("shopify needs a shop domain — pass ?shop=your-store.myshopify.com")
	}
	if !strings.Contains(shop, ".") {
		shop += ".myshopify.com"
	}
	if !shopDomainRe.MatchString(shop) {
		return "", fmt.Errorf("invalid shop domain %q — expected your-store.myshopify.com", shop)
	}
	return shop, nil
}

func exchangeShopifyCode(code, shop string) (*models.IntegrationConnection, error) {
	if shop == "" {
		return nil, fmt.Errorf("shopify callback is missing the shop domain — try connecting again")
	}
	body, _ := json.Marshal(map[string]string{
		"client_id":     os.Getenv("SHOPIFY_CLIENT_ID"),
		"client_secret": os.Getenv("SHOPIFY_CLIENT_SECRET"),
		"code":          code,
	})
	req, _ := http.NewRequest(http.MethodPost, "https://"+shop+"/admin/oauth/access_token", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("shopify token exchange returned no access token")
	}
	return &models.IntegrationConnection{
		Provider:      "shopify",
		AccessToken:   tok.AccessToken,
		Scope:         tok.Scope,
		WorkspaceName: shop,
		WorkspaceID:   shop,
	}, nil
}

func shopifyResources(token, shop string) ([]integrationResource, error) {
	if shop == "" {
		return nil, fmt.Errorf("shopify connection is missing the shop domain — reconnect the store")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://"+shop+"/admin/api/2024-01/products.json?limit=100", nil)
	req.Header.Set("X-Shopify-Access-Token", token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		Products []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"products"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse shopify products: %w", err)
	}
	out := make([]integrationResource, 0, len(res.Products))
	for _, p := range res.Products {
		out = append(out, integrationResource{ID: strconv.FormatInt(p.ID, 10), Name: p.Title, Type: "product"})
	}
	return out, nil
}
