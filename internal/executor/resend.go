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

// Resend. A key-authenticated REST API covering transactional email plus a
// marketing side: contacts, segments, broadcasts, templates and suppressions.
//
// Two shapes to know:
//   - Contacts are top-level (/contacts), not nested under a segment. A contact
//     joins segments by listing segment ids in its "segments" field.
//   - scheduled_at accepts natural language ("in 1 hour") as well as ISO 8601,
//     so it is passed through untouched rather than parsed here.

const resendAPI = "https://api.resend.com"

func resendCall(ctx context.Context, key, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, resendAPI+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("resend request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			msg := e.Message
			// The most common first-run failure by a wide margin.
			if strings.Contains(strings.ToLower(e.Message), "domain") &&
				strings.Contains(strings.ToLower(e.Message), "verif") {
				msg += " — verify the sending domain in Resend before sending from an address on it"
			}
			return "", fmt.Errorf("Resend API error (%d): %s", resp.StatusCode, msg)
		}
		return "", fmt.Errorf("Resend API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

// resendRecipients turns a comma-separated field into the array Resend expects.
func resendRecipients(s string) []string { return splitCSV(s) }

func runResend(ctx context.Context, key string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.ResendLimit, 25)
	listQuery := func() string {
		q := url.Values{"limit": {fmt.Sprint(limit)}}
		return "?" + q.Encode()
	}

	switch d.IntegrationOp {
	// ---- transactional email ----
	case "send_email":
		payload, err := resendEmailPayload(d, sub)
		if err != nil {
			return "", err
		}
		return resendCall(ctx, key, http.MethodPost, "/emails", payload)

	case "send_batch":
		// Each line of the batch field is one complete email object.
		raw := strings.TrimSpace(sub(d.ResendBatch))
		if raw == "" {
			return "", fmt.Errorf("send_batch needs a JSON array of email objects")
		}
		var emails []map[string]any
		if json.Unmarshal([]byte(raw), &emails) != nil {
			return "", fmt.Errorf(`send_batch expects a JSON array, e.g. [{"from":"…","to":"…","subject":"…","html":"…"}]`)
		}
		if len(emails) == 0 {
			return "", fmt.Errorf("send_batch was given an empty array")
		}
		return resendCall(ctx, key, http.MethodPost, "/emails/batch", emails)

	case "get_email":
		if sub(d.ResendEmailId) == "" {
			return "", fmt.Errorf("get_email needs an email ID")
		}
		return resendCall(ctx, key, http.MethodGet, "/emails/"+sub(d.ResendEmailId), nil)

	case "list_sent_emails":
		raw, err := resendCall(ctx, key, http.MethodGet, "/emails"+listQuery(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_received_emails":
		raw, err := resendCall(ctx, key, http.MethodGet, "/emails/received"+listQuery(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_received_email":
		if sub(d.ResendEmailId) == "" {
			return "", fmt.Errorf("get_received_email needs an email ID")
		}
		return resendCall(ctx, key, http.MethodGet, "/emails/received/"+sub(d.ResendEmailId), nil)

	case "reschedule_email":
		if sub(d.ResendEmailId) == "" || sub(d.ResendScheduledAt) == "" {
			return "", fmt.Errorf("reschedule_email needs an email ID and a new send time")
		}
		return resendCall(ctx, key, http.MethodPatch, "/emails/"+sub(d.ResendEmailId),
			map[string]any{"scheduled_at": sub(d.ResendScheduledAt)})

	case "cancel_email":
		if sub(d.ResendEmailId) == "" {
			return "", fmt.Errorf("cancel_email needs an email ID")
		}
		// Only a scheduled email can be cancelled; one already sent cannot.
		return resendCall(ctx, key, http.MethodPost, "/emails/"+sub(d.ResendEmailId)+"/cancel", nil)

	// ---- domains ----
	case "list_domains":
		return resendCall(ctx, key, http.MethodGet, "/domains", nil)

	case "get_domain":
		if sub(d.ResendDomainId) == "" {
			return "", fmt.Errorf("get_domain needs a domain ID")
		}
		return resendCall(ctx, key, http.MethodGet, "/domains/"+sub(d.ResendDomainId), nil)

	case "create_domain":
		if sub(d.ResendDomain) == "" {
			return "", fmt.Errorf("create_domain needs a domain name")
		}
		payload := map[string]any{"name": sub(d.ResendDomain)}
		if r := sub(d.ResendRegion); r != "" {
			payload["region"] = r
		}
		return resendCall(ctx, key, http.MethodPost, "/domains", payload)

	case "verify_domain":
		if sub(d.ResendDomainId) == "" {
			return "", fmt.Errorf("verify_domain needs a domain ID")
		}
		return resendCall(ctx, key, http.MethodPost, "/domains/"+sub(d.ResendDomainId)+"/verify", nil)

	case "delete_domain":
		if sub(d.ResendDomainId) == "" {
			return "", fmt.Errorf("delete_domain needs a domain ID")
		}
		return resendCall(ctx, key, http.MethodDelete, "/domains/"+sub(d.ResendDomainId), nil)

	// ---- contacts ----
	case "create_contact", "update_contact":
		if sub(d.ResendEmail) == "" && sub(d.ResendContactId) == "" {
			return "", fmt.Errorf("%s needs a contact email or ID", d.IntegrationOp)
		}
		payload := map[string]any{}
		if v := sub(d.ResendEmail); v != "" {
			payload["email"] = v
		}
		if v := sub(d.ResendFirstName); v != "" {
			payload["first_name"] = v
		}
		if v := sub(d.ResendLastName); v != "" {
			payload["last_name"] = v
		}
		if v := sub(d.ResendUnsubscribed); v != "" {
			payload["unsubscribed"] = strings.EqualFold(v, "true")
		}
		if segs := resendRecipients(sub(d.ResendSegmentId)); len(segs) > 0 {
			payload["segments"] = segs
		}
		if props := strings.TrimSpace(sub(d.ResendProperties)); props != "" {
			var m map[string]any
			if json.Unmarshal([]byte(props), &m) != nil {
				return "", fmt.Errorf(`properties must be a JSON object, e.g. {"plan":"pro"}`)
			}
			payload["properties"] = m
		}
		if d.IntegrationOp == "create_contact" {
			return resendCall(ctx, key, http.MethodPost, "/contacts", payload)
		}
		return resendCall(ctx, key, http.MethodPatch, "/contacts/"+resendContactRef(sub(d.ResendContactId), sub(d.ResendEmail)), payload)

	case "get_contact":
		ref := resendContactRef(sub(d.ResendContactId), sub(d.ResendEmail))
		if ref == "" {
			return "", fmt.Errorf("get_contact needs a contact ID or email")
		}
		return resendCall(ctx, key, http.MethodGet, "/contacts/"+ref, nil)

	case "list_contacts":
		raw, err := resendCall(ctx, key, http.MethodGet, "/contacts"+listQuery(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "delete_contact":
		ref := resendContactRef(sub(d.ResendContactId), sub(d.ResendEmail))
		if ref == "" {
			return "", fmt.Errorf("delete_contact needs a contact ID or email")
		}
		return resendCall(ctx, key, http.MethodDelete, "/contacts/"+ref, nil)

	case "add_contact_to_segment":
		ref := resendContactRef(sub(d.ResendContactId), sub(d.ResendEmail))
		if ref == "" || sub(d.ResendSegmentId) == "" {
			return "", fmt.Errorf("add_contact_to_segment needs a contact and a segment ID")
		}
		return resendCall(ctx, key, http.MethodPost,
			"/contacts/"+ref+"/segments/"+sub(d.ResendSegmentId), nil)

	case "remove_contact_from_segment":
		ref := resendContactRef(sub(d.ResendContactId), sub(d.ResendEmail))
		if ref == "" || sub(d.ResendSegmentId) == "" {
			return "", fmt.Errorf("remove_contact_from_segment needs a contact and a segment ID")
		}
		return resendCall(ctx, key, http.MethodDelete,
			"/contacts/"+ref+"/segments/"+sub(d.ResendSegmentId), nil)

	case "list_contact_segments":
		ref := resendContactRef(sub(d.ResendContactId), sub(d.ResendEmail))
		if ref == "" {
			return "", fmt.Errorf("list_contact_segments needs a contact ID or email")
		}
		return resendCall(ctx, key, http.MethodGet, "/contacts/"+ref+"/segments", nil)

	// ---- segments ----
	case "create_segment":
		if sub(d.ResendName) == "" {
			return "", fmt.Errorf("create_segment needs a name")
		}
		return resendCall(ctx, key, http.MethodPost, "/segments",
			map[string]any{"name": sub(d.ResendName)})

	case "list_segments":
		return resendCall(ctx, key, http.MethodGet, "/segments", nil)

	case "get_segment":
		if sub(d.ResendSegmentId) == "" {
			return "", fmt.Errorf("get_segment needs a segment ID")
		}
		return resendCall(ctx, key, http.MethodGet, "/segments/"+sub(d.ResendSegmentId), nil)

	case "delete_segment":
		if sub(d.ResendSegmentId) == "" {
			return "", fmt.Errorf("delete_segment needs a segment ID")
		}
		return resendCall(ctx, key, http.MethodDelete, "/segments/"+sub(d.ResendSegmentId), nil)

	case "list_segment_contacts":
		if sub(d.ResendSegmentId) == "" {
			return "", fmt.Errorf("list_segment_contacts needs a segment ID")
		}
		raw, err := resendCall(ctx, key, http.MethodGet,
			"/segments/"+sub(d.ResendSegmentId)+"/contacts"+listQuery(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	// ---- broadcasts ----
	case "create_broadcast":
		if sub(d.ResendSegmentId) == "" {
			return "", fmt.Errorf("create_broadcast needs a segment ID to send to")
		}
		payload := map[string]any{
			"segment_id": sub(d.ResendSegmentId),
			"from":       sub(d.ResendFrom),
			"subject":    sub(d.ResendSubject),
		}
		if v := sub(d.ResendHtml); v != "" {
			payload["html"] = v
		}
		if v := sub(d.ResendText); v != "" {
			payload["text"] = v
		}
		if v := sub(d.ResendName); v != "" {
			payload["name"] = v
		}
		if v := sub(d.ResendReplyTo); v != "" {
			payload["reply_to"] = resendRecipients(v)
		}
		return resendCall(ctx, key, http.MethodPost, "/broadcasts", payload)

	case "list_broadcasts":
		return resendCall(ctx, key, http.MethodGet, "/broadcasts", nil)

	case "get_broadcast":
		if sub(d.ResendBroadcastId) == "" {
			return "", fmt.Errorf("get_broadcast needs a broadcast ID")
		}
		return resendCall(ctx, key, http.MethodGet, "/broadcasts/"+sub(d.ResendBroadcastId), nil)

	case "send_broadcast":
		if sub(d.ResendBroadcastId) == "" {
			return "", fmt.Errorf("send_broadcast needs a broadcast ID")
		}
		payload := map[string]any{}
		// Omitting scheduled_at sends immediately.
		if v := sub(d.ResendScheduledAt); v != "" {
			payload["scheduled_at"] = v
		}
		return resendCall(ctx, key, http.MethodPost,
			"/broadcasts/"+sub(d.ResendBroadcastId)+"/send", payload)

	case "delete_broadcast":
		if sub(d.ResendBroadcastId) == "" {
			return "", fmt.Errorf("delete_broadcast needs a broadcast ID")
		}
		return resendCall(ctx, key, http.MethodDelete, "/broadcasts/"+sub(d.ResendBroadcastId), nil)

	case "get_broadcast_metrics":
		if sub(d.ResendBroadcastId) == "" {
			return "", fmt.Errorf("get_broadcast_metrics needs a broadcast ID")
		}
		return resendCall(ctx, key, http.MethodGet,
			"/broadcasts/"+sub(d.ResendBroadcastId)+"/metrics", nil)

	// ---- templates ----
	case "create_template":
		if sub(d.ResendName) == "" {
			return "", fmt.Errorf("create_template needs a name")
		}
		payload := map[string]any{"name": sub(d.ResendName)}
		if v := sub(d.ResendHtml); v != "" {
			payload["html"] = v
		}
		if v := sub(d.ResendText); v != "" {
			payload["text"] = v
		}
		if v := sub(d.ResendSubject); v != "" {
			payload["subject"] = v
		}
		return resendCall(ctx, key, http.MethodPost, "/templates", payload)

	case "list_templates":
		return resendCall(ctx, key, http.MethodGet, "/templates", nil)

	case "get_template":
		if sub(d.ResendTemplateId) == "" {
			return "", fmt.Errorf("get_template needs a template ID")
		}
		return resendCall(ctx, key, http.MethodGet, "/templates/"+sub(d.ResendTemplateId), nil)

	case "publish_template":
		// A template must be published before send_email can reference it.
		if sub(d.ResendTemplateId) == "" {
			return "", fmt.Errorf("publish_template needs a template ID")
		}
		return resendCall(ctx, key, http.MethodPost,
			"/templates/"+sub(d.ResendTemplateId)+"/publish", nil)

	case "delete_template":
		if sub(d.ResendTemplateId) == "" {
			return "", fmt.Errorf("delete_template needs a template ID")
		}
		return resendCall(ctx, key, http.MethodDelete, "/templates/"+sub(d.ResendTemplateId), nil)

	// ---- suppressions ----
	case "add_suppression":
		if sub(d.ResendEmail) == "" {
			return "", fmt.Errorf("add_suppression needs an email address")
		}
		return resendCall(ctx, key, http.MethodPost, "/suppressions",
			map[string]any{"email": sub(d.ResendEmail)})

	case "list_suppressions":
		raw, err := resendCall(ctx, key, http.MethodGet, "/suppressions"+listQuery(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "remove_suppression":
		if sub(d.ResendEmail) == "" {
			return "", fmt.Errorf("remove_suppression needs an email address")
		}
		return resendCall(ctx, key, http.MethodDelete,
			"/suppressions/"+url.PathEscape(sub(d.ResendEmail)), nil)

	// ---- webhooks & logs ----
	case "list_webhooks":
		return resendCall(ctx, key, http.MethodGet, "/webhooks", nil)

	case "create_webhook":
		if sub(d.ResendUrl) == "" {
			return "", fmt.Errorf("create_webhook needs an endpoint URL")
		}
		payload := map[string]any{"endpoint": sub(d.ResendUrl)}
		if ev := resendRecipients(sub(d.ResendEvents)); len(ev) > 0 {
			payload["events"] = ev
		}
		return resendCall(ctx, key, http.MethodPost, "/webhooks", payload)

	case "delete_webhook":
		if sub(d.ResendWebhookId) == "" {
			return "", fmt.Errorf("delete_webhook needs a webhook ID")
		}
		return resendCall(ctx, key, http.MethodDelete, "/webhooks/"+sub(d.ResendWebhookId), nil)

	case "list_logs":
		raw, err := resendCall(ctx, key, http.MethodGet, "/logs"+listQuery(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_api_keys":
		return resendCall(ctx, key, http.MethodGet, "/api-keys", nil)

	case "":
		return "", fmt.Errorf("no Resend operation selected")
	}
	return "", fmt.Errorf("unsupported Resend operation: %s", d.IntegrationOp)
}

// resendContactRef prefers an explicit id but accepts an email, which Resend
// also resolves on the contacts path.
func resendContactRef(id, email string) string {
	if id = strings.TrimSpace(id); id != "" {
		return id
	}
	if email = strings.TrimSpace(email); email != "" {
		return url.PathEscape(email)
	}
	return ""
}

// resendEmailPayload assembles a send. from/to/subject are required by the API;
// everything else is set only when provided.
func resendEmailPayload(d FlowNodeData, sub func(string) string) (map[string]any, error) {
	to := resendRecipients(sub(d.ResendTo))
	if len(to) == 0 {
		return nil, fmt.Errorf("send_email needs at least one recipient")
	}
	if sub(d.ResendFrom) == "" {
		return nil, fmt.Errorf("send_email needs a from address on a domain verified in Resend")
	}
	payload := map[string]any{
		"from":    sub(d.ResendFrom),
		"to":      to,
		"subject": sub(d.ResendSubject),
	}
	if v := sub(d.ResendHtml); v != "" {
		payload["html"] = v
	}
	if v := sub(d.ResendText); v != "" {
		payload["text"] = v
	}
	if v := resendRecipients(sub(d.ResendCc)); len(v) > 0 {
		payload["cc"] = v
	}
	if v := resendRecipients(sub(d.ResendBcc)); len(v) > 0 {
		payload["bcc"] = v
	}
	if v := resendRecipients(sub(d.ResendReplyTo)); len(v) > 0 {
		payload["reply_to"] = v
	}
	if v := sub(d.ResendScheduledAt); v != "" {
		// Natural language ("in 1 hour") and ISO 8601 are both accepted upstream.
		payload["scheduled_at"] = v
	}
	if id := sub(d.ResendTemplateId); id != "" {
		// A template replaces html/text rather than supplementing them.
		delete(payload, "html")
		delete(payload, "text")
		tmpl := map[string]any{"id": id}
		if vars := strings.TrimSpace(sub(d.ResendTemplateVars)); vars != "" {
			var m map[string]any
			if json.Unmarshal([]byte(vars), &m) != nil {
				return nil, fmt.Errorf(`template variables must be a JSON object, e.g. {"name":"Jane"}`)
			}
			tmpl["variables"] = m
		}
		payload["template"] = tmpl
	} else if payload["html"] == nil && payload["text"] == nil {
		return nil, fmt.Errorf("send_email needs an HTML body, a text body, or a template")
	}
	if hdrs := strings.TrimSpace(sub(d.ResendHeaders)); hdrs != "" {
		var m map[string]string
		if json.Unmarshal([]byte(hdrs), &m) != nil {
			return nil, fmt.Errorf(`headers must be a JSON object, e.g. {"X-Entity-Ref":"123"}`)
		}
		payload["headers"] = m
	}
	if tags := strings.TrimSpace(sub(d.ResendTags)); tags != "" {
		var m map[string]string
		if json.Unmarshal([]byte(tags), &m) != nil {
			return nil, fmt.Errorf(`tags must be a JSON object, e.g. {"campaign":"launch"}`)
		}
		// Resend takes tags as a list of name/value pairs, not a map.
		list := make([]any, 0, len(m))
		for name, value := range m {
			list = append(list, map[string]string{"name": name, "value": value})
		}
		payload["tags"] = list
	}
	return payload, nil
}
