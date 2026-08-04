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

// Airtable Web API. Data operations live under /v0/{baseId}/{table}; schema and
// account operations under /v0/meta.
//
// Three constraints shape the ops below:
//   - Writes are batched, capped at 10 records per request. Anything that takes a
//     records array enforces that here rather than letting Airtable reject it.
//   - A field can be addressed by name or by id, and mixing the two silently
//     writes nothing. The node picks one mode explicitly.
//   - Values are strictly typed unless typecast is set: writing "5" into a number
//     field fails without it, which is unhelpful for a workflow assembling values
//     from text, so typecast defaults on for writes.

const airtableAPI = "https://api.airtable.com/v0"

// airtableBatchLimit is Airtable's hard cap on records per write request.
const airtableBatchLimit = 10

func airtableCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, airtableAPI+path, reader)
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
		return "", fmt.Errorf("airtable request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", airtableError(resp.StatusCode, raw)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

// airtableError unpacks both error shapes: a bare string, and the {type,message}
// object used for the more specific failures.
func airtableError(status int, raw []byte) error {
	var asObject struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal(raw, &asObject) == nil && asObject.Error.Type != "" {
		msg = asObject.Error.Type
		if asObject.Error.Message != "" {
			msg += ": " + asObject.Error.Message
		}
	}
	if msg == "" {
		var asString struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &asString) == nil && asString.Error != "" {
			msg = asString.Error
		}
	}
	if msg == "" {
		msg = truncateStr(string(raw), 300)
	}
	switch status {
	case http.StatusNotFound:
		msg += " — check the base ID and that the table name is spelled exactly as in Airtable"
	case http.StatusForbidden:
		msg += " — the connection may not have access to this base, or the token is missing a scope"
	case http.StatusTooManyRequests:
		msg += " — Airtable allows 5 requests a second per base"
	}
	return fmt.Errorf("Airtable API error (%d): %s", status, msg)
}

// airtableFields parses the JSON object a node supplies for a record's fields.
func airtableFields(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf(`this operation needs a fields object, e.g. {"Name":"Acme","Status":"Active"}`)
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil, fmt.Errorf(`fields must be a JSON object keyed by column name, ` +
			`e.g. {"Name":"Acme","Status":"Active"}`)
	}
	return m, nil
}

func runAirtable(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	base := func() string { return sub(d.AirtableBaseId) }
	// A table name can contain spaces and slashes, so it always gets escaped.
	table := func() string { return url.PathEscape(sub(d.AirtableTable)) }

	needsBaseAndTable := func() error {
		if base() == "" {
			return fmt.Errorf("this operation needs a base ID")
		}
		if sub(d.AirtableTable) == "" {
			return fmt.Errorf("this operation needs a table name or ID")
		}
		return nil
	}
	// Typecast lets Airtable coerce a string into a number, date or select
	// option, which is nearly always what a workflow wants.
	typecast := func() bool { return !strings.EqualFold(sub(d.AirtableTypecast), "false") }

	switch d.IntegrationOp {
	// ---- records ----
	case "list_records":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		q := url.Values{}
		if n := intOr(d.AirtableLimit, 0); n > 0 {
			q.Set("maxRecords", fmt.Sprint(n))
		}
		if v := sub(d.AirtableView); v != "" {
			q.Set("view", v)
		}
		if v := sub(d.AirtableFormula); v != "" {
			q.Set("filterByFormula", v)
		}
		if v := sub(d.AirtableOffset); v != "" {
			q.Set("offset", v)
		}
		for _, f := range splitCSV(sub(d.AirtableFieldNames)) {
			q.Add("fields[]", f)
		}
		if v := sub(d.AirtableSortField); v != "" {
			q.Set("sort[0][field]", v)
			q.Set("sort[0][direction]", firstNonEmpty(sub(d.AirtableSortDirection), "asc"))
		}
		path := "/" + base() + "/" + table()
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		raw, err := airtableCall(ctx, token, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "get_record":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		if sub(d.AirtableRecordId) == "" {
			return "", fmt.Errorf("get_record needs a record ID")
		}
		return airtableCall(ctx, token, http.MethodGet,
			"/"+base()+"/"+table()+"/"+sub(d.AirtableRecordId), nil)

	case "create_record":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		fields, err := airtableFields(sub(d.AirtableFields))
		if err != nil {
			return "", err
		}
		return airtableCall(ctx, token, http.MethodPost, "/"+base()+"/"+table(), map[string]any{
			"records":  []any{map[string]any{"fields": fields}},
			"typecast": typecast(),
		})

	case "create_records":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		records, err := airtableRecordArray(sub(d.AirtableRecords))
		if err != nil {
			return "", err
		}
		return airtableCall(ctx, token, http.MethodPost, "/"+base()+"/"+table(), map[string]any{
			"records":  records,
			"typecast": typecast(),
		})

	case "update_record":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		if sub(d.AirtableRecordId) == "" {
			return "", fmt.Errorf("update_record needs a record ID")
		}
		fields, err := airtableFields(sub(d.AirtableFields))
		if err != nil {
			return "", err
		}
		// PATCH leaves unlisted fields alone; PUT would clear them.
		return airtableCall(ctx, token, http.MethodPatch,
			"/"+base()+"/"+table()+"/"+sub(d.AirtableRecordId),
			map[string]any{"fields": fields, "typecast": typecast()})

	case "update_records":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		records, err := airtableRecordArray(sub(d.AirtableRecords))
		if err != nil {
			return "", err
		}
		return airtableCall(ctx, token, http.MethodPatch, "/"+base()+"/"+table(), map[string]any{
			"records":  records,
			"typecast": typecast(),
		})

	case "upsert_records":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		merge := splitCSV(sub(d.AirtableMergeOn))
		if len(merge) == 0 {
			return "", fmt.Errorf("upsert_records needs the field(s) to match on, " +
				"so Airtable can tell an update from an insert")
		}
		records, err := airtableRecordArray(sub(d.AirtableRecords))
		if err != nil {
			return "", err
		}
		return airtableCall(ctx, token, http.MethodPatch, "/"+base()+"/"+table(), map[string]any{
			"records":       records,
			"typecast":      typecast(),
			"performUpsert": map[string]any{"fieldsToMergeOn": merge},
		})

	case "delete_record":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		if sub(d.AirtableRecordId) == "" {
			return "", fmt.Errorf("delete_record needs a record ID")
		}
		return airtableCall(ctx, token, http.MethodDelete,
			"/"+base()+"/"+table()+"/"+sub(d.AirtableRecordId), nil)

	case "delete_records":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		ids := splitCSV(sub(d.AirtableRecordId))
		if len(ids) == 0 {
			return "", fmt.Errorf("delete_records needs at least one record ID")
		}
		if len(ids) > airtableBatchLimit {
			return "", fmt.Errorf("Airtable deletes at most %d records per request, got %d",
				airtableBatchLimit, len(ids))
		}
		q := url.Values{}
		for _, id := range ids {
			q.Add("records[]", id)
		}
		return airtableCall(ctx, token, http.MethodDelete,
			"/"+base()+"/"+table()+"?"+q.Encode(), nil)

	// ---- comments ----
	case "list_comments":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		if sub(d.AirtableRecordId) == "" {
			return "", fmt.Errorf("list_comments needs a record ID")
		}
		return airtableCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"/%s/%s/%s/comments?pageSize=%d", base(), table(), sub(d.AirtableRecordId),
			intOr(d.AirtableLimit, 25)), nil)

	case "create_comment":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		if sub(d.AirtableRecordId) == "" || sub(d.AirtableComment) == "" {
			return "", fmt.Errorf("create_comment needs a record ID and comment text")
		}
		return airtableCall(ctx, token, http.MethodPost,
			"/"+base()+"/"+table()+"/"+sub(d.AirtableRecordId)+"/comments",
			map[string]any{"text": sub(d.AirtableComment)})

	case "update_comment":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		if sub(d.AirtableCommentId) == "" || sub(d.AirtableComment) == "" {
			return "", fmt.Errorf("update_comment needs a comment ID and new text")
		}
		return airtableCall(ctx, token, http.MethodPatch,
			"/"+base()+"/"+table()+"/"+sub(d.AirtableRecordId)+"/comments/"+sub(d.AirtableCommentId),
			map[string]any{"text": sub(d.AirtableComment)})

	case "delete_comment":
		if err := needsBaseAndTable(); err != nil {
			return "", err
		}
		if sub(d.AirtableCommentId) == "" {
			return "", fmt.Errorf("delete_comment needs a comment ID")
		}
		return airtableCall(ctx, token, http.MethodDelete,
			"/"+base()+"/"+table()+"/"+sub(d.AirtableRecordId)+"/comments/"+sub(d.AirtableCommentId), nil)

	// ---- bases & schema ----
	case "list_bases":
		return airtableCall(ctx, token, http.MethodGet, "/meta/bases", nil)

	case "get_base_schema":
		if base() == "" {
			return "", fmt.Errorf("get_base_schema needs a base ID")
		}
		raw, err := airtableCall(ctx, token, http.MethodGet, "/meta/bases/"+base()+"/tables", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "create_base":
		if sub(d.AirtableName) == "" || sub(d.AirtableWorkspaceId) == "" {
			return "", fmt.Errorf("create_base needs a name and a workspace ID")
		}
		tables, err := airtableJSONArray(sub(d.AirtableTables))
		if err != nil {
			return "", fmt.Errorf("create_base needs a tables array describing at least one table: %w", err)
		}
		return airtableCall(ctx, token, http.MethodPost, "/meta/bases", map[string]any{
			"name":        sub(d.AirtableName),
			"workspaceId": sub(d.AirtableWorkspaceId),
			"tables":      tables,
		})

	case "create_table":
		if base() == "" || sub(d.AirtableName) == "" {
			return "", fmt.Errorf("create_table needs a base ID and a table name")
		}
		fields, err := airtableJSONArray(sub(d.AirtableTableFields))
		if err != nil {
			return "", fmt.Errorf("create_table needs a fields array, e.g. "+
				`[{"name":"Name","type":"singleLineText"}]: %w`, err)
		}
		payload := map[string]any{"name": sub(d.AirtableName), "fields": fields}
		if v := sub(d.AirtableDescription); v != "" {
			payload["description"] = v
		}
		return airtableCall(ctx, token, http.MethodPost, "/meta/bases/"+base()+"/tables", payload)

	case "update_table":
		if base() == "" || sub(d.AirtableTableId) == "" {
			return "", fmt.Errorf("update_table needs a base ID and a table ID")
		}
		payload := map[string]any{}
		if v := sub(d.AirtableName); v != "" {
			payload["name"] = v
		}
		if v := sub(d.AirtableDescription); v != "" {
			payload["description"] = v
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("update_table needs a new name or description")
		}
		return airtableCall(ctx, token, http.MethodPatch,
			"/meta/bases/"+base()+"/tables/"+sub(d.AirtableTableId), payload)

	case "create_field":
		if base() == "" || sub(d.AirtableTableId) == "" || sub(d.AirtableName) == "" {
			return "", fmt.Errorf("create_field needs a base ID, a table ID and a field name")
		}
		payload := map[string]any{
			"name": sub(d.AirtableName),
			"type": firstNonEmpty(sub(d.AirtableFieldType), "singleLineText"),
		}
		// Choice fields carry their options in a shape that varies by type, so it
		// is passed through as given rather than guessed at.
		if opts := strings.TrimSpace(sub(d.AirtableFieldOptions)); opts != "" {
			var m map[string]any
			if json.Unmarshal([]byte(opts), &m) != nil {
				return "", fmt.Errorf("field options must be a JSON object")
			}
			payload["options"] = m
		}
		return airtableCall(ctx, token, http.MethodPost,
			"/meta/bases/"+base()+"/tables/"+sub(d.AirtableTableId)+"/fields", payload)

	case "update_field":
		if base() == "" || sub(d.AirtableTableId) == "" || sub(d.AirtableFieldId) == "" {
			return "", fmt.Errorf("update_field needs a base ID, a table ID and a field ID")
		}
		payload := map[string]any{}
		if v := sub(d.AirtableName); v != "" {
			payload["name"] = v
		}
		if v := sub(d.AirtableDescription); v != "" {
			payload["description"] = v
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("update_field needs a new name or description")
		}
		return airtableCall(ctx, token, http.MethodPatch,
			"/meta/bases/"+base()+"/tables/"+sub(d.AirtableTableId)+"/fields/"+sub(d.AirtableFieldId),
			payload)

	// ---- webhooks ----
	case "list_webhooks":
		if base() == "" {
			return "", fmt.Errorf("list_webhooks needs a base ID")
		}
		return airtableCall(ctx, token, http.MethodGet, "/bases/"+base()+"/webhooks", nil)

	case "create_webhook":
		if base() == "" || sub(d.AirtableUrl) == "" {
			return "", fmt.Errorf("create_webhook needs a base ID and a notification URL")
		}
		spec := map[string]any{
			"options": map[string]any{
				"filters": map[string]any{
					"dataTypes": []string{"tableData"},
				},
			},
		}
		return airtableCall(ctx, token, http.MethodPost, "/bases/"+base()+"/webhooks",
			map[string]any{"notificationUrl": sub(d.AirtableUrl), "specification": spec})

	case "delete_webhook":
		if base() == "" || sub(d.AirtableWebhookId) == "" {
			return "", fmt.Errorf("delete_webhook needs a base ID and a webhook ID")
		}
		return airtableCall(ctx, token, http.MethodDelete,
			"/bases/"+base()+"/webhooks/"+sub(d.AirtableWebhookId), nil)

	case "refresh_webhook":
		// Airtable webhooks expire after 7 days unless refreshed.
		if base() == "" || sub(d.AirtableWebhookId) == "" {
			return "", fmt.Errorf("refresh_webhook needs a base ID and a webhook ID")
		}
		return airtableCall(ctx, token, http.MethodPost,
			"/bases/"+base()+"/webhooks/"+sub(d.AirtableWebhookId)+"/refresh", nil)

	case "list_webhook_payloads":
		if base() == "" || sub(d.AirtableWebhookId) == "" {
			return "", fmt.Errorf("list_webhook_payloads needs a base ID and a webhook ID")
		}
		q := url.Values{}
		if v := sub(d.AirtableCursor); v != "" {
			q.Set("cursor", v)
		}
		path := "/bases/" + base() + "/webhooks/" + sub(d.AirtableWebhookId) + "/payloads"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		raw, err := airtableCall(ctx, token, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "whoami":
		return airtableCall(ctx, token, http.MethodGet, "/meta/whoami", nil)

	case "":
		return "", fmt.Errorf("no Airtable operation selected")
	}
	return "", fmt.Errorf("unsupported Airtable operation: %s", d.IntegrationOp)
}

// airtableRecordArray parses and validates a batch of records, enforcing
// Airtable's cap here so the error names the limit rather than relaying a 422.
func airtableRecordArray(raw string) ([]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf(`this operation needs a records array, e.g. ` +
			`[{"fields":{"Name":"Acme"}}] — include "id" on each record when updating`)
	}
	var records []any
	if json.Unmarshal([]byte(raw), &records) != nil {
		return nil, fmt.Errorf(`records must be a JSON array, e.g. [{"fields":{"Name":"Acme"}}]`)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("the records array is empty")
	}
	if len(records) > airtableBatchLimit {
		return nil, fmt.Errorf("Airtable writes at most %d records per request, got %d — "+
			"use a loop node to send them in batches", airtableBatchLimit, len(records))
	}
	return records, nil
}

func airtableJSONArray(raw string) ([]any, error) {
	var out []any
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &out) != nil {
		return nil, fmt.Errorf("expected a JSON array")
	}
	return out, nil
}
