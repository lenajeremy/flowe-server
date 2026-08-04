package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

// Supabase Management API v1 — the control plane for projects, not the data
// plane. Reading rows out of a table is PostgREST on the project's own host and
// is not reachable from here.
//
// Four things shape the ops below:
//
//   - A personal access token (sbp_…) and an OAuth access token are both sent as
//     Bearer, so the same call helper serves either credential. What differs is
//     reach: a PAT inherits everything its owner can see, while an OAuth token is
//     limited to the scopes the app was granted, which is why 403 is a scope
//     problem far more often than a permission problem.
//   - Endpoints key off the project ref — 20 lowercase letters — and not the
//     project's UUID id. Passing the id returns an opaque 404, so the ref is
//     validated here instead.
//   - The SQL endpoint runs whatever it is given as the database owner. It is
//     split into two ops so the destructive one has to be chosen on purpose.
//   - Templates are substituted as raw text. SQL built by concatenating a
//     template is injectable, so both SQL ops accept a parameters array and the
//     errors point at it.

const supabaseAPI = "https://api.supabase.com/v1"

// supabaseRefLen is the documented length of a project ref.
const supabaseRefLen = 20

// supabaseSecretsMax is the API's cap on secrets per bulk create.
const supabaseSecretsMax = 100

func supabaseCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, supabaseAPI+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return supabaseDo(req)
}

func supabaseDo(req *http.Request) (string, error) {
	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("supabase request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", supabaseError(resp.StatusCode, raw)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

// supabaseError unpacks the {"message":…} envelope. Validation failures return a
// list of messages under the same key, so both shapes are tried.
func supabaseError(status int, raw []byte) error {
	msg := ""
	var one struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if json.Unmarshal(raw, &one) == nil {
		msg = firstNonEmpty(one.Message, one.Desc, one.Error)
	}
	if msg == "" {
		var many struct {
			Message []string `json:"message"`
		}
		if json.Unmarshal(raw, &many) == nil && len(many.Message) > 0 {
			msg = strings.Join(many.Message, "; ")
		}
	}
	if msg == "" {
		msg = truncateStr(string(raw), 300)
	}

	switch status {
	case http.StatusUnauthorized:
		msg += " — reconnect Supabase; a personal access token stops working the moment it is rotated or revoked"
	case http.StatusForbidden:
		msg += " — the connection is missing the OAuth scope this endpoint needs " +
			"(database, edge_functions, secrets, auth, environment, domains and rest are separate grants)"
	case http.StatusPaymentRequired:
		msg += " — this endpoint needs a paid plan on the project's organization"
	case http.StatusNotFound:
		msg += " — check the project ref is the 20-letter ID from the project URL, not the project name or its UUID"
	case http.StatusTooManyRequests:
		msg += " — the Management API allows 120 requests a minute per user; " +
			"log, analytics, domain and database-metadata endpoints are capped far lower"
	}
	return fmt.Errorf("Supabase Management API error (%d): %s", status, msg)
}

// supabaseRef validates a project ref before it reaches a URL. The two values
// people paste instead — the dashboard URL and the project's UUID — both fail as
// a bare 404, so each gets named.
func supabaseRef(v string) (string, error) {
	v = strings.TrimSpace(v)
	// A pasted dashboard URL is unambiguous, so take the last segment rather than
	// making the user re-copy it.
	if i := strings.LastIndex(v, "/"); i >= 0 {
		v = v[i+1:]
	}
	if v == "" {
		return "", fmt.Errorf("this operation needs a project ref — the 20-letter ID in " +
			"https://supabase.com/dashboard/project/<ref>")
	}
	if strings.Count(v, "-") == 4 {
		return "", fmt.Errorf("that is a project UUID, not a project ref — " +
			"use the 20-letter ref from the project URL")
	}
	if len(v) != supabaseRefLen {
		return "", fmt.Errorf("a project ref is exactly %d lowercase letters, got %d characters",
			supabaseRefLen, len(v))
	}
	for _, r := range v {
		if r < 'a' || r > 'z' {
			return "", fmt.Errorf("a project ref is %d lowercase letters with no digits or dashes — "+
				"check the project URL", supabaseRefLen)
		}
	}
	return v, nil
}

// supabaseJSONObject parses a passthrough object for endpoints whose body has
// too many fields to surface individually.
func supabaseJSONObject(raw, label string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil, fmt.Errorf("%s must be a JSON object, e.g. {\"site_url\":\"https://example.com\"}", label)
	}
	return m, nil
}

// supabaseSQLParams parses the bind values for a parameterised statement.
func supabaseSQLParams(raw string) ([]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []any
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil, fmt.Errorf(`SQL parameters must be a JSON array matching $1, $2 … in the statement, ` +
			`e.g. ["acme", 42]`)
	}
	return out, nil
}

// supabaseSecretPairs accepts either a flat object or the API's array form and
// normalises to the array the API wants. Both the count cap and the reserved
// prefix are checked here so the error names the rule instead of relaying a 422.
func supabaseSecretPairs(raw string) ([]map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf(`this operation needs the secrets to set, e.g. {"OPENAI_API_KEY":"sk-…"}`)
	}

	pairs := []map[string]string{}
	var flat map[string]string
	if json.Unmarshal([]byte(raw), &flat) == nil {
		for k, v := range flat {
			pairs = append(pairs, map[string]string{"name": k, "value": v})
		}
	} else {
		var arr []map[string]string
		if json.Unmarshal([]byte(raw), &arr) != nil {
			return nil, fmt.Errorf(`secrets must be a JSON object like {"NAME":"value"} ` +
				`or an array like [{"name":"NAME","value":"value"}]`)
		}
		for _, p := range arr {
			if p["name"] == "" {
				return nil, fmt.Errorf("every secret in the array needs a name")
			}
			pairs = append(pairs, map[string]string{"name": p["name"], "value": p["value"]})
		}
	}

	if len(pairs) == 0 {
		return nil, fmt.Errorf("no secrets to set")
	}
	if len(pairs) > supabaseSecretsMax {
		return nil, fmt.Errorf("Supabase sets at most %d secrets per request, got %d",
			supabaseSecretsMax, len(pairs))
	}
	for _, p := range pairs {
		if strings.HasPrefix(p["name"], "SUPABASE_") {
			return nil, fmt.Errorf("%q is rejected: Supabase reserves the SUPABASE_ prefix for the "+
				"variables it injects into every function", p["name"])
		}
	}
	return pairs, nil
}

// supabaseDeploy sends the multipart body the deploy endpoint requires: the
// source as a file part, and the metadata as its own JSON part.
func supabaseDeploy(ctx context.Context, token, path, entrypoint, source string, metadata map[string]any) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	meta, _ := json.Marshal(metadata)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="metadata"`)
	h.Set("Content-Type", "application/json")
	part, err := w.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(meta); err != nil {
		return "", err
	}

	file, err := w.CreateFormFile("file", entrypoint)
	if err != nil {
		return "", err
	}
	if _, err := file.Write([]byte(source)); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, supabaseAPI+path, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", w.FormDataContentType())
	return supabaseDo(req)
}

func runSupabase(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	ref := func() (string, error) { return supabaseRef(sub(d.SupabaseProjectRef)) }
	need := func(label, v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("this operation needs %s", label)
		}
		return nil
	}
	// Branch endpoints take a branch ref, falling back to the project ref so
	// operating on the default branch does not need the field filled twice.
	branch := func() (string, error) {
		if v := strings.TrimSpace(sub(d.SupabaseBranchRef)); v != "" {
			return supabaseRef(v)
		}
		return ref()
	}
	flag := func(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "true") }

	switch d.IntegrationOp {
	// ---- projects ----
	case "list_projects":
		raw, err := supabaseCall(ctx, token, http.MethodGet, "/projects", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_project":
		r, err := ref()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodGet, "/projects/"+r, nil)

	case "get_project_health":
		r, err := ref()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/health", nil)

	case "list_regions":
		return supabaseCall(ctx, token, http.MethodGet, "/projects/available-regions", nil)

	case "create_project":
		if err := need("a project name", sub(d.SupabaseName)); err != nil {
			return "", err
		}
		if err := need("an organization slug", sub(d.SupabaseOrgSlug)); err != nil {
			return "", err
		}
		// Supabase never shows the database password again, and there is no
		// endpoint that returns it, so a blank one would strand the project.
		if err := need("a database password — it cannot be recovered later, only reset",
			sub(d.SupabaseDbPass)); err != nil {
			return "", err
		}
		payload := map[string]any{
			"name":              sub(d.SupabaseName),
			"organization_slug": sub(d.SupabaseOrgSlug),
			"db_pass":           sub(d.SupabaseDbPass),
		}
		if v := sub(d.SupabaseRegion); v != "" {
			payload["region"] = v
		}
		if v := sub(d.SupabaseInstanceSize); v != "" {
			payload["desired_instance_size"] = v
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects", payload)

	case "delete_project":
		r, err := ref()
		if err != nil {
			return "", err
		}
		// Deletion takes the database, storage objects and backups with it, so the
		// ref has to be typed a second time rather than clicked once.
		if strings.TrimSpace(sub(d.SupabaseConfirmDelete)) != r {
			return "", fmt.Errorf("delete_project destroys the database, its backups and all storage "+
				"objects, and cannot be undone — type %q into the confirmation field to proceed", r)
		}
		return supabaseCall(ctx, token, http.MethodDelete, "/projects/"+r, nil)

	case "pause_project":
		r, err := ref()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/pause", nil)

	case "restore_project":
		r, err := ref()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/restore", nil)

	case "restart_project":
		r, err := ref()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/restart", nil)

	// ---- project API keys ----
	case "list_api_keys":
		r, err := ref()
		if err != nil {
			return "", err
		}
		path := "/projects/" + r + "/api-keys"
		// Without reveal the secret keys come back masked, which is the right
		// default for something a workflow will log.
		if flag(sub(d.SupabaseRevealKeys)) {
			path += "?reveal=true"
		}
		return supabaseCall(ctx, token, http.MethodGet, path, nil)

	case "create_api_key":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a key name", sub(d.SupabaseName)); err != nil {
			return "", err
		}
		kind := firstNonEmpty(strings.ToLower(sub(d.SupabaseApiKeyType)), "publishable")
		if kind != "publishable" && kind != "secret" {
			return "", fmt.Errorf(`key type must be "publishable" (safe in a browser) or "secret" ` +
				`(bypasses row level security)`)
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/api-keys",
			map[string]any{"type": kind, "name": sub(d.SupabaseName)})

	case "delete_api_key":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("an API key ID", sub(d.SupabaseApiKeyId)); err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodDelete,
			"/projects/"+r+"/api-keys/"+url.PathEscape(sub(d.SupabaseApiKeyId)), nil)

	// ---- organizations ----
	case "list_organizations":
		return supabaseCall(ctx, token, http.MethodGet, "/organizations", nil)

	case "get_organization":
		if err := need("an organization slug", sub(d.SupabaseOrgSlug)); err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodGet,
			"/organizations/"+url.PathEscape(sub(d.SupabaseOrgSlug)), nil)

	case "list_organization_projects":
		if err := need("an organization slug", sub(d.SupabaseOrgSlug)); err != nil {
			return "", err
		}
		raw, err := supabaseCall(ctx, token, http.MethodGet,
			"/organizations/"+url.PathEscape(sub(d.SupabaseOrgSlug))+"/projects", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_organization_members":
		if err := need("an organization slug", sub(d.SupabaseOrgSlug)); err != nil {
			return "", err
		}
		raw, err := supabaseCall(ctx, token, http.MethodGet,
			"/organizations/"+url.PathEscape(sub(d.SupabaseOrgSlug))+"/members", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	// ---- database: SQL ----
	case "run_sql_read_only":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a SQL statement", sub(d.SupabaseSql)); err != nil {
			return "", err
		}
		params, err := supabaseSQLParams(sub(d.SupabaseSqlParams))
		if err != nil {
			return "", err
		}
		payload := map[string]any{"query": sub(d.SupabaseSql)}
		if params != nil {
			payload["parameters"] = params
		}
		// This is a different endpoint from run_sql, not a flag on it: Postgres
		// executes the statement as supabase_read_only_user, so a write fails in
		// the database rather than being filtered by us.
		raw, err := supabaseCall(ctx, token, http.MethodPost,
			"/projects/"+r+"/database/query/read-only", payload)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "run_sql":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a SQL statement", sub(d.SupabaseSql)); err != nil {
			return "", err
		}
		if !flag(sub(d.SupabaseAllowWrite)) {
			return "", fmt.Errorf("run_sql executes arbitrary SQL on the live database as the owner — " +
				"DROP, DELETE and ALTER all succeed and nothing is recorded in migration history. " +
				`Switch to run_sql_read_only, or turn on "allow writes" to confirm`)
		}
		params, err := supabaseSQLParams(sub(d.SupabaseSqlParams))
		if err != nil {
			return "", err
		}
		payload := map[string]any{"query": sub(d.SupabaseSql)}
		if params != nil {
			payload["parameters"] = params
		}
		raw, err := supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/database/query", payload)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "get_database_metadata":
		r, err := ref()
		if err != nil {
			return "", err
		}
		raw, err := supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/database/context", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	// ---- database: migrations ----
	case "list_migrations":
		r, err := ref()
		if err != nil {
			return "", err
		}
		raw, err := supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/database/migrations", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "apply_migration":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("the migration SQL", sub(d.SupabaseSql)); err != nil {
			return "", err
		}
		payload := map[string]any{"query": sub(d.SupabaseSql)}
		// Unattended runs otherwise leave unnamed rows in the history table, so an
		// omitted name gets the UTC stamp the Supabase CLI would have used.
		payload["name"] = firstNonEmpty(sub(d.SupabaseMigrationName),
			clockNow().UTC().Format("20060102150405"))
		if v := sub(d.SupabaseRollbackSql); v != "" {
			payload["rollback"] = v
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/database/migrations", payload)

	case "rollback_migrations":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("the version to roll back to", sub(d.SupabaseMigrationVersion)); err != nil {
			return "", err
		}
		// This reverts every migration at or above the version and deletes their
		// history rows, so it is gated like run_sql.
		if !flag(sub(d.SupabaseAllowWrite)) {
			return "", fmt.Errorf("rollback_migrations runs the down-migration for every version at or "+
				"above %s and removes them from history — turn on \"allow writes\" to confirm",
				sub(d.SupabaseMigrationVersion))
		}
		return supabaseCall(ctx, token, http.MethodDelete,
			"/projects/"+r+"/database/migrations?gte="+url.QueryEscape(sub(d.SupabaseMigrationVersion)), nil)

	// ---- database: backups ----
	case "list_backups":
		r, err := ref()
		if err != nil {
			return "", err
		}
		raw, err := supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/database/backups", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "restore_pitr":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a recovery target as a unix timestamp in seconds",
			sub(d.SupabaseRecoveryTimeUnix)); err != nil {
			return "", err
		}
		target, err := atoiSafe(sub(d.SupabaseRecoveryTimeUnix))
		if err != nil {
			return "", fmt.Errorf("the recovery target must be a unix timestamp in seconds, not a date string")
		}
		if !flag(sub(d.SupabaseAllowWrite)) {
			return "", fmt.Errorf("restore_pitr rewinds the database and discards everything written " +
				"after the recovery target — turn on \"allow writes\" to confirm")
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/database/backups/restore-pitr",
			map[string]any{"recovery_time_target_unix": target})

	// ---- edge functions ----
	case "list_functions":
		r, err := ref()
		if err != nil {
			return "", err
		}
		raw, err := supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/functions", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_function":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a function slug", sub(d.SupabaseFunctionSlug)); err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodGet,
			"/projects/"+r+"/functions/"+url.PathEscape(sub(d.SupabaseFunctionSlug)), nil)

	case "get_function_body":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a function slug", sub(d.SupabaseFunctionSlug)); err != nil {
			return "", err
		}
		// Returns the source itself rather than JSON.
		raw, err := supabaseCall(ctx, token, http.MethodGet,
			"/projects/"+r+"/functions/"+url.PathEscape(sub(d.SupabaseFunctionSlug))+"/body", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "create_function":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a function slug", sub(d.SupabaseFunctionSlug)); err != nil {
			return "", err
		}
		if err := need("the function source", sub(d.SupabaseFunctionBody)); err != nil {
			return "", err
		}
		payload := map[string]any{
			"slug": sub(d.SupabaseFunctionSlug),
			"name": firstNonEmpty(sub(d.SupabaseName), sub(d.SupabaseFunctionSlug)),
			"body": sub(d.SupabaseFunctionBody),
		}
		if v := sub(d.SupabaseVerifyJwt); v != "" {
			payload["verify_jwt"] = flag(v)
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/functions", payload)

	case "update_function":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a function slug", sub(d.SupabaseFunctionSlug)); err != nil {
			return "", err
		}
		payload := map[string]any{}
		if v := sub(d.SupabaseName); v != "" {
			payload["name"] = v
		}
		if v := sub(d.SupabaseFunctionBody); v != "" {
			payload["body"] = v
		}
		if v := sub(d.SupabaseVerifyJwt); v != "" {
			payload["verify_jwt"] = flag(v)
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("update_function needs a new name, new source, or a JWT verification setting")
		}
		return supabaseCall(ctx, token, http.MethodPatch,
			"/projects/"+r+"/functions/"+url.PathEscape(sub(d.SupabaseFunctionSlug)), payload)

	case "deploy_function":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a function slug", sub(d.SupabaseFunctionSlug)); err != nil {
			return "", err
		}
		if err := need("the function source", sub(d.SupabaseFunctionBody)); err != nil {
			return "", err
		}
		entrypoint := firstNonEmpty(sub(d.SupabaseEntrypointPath), "index.ts")
		metadata := map[string]any{
			"entrypoint_path": entrypoint,
			"name":            firstNonEmpty(sub(d.SupabaseName), sub(d.SupabaseFunctionSlug)),
		}
		if v := sub(d.SupabaseVerifyJwt); v != "" {
			metadata["verify_jwt"] = flag(v)
		}
		if v := sub(d.SupabaseImportMapPath); v != "" {
			metadata["import_map_path"] = v
		}
		return supabaseDeploy(ctx, token,
			"/projects/"+r+"/functions/deploy?slug="+url.QueryEscape(sub(d.SupabaseFunctionSlug)),
			entrypoint, sub(d.SupabaseFunctionBody), metadata)

	case "delete_function":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a function slug", sub(d.SupabaseFunctionSlug)); err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodDelete,
			"/projects/"+r+"/functions/"+url.PathEscape(sub(d.SupabaseFunctionSlug)), nil)

	// ---- function secrets ----
	case "list_secrets":
		r, err := ref()
		if err != nil {
			return "", err
		}
		// Values come back in full; this is the plaintext of every function secret.
		raw, err := supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/secrets", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "create_secrets":
		r, err := ref()
		if err != nil {
			return "", err
		}
		pairs, err := supabaseSecretPairs(sub(d.SupabaseSecrets))
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/secrets", pairs)

	case "delete_secrets":
		r, err := ref()
		if err != nil {
			return "", err
		}
		names := splitCSV(sub(d.SupabaseSecretNames))
		if len(names) == 0 {
			return "", fmt.Errorf("delete_secrets needs at least one secret name")
		}
		// The body is a bare array of names, not an object.
		return supabaseCall(ctx, token, http.MethodDelete, "/projects/"+r+"/secrets", names)

	// ---- auth config ----
	case "get_auth_config":
		r, err := ref()
		if err != nil {
			return "", err
		}
		raw, err := supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/config/auth", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "update_auth_config":
		r, err := ref()
		if err != nil {
			return "", err
		}
		// The body has well over a hundred fields, so the two that workflows
		// actually change get their own inputs and the rest go through as JSON.
		payload, err := supabaseJSONObject(sub(d.SupabaseAuthConfig), "the auth config")
		if err != nil {
			return "", err
		}
		if v := sub(d.SupabaseSiteUrl); v != "" {
			payload["site_url"] = v
		}
		if v := sub(d.SupabaseUriAllowList); v != "" {
			payload["uri_allow_list"] = v
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("update_auth_config needs a site URL, a redirect allow-list, " +
				"or a JSON object of auth settings to change")
		}
		return supabaseCall(ctx, token, http.MethodPatch, "/projects/"+r+"/config/auth", payload)

	// ---- storage ----
	case "list_storage_buckets":
		r, err := ref()
		if err != nil {
			return "", err
		}
		// Listing is all the Management API exposes; creating and emptying buckets
		// is the Storage API on the project's own host.
		raw, err := supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/storage/buckets", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	// ---- branches ----
	case "list_branches":
		r, err := ref()
		if err != nil {
			return "", err
		}
		raw, err := supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/branches", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_branch":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a branch name", sub(d.SupabaseBranchName)); err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodGet,
			"/projects/"+r+"/branches/"+url.PathEscape(sub(d.SupabaseBranchName)), nil)

	case "create_branch":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a branch name", sub(d.SupabaseBranchName)); err != nil {
			return "", err
		}
		payload := map[string]any{"branch_name": sub(d.SupabaseBranchName)}
		if v := sub(d.SupabaseGitBranch); v != "" {
			payload["git_branch"] = v
		}
		if v := sub(d.SupabasePersistent); v != "" {
			payload["persistent"] = flag(v)
		}
		if v := sub(d.SupabaseWithData); v != "" {
			payload["with_data"] = flag(v)
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/branches", payload)

	case "delete_branch":
		// Branch endpoints hang off /branches/{branch_ref}, not the parent project.
		b, err := branch()
		if err != nil {
			return "", err
		}
		path := "/branches/" + b
		if flag(sub(d.SupabaseForce)) {
			path += "?force=true"
		}
		return supabaseCall(ctx, token, http.MethodDelete, path, nil)

	case "merge_branch":
		b, err := branch()
		if err != nil {
			return "", err
		}
		payload := map[string]any{}
		if v := sub(d.SupabaseMigrationVersion); v != "" {
			payload["migration_version"] = v
		}
		// Merging applies the branch's migrations to production.
		return supabaseCall(ctx, token, http.MethodPost, "/branches/"+b+"/merge", payload)

	case "reset_branch":
		b, err := branch()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodPost, "/branches/"+b+"/reset", nil)

	// ---- custom hostname ----
	case "get_custom_hostname":
		r, err := ref()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/custom-hostname", nil)

	case "set_custom_hostname":
		r, err := ref()
		if err != nil {
			return "", err
		}
		if err := need("a hostname", sub(d.SupabaseHostname)); err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/custom-hostname/initialize",
			map[string]any{"custom_hostname": sub(d.SupabaseHostname)})

	case "verify_custom_hostname":
		r, err := ref()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/custom-hostname/reverify", nil)

	case "activate_custom_hostname":
		r, err := ref()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/custom-hostname/activate", nil)

	case "delete_custom_hostname":
		r, err := ref()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodDelete, "/projects/"+r+"/custom-hostname", nil)

	// ---- network ----
	case "get_network_restrictions":
		r, err := ref()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/network-restrictions", nil)

	case "apply_network_restrictions":
		r, err := ref()
		if err != nil {
			return "", err
		}
		v4 := splitCSV(sub(d.SupabaseAllowedCidrs))
		v6 := splitCSV(sub(d.SupabaseAllowedCidrsV6))
		// The call replaces the allow-list rather than adding to it, so sending a
		// partial list is how people lock themselves out of their own database.
		if len(v4) == 0 && len(v6) == 0 {
			return "", fmt.Errorf("apply_network_restrictions replaces the whole allow-list — " +
				"list every CIDR that should keep database access, e.g. 0.0.0.0/0 for open")
		}
		payload := map[string]any{}
		if len(v4) > 0 {
			payload["dbAllowedCidrs"] = v4
		}
		if len(v6) > 0 {
			payload["dbAllowedCidrsV6"] = v6
		}
		return supabaseCall(ctx, token, http.MethodPost,
			"/projects/"+r+"/network-restrictions/apply", payload)

	case "list_network_bans":
		r, err := ref()
		if err != nil {
			return "", err
		}
		// A POST that only reads; there is no GET for this.
		return supabaseCall(ctx, token, http.MethodPost, "/projects/"+r+"/network-bans/retrieve", nil)

	case "delete_network_bans":
		r, err := ref()
		if err != nil {
			return "", err
		}
		ips := splitCSV(sub(d.SupabaseIpAddresses))
		if len(ips) == 0 {
			return "", fmt.Errorf("delete_network_bans needs the IPv4 addresses to unban, " +
				"as listed by list_network_bans")
		}
		return supabaseCall(ctx, token, http.MethodDelete, "/projects/"+r+"/network-bans",
			map[string]any{"ipv4_addresses": ips})

	// ---- PostgREST ----
	case "get_postgrest_config":
		r, err := ref()
		if err != nil {
			return "", err
		}
		return supabaseCall(ctx, token, http.MethodGet, "/projects/"+r+"/postgrest", nil)

	case "update_postgrest_config":
		r, err := ref()
		if err != nil {
			return "", err
		}
		payload := map[string]any{}
		if v := sub(d.SupabasePostgrestSchema); v != "" {
			payload["db_schema"] = v
		}
		if v := sub(d.SupabasePostgrestSearchPath); v != "" {
			payload["db_extra_search_path"] = v
		}
		if n := intOr(d.SupabasePostgrestMaxRows, 0); n > 0 {
			payload["max_rows"] = n
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("update_postgrest_config needs an exposed schema, " +
				"an extra search path, or a max rows limit")
		}
		return supabaseCall(ctx, token, http.MethodPatch, "/projects/"+r+"/postgrest", payload)

	// ---- TypeScript types ----
	case "generate_types":
		r, err := ref()
		if err != nil {
			return "", err
		}
		path := "/projects/" + r + "/types/typescript"
		// Defaults to public only; a project using custom schemas has to name them.
		if v := sub(d.SupabaseIncludedSchemas); v != "" {
			path += "?included_schemas=" + url.QueryEscape(v)
		}
		raw, err := supabaseCall(ctx, token, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		// The source arrives wrapped in {"types":…}; a workflow writing a file
		// wants the source, not the wrapper.
		if types := jsonField(raw, "types"); types != "" {
			return truncateStr(types, 12000), nil
		}
		return truncateStr(raw, 12000), nil

	// ---- SQL snippets ----
	case "list_snippets":
		q := url.Values{}
		// Snippets belong to the user, not the project, so the ref only filters.
		if v := strings.TrimSpace(sub(d.SupabaseProjectRef)); v != "" {
			r, err := ref()
			if err != nil {
				return "", err
			}
			q.Set("project_ref", r)
		}
		if n := intOr(d.SupabaseLimit, 0); n > 0 {
			q.Set("limit", fmt.Sprint(n))
		}
		if v := sub(d.SupabaseCursor); v != "" {
			q.Set("cursor", v)
		}
		if v := sub(d.SupabaseSortBy); v != "" {
			q.Set("sort_by", v)
		}
		if v := sub(d.SupabaseSortOrder); v != "" {
			q.Set("sort_order", v)
		}
		path := "/snippets"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		raw, err := supabaseCall(ctx, token, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_snippet":
		if err := need("a snippet ID", sub(d.SupabaseSnippetId)); err != nil {
			return "", err
		}
		raw, err := supabaseCall(ctx, token, http.MethodGet,
			"/snippets/"+url.PathEscape(sub(d.SupabaseSnippetId)), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "":
		return "", fmt.Errorf("no Supabase operation selected")
	}
	return "", fmt.Errorf("unsupported Supabase operation: %s", d.IntegrationOp)
}
