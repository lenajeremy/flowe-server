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

// Kit (formerly ConvertKit), API v4.
//
// Authenticated with an X-Kit-Api-Key header rather than a bearer token, which is
// why this provider carries its own call helper instead of reusing a shared one.
// Kit allows 120 requests/minute on a key (600 on OAuth), so list ops here take a
// per_page rather than paging eagerly.
//
// Kit's own guidance is that keys are for automating your own account; a public
// multi-tenant integration is supposed to use OAuth (which needs PKCE). Worth
// knowing before this is offered to end customers rather than to yourself.

const kitAPI = "https://api.kit.com/v4"

func kitCall(ctx context.Context, key, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, kitAPI+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Kit-Api-Key", key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("kit request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Errors []string `json:"errors"`
			Error  string   `json:"error"`
		}
		msg := ""
		if json.Unmarshal(raw, &e) == nil {
			msg = strings.Join(e.Errors, "; ")
			if msg == "" {
				msg = e.Error
			}
		}
		if msg == "" {
			msg = truncateStr(string(raw), 300)
		}
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			msg += " — Kit allows 120 requests a minute on an API key"
		case http.StatusUnauthorized:
			msg += " — the key was rejected; note that a v3 key will not work on v4, " +
				"so create a V4 key under Settings → Developer"
		}
		return "", fmt.Errorf("Kit API error (%d): %s", resp.StatusCode, msg)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

func runKit(ctx context.Context, key string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.KitLimit, 25)
	page := func(path string) string {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		return fmt.Sprintf("%s%sper_page=%d", path, sep, limit)
	}
	// Kit takes custom field values as a fields object on the subscriber.
	fields := func() (map[string]any, error) {
		raw := strings.TrimSpace(sub(d.KitFields))
		if raw == "" {
			return nil, nil
		}
		var m map[string]any
		if json.Unmarshal([]byte(raw), &m) != nil {
			return nil, fmt.Errorf(`custom fields must be a JSON object, e.g. {"last_name":"Doe"} — ` +
				`create the field in Kit first`)
		}
		return m, nil
	}

	switch d.IntegrationOp {
	// ---- subscribers ----
	case "create_subscriber":
		if sub(d.KitEmail) == "" {
			return "", fmt.Errorf("create_subscriber needs an email address")
		}
		payload := map[string]any{"email_address": sub(d.KitEmail)}
		if v := sub(d.KitFirstName); v != "" {
			payload["first_name"] = v
		}
		if v := sub(d.KitState); v != "" {
			payload["state"] = v
		}
		f, err := fields()
		if err != nil {
			return "", err
		}
		if f != nil {
			payload["fields"] = f
		}
		return kitCall(ctx, key, http.MethodPost, "/subscribers", payload)

	case "list_subscribers":
		q := url.Values{}
		if v := sub(d.KitState); v != "" {
			q.Set("status", v)
		}
		if v := sub(d.KitEmail); v != "" {
			// Kit filters by exact address, which is how you look one up by email.
			q.Set("email_address", v)
		}
		if v := sub(d.KitCreatedAfter); v != "" {
			q.Set("created_after", v)
		}
		path := "/subscribers"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		raw, err := kitCall(ctx, key, http.MethodGet, page(path), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_subscriber":
		if sub(d.KitSubscriberId) == "" {
			return "", fmt.Errorf("get_subscriber needs a subscriber ID — list_subscribers with an email finds one")
		}
		return kitCall(ctx, key, http.MethodGet, "/subscribers/"+sub(d.KitSubscriberId), nil)

	case "update_subscriber":
		if sub(d.KitSubscriberId) == "" {
			return "", fmt.Errorf("update_subscriber needs a subscriber ID")
		}
		payload := map[string]any{}
		if v := sub(d.KitEmail); v != "" {
			payload["email_address"] = v
		}
		if v := sub(d.KitFirstName); v != "" {
			payload["first_name"] = v
		}
		f, err := fields()
		if err != nil {
			return "", err
		}
		if f != nil {
			payload["fields"] = f
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("update_subscriber needs at least one field to change")
		}
		return kitCall(ctx, key, http.MethodPatch, "/subscribers/"+sub(d.KitSubscriberId), payload)

	case "unsubscribe":
		if sub(d.KitSubscriberId) == "" {
			return "", fmt.Errorf("unsubscribe needs a subscriber ID")
		}
		return kitCall(ctx, key, http.MethodPost,
			"/subscribers/"+sub(d.KitSubscriberId)+"/unsubscribe", nil)

	case "get_subscriber_stats":
		if sub(d.KitSubscriberId) == "" {
			return "", fmt.Errorf("get_subscriber_stats needs a subscriber ID")
		}
		return kitCall(ctx, key, http.MethodGet,
			"/subscribers/"+sub(d.KitSubscriberId)+"/stats", nil)

	case "list_subscriber_tags":
		if sub(d.KitSubscriberId) == "" {
			return "", fmt.Errorf("list_subscriber_tags needs a subscriber ID")
		}
		return kitCall(ctx, key, http.MethodGet,
			page("/subscribers/"+sub(d.KitSubscriberId)+"/tags"), nil)

	// ---- tags ----
	case "list_tags":
		return kitCall(ctx, key, http.MethodGet, page("/tags"), nil)

	case "create_tag":
		if sub(d.KitName) == "" {
			return "", fmt.Errorf("create_tag needs a name")
		}
		return kitCall(ctx, key, http.MethodPost, "/tags", map[string]any{"name": sub(d.KitName)})

	case "rename_tag":
		if sub(d.KitTagId) == "" || sub(d.KitName) == "" {
			return "", fmt.Errorf("rename_tag needs a tag ID and a new name")
		}
		return kitCall(ctx, key, http.MethodPatch, "/tags/"+sub(d.KitTagId),
			map[string]any{"name": sub(d.KitName)})

	case "tag_subscriber":
		if sub(d.KitTagId) == "" {
			return "", fmt.Errorf("tag_subscriber needs a tag ID")
		}
		// Kit accepts either an id or an address here, which lets a workflow tag
		// someone it has only just collected.
		payload := map[string]any{}
		if v := sub(d.KitEmail); v != "" {
			payload["email_address"] = v
		} else if v := sub(d.KitSubscriberId); v != "" {
			payload["id"] = v
		} else {
			return "", fmt.Errorf("tag_subscriber needs a subscriber email or ID")
		}
		return kitCall(ctx, key, http.MethodPost, "/tags/"+sub(d.KitTagId)+"/subscribers", payload)

	case "untag_subscriber":
		if sub(d.KitTagId) == "" || sub(d.KitSubscriberId) == "" {
			return "", fmt.Errorf("untag_subscriber needs a tag ID and a subscriber ID")
		}
		return kitCall(ctx, key, http.MethodDelete,
			"/tags/"+sub(d.KitTagId)+"/subscribers/"+sub(d.KitSubscriberId), nil)

	case "list_tag_subscribers":
		if sub(d.KitTagId) == "" {
			return "", fmt.Errorf("list_tag_subscribers needs a tag ID")
		}
		raw, err := kitCall(ctx, key, http.MethodGet,
			page("/tags/"+sub(d.KitTagId)+"/subscribers"), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	// ---- forms ----
	case "list_forms":
		return kitCall(ctx, key, http.MethodGet, page("/forms"), nil)

	case "add_subscriber_to_form":
		if sub(d.KitFormId) == "" || sub(d.KitEmail) == "" {
			return "", fmt.Errorf("add_subscriber_to_form needs a form ID and an email address")
		}
		payload := map[string]any{"email_address": sub(d.KitEmail)}
		if v := sub(d.KitFirstName); v != "" {
			payload["first_name"] = v
		}
		f, err := fields()
		if err != nil {
			return "", err
		}
		if f != nil {
			payload["fields"] = f
		}
		return kitCall(ctx, key, http.MethodPost, "/forms/"+sub(d.KitFormId)+"/subscribers", payload)

	case "list_form_subscribers":
		if sub(d.KitFormId) == "" {
			return "", fmt.Errorf("list_form_subscribers needs a form ID")
		}
		raw, err := kitCall(ctx, key, http.MethodGet,
			page("/forms/"+sub(d.KitFormId)+"/subscribers"), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	// ---- sequences ----
	case "list_sequences":
		return kitCall(ctx, key, http.MethodGet, page("/sequences"), nil)

	case "get_sequence":
		if sub(d.KitSequenceId) == "" {
			return "", fmt.Errorf("get_sequence needs a sequence ID")
		}
		return kitCall(ctx, key, http.MethodGet, "/sequences/"+sub(d.KitSequenceId), nil)

	case "create_sequence":
		if sub(d.KitName) == "" {
			return "", fmt.Errorf("create_sequence needs a name")
		}
		return kitCall(ctx, key, http.MethodPost, "/sequences",
			map[string]any{"name": sub(d.KitName)})

	case "add_subscriber_to_sequence":
		if sub(d.KitSequenceId) == "" || sub(d.KitEmail) == "" {
			return "", fmt.Errorf("add_subscriber_to_sequence needs a sequence ID and an email address")
		}
		return kitCall(ctx, key, http.MethodPost,
			"/sequences/"+sub(d.KitSequenceId)+"/subscribers",
			map[string]any{"email_address": sub(d.KitEmail)})

	case "list_sequence_subscribers":
		if sub(d.KitSequenceId) == "" {
			return "", fmt.Errorf("list_sequence_subscribers needs a sequence ID")
		}
		raw, err := kitCall(ctx, key, http.MethodGet,
			page("/sequences/"+sub(d.KitSequenceId)+"/subscribers"), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	// ---- broadcasts ----
	case "list_broadcasts":
		return kitCall(ctx, key, http.MethodGet, page("/broadcasts"), nil)

	case "get_broadcast":
		if sub(d.KitBroadcastId) == "" {
			return "", fmt.Errorf("get_broadcast needs a broadcast ID")
		}
		return kitCall(ctx, key, http.MethodGet, "/broadcasts/"+sub(d.KitBroadcastId), nil)

	case "create_broadcast":
		if sub(d.KitSubject) == "" {
			return "", fmt.Errorf("create_broadcast needs a subject")
		}
		payload := map[string]any{"subject": sub(d.KitSubject)}
		if v := sub(d.KitContent); v != "" {
			payload["content"] = v
		}
		if v := sub(d.KitDescription); v != "" {
			payload["description"] = v
		}
		// Omitting send_at leaves it a draft, which is the safer default.
		if v := sub(d.KitSendAt); v != "" {
			payload["send_at"] = v
		}
		if v := splitCSV(sub(d.KitTagId)); len(v) > 0 {
			payload["subscriber_filter"] = []any{
				map[string]any{"all": []any{map[string]any{"type": "tag", "ids": v}}},
			}
		}
		return kitCall(ctx, key, http.MethodPost, "/broadcasts", payload)

	case "update_broadcast":
		if sub(d.KitBroadcastId) == "" {
			return "", fmt.Errorf("update_broadcast needs a broadcast ID")
		}
		payload := map[string]any{}
		if v := sub(d.KitSubject); v != "" {
			payload["subject"] = v
		}
		if v := sub(d.KitContent); v != "" {
			payload["content"] = v
		}
		if v := sub(d.KitSendAt); v != "" {
			payload["send_at"] = v
		}
		if len(payload) == 0 {
			return "", fmt.Errorf("update_broadcast needs at least one field to change")
		}
		return kitCall(ctx, key, http.MethodPatch, "/broadcasts/"+sub(d.KitBroadcastId), payload)

	case "delete_broadcast":
		if sub(d.KitBroadcastId) == "" {
			return "", fmt.Errorf("delete_broadcast needs a broadcast ID")
		}
		return kitCall(ctx, key, http.MethodDelete, "/broadcasts/"+sub(d.KitBroadcastId), nil)

	case "get_broadcast_stats":
		if sub(d.KitBroadcastId) == "" {
			return "", fmt.Errorf("get_broadcast_stats needs a broadcast ID")
		}
		return kitCall(ctx, key, http.MethodGet,
			"/broadcasts/"+sub(d.KitBroadcastId)+"/stats", nil)

	case "get_broadcast_link_clicks":
		if sub(d.KitBroadcastId) == "" {
			return "", fmt.Errorf("get_broadcast_link_clicks needs a broadcast ID")
		}
		return kitCall(ctx, key, http.MethodGet,
			"/broadcasts/"+sub(d.KitBroadcastId)+"/link-clicks", nil)

	// ---- custom fields ----
	case "list_custom_fields":
		return kitCall(ctx, key, http.MethodGet, page("/custom-fields"), nil)

	case "create_custom_field":
		if sub(d.KitName) == "" {
			return "", fmt.Errorf("create_custom_field needs a label")
		}
		return kitCall(ctx, key, http.MethodPost, "/custom-fields",
			map[string]any{"label": sub(d.KitName)})

	case "delete_custom_field":
		if sub(d.KitFieldId) == "" {
			return "", fmt.Errorf("delete_custom_field needs a field ID")
		}
		return kitCall(ctx, key, http.MethodDelete, "/custom-fields/"+sub(d.KitFieldId), nil)

	// ---- purchases ----
	case "list_purchases":
		raw, err := kitCall(ctx, key, http.MethodGet, page("/purchases"), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_purchase":
		if sub(d.KitPurchaseId) == "" {
			return "", fmt.Errorf("get_purchase needs a purchase ID")
		}
		return kitCall(ctx, key, http.MethodGet, "/purchases/"+sub(d.KitPurchaseId), nil)

	case "create_purchase":
		raw := strings.TrimSpace(sub(d.KitPurchase))
		if raw == "" {
			return "", fmt.Errorf("create_purchase needs a JSON purchase object")
		}
		var m map[string]any
		if json.Unmarshal([]byte(raw), &m) != nil {
			return "", fmt.Errorf(`purchase must be a JSON object with email_address, transaction_id, ` +
				`currency and a products array`)
		}
		return kitCall(ctx, key, http.MethodPost, "/purchases", m)

	// ---- webhooks, segments, templates, account ----
	case "list_webhooks":
		return kitCall(ctx, key, http.MethodGet, page("/webhooks"), nil)

	case "create_webhook":
		if sub(d.KitUrl) == "" || sub(d.KitEvent) == "" {
			return "", fmt.Errorf("create_webhook needs a target URL and an event name")
		}
		return kitCall(ctx, key, http.MethodPost, "/webhooks", map[string]any{
			"target_url": sub(d.KitUrl),
			"event":      map[string]any{"name": sub(d.KitEvent)},
		})

	case "delete_webhook":
		if sub(d.KitWebhookId) == "" {
			return "", fmt.Errorf("delete_webhook needs a webhook ID")
		}
		return kitCall(ctx, key, http.MethodDelete, "/webhooks/"+sub(d.KitWebhookId), nil)

	case "list_segments":
		return kitCall(ctx, key, http.MethodGet, page("/segments"), nil)

	case "list_email_templates":
		return kitCall(ctx, key, http.MethodGet, page("/email-templates"), nil)

	case "get_account":
		return kitCall(ctx, key, http.MethodGet, "/account", nil)

	case "get_email_stats":
		return kitCall(ctx, key, http.MethodGet, "/account/email-stats", nil)

	case "get_growth_stats":
		return kitCall(ctx, key, http.MethodGet, "/account/growth-stats", nil)

	case "":
		return "", fmt.Errorf("no Kit operation selected")
	}
	return "", fmt.Errorf("unsupported Kit operation: %s", d.IntegrationOp)
}
