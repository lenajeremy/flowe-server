package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Netlify API v1.
//
// Three things shape the ops below:
//
//   - Netlify has no OAuth scopes and issues no refresh token, so a token is
//     all-or-nothing and never needs renewing. A 401 is terminal: the only fix is
//     reconnecting. Personal access tokens use the same Bearer header, but unlike
//     OAuth tokens they carry a user-chosen expiry, so 401 covers both cases.
//   - Three different ids are not interchangeable: site_id (a site), account_id
//     (a team) and account_slug (that team's URL name). Sending an account id
//     where a slug belongs is a 404, not a validation error, so each op names
//     which one it needs.
//   - Environment variables live on the account, not the site. The site is
//     selected with an optional site_id *query* param on the same account path —
//     omitting it silently reads or writes team-wide variables instead. Because a
//     workflow usually only knows the site, netlifyAccount resolves the account
//     id from the site record rather than making the user go find it.
const netlifyAPI = "https://api.netlify.com/api/v1"

func netlifyCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, netlifyAPI+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("netlify request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		msg := ""
		if json.Unmarshal(raw, &e) == nil {
			msg = firstNonEmpty(e.Message, e.Error)
		}
		if msg == "" {
			msg = truncateStr(string(raw), 300)
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			msg += " — reconnect Netlify; tokens are never refreshed, and a personal " +
				"access token may have passed its expiry date"
		case http.StatusForbidden:
			msg += " — the connected account may not belong to this team, or the team " +
				"uses SAML SSO and the token was created without team access granted"
		case http.StatusNotFound:
			msg += " — check the ID is the right kind: a site ID, an account (team) ID, " +
				"and an account slug are not interchangeable"
		case http.StatusUnprocessableEntity:
			msg += " — Netlify rejected the field values; a name may already be taken"
		case http.StatusTooManyRequests:
			msg += " — Netlify allows 500 requests/minute overall, but only 3 deploys " +
				"per minute and 100 per day"
		}
		return "", fmt.Errorf("Netlify API error (%d): %s", resp.StatusCode, msg)
	}
	// Deletes, rollback and enable/disable answer 204 with no body.
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

// netlifyAccount resolves the team id an operation needs, falling back to reading
// it off the site. Environment variable writes are account-scoped but a workflow
// usually only carries a site id.
func netlifyAccount(ctx context.Context, token, accountID, siteID string) (string, error) {
	if accountID != "" {
		return accountID, nil
	}
	if siteID == "" {
		return "", fmt.Errorf("this operation needs an account (team) ID, or a site ID to " +
			"look it up from — list_accounts returns both the ID and the slug")
	}
	raw, err := netlifyCall(ctx, token, http.MethodGet, "/sites/"+url.PathEscape(siteID), nil)
	if err != nil {
		return "", err
	}
	id := jsonField(raw, "account_id")
	if id == "" {
		return "", fmt.Errorf("could not read an account ID from that site — set the " +
			"account ID explicitly")
	}
	return id, nil
}

func runNetlify(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	need := func(label, v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("this operation needs %s", label)
		}
		return nil
	}
	site := func() string { return sub(d.NetlifySiteId) }
	esc := url.PathEscape

	// page is 1-based and per_page is capped at 100 server-side; clamping here
	// stops a larger value from looking like it worked.
	paging := func(q url.Values) url.Values {
		if n := intOr(d.NetlifyPage, 0); n > 0 {
			q.Set("page", fmt.Sprint(n))
		}
		if n := intOr(d.NetlifyPerPage, 0); n > 0 {
			if n > 100 {
				n = 100
			}
			q.Set("per_page", fmt.Sprint(n))
		}
		return q
	}
	withQuery := func(path string, q url.Values) string {
		if len(q) == 0 {
			return path
		}
		return path + "?" + q.Encode()
	}
	// Every env var path is account-scoped; site_id narrows it to one site.
	envQuery := func() url.Values {
		q := url.Values{}
		if v := site(); v != "" {
			q.Set("site_id", v)
		}
		return q
	}

	switch d.IntegrationOp {
	// ---- sites ----
	case "list_sites":
		q := paging(url.Values{})
		if v := sub(d.NetlifyName); v != "" {
			q.Set("name", v)
		}
		if v := sub(d.NetlifyFilter); v != "" {
			q.Set("filter", v)
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet, withQuery("/sites", q), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_account_sites":
		if err := need("an account slug (the team's URL name, not its ID)", sub(d.NetlifyAccountSlug)); err != nil {
			return "", err
		}
		q := paging(url.Values{})
		if v := sub(d.NetlifyName); v != "" {
			q.Set("name", v)
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet,
			withQuery("/"+esc(sub(d.NetlifyAccountSlug))+"/sites", q), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_site":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet, "/sites/"+esc(site()), nil)

	case "create_site":
		payload, err := netlifySitePayload(d, sub)
		if err != nil {
			return "", err
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("create_site needs at least a name, or a JSON site configuration")
		}
		q := url.Values{}
		if strings.EqualFold(sub(d.NetlifyConfigureDns), "true") {
			q.Set("configure_dns", "true")
		}
		// A slug puts the site on that team; without one it lands on the default team.
		if slug := sub(d.NetlifyAccountSlug); slug != "" {
			return netlifyCall(ctx, token, http.MethodPost,
				withQuery("/"+esc(slug)+"/sites", q), payload)
		}
		return netlifyCall(ctx, token, http.MethodPost, withQuery("/sites", q), payload)

	case "update_site":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		payload, err := netlifySitePayload(d, sub)
		if err != nil {
			return "", err
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("update_site needs at least one field to change")
		}
		return netlifyCall(ctx, token, http.MethodPatch, "/sites/"+esc(site()), payload)

	case "delete_site":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodDelete, "/sites/"+esc(site()), nil)

	case "enable_site":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPut, "/sites/"+esc(site())+"/enable", nil)

	case "disable_site":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		// Netlify requires a reason; there is no default.
		if err := need("a reason for disabling the site", sub(d.NetlifyReason)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPut,
			"/sites/"+esc(site())+"/disable?reason="+url.QueryEscape(sub(d.NetlifyReason)), nil)

	// ---- deploys ----
	case "list_deploys":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet,
			withQuery("/sites/"+esc(site())+"/deploys", paging(url.Values{})), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_deploy":
		if err := need("a deploy ID", sub(d.NetlifyDeployId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet, "/deploys/"+esc(sub(d.NetlifyDeployId)), nil)

	case "create_deploy":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		// The body is a complete manifest of the site: a files hash of path → SHA1.
		// Netlify publishes exactly what is listed, so an empty one wipes the site.
		files := strings.TrimSpace(sub(d.NetlifyDeployFiles))
		if files == "" {
			return "", fmt.Errorf("create_deploy needs a JSON files manifest mapping each " +
				`path to its SHA1, e.g. {"/index.html":"<sha1>"} — Netlify publishes ` +
				"exactly what is listed, so an empty manifest would empty the site. To " +
				"rebuild from the connected git repo instead, use start_build")
		}
		var manifest map[string]any
		if json.Unmarshal([]byte(files), &manifest) != nil {
			return "", fmt.Errorf(`the files manifest must be a JSON object of path → SHA1, ` +
				`e.g. {"/index.html":"da39a3ee5e6b4b0d3255bfef95601890afd80709"}`)
		}
		payload := map[string]any{"files": manifest}
		if strings.EqualFold(sub(d.NetlifyDraft), "true") {
			// A draft deploy gets its own URL and does not become the live site.
			payload["draft"] = true
		}
		q := url.Values{}
		if v := sub(d.NetlifyTitle); v != "" {
			q.Set("title", v)
		}
		return netlifyCall(ctx, token, http.MethodPost,
			withQuery("/sites/"+esc(site())+"/deploys", q), payload)

	case "cancel_deploy":
		// Not under /sites despite Netlify naming it cancelSiteDeploy.
		if err := need("a deploy ID", sub(d.NetlifyDeployId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPost,
			"/deploys/"+esc(sub(d.NetlifyDeployId))+"/cancel", nil)

	case "restore_deploy":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		if err := need("the deploy ID to restore", sub(d.NetlifyDeployId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPost,
			"/sites/"+esc(site())+"/deploys/"+esc(sub(d.NetlifyDeployId))+"/restore", nil)

	case "rollback_site":
		// Rolls back to whatever Netlify considers the previous deploy; it takes no
		// deploy id. To pick a specific one, use restore_deploy.
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPut, "/sites/"+esc(site())+"/rollback", nil)

	case "lock_deploy":
		if err := need("a deploy ID", sub(d.NetlifyDeployId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPost,
			"/deploys/"+esc(sub(d.NetlifyDeployId))+"/lock", nil)

	case "unlock_deploy":
		if err := need("a deploy ID", sub(d.NetlifyDeployId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPost,
			"/deploys/"+esc(sub(d.NetlifyDeployId))+"/unlock", nil)

	case "delete_deploy":
		if err := need("a deploy ID", sub(d.NetlifyDeployId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodDelete,
			"/deploys/"+esc(sub(d.NetlifyDeployId)), nil)

	// ---- builds ----
	case "list_builds":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet,
			withQuery("/sites/"+esc(site())+"/builds", paging(url.Values{})), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_build":
		if err := need("a build ID", sub(d.NetlifyBuildId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet, "/builds/"+esc(sub(d.NetlifyBuildId)), nil)

	case "start_build":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		// This endpoint reads multipart form data, not JSON: every option is a query
		// param and the only body field is an optional zip upload. So: no body.
		q := url.Values{}
		if v := sub(d.NetlifyBranch); v != "" {
			q.Set("branch", v)
		}
		if v := sub(d.NetlifyTitle); v != "" {
			q.Set("title", v)
		}
		if strings.EqualFold(sub(d.NetlifyClearCache), "true") {
			q.Set("clear_cache", "true")
		}
		return netlifyCall(ctx, token, http.MethodPost,
			withQuery("/sites/"+esc(site())+"/builds", q), nil)

	case "get_account_build_status":
		// Netlify hangs this off the bare account id at the API root, not /accounts.
		acct, err := netlifyAccount(ctx, token, sub(d.NetlifyAccountId), site())
		if err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet, "/"+esc(acct)+"/builds/status", nil)

	// ---- environment variables (account-scoped) ----
	case "list_env_vars":
		acct, err := netlifyAccount(ctx, token, sub(d.NetlifyAccountId), site())
		if err != nil {
			return "", err
		}
		q := envQuery()
		if v := sub(d.NetlifyEnvContext); v != "" {
			q.Set("context_name", v)
		}
		if v := sub(d.NetlifyEnvScopes); v != "" {
			q.Set("scope", v)
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet,
			withQuery("/accounts/"+esc(acct)+"/env", q), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_site_env_vars":
		// Read-only shortcut that skips resolving the account id.
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		q := url.Values{}
		if v := sub(d.NetlifyEnvContext); v != "" {
			q.Set("context_name", v)
		}
		if v := sub(d.NetlifyEnvScopes); v != "" {
			q.Set("scope", v)
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet,
			withQuery("/sites/"+esc(site())+"/env", q), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_env_var":
		if err := need("a variable key", sub(d.NetlifyEnvKey)); err != nil {
			return "", err
		}
		acct, err := netlifyAccount(ctx, token, sub(d.NetlifyAccountId), site())
		if err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet,
			withQuery("/accounts/"+esc(acct)+"/env/"+esc(sub(d.NetlifyEnvKey)), envQuery()), nil)

	case "create_env_vars":
		acct, err := netlifyAccount(ctx, token, sub(d.NetlifyAccountId), site())
		if err != nil {
			return "", err
		}
		payload, err := netlifyEnvVarsPayload(d, sub)
		if err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPost,
			withQuery("/accounts/"+esc(acct)+"/env", envQuery()), payload)

	case "update_env_var":
		if err := need("a variable key", sub(d.NetlifyEnvKey)); err != nil {
			return "", err
		}
		acct, err := netlifyAccount(ctx, token, sub(d.NetlifyAccountId), site())
		if err != nil {
			return "", err
		}
		vars, err := netlifyEnvVarsPayload(d, sub)
		if err != nil {
			return "", err
		}
		// PUT replaces the whole variable, values array included.
		return netlifyCall(ctx, token, http.MethodPut,
			withQuery("/accounts/"+esc(acct)+"/env/"+esc(sub(d.NetlifyEnvKey)), envQuery()), vars[0])

	case "set_env_var_value":
		if err := need("a variable key", sub(d.NetlifyEnvKey)); err != nil {
			return "", err
		}
		acct, err := netlifyAccount(ctx, token, sub(d.NetlifyAccountId), site())
		if err != nil {
			return "", err
		}
		ctxName := firstNonEmpty(sub(d.NetlifyEnvContext), "all")
		payload := map[string]any{"context": ctxName, "value": sub(d.NetlifyEnvValue)}
		if ctxName == "branch" {
			// A branch context is meaningless without naming the branch.
			branch := firstNonEmpty(sub(d.NetlifyEnvContextParameter), sub(d.NetlifyBranch))
			if branch == "" {
				return "", fmt.Errorf(`the "branch" context also needs the branch name`)
			}
			payload["context_parameter"] = branch
		}
		return netlifyCall(ctx, token, http.MethodPatch,
			withQuery("/accounts/"+esc(acct)+"/env/"+esc(sub(d.NetlifyEnvKey)), envQuery()), payload)

	case "delete_env_var":
		if err := need("a variable key", sub(d.NetlifyEnvKey)); err != nil {
			return "", err
		}
		acct, err := netlifyAccount(ctx, token, sub(d.NetlifyAccountId), site())
		if err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodDelete,
			withQuery("/accounts/"+esc(acct)+"/env/"+esc(sub(d.NetlifyEnvKey)), envQuery()), nil)

	case "delete_env_var_value":
		if err := need("a variable key and the value ID", sub(d.NetlifyEnvKey)+sub(d.NetlifyEnvValueId)); err != nil {
			return "", err
		}
		if sub(d.NetlifyEnvValueId) == "" {
			return "", fmt.Errorf("delete_env_var_value needs the value ID, which get_env_var " +
				"returns for each context")
		}
		acct, err := netlifyAccount(ctx, token, sub(d.NetlifyAccountId), site())
		if err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodDelete,
			withQuery("/accounts/"+esc(acct)+"/env/"+esc(sub(d.NetlifyEnvKey))+
				"/value/"+esc(sub(d.NetlifyEnvValueId)), envQuery()), nil)

	// ---- forms & submissions ----
	case "list_forms":
		// There is no endpoint for a single form; list and filter.
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet, "/sites/"+esc(site())+"/forms", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "delete_form":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		if err := need("a form ID", sub(d.NetlifyFormId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodDelete,
			"/sites/"+esc(site())+"/forms/"+esc(sub(d.NetlifyFormId)), nil)

	case "list_site_submissions":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet,
			withQuery("/sites/"+esc(site())+"/submissions", paging(url.Values{})), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_form_submissions":
		if err := need("a form ID", sub(d.NetlifyFormId)); err != nil {
			return "", err
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet,
			withQuery("/forms/"+esc(sub(d.NetlifyFormId))+"/submissions", paging(url.Values{})), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_submission":
		if err := need("a submission ID", sub(d.NetlifySubmissionId)); err != nil {
			return "", err
		}
		q := paging(url.Values{})
		if v := sub(d.NetlifyQuery); v != "" {
			q.Set("query", v)
		}
		return netlifyCall(ctx, token, http.MethodGet,
			withQuery("/submissions/"+esc(sub(d.NetlifySubmissionId)), q), nil)

	case "delete_submission":
		if err := need("a submission ID", sub(d.NetlifySubmissionId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodDelete,
			"/submissions/"+esc(sub(d.NetlifySubmissionId)), nil)

	// ---- DNS ----
	case "list_dns_zones":
		q := url.Values{}
		if v := sub(d.NetlifyAccountSlug); v != "" {
			q.Set("account_slug", v)
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet, withQuery("/dns_zones", q), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_dns_zone":
		if err := need("a DNS zone ID", sub(d.NetlifyZoneId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet, "/dns_zones/"+esc(sub(d.NetlifyZoneId)), nil)

	case "create_dns_zone":
		if err := need("a domain name", sub(d.NetlifyName)); err != nil {
			return "", err
		}
		payload := map[string]any{"name": sub(d.NetlifyName)}
		// The zone belongs to a team, named by slug here rather than by id.
		if v := sub(d.NetlifyAccountSlug); v != "" {
			payload["account_slug"] = v
		}
		if v := site(); v != "" {
			payload["site_id"] = v
		}
		return netlifyCall(ctx, token, http.MethodPost, "/dns_zones", payload)

	case "delete_dns_zone":
		if err := need("a DNS zone ID", sub(d.NetlifyZoneId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodDelete, "/dns_zones/"+esc(sub(d.NetlifyZoneId)), nil)

	case "list_dns_records":
		if err := need("a DNS zone ID", sub(d.NetlifyZoneId)); err != nil {
			return "", err
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet,
			"/dns_zones/"+esc(sub(d.NetlifyZoneId))+"/dns_records", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_dns_record":
		if err := need("a zone ID and a record ID", sub(d.NetlifyZoneId)+sub(d.NetlifyRecordId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet,
			"/dns_zones/"+esc(sub(d.NetlifyZoneId))+"/dns_records/"+esc(sub(d.NetlifyRecordId)), nil)

	case "create_dns_record":
		if err := need("a DNS zone ID", sub(d.NetlifyZoneId)); err != nil {
			return "", err
		}
		payload, err := netlifyDnsRecordPayload(d, sub)
		if err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPost,
			"/dns_zones/"+esc(sub(d.NetlifyZoneId))+"/dns_records", payload)

	case "delete_dns_record":
		// Netlify has no record update; changing one means delete then create.
		if err := need("a zone ID and a record ID", sub(d.NetlifyZoneId)+sub(d.NetlifyRecordId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodDelete,
			"/dns_zones/"+esc(sub(d.NetlifyZoneId))+"/dns_records/"+esc(sub(d.NetlifyRecordId)), nil)

	case "get_site_dns":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet, "/sites/"+esc(site())+"/dns", nil)

	case "configure_site_dns":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPut, "/sites/"+esc(site())+"/dns", nil)

	// ---- build hooks ----
	case "list_build_hooks":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet, "/sites/"+esc(site())+"/build_hooks", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_build_hook":
		if err := need("a site ID and a build hook ID", site()+sub(d.NetlifyBuildHookId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet,
			"/sites/"+esc(site())+"/build_hooks/"+esc(sub(d.NetlifyBuildHookId)), nil)

	case "create_build_hook":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		// The response carries a url that triggers a build with no authentication.
		payload := map[string]any{"title": firstNonEmpty(sub(d.NetlifyTitle), "Fernary")}
		if v := sub(d.NetlifyBranch); v != "" {
			payload["branch"] = v
		}
		return netlifyCall(ctx, token, http.MethodPost,
			"/sites/"+esc(site())+"/build_hooks", payload)

	case "update_build_hook":
		if err := need("a site ID and a build hook ID", site()+sub(d.NetlifyBuildHookId)); err != nil {
			return "", err
		}
		payload := map[string]any{}
		if v := sub(d.NetlifyTitle); v != "" {
			payload["title"] = v
		}
		if v := sub(d.NetlifyBranch); v != "" {
			payload["branch"] = v
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("update_build_hook needs a new title or branch")
		}
		return netlifyCall(ctx, token, http.MethodPut,
			"/sites/"+esc(site())+"/build_hooks/"+esc(sub(d.NetlifyBuildHookId)), payload)

	case "delete_build_hook":
		if err := need("a site ID and a build hook ID", site()+sub(d.NetlifyBuildHookId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodDelete,
			"/sites/"+esc(site())+"/build_hooks/"+esc(sub(d.NetlifyBuildHookId)), nil)

	// ---- hooks (outgoing notifications) ----
	case "list_hooks":
		// site_id is a query param here, not a path segment.
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet,
			"/hooks?site_id="+url.QueryEscape(site()), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_hook":
		if err := need("a hook ID", sub(d.NetlifyHookId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet, "/hooks/"+esc(sub(d.NetlifyHookId)), nil)

	case "create_hook":
		if err := need("a site ID", site()); err != nil {
			return "", err
		}
		if err := need("an event to listen for, such as deploy_created", sub(d.NetlifyHookEvent)); err != nil {
			return "", err
		}
		payload, err := netlifyHookPayload(d, sub)
		if err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPost,
			"/hooks?site_id="+url.QueryEscape(site()), payload)

	case "update_hook":
		if err := need("a hook ID", sub(d.NetlifyHookId)); err != nil {
			return "", err
		}
		payload, err := netlifyHookPayload(d, sub)
		if err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPut, "/hooks/"+esc(sub(d.NetlifyHookId)), payload)

	case "delete_hook":
		if err := need("a hook ID", sub(d.NetlifyHookId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodDelete, "/hooks/"+esc(sub(d.NetlifyHookId)), nil)

	case "enable_hook":
		if err := need("a hook ID", sub(d.NetlifyHookId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodPost,
			"/hooks/"+esc(sub(d.NetlifyHookId))+"/enable", nil)

	case "list_hook_types":
		raw, err := netlifyCall(ctx, token, http.MethodGet, "/hooks/types", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	// ---- deploy keys ----
	case "list_deploy_keys":
		raw, err := netlifyCall(ctx, token, http.MethodGet, "/deploy_keys", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_deploy_key":
		if err := need("a deploy key ID", sub(d.NetlifyKeyId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet, "/deploy_keys/"+esc(sub(d.NetlifyKeyId)), nil)

	case "create_deploy_key":
		// Netlify generates the keypair; there is nothing to send.
		return netlifyCall(ctx, token, http.MethodPost, "/deploy_keys", nil)

	case "delete_deploy_key":
		if err := need("a deploy key ID", sub(d.NetlifyKeyId)); err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodDelete, "/deploy_keys/"+esc(sub(d.NetlifyKeyId)), nil)

	// ---- account & user ----
	case "get_current_user":
		return netlifyCall(ctx, token, http.MethodGet, "/user", nil)

	case "list_accounts":
		// Returns both the id and the slug, which the other ops need separately.
		raw, err := netlifyCall(ctx, token, http.MethodGet, "/accounts", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_account":
		acct, err := netlifyAccount(ctx, token, sub(d.NetlifyAccountId), site())
		if err != nil {
			return "", err
		}
		return netlifyCall(ctx, token, http.MethodGet, "/accounts/"+esc(acct), nil)

	case "list_account_members":
		if err := need("an account slug (the team's URL name, not its ID)", sub(d.NetlifyAccountSlug)); err != nil {
			return "", err
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet,
			"/"+esc(sub(d.NetlifyAccountSlug))+"/members", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_audit_events":
		acct, err := netlifyAccount(ctx, token, sub(d.NetlifyAccountId), site())
		if err != nil {
			return "", err
		}
		q := paging(url.Values{})
		if v := sub(d.NetlifyQuery); v != "" {
			q.Set("query", v)
		}
		if v := sub(d.NetlifyLogType); v != "" {
			q.Set("log_type", v)
		}
		raw, err := netlifyCall(ctx, token, http.MethodGet,
			withQuery("/accounts/"+esc(acct)+"/audit", q), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "":
		return "", fmt.Errorf("no Netlify operation selected")
	}
	return "", fmt.Errorf("unsupported Netlify operation: %s", d.IntegrationOp)
}

// netlifySitePayload assembles a create or update body. Only provided fields are
// sent so an update does not clear an untouched setting.
func netlifySitePayload(d FlowNodeData, sub func(string) string) (map[string]any, error) {
	payload := map[string]any{}
	// The raw config goes in first so the named fields below win over it.
	if cfg := strings.TrimSpace(sub(d.NetlifySiteConfig)); cfg != "" {
		if json.Unmarshal([]byte(cfg), &payload) != nil {
			return nil, fmt.Errorf(`the site configuration must be a JSON object, ` +
				`e.g. {"build_settings":{"cmd":"npm run build","dir":"dist"}}`)
		}
	}
	if v := sub(d.NetlifyName); v != "" {
		payload["name"] = v
	}
	if v := sub(d.NetlifyCustomDomain); v != "" {
		payload["custom_domain"] = v
	}
	if v := strings.TrimSpace(sub(d.NetlifyRepo)); v != "" {
		var repo map[string]any
		if json.Unmarshal([]byte(v), &repo) != nil {
			return nil, fmt.Errorf(`the repo settings must be a JSON object, e.g. ` +
				`{"provider":"github","repo_path":"owner/name","repo_branch":"main",` +
				`"cmd":"npm run build","dir":"dist"}`)
		}
		payload["repo"] = repo
	}
	return payload, nil
}

// netlifyEnvVarsPayload builds the variable list shared by create and update.
// Netlify models a variable as a key plus one value per deploy context, so a
// single key/value pair still has to be nested.
func netlifyEnvVarsPayload(d FlowNodeData, sub func(string) string) ([]map[string]any, error) {
	if raw := strings.TrimSpace(sub(d.NetlifyEnvVarsJson)); raw != "" {
		var vars []map[string]any
		if json.Unmarshal([]byte(raw), &vars) == nil && len(vars) > 0 {
			return vars, nil
		}
		// A single object is a forgiving alternative to a one-element array.
		var one map[string]any
		if json.Unmarshal([]byte(raw), &one) == nil && len(one) > 0 {
			return []map[string]any{one}, nil
		}
		return nil, fmt.Errorf(`the variables JSON must be an array of objects, e.g. ` +
			`[{"key":"API_URL","values":[{"value":"https://x.dev","context":"all"}]}]`)
	}

	key := sub(d.NetlifyEnvKey)
	if key == "" {
		return nil, fmt.Errorf("this operation needs a variable key, or a JSON array of variables")
	}
	value := map[string]any{
		"value":   sub(d.NetlifyEnvValue),
		"context": firstNonEmpty(sub(d.NetlifyEnvContext), "all"),
	}
	if value["context"] == "branch" {
		branch := firstNonEmpty(sub(d.NetlifyEnvContextParameter), sub(d.NetlifyBranch))
		if branch == "" {
			return nil, fmt.Errorf(`the "branch" context also needs the branch name`)
		}
		value["context_parameter"] = branch
	}
	one := map[string]any{"key": key, "values": []any{value}}
	if scopes := splitCSV(sub(d.NetlifyEnvScopes)); len(scopes) > 0 {
		// Scopes are a Pro-plan feature; sending them on a lower plan is rejected.
		one["scopes"] = scopes
	}
	if strings.EqualFold(sub(d.NetlifyEnvIsSecret), "true") {
		// A secret value cannot be read back through the API afterwards.
		one["is_secret"] = true
	}
	return []map[string]any{one}, nil
}

// netlifyDnsRecordPayload builds a record body. The numeric fields only apply to
// some record types (priority to MX, port and weight to SRV, flag and tag to CAA),
// so each is sent only when given.
func netlifyDnsRecordPayload(d FlowNodeData, sub func(string) string) (map[string]any, error) {
	kind := strings.ToUpper(sub(d.NetlifyRecordType))
	if kind == "" {
		return nil, fmt.Errorf("create_dns_record needs a record type, such as A, AAAA, " +
			"CNAME, MX, TXT, NS, SRV or CAA")
	}
	if sub(d.NetlifyHostname) == "" {
		return nil, fmt.Errorf("create_dns_record needs a hostname, such as www.example.com")
	}
	if sub(d.NetlifyRecordValue) == "" {
		return nil, fmt.Errorf("create_dns_record needs a value — the target the record points at")
	}
	payload := map[string]any{
		"type":     kind,
		"hostname": sub(d.NetlifyHostname),
		"value":    sub(d.NetlifyRecordValue),
	}
	numeric := map[string]string{
		"ttl":      sub(d.NetlifyTtl),
		"priority": sub(d.NetlifyPriority),
		"weight":   sub(d.NetlifyWeight),
		"port":     sub(d.NetlifyPort),
		"flag":     sub(d.NetlifyFlag),
	}
	for field, raw := range numeric {
		if raw == "" {
			continue
		}
		n, err := atoiSafe(raw)
		if err != nil {
			return nil, fmt.Errorf("%s must be a whole number", field)
		}
		payload[field] = n
	}
	if v := sub(d.NetlifyTag); v != "" {
		payload["tag"] = v
	}
	return payload, nil
}

// netlifyHookPayload builds a notification body. Netlify keeps the destination in
// a free-form data object whose shape depends on the hook type — a url hook wants
// {"url":…}, an email hook wants {"email":…} — so a raw JSON escape hatch sits
// alongside the common url case.
func netlifyHookPayload(d FlowNodeData, sub func(string) string) (map[string]any, error) {
	payload := map[string]any{}
	if v := sub(d.NetlifyHookEvent); v != "" {
		payload["event"] = v
	}
	kind := firstNonEmpty(sub(d.NetlifyHookType), "url")
	payload["type"] = kind

	if raw := strings.TrimSpace(sub(d.NetlifyHookData)); raw != "" {
		var data map[string]any
		if json.Unmarshal([]byte(raw), &data) != nil {
			return nil, fmt.Errorf(`the hook data must be a JSON object, e.g. ` +
				`{"url":"https://example.com/webhook"}`)
		}
		payload["data"] = data
		return payload, nil
	}
	if kind == "url" {
		if sub(d.NetlifyUrl) == "" {
			return nil, fmt.Errorf("a url hook needs the URL Netlify should POST to")
		}
		payload["data"] = map[string]any{"url": sub(d.NetlifyUrl)}
		return payload, nil
	}
	return nil, fmt.Errorf("a %s hook needs its settings as JSON in the hook data field", kind)
}
