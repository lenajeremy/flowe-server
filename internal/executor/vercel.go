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

// Vercel REST API.
//
// Four things shape the ops below.
//
//   - **Every path carries its own version.** Vercel versions per endpoint, not
//     per API: deployments are read at v13 but listed at v7 and cancelled at v12,
//     projects are v9 but listed at v10. There is no "current" version to hoist
//     into the base URL, so each op states its own. Guessing a uniform version is
//     how you get a 404 that looks like a missing resource.
//
//   - **Team scoping is a query parameter, not a token property.** A token that
//     can see several teams defaults to the user's personal scope, so a project
//     that plainly exists in the dashboard 404s until `teamId` (or `slug`) is
//     sent. Because that failure is indistinguishable from a typo'd id, teamQuery
//     is applied to every single call and the 404 message says so.
//
//   - **Auth is a personal access token, not OAuth.** Vercel's OAuth-based
//     "connectable account" integration needs an approved entry in their
//     Integrations Console, and the newer Sign in with Vercel issues tokens whose
//     API permissions are, per Vercel's own docs, "currently in private beta". A
//     PAT is the only mechanism that reaches the REST API today. It is a plain
//     Bearer token, and it may carry a user-chosen expiry, so a 401 means expired
//     or revoked and the only fix is pasting a new one.
//
//   - **Redeploy is a create, not a verb on a deployment.** POST /v13/deployments
//     with `deploymentId` inherits every setting from that deployment. `name` is
//     required even then, and without forceNew=1 Vercel deduplicates and hands
//     back the *existing* deployment, which reads as "my redeploy did nothing".
//
// A var, not a const, so tests can point it at a stub server. Nothing in
// production reassigns it.
var vercelAPI = "https://api.vercel.com"

func vercelCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, vercelAPI+path, reader)
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
		return "", fmt.Errorf("vercel request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Vercel nests its message: {"error":{"code":"...","message":"..."}}
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		msg := ""
		if json.Unmarshal(raw, &e) == nil {
			msg = firstNonEmpty(e.Error.Message, e.Message)
			if e.Error.Code != "" {
				msg = fmt.Sprintf("%s (%s)", msg, e.Error.Code)
			}
		}
		if msg == "" {
			msg = truncateStr(string(raw), 300)
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			msg += " — reconnect Vercel; a personal access token is never refreshed, " +
				"so it has expired or been revoked"
		case http.StatusForbidden:
			msg += " — the token's scope does not cover this resource. A token created " +
				"for one team cannot reach another, and a personal-scope token cannot " +
				"reach team projects at all"
		case http.StatusNotFound:
			msg += " — if the resource exists in the dashboard, the team is almost " +
				"certainly missing: set Team ID on this node, or the request runs " +
				"against your personal scope instead"
		case http.StatusTooManyRequests:
			msg += " — Vercel rate-limits per endpoint and per token; deployment " +
				"creation is limited far more tightly than reads"
		}
		return "", fmt.Errorf("Vercel API error (%d): %s", resp.StatusCode, msg)
	}
	// Deletes and cancels can answer with an empty body.
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

func runVercel(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	need := func(label, v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("this operation needs %s", label)
		}
		return nil
	}
	esc := url.PathEscape

	// teamQuery goes on every request. A token belonging to someone in several
	// teams resolves to their personal scope by default, so omitting this turns a
	// team project into a 404 rather than a permission error.
	teamQuery := func(q url.Values) url.Values {
		if q == nil {
			q = url.Values{}
		}
		if v := sub(d.VercelTeamId); v != "" {
			q.Set("teamId", v)
		} else if v := sub(d.VercelTeamSlug); v != "" {
			q.Set("slug", v)
		}
		return q
	}
	withQuery := func(path string, q url.Values) string {
		q = teamQuery(q)
		if len(q) == 0 {
			return path
		}
		return path + "?" + q.Encode()
	}
	limitQuery := func(q url.Values, fallback int) url.Values {
		if q == nil {
			q = url.Values{}
		}
		q.Set("limit", fmt.Sprint(intOr(d.VercelLimit, fallback)))
		return q
	}

	project := func() string { return sub(d.VercelProjectId) }
	deployment := func() string { return sub(d.VercelDeploymentId) }

	// Env vars are addressed by target ("production", "preview", "development"),
	// which Vercel takes as a repeated parameter rather than a CSV.
	envTargets := func() []string {
		raw := sub(d.VercelEnvTarget)
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		var out []string
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	}

	switch d.IntegrationOp {

	// ---- deployments ----
	case "list_deployments":
		q := limitQuery(url.Values{}, 20)
		if v := project(); v != "" {
			q.Set("projectId", v)
		}
		if v := sub(d.VercelTarget); v != "" {
			q.Set("target", v)
		}
		if v := sub(d.VercelState); v != "" {
			// Vercel accepts a comma-separated list here, and rejects unknown
			// values, so pass it through uppercased rather than validating twice.
			q.Set("state", strings.ToUpper(v))
		}
		if v := sub(d.VercelBranch); v != "" {
			q.Set("branch", v)
		}
		if v := sub(d.VercelSha); v != "" {
			q.Set("sha", v)
		}
		raw, err := vercelCall(ctx, token, http.MethodGet, withQuery("/v7/deployments", q), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_deployment":
		// Takes an id OR a deployment URL, which is what a webhook payload and a
		// Slack message both tend to carry.
		if err := need("a deployment ID or URL", deployment()); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodGet,
			withQuery("/v13/deployments/"+esc(deployment()), nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_deployment_events":
		// The build log. Distinct from runtime logs, which are per request and live
		// under the project.
		//
		// follow=1 is deliberately never sent: it turns this endpoint into an
		// infinite stream of live events, which would hang the run until the request
		// timed out rather than returning the log. Same reason limit is never -1.
		// direction is left at its default (forward) so the log reads in order and
		// the tail below is the end of the build, where the failure is.
		if err := need("a deployment ID or URL", deployment()); err != nil {
			return "", err
		}
		q := limitQuery(url.Values{}, 100)
		// Vercel calls the build id "name" on this endpoint.
		if v := sub(d.VercelBuildId); v != "" {
			q.Set("name", v)
		}
		raw, err := vercelCall(ctx, token, http.MethodGet,
			withQuery("/v3/deployments/"+esc(deployment())+"/events", q), nil)
		if err != nil {
			return "", err
		}
		// Build logs are the single largest response this node can return, and a
		// failed build is mostly stack trace. Keep the tail: the error is at the end.
		return tailStr(raw, 12000), nil

	case "get_runtime_logs":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		if err := need("a deployment ID", deployment()); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodGet,
			withQuery("/v1/projects/"+esc(project())+"/deployments/"+esc(deployment())+"/runtime-logs", nil), nil)
		if err != nil {
			return "", err
		}
		return tailStr(raw, 12000), nil

	case "redeploy":
		if err := need("a deployment ID to redeploy from", deployment()); err != nil {
			return "", err
		}
		// name is required even for a redeploy. It is the project name, which the
		// deployment itself knows, so read it back rather than making the user
		// restate something Vercel already has.
		name := sub(d.VercelName)
		if name == "" {
			existing, err := vercelCall(ctx, token, http.MethodGet,
				withQuery("/v13/deployments/"+esc(deployment()), nil), nil)
			if err != nil {
				return "", fmt.Errorf("could not read the deployment to redeploy: %w", err)
			}
			name = firstNonEmpty(jsonField(existing, "name"), jsonField(existing, "project"))
			if name == "" {
				return "", fmt.Errorf("could not read the project name off that deployment — " +
					"set Name on this node")
			}
		}
		body := map[string]any{"deploymentId": deployment(), "name": name}
		if v := sub(d.VercelTarget); v != "" {
			body["target"] = v
		}
		// Without forceNew Vercel deduplicates and returns the deployment that
		// already exists, so a redeploy silently does nothing.
		raw, err := vercelCall(ctx, token, http.MethodPost,
			withQuery("/v13/deployments", url.Values{"forceNew": {"1"}}), body)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 4000), nil

	case "cancel_deployment":
		if err := need("a deployment ID", deployment()); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodPatch,
			withQuery("/v12/deployments/"+esc(deployment())+"/cancel", nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	case "delete_deployment":
		if err := need("a deployment ID", deployment()); err != nil {
			return "", err
		}
		q := url.Values{}
		if v := sub(d.VercelUrl); v != "" {
			q.Set("url", v)
		}
		raw, err := vercelCall(ctx, token, http.MethodDelete,
			withQuery("/v13/deployments/"+esc(deployment()), q), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	case "list_deployment_aliases":
		if err := need("a deployment ID", deployment()); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodGet,
			withQuery("/v2/deployments/"+esc(deployment())+"/aliases", nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 4000), nil

	case "assign_alias":
		if err := need("a deployment ID", deployment()); err != nil {
			return "", err
		}
		if err := need("an alias hostname", sub(d.VercelAlias)); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodPost,
			withQuery("/v2/deployments/"+esc(deployment())+"/aliases", nil),
			map[string]any{"alias": sub(d.VercelAlias)})
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	// ---- projects ----
	case "list_projects":
		q := limitQuery(url.Values{}, 20)
		if v := sub(d.VercelSearch); v != "" {
			q.Set("search", v)
		}
		raw, err := vercelCall(ctx, token, http.MethodGet, withQuery("/v10/projects", q), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_project":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodGet,
			withQuery("/v9/projects/"+esc(project()), nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "update_project":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		if err := need("a JSON body of project settings", sub(d.VercelProjectConfig)); err != nil {
			return "", err
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(sub(d.VercelProjectConfig)), &body); err != nil {
			return "", fmt.Errorf("project settings must be a JSON object: %w", err)
		}
		raw, err := vercelCall(ctx, token, http.MethodPatch,
			withQuery("/v9/projects/"+esc(project()), nil), body)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 4000), nil

	case "promote_deployment":
		if err := need("a project ID", project()); err != nil {
			return "", err
		}
		if err := need("the deployment ID to promote", deployment()); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodPost,
			withQuery("/v10/projects/"+esc(project())+"/promote/"+esc(deployment()), nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	case "rollback_deployment":
		if err := need("a project ID", project()); err != nil {
			return "", err
		}
		if err := need("the previous deployment ID to roll back to", deployment()); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodPost,
			withQuery("/v1/projects/"+esc(project())+"/rollback/"+esc(deployment()), nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	// ---- environment variables ----
	case "list_env_vars":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		// Values come back encrypted unless decrypt=true is asked for, which is
		// deliberately NOT offered here — get_env_var_value is the explicit,
		// separately-approvable way to read one secret.
		raw, err := vercelCall(ctx, token, http.MethodGet,
			withQuery("/v10/projects/"+esc(project())+"/env", nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_env_var_value":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		if err := need("the environment variable's ID (from list_env_vars)", sub(d.VercelEnvVarId)); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodGet,
			withQuery("/v1/projects/"+esc(project())+"/env/"+esc(sub(d.VercelEnvVarId)), nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	case "create_env_var":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		if err := need("the variable's key", sub(d.VercelEnvKey)); err != nil {
			return "", err
		}
		targets := envTargets()
		if len(targets) == 0 {
			return "", fmt.Errorf("this operation needs at least one target — " +
				"production, preview or development")
		}
		body := map[string]any{
			"key":    sub(d.VercelEnvKey),
			"value":  sub(d.VercelEnvValue),
			"target": targets,
			"type":   firstNonEmpty(sub(d.VercelEnvType), "encrypted"),
		}
		if v := sub(d.VercelGitBranch); v != "" {
			body["gitBranch"] = v
		}
		raw, err := vercelCall(ctx, token, http.MethodPost,
			withQuery("/v10/projects/"+esc(project())+"/env", nil), body)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	case "update_env_var":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		if err := need("the environment variable's ID (from list_env_vars)", sub(d.VercelEnvVarId)); err != nil {
			return "", err
		}
		// A PATCH sends only what changed; sending an empty value would blank the
		// variable, so each field is included only when it was actually set.
		body := map[string]any{}
		if v := sub(d.VercelEnvKey); v != "" {
			body["key"] = v
		}
		if v := sub(d.VercelEnvValue); v != "" {
			body["value"] = v
		}
		if targets := envTargets(); len(targets) > 0 {
			body["target"] = targets
		}
		if v := sub(d.VercelEnvType); v != "" {
			body["type"] = v
		}
		if v := sub(d.VercelGitBranch); v != "" {
			body["gitBranch"] = v
		}
		if len(body) == 0 {
			return "", fmt.Errorf("nothing to update — set a value, key, target or type")
		}
		raw, err := vercelCall(ctx, token, http.MethodPatch,
			withQuery("/v9/projects/"+esc(project())+"/env/"+esc(sub(d.VercelEnvVarId)), nil), body)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	case "delete_env_var":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		if err := need("the environment variable's ID (from list_env_vars)", sub(d.VercelEnvVarId)); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodDelete,
			withQuery("/v9/projects/"+esc(project())+"/env/"+esc(sub(d.VercelEnvVarId)), nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	// ---- domains ----
	case "list_domains":
		raw, err := vercelCall(ctx, token, http.MethodGet,
			withQuery("/v5/domains", limitQuery(url.Values{}, 20)), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_domain":
		if err := need("a domain name", sub(d.VercelDomain)); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodGet,
			withQuery("/v5/domains/"+esc(sub(d.VercelDomain)), nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 4000), nil

	case "list_project_domains":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodGet,
			withQuery("/v9/projects/"+esc(project())+"/domains", nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 4000), nil

	case "add_project_domain":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		if err := need("a domain name", sub(d.VercelDomain)); err != nil {
			return "", err
		}
		body := map[string]any{"name": sub(d.VercelDomain)}
		if v := sub(d.VercelGitBranch); v != "" {
			body["gitBranch"] = v
		}
		if v := sub(d.VercelRedirect); v != "" {
			body["redirect"] = v
		}
		raw, err := vercelCall(ctx, token, http.MethodPost,
			withQuery("/v10/projects/"+esc(project())+"/domains", nil), body)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	case "verify_project_domain":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		if err := need("a domain name", sub(d.VercelDomain)); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodPost,
			withQuery("/v9/projects/"+esc(project())+"/domains/"+esc(sub(d.VercelDomain))+"/verify", nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	case "remove_project_domain":
		if err := need("a project ID or name", project()); err != nil {
			return "", err
		}
		if err := need("a domain name", sub(d.VercelDomain)); err != nil {
			return "", err
		}
		raw, err := vercelCall(ctx, token, http.MethodDelete,
			withQuery("/v9/projects/"+esc(project())+"/domains/"+esc(sub(d.VercelDomain)), nil), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	// ---- account ----
	case "list_teams":
		raw, err := vercelCall(ctx, token, http.MethodGet,
			"/v2/teams?limit="+fmt.Sprint(intOr(d.VercelLimit, 20)), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 4000), nil

	case "get_current_user":
		// No team query: this endpoint describes the token's owner, and sending
		// teamId here is meaningless.
		raw, err := vercelCall(ctx, token, http.MethodGet, "/v2/user", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil
	}

	return "", fmt.Errorf("unsupported Vercel operation: %s", d.IntegrationOp)
}
