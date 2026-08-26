package handlers

import (
	"context"
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
	"workflow-ai/server/internal/githubapp"
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
		// A GitHub App with expiring user tokens returns these; a classic OAuth
		// App omits them and its token never expires. Capturing them is what
		// keeps scheduled runs working past the 8-hour user-token lifetime —
		// without them the connection silently dies overnight.
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		if tok.ErrorDesc != "" {
			return nil, fmt.Errorf("github token exchange failed: %s", tok.ErrorDesc)
		}
		return nil, fmt.Errorf("github token exchange returned no access token")
	}

	conn := &models.IntegrationConnection{
		Provider:     "github",
		AccessToken:  tok.AccessToken,
		Scope:        tok.Scope,
		RefreshToken: tok.RefreshToken,
	}
	if tok.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		conn.ExpiresAt = &exp
	}
	// Best-effort: resolve both the display login and immutable account id. The
	// latter lets a github_app_authorization revocation still find this grant
	// after the user renames their GitHub account.
	if login, accountID := githubIdentity(tok.AccessToken); login != "" {
		conn.WorkspaceName = login
		conn.WorkspaceID = accountID
	}
	return conn, nil
}

func githubIdentity(token string) (login, accountID string) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return "", ""
	}
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return "", ""
	}
	if u.ID > 0 {
		accountID = strconv.FormatInt(u.ID, 10)
	}
	return u.Login, accountID
}

func githubResources(token string) ([]integrationResource, error) {
	client := githubapp.NewClient(token, &http.Client{Timeout: 30 * time.Second})
	installations, err := client.ListInstallations(context.Background())
	if err != nil {
		return nil, err
	}
	out := []integrationResource{}
	seen := make(map[string]bool)
	for _, installation := range installations {
		if installation.SuspendedAt != nil {
			continue
		}
		repositories, err := client.ListInstallationRepositories(context.Background(), installation.ID)
		if err != nil {
			return nil, err
		}
		for _, repository := range repositories {
			key := strings.ToLower(repository.FullName)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, integrationResource{ID: repository.FullName, Name: repository.FullName, Type: "repo"})
		}
	}
	return out, nil
}

// githubBranchResources lists a repository's branches, so "only PRs into main"
// is a choice from a list rather than a branch name typed from memory.
func githubBranchResources(token, repo string) ([]integrationResource, error) {
	req, _ := http.NewRequest(http.MethodGet,
		"https://api.github.com/repos/"+repo+"/branches?per_page=100", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var branches []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &branches); err != nil {
		return nil, fmt.Errorf("parse github branches: %w", err)
	}
	out := make([]integrationResource, 0, len(branches))
	for _, b := range branches {
		out = append(out, integrationResource{ID: b.Name, Name: b.Name, Type: "branch"})
	}
	return out, nil
}

// githubCollaboratorResources lists the people associated with a repository,
// for filters like "only PRs opened by…".
//
// Contributors, not collaborators. /collaborators is the more obvious endpoint
// and it 403s ("Resource not accessible by integration") for a GitHub App
// without the Members permission — measured against a real connection, on every
// repository. /contributors needs only repository read, returns actual humans
// who have touched the code, and is the better list to filter by anyway.
func githubCollaboratorResources(token, repo string) ([]integrationResource, error) {
	req, _ := http.NewRequest(http.MethodGet,
		"https://api.github.com/repos/"+repo+"/contributors?per_page=100", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var people []struct {
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(raw, &people); err != nil {
		return nil, fmt.Errorf("parse github contributors: %w", err)
	}
	out := make([]integrationResource, 0, len(people))
	for _, p := range people {
		// The login is what the webhook payload carries, so it has to be the id
		// even when a friendlier display name exists.
		label := p.Login
		if p.Name != "" && p.Name != p.Login {
			label = p.Name + " (" + p.Login + ")"
		}
		out = append(out, integrationResource{ID: p.Login, Name: label, Type: "user"})
	}
	return out, nil
}

// ── GitLab ────────────────────────────────────────────────────

var gitlabAPIBase = "https://gitlab.com/api/v4"

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
	req, _ := http.NewRequest(http.MethodGet, gitlabAPIBase+"/user", nil)
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
	out := []integrationResource{}
	const perPage = 100
	for page := 1; ; page++ {
		req, _ := http.NewRequest(http.MethodGet, gitlabAPIBase+"/projects?membership=true&per_page="+
			strconv.Itoa(perPage)+"&order_by=last_activity_at&page="+strconv.Itoa(page), nil)
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
			return nil, fmt.Errorf("parse gitlab projects page %d: %w", page, err)
		}
		for _, project := range projects {
			out = append(out, integrationResource{
				ID: strconv.FormatInt(project.ID, 10), Name: project.PathWithNamespace, Type: "project",
			})
		}
		if len(projects) < perPage {
			break
		}
	}
	return out, nil
}

func gitlabBranchResources(token, projectID string) ([]integrationResource, error) {
	projectID, err := gitlabNumericID(projectID)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest(http.MethodGet,
		gitlabAPIBase+"/projects/"+projectID+"/repository/branches?per_page=100", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var branches []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &branches); err != nil {
		return nil, fmt.Errorf("parse gitlab branches: %w", err)
	}
	out := make([]integrationResource, 0, len(branches))
	for _, branch := range branches {
		if name := strings.TrimSpace(branch.Name); name != "" {
			out = append(out, integrationResource{ID: name, Name: name, Type: "branch"})
		}
	}
	return out, nil
}

func gitlabMemberResources(token, projectID string) ([]integrationResource, error) {
	projectID, err := gitlabNumericID(projectID)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest(http.MethodGet,
		gitlabAPIBase+"/projects/"+projectID+"/members/all?per_page=100", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	raw, err := doOAuthRequest(req)
	if err != nil {
		return nil, err
	}
	var members []struct {
		Name     string `json:"name"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, fmt.Errorf("parse gitlab members: %w", err)
	}
	out := make([]integrationResource, 0, len(members))
	for _, member := range members {
		username := strings.TrimSpace(member.Username)
		if username == "" {
			continue
		}
		label := strings.TrimSpace(member.Name)
		if label == "" || strings.EqualFold(label, username) {
			label = username
		} else {
			label += " (@" + username + ")"
		}
		out = append(out, integrationResource{ID: username, Name: label, Type: "user"})
	}
	return out, nil
}

func gitlabNumericID(value string) (string, error) {
	value = strings.TrimSpace(value)
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("invalid GitLab project id %q", value)
	}
	return strconv.FormatInt(id, 10), nil
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
