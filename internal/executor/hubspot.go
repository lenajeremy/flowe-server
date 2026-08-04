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

// HubSpot CRM v3.
//
// The API is uniform in a way that shapes this file: contacts, companies, deals,
// tickets and the engagement types (notes, tasks, calls, emails, meetings) are all
// "objects" behind the same five endpoints. So rather than forty near-identical
// operations, the ops name a verb and take an object type — which also means a new
// HubSpot object type works here without a code change.
//
// Two things that surprise people:
//
//   - A property that is absent from the response is not necessarily empty. v3
//     returns only a default subset, so anything beyond name and id has to be
//     asked for by name. hubspotProperties handles that.
//   - Associations moved to v4 while the objects stayed on v3, so those calls use a
//     different version on the same host.

const hubspotAPI = "https://api.hubapi.com"

// hubspotObjectTypes are the built-in types, used to validate early and to give
// the AI a closed list. A custom object's type id also works.
var hubspotObjectTypes = map[string]bool{
	"contacts": true, "companies": true, "deals": true, "tickets": true,
	"line_items": true, "products": true, "quotes": true,
	"notes": true, "tasks": true, "calls": true, "emails": true, "meetings": true,
}

func hubspotCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, hubspotAPI+path, reader)
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
		return "", fmt.Errorf("hubspot request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", hubspotError(resp.StatusCode, raw)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

// hubspotError unpacks HubSpot's error envelope, which puts the useful part in
// message and the actionable part in a per-field context map.
func hubspotError(status int, raw []byte) error {
	var e struct {
		Message  string `json:"message"`
		Category string `json:"category"`
		Errors   []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	var parts []string
	if json.Unmarshal(raw, &e) == nil {
		if e.Message != "" {
			parts = append(parts, e.Message)
		}
		for _, x := range e.Errors {
			if x.Message != "" && x.Message != e.Message {
				parts = append(parts, x.Message)
			}
		}
	}
	msg := strings.Join(parts, "; ")
	if msg == "" {
		msg = truncateStr(string(raw), 300)
	}
	switch {
	case e.Category == "MISSING_SCOPES" || status == http.StatusForbidden:
		msg += " — the connection is missing a scope for this object; reconnect HubSpot " +
			"after enabling it on the app"
	case status == http.StatusConflict:
		// The usual cause is a unique property, most often a duplicate email.
		msg += " — a record with that unique property already exists; search for it and update instead"
	case status == http.StatusTooManyRequests:
		msg += " — HubSpot rate limit reached; space these calls out"
	}
	return fmt.Errorf("HubSpot API error (%d): %s", status, msg)
}

func runHubSpot(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.HubspotLimit, 25)

	objectType := func() (string, error) {
		t := strings.TrimSpace(strings.ToLower(sub(d.HubspotObjectType)))
		if t == "" {
			return "", fmt.Errorf("this operation needs an object type, e.g. contacts, companies, " +
				"deals, tickets, notes or tasks")
		}
		// A custom object arrives as a numeric type id, which is equally valid.
		if !hubspotObjectTypes[t] && !isAllDigits(t) && !strings.HasPrefix(t, "p_") {
			return "", fmt.Errorf("%q is not a HubSpot object type — use one of contacts, companies, "+
				"deals, tickets, line_items, products, quotes, notes, tasks, calls, emails, "+
				"meetings, or a custom object's type id", t)
		}
		return t, nil
	}
	// v3 returns a default subset of properties, so anything else is requested
	// explicitly. Without this, a workflow reading a custom property gets nothing
	// and no error.
	propsQuery := func() string {
		props := splitCSV(sub(d.HubspotProperties))
		if len(props) == 0 {
			return ""
		}
		q := url.Values{}
		q.Set("properties", strings.Join(props, ","))
		return "&" + q.Encode()
	}
	propsBody := func() (map[string]any, error) {
		raw := strings.TrimSpace(sub(d.HubspotPropertyValues))
		if raw == "" {
			return nil, fmt.Errorf(`this operation needs properties as a JSON object, ` +
				`e.g. {"email":"jane@acme.com","firstname":"Jane"}`)
		}
		var m map[string]any
		if json.Unmarshal([]byte(raw), &m) != nil {
			return nil, fmt.Errorf(`properties must be a JSON object keyed by HubSpot's internal ` +
				`property names, e.g. {"email":"jane@acme.com"} — note firstname, not First Name`)
		}
		return m, nil
	}

	switch d.IntegrationOp {
	// ---- objects ----
	case "list_objects":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		path := fmt.Sprintf("/crm/v3/objects/%s?limit=%d%s", t, limit, propsQuery())
		if v := sub(d.HubspotAfter); v != "" {
			path += "&after=" + url.QueryEscape(v)
		}
		if strings.EqualFold(sub(d.HubspotArchived), "true") {
			path += "&archived=true"
		}
		raw, err := hubspotCall(ctx, token, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "get_object":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		if sub(d.HubspotObjectId) == "" {
			return "", fmt.Errorf("get_object needs a record ID")
		}
		path := fmt.Sprintf("/crm/v3/objects/%s/%s?archived=false%s",
			t, url.PathEscape(sub(d.HubspotObjectId)), propsQuery())
		// idProperty lets a workflow look a record up by email rather than by id.
		if v := sub(d.HubspotIdProperty); v != "" {
			path += "&idProperty=" + url.QueryEscape(v)
		}
		return hubspotCall(ctx, token, http.MethodGet, path, nil)

	case "search_objects":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		body := map[string]any{"limit": limit}
		if props := splitCSV(sub(d.HubspotProperties)); len(props) > 0 {
			body["properties"] = props
		}
		if v := sub(d.HubspotAfter); v != "" {
			body["after"] = v
		}
		if q := sub(d.HubspotQuery); q != "" {
			// A bare query is a full-text search across the object's indexed fields.
			body["query"] = q
		}
		if f := strings.TrimSpace(sub(d.HubspotFilters)); f != "" {
			var groups []any
			if json.Unmarshal([]byte(f), &groups) != nil {
				return "", fmt.Errorf(`filters must be a JSON array of filter groups, e.g. ` +
					`[{"filters":[{"propertyName":"email","operator":"EQ","value":"jane@acme.com"}]}]`)
			}
			body["filterGroups"] = groups
		}
		if s := sub(d.HubspotSortProperty); s != "" {
			dir := "DESCENDING"
			if strings.EqualFold(sub(d.HubspotSortDirection), "asc") {
				dir = "ASCENDING"
			}
			body["sorts"] = []any{map[string]any{"propertyName": s, "direction": dir}}
		}
		raw, err := hubspotCall(ctx, token, http.MethodPost,
			"/crm/v3/objects/"+t+"/search", body)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "create_object":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		props, err := propsBody()
		if err != nil {
			return "", err
		}
		body := map[string]any{"properties": props}
		if assoc := strings.TrimSpace(sub(d.HubspotAssociations)); assoc != "" {
			var list []any
			if json.Unmarshal([]byte(assoc), &list) != nil {
				return "", fmt.Errorf("associations must be a JSON array as HubSpot documents them")
			}
			body["associations"] = list
		}
		return hubspotCall(ctx, token, http.MethodPost, "/crm/v3/objects/"+t, body)

	case "update_object":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		if sub(d.HubspotObjectId) == "" {
			return "", fmt.Errorf("update_object needs a record ID")
		}
		props, err := propsBody()
		if err != nil {
			return "", err
		}
		// PATCH leaves unlisted properties alone; there is no full replace here.
		path := "/crm/v3/objects/" + t + "/" + url.PathEscape(sub(d.HubspotObjectId))
		if v := sub(d.HubspotIdProperty); v != "" {
			path += "?idProperty=" + url.QueryEscape(v)
		}
		return hubspotCall(ctx, token, http.MethodPatch, path,
			map[string]any{"properties": props})

	case "delete_object":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		if sub(d.HubspotObjectId) == "" {
			return "", fmt.Errorf("delete_object needs a record ID")
		}
		// HubSpot archives rather than hard-deletes, so this is recoverable in-app.
		if _, err := hubspotCall(ctx, token, http.MethodDelete,
			"/crm/v3/objects/"+t+"/"+url.PathEscape(sub(d.HubspotObjectId)), nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"archived":%q,"objectType":%q}`, sub(d.HubspotObjectId), t), nil

	case "batch_create_objects", "batch_update_objects", "batch_read_objects", "batch_archive_objects":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		raw := strings.TrimSpace(sub(d.HubspotBatchInputs))
		if raw == "" {
			return "", fmt.Errorf("this operation needs an inputs array, e.g. " +
				`[{"properties":{"email":"a@b.com"}}] to create, or [{"id":"123"}] to read`)
		}
		var inputs []any
		if json.Unmarshal([]byte(raw), &inputs) != nil {
			return "", fmt.Errorf("inputs must be a JSON array")
		}
		if len(inputs) > 100 {
			return "", fmt.Errorf("HubSpot batches at most 100 records per request, got %d", len(inputs))
		}
		action := map[string]string{
			"batch_create_objects":  "create",
			"batch_update_objects":  "update",
			"batch_read_objects":    "read",
			"batch_archive_objects": "archive",
		}[d.IntegrationOp]
		body := map[string]any{"inputs": inputs}
		if action == "read" {
			if props := splitCSV(sub(d.HubspotProperties)); len(props) > 0 {
				body["properties"] = props
			}
			if v := sub(d.HubspotIdProperty); v != "" {
				body["idProperty"] = v
			}
		}
		out, err := hubspotCall(ctx, token, http.MethodPost,
			fmt.Sprintf("/crm/v3/objects/%s/batch/%s", t, action), body)
		if err != nil {
			return "", err
		}
		return truncateStr(out, 12000), nil

	// ---- associations (v4) ----
	case "list_associations":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		if sub(d.HubspotObjectId) == "" || sub(d.HubspotToObjectType) == "" {
			return "", fmt.Errorf("list_associations needs a record ID and the type to look for")
		}
		return hubspotCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"/crm/v4/objects/%s/%s/associations/%s", t,
			url.PathEscape(sub(d.HubspotObjectId)), sub(d.HubspotToObjectType)), nil)

	case "associate_objects":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		if sub(d.HubspotObjectId) == "" || sub(d.HubspotToObjectType) == "" || sub(d.HubspotToObjectId) == "" {
			return "", fmt.Errorf("associate_objects needs both records and the target type")
		}
		// The default association type is inferred by HubSpot when none is given.
		path := fmt.Sprintf("/crm/v4/objects/%s/%s/associations/default/%s/%s", t,
			url.PathEscape(sub(d.HubspotObjectId)), sub(d.HubspotToObjectType),
			url.PathEscape(sub(d.HubspotToObjectId)))
		return hubspotCall(ctx, token, http.MethodPut, path, nil)

	case "disassociate_objects":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		if sub(d.HubspotObjectId) == "" || sub(d.HubspotToObjectType) == "" || sub(d.HubspotToObjectId) == "" {
			return "", fmt.Errorf("disassociate_objects needs both records and the target type")
		}
		return hubspotCall(ctx, token, http.MethodDelete, fmt.Sprintf(
			"/crm/v4/objects/%s/%s/associations/%s/%s", t,
			url.PathEscape(sub(d.HubspotObjectId)), sub(d.HubspotToObjectType),
			url.PathEscape(sub(d.HubspotToObjectId))), nil)

	// ---- schema & metadata ----
	case "list_properties":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		raw, err := hubspotCall(ctx, token, http.MethodGet, "/crm/v3/properties/"+t, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "get_property":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		if sub(d.HubspotPropertyName) == "" {
			return "", fmt.Errorf("get_property needs the property's internal name")
		}
		return hubspotCall(ctx, token, http.MethodGet,
			"/crm/v3/properties/"+t+"/"+url.PathEscape(sub(d.HubspotPropertyName)), nil)

	case "create_property":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		if sub(d.HubspotPropertyName) == "" || sub(d.HubspotLabel) == "" {
			return "", fmt.Errorf("create_property needs an internal name and a label")
		}
		return hubspotCall(ctx, token, http.MethodPost, "/crm/v3/properties/"+t, map[string]any{
			"name":      sub(d.HubspotPropertyName),
			"label":     sub(d.HubspotLabel),
			"type":      firstNonEmpty(sub(d.HubspotPropertyType), "string"),
			"fieldType": firstNonEmpty(sub(d.HubspotFieldType), "text"),
			"groupName": firstNonEmpty(sub(d.HubspotGroupName), t+"information"),
		})

	case "list_pipelines":
		t, err := objectType()
		if err != nil {
			return "", err
		}
		return hubspotCall(ctx, token, http.MethodGet, "/crm/v3/pipelines/"+t, nil)

	case "list_owners":
		return hubspotCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/crm/v3/owners?limit=%d", limit), nil)

	// ---- lists ----
	case "search_lists":
		body := map[string]any{"count": limit}
		if q := sub(d.HubspotQuery); q != "" {
			body["query"] = q
		}
		return hubspotCall(ctx, token, http.MethodPost, "/crm/v3/lists/search", body)

	case "get_list":
		if sub(d.HubspotListId) == "" {
			return "", fmt.Errorf("get_list needs a list ID")
		}
		return hubspotCall(ctx, token, http.MethodGet,
			"/crm/v3/lists/"+url.PathEscape(sub(d.HubspotListId)), nil)

	case "list_memberships":
		if sub(d.HubspotListId) == "" {
			return "", fmt.Errorf("list_memberships needs a list ID")
		}
		raw, err := hubspotCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"/crm/v3/lists/%s/memberships?limit=%d", url.PathEscape(sub(d.HubspotListId)), limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "add_to_list", "remove_from_list":
		if sub(d.HubspotListId) == "" {
			return "", fmt.Errorf("this operation needs a list ID")
		}
		ids := splitCSV(sub(d.HubspotObjectId))
		if len(ids) == 0 {
			return "", fmt.Errorf("this operation needs at least one record ID")
		}
		list := make([]any, 0, len(ids))
		for _, id := range ids {
			list = append(list, id)
		}
		// A static list only; HubSpot computes membership of a dynamic list itself
		// and rejects a manual change.
		verb := "add"
		if d.IntegrationOp == "remove_from_list" {
			verb = "remove"
		}
		return hubspotCall(ctx, token, http.MethodPut, fmt.Sprintf(
			"/crm/v3/lists/%s/memberships/%s", url.PathEscape(sub(d.HubspotListId)), verb), list)

	case "":
		return "", fmt.Errorf("no HubSpot operation selected")
	}
	return "", fmt.Errorf("unsupported HubSpot operation: %s", d.IntegrationOp)
}
