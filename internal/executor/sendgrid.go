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

// SendGrid v3. Two halves that barely resemble each other: the Mail Send API,
// and the Marketing Campaigns API under /marketing.
//
// The trap worth knowing up front is that contact upserts are asynchronous. A
// 202 with a job_id means "queued", not "saved" — so upsert_contacts reports the
// job id rather than pretending the contact exists, and a workflow that needs to
// read the contact back has to poll get_import_status.

const sendgridAPI = "https://api.sendgrid.com/v3"

func sendgridCall(ctx context.Context, key, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, sendgridAPI+path, reader)
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
		return "", fmt.Errorf("sendgrid request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", sendgridError(resp.StatusCode, raw)
	}
	// mail/send and several deletes answer 202/204 with no body.
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

// sendgridError unpacks the {errors:[{message,field}]} envelope and names the two
// failures that account for most first-run trouble.
func sendgridError(status int, raw []byte) error {
	var e struct {
		Errors []struct {
			Message string `json:"message"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	var parts []string
	if json.Unmarshal(raw, &e) == nil {
		for _, x := range e.Errors {
			if x.Field != "" {
				parts = append(parts, x.Field+": "+x.Message)
			} else if x.Message != "" {
				parts = append(parts, x.Message)
			}
		}
	}
	msg := strings.Join(parts, "; ")
	if msg == "" {
		msg = truncateStr(string(raw), 300)
	}
	switch status {
	case http.StatusForbidden:
		// Almost always a key without the right scope, or an unverified sender.
		msg += " — check the API key has the needed permissions, and that the " +
			"from-address is a verified sender in SendGrid"
	case http.StatusUnauthorized:
		msg += " — the API key was rejected; it may have been revoked"
	}
	return fmt.Errorf("SendGrid API error (%d): %s", status, msg)
}

func runSendGrid(ctx context.Context, key string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.SendGridLimit, 25)

	switch d.IntegrationOp {
	// ---- transactional mail ----
	case "send_email":
		payload, err := sendgridMailPayload(d, sub)
		if err != nil {
			return "", err
		}
		if _, err := sendgridCall(ctx, key, http.MethodPost, "/mail/send", payload); err != nil {
			return "", err
		}
		// A 202 carries no body, so report something a later node can branch on.
		return fmt.Sprintf(`{"ok":true,"accepted":true,"to":%q}`, sub(d.SendGridTo)), nil

	// ---- marketing contacts ----
	case "upsert_contact":
		if sub(d.SendGridEmail) == "" {
			return "", fmt.Errorf("upsert_contact needs an email address")
		}
		contact := map[string]any{"email": sub(d.SendGridEmail)}
		if v := sub(d.SendGridFirstName); v != "" {
			contact["first_name"] = v
		}
		if v := sub(d.SendGridLastName); v != "" {
			contact["last_name"] = v
		}
		if props := strings.TrimSpace(sub(d.SendGridCustomFields)); props != "" {
			var m map[string]any
			if json.Unmarshal([]byte(props), &m) != nil {
				return "", fmt.Errorf(`custom fields must be a JSON object keyed by field ID, ` +
					`e.g. {"e1_T":"pro"} — create the field in SendGrid first`)
			}
			contact["custom_fields"] = m
		}
		payload := map[string]any{"contacts": []any{contact}}
		if ids := splitCSV(sub(d.SendGridListId)); len(ids) > 0 {
			payload["list_ids"] = ids
		}
		raw, err := sendgridCall(ctx, key, http.MethodPut, "/marketing/contacts", payload)
		if err != nil {
			return "", err
		}
		// Be honest that this is queued, not done.
		return fmt.Sprintf(`{"queued":true,"note":"SendGrid processes contact upserts asynchronously; `+
			`poll get_import_status with this job id","job":%s}`, raw), nil

	case "get_import_status":
		if sub(d.SendGridJobId) == "" {
			return "", fmt.Errorf("get_import_status needs the job ID returned by upsert_contact")
		}
		return sendgridCall(ctx, key, http.MethodGet,
			"/marketing/contacts/imports/"+sub(d.SendGridJobId), nil)

	case "get_contact":
		if sub(d.SendGridContactId) == "" {
			return "", fmt.Errorf("get_contact needs a contact ID — use search_contacts to find one by email")
		}
		return sendgridCall(ctx, key, http.MethodGet,
			"/marketing/contacts/"+sub(d.SendGridContactId), nil)

	case "search_contacts":
		// SGQL, e.g. email LIKE '%@acme.com'
		q := sub(d.SendGridQuery)
		if q == "" {
			return "", fmt.Errorf(`search_contacts needs an SGQL query, e.g. email LIKE '%%@acme.com'`)
		}
		raw, err := sendgridCall(ctx, key, http.MethodPost, "/marketing/contacts/search",
			map[string]any{"query": q})
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_contacts":
		raw, err := sendgridCall(ctx, key, http.MethodGet,
			fmt.Sprintf("/marketing/contacts?page_size=%d", limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "delete_contact":
		if sub(d.SendGridContactId) == "" {
			return "", fmt.Errorf("delete_contact needs a contact ID")
		}
		return sendgridCall(ctx, key, http.MethodDelete,
			"/marketing/contacts?ids="+url.QueryEscape(sub(d.SendGridContactId)), nil)

	case "get_contact_count":
		return sendgridCall(ctx, key, http.MethodGet, "/marketing/contacts/count", nil)

	// ---- lists ----
	case "list_lists":
		return sendgridCall(ctx, key, http.MethodGet,
			fmt.Sprintf("/marketing/lists?page_size=%d", limit), nil)

	case "create_list":
		if sub(d.SendGridName) == "" {
			return "", fmt.Errorf("create_list needs a name")
		}
		return sendgridCall(ctx, key, http.MethodPost, "/marketing/lists",
			map[string]any{"name": sub(d.SendGridName)})

	case "get_list":
		if sub(d.SendGridListId) == "" {
			return "", fmt.Errorf("get_list needs a list ID")
		}
		return sendgridCall(ctx, key, http.MethodGet, "/marketing/lists/"+sub(d.SendGridListId), nil)

	case "update_list":
		if sub(d.SendGridListId) == "" || sub(d.SendGridName) == "" {
			return "", fmt.Errorf("update_list needs a list ID and a new name")
		}
		return sendgridCall(ctx, key, http.MethodPatch, "/marketing/lists/"+sub(d.SendGridListId),
			map[string]any{"name": sub(d.SendGridName)})

	case "delete_list":
		if sub(d.SendGridListId) == "" {
			return "", fmt.Errorf("delete_list needs a list ID")
		}
		return sendgridCall(ctx, key, http.MethodDelete,
			"/marketing/lists/"+sub(d.SendGridListId), nil)

	case "remove_contacts_from_list":
		ids := splitCSV(sub(d.SendGridContactId))
		if sub(d.SendGridListId) == "" || len(ids) == 0 {
			return "", fmt.Errorf("remove_contacts_from_list needs a list ID and contact IDs")
		}
		return sendgridCall(ctx, key, http.MethodDelete, fmt.Sprintf(
			"/marketing/lists/%s/contacts?contact_ids=%s",
			sub(d.SendGridListId), url.QueryEscape(strings.Join(ids, ","))), nil)

	// ---- segments ----
	case "list_segments":
		return sendgridCall(ctx, key, http.MethodGet, "/marketing/segments/2.0", nil)

	case "get_segment":
		if sub(d.SendGridSegmentId) == "" {
			return "", fmt.Errorf("get_segment needs a segment ID")
		}
		return sendgridCall(ctx, key, http.MethodGet,
			"/marketing/segments/2.0/"+sub(d.SendGridSegmentId), nil)

	case "create_segment":
		if sub(d.SendGridName) == "" || sub(d.SendGridQuery) == "" {
			return "", fmt.Errorf("create_segment needs a name and an SGQL query")
		}
		payload := map[string]any{"name": sub(d.SendGridName), "query_dsl": sub(d.SendGridQuery)}
		if v := sub(d.SendGridListId); v != "" {
			payload["parent_list_ids"] = splitCSV(v)
		}
		return sendgridCall(ctx, key, http.MethodPost, "/marketing/segments/2.0", payload)

	case "delete_segment":
		if sub(d.SendGridSegmentId) == "" {
			return "", fmt.Errorf("delete_segment needs a segment ID")
		}
		return sendgridCall(ctx, key, http.MethodDelete,
			"/marketing/segments/2.0/"+sub(d.SendGridSegmentId), nil)

	// ---- single sends (campaigns) ----
	case "list_single_sends":
		return sendgridCall(ctx, key, http.MethodGet,
			fmt.Sprintf("/marketing/singlesends?page_size=%d", limit), nil)

	case "get_single_send":
		if sub(d.SendGridSingleSendId) == "" {
			return "", fmt.Errorf("get_single_send needs a single send ID")
		}
		return sendgridCall(ctx, key, http.MethodGet,
			"/marketing/singlesends/"+sub(d.SendGridSingleSendId), nil)

	case "create_single_send":
		if sub(d.SendGridName) == "" {
			return "", fmt.Errorf("create_single_send needs a name")
		}
		payload := map[string]any{"name": sub(d.SendGridName)}
		emailConfig := map[string]any{}
		if v := sub(d.SendGridSubject); v != "" {
			emailConfig["subject"] = v
		}
		if v := sub(d.SendGridHtml); v != "" {
			emailConfig["html_content"] = v
		}
		if v := sub(d.SendGridFrom); v != "" {
			emailConfig["sender_id"] = v
		}
		if len(emailConfig) > 0 {
			payload["email_config"] = emailConfig
		}
		if ids := splitCSV(sub(d.SendGridListId)); len(ids) > 0 {
			payload["send_to"] = map[string]any{"list_ids": ids}
		}
		return sendgridCall(ctx, key, http.MethodPost, "/marketing/singlesends", payload)

	case "schedule_single_send":
		if sub(d.SendGridSingleSendId) == "" {
			return "", fmt.Errorf("schedule_single_send needs a single send ID")
		}
		// "now" is accepted in place of a timestamp.
		when := firstNonEmpty(sub(d.SendGridSendAt), "now")
		return sendgridCall(ctx, key, http.MethodPut,
			"/marketing/singlesends/"+sub(d.SendGridSingleSendId)+"/schedule",
			map[string]any{"send_at": when})

	case "delete_single_send":
		if sub(d.SendGridSingleSendId) == "" {
			return "", fmt.Errorf("delete_single_send needs a single send ID")
		}
		return sendgridCall(ctx, key, http.MethodDelete,
			"/marketing/singlesends/"+sub(d.SendGridSingleSendId), nil)

	// ---- templates ----
	case "list_templates":
		return sendgridCall(ctx, key, http.MethodGet,
			fmt.Sprintf("/templates?generations=dynamic&page_size=%d", limit), nil)

	case "get_template":
		if sub(d.SendGridTemplateId) == "" {
			return "", fmt.Errorf("get_template needs a template ID")
		}
		return sendgridCall(ctx, key, http.MethodGet, "/templates/"+sub(d.SendGridTemplateId), nil)

	case "create_template":
		if sub(d.SendGridName) == "" {
			return "", fmt.Errorf("create_template needs a name")
		}
		return sendgridCall(ctx, key, http.MethodPost, "/templates",
			map[string]any{"name": sub(d.SendGridName), "generation": "dynamic"})

	case "delete_template":
		if sub(d.SendGridTemplateId) == "" {
			return "", fmt.Errorf("delete_template needs a template ID")
		}
		return sendgridCall(ctx, key, http.MethodDelete, "/templates/"+sub(d.SendGridTemplateId), nil)

	// ---- suppressions ----
	case "list_bounces":
		return sendgridCall(ctx, key, http.MethodGet, "/suppression/bounces", nil)

	case "list_blocks":
		return sendgridCall(ctx, key, http.MethodGet, "/suppression/blocks", nil)

	case "list_spam_reports":
		return sendgridCall(ctx, key, http.MethodGet, "/suppression/spam_reports", nil)

	case "list_invalid_emails":
		return sendgridCall(ctx, key, http.MethodGet, "/suppression/invalid_emails", nil)

	case "list_global_unsubscribes":
		return sendgridCall(ctx, key, http.MethodGet, "/asm/suppressions/global", nil)

	case "add_global_unsubscribe":
		emails := splitCSV(sub(d.SendGridEmail))
		if len(emails) == 0 {
			return "", fmt.Errorf("add_global_unsubscribe needs at least one email address")
		}
		return sendgridCall(ctx, key, http.MethodPost, "/asm/suppressions/global",
			map[string]any{"recipient_emails": emails})

	case "delete_bounce":
		if sub(d.SendGridEmail) == "" {
			return "", fmt.Errorf("delete_bounce needs an email address")
		}
		return sendgridCall(ctx, key, http.MethodDelete,
			"/suppression/bounces/"+url.PathEscape(sub(d.SendGridEmail)), nil)

	case "delete_global_unsubscribe":
		if sub(d.SendGridEmail) == "" {
			return "", fmt.Errorf("delete_global_unsubscribe needs an email address")
		}
		return sendgridCall(ctx, key, http.MethodDelete,
			"/asm/suppressions/global/"+url.PathEscape(sub(d.SendGridEmail)), nil)

	// ---- stats & account ----
	case "get_stats":
		if sub(d.SendGridStartDate) == "" {
			return "", fmt.Errorf("get_stats needs a start date (YYYY-MM-DD)")
		}
		q := url.Values{"start_date": {sub(d.SendGridStartDate)}}
		if v := sub(d.SendGridEndDate); v != "" {
			q.Set("end_date", v)
		}
		if v := sub(d.SendGridAggregate); v != "" {
			q.Set("aggregated_by", v)
		}
		raw, err := sendgridCall(ctx, key, http.MethodGet, "/stats?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_verified_senders":
		return sendgridCall(ctx, key, http.MethodGet, "/verified_senders", nil)

	case "list_custom_fields":
		return sendgridCall(ctx, key, http.MethodGet, "/marketing/field_definitions", nil)

	case "create_custom_field":
		if sub(d.SendGridName) == "" {
			return "", fmt.Errorf("create_custom_field needs a name")
		}
		return sendgridCall(ctx, key, http.MethodPost, "/marketing/field_definitions",
			map[string]any{
				"name":       sub(d.SendGridName),
				"field_type": sendgridFieldType(sub(d.SendGridFieldType)),
			})

	case "get_account":
		return sendgridCall(ctx, key, http.MethodGet, "/user/profile", nil)

	case "list_key_scopes":
		// What this key is actually allowed to do — the fastest way to explain a 403.
		return sendgridCall(ctx, key, http.MethodGet, "/scopes", nil)

	case "":
		return "", fmt.Errorf("no SendGrid operation selected")
	}
	return "", fmt.Errorf("unsupported SendGrid operation: %s", d.IntegrationOp)
}

// sendgridMailPayload builds a v3 mail/send body. The nesting is unusual: a
// personalizations array carries the recipients, while from/content sit at the
// top level.
func sendgridMailPayload(d FlowNodeData, sub func(string) string) (map[string]any, error) {
	to := splitCSV(sub(d.SendGridTo))
	if len(to) == 0 {
		return nil, fmt.Errorf("send_email needs at least one recipient")
	}
	if sub(d.SendGridFrom) == "" {
		return nil, fmt.Errorf("send_email needs a from address that is a verified sender in SendGrid")
	}
	addrs := func(list []string) []any {
		out := make([]any, 0, len(list))
		for _, a := range list {
			out = append(out, map[string]string{"email": a})
		}
		return out
	}
	personalization := map[string]any{"to": addrs(to)}
	if v := splitCSV(sub(d.SendGridCc)); len(v) > 0 {
		personalization["cc"] = addrs(v)
	}
	if v := splitCSV(sub(d.SendGridBcc)); len(v) > 0 {
		personalization["bcc"] = addrs(v)
	}

	payload := map[string]any{
		"from":             map[string]string{"email": sub(d.SendGridFrom)},
		"personalizations": []any{personalization},
	}
	if v := sub(d.SendGridReplyTo); v != "" {
		payload["reply_to"] = map[string]string{"email": v}
	}

	if id := sub(d.SendGridTemplateId); id != "" {
		// A dynamic template supplies subject and body, so neither is sent; the
		// variables ride on the personalization instead.
		payload["template_id"] = id
		if vars := strings.TrimSpace(sub(d.SendGridTemplateData)); vars != "" {
			var m map[string]any
			if json.Unmarshal([]byte(vars), &m) != nil {
				return nil, fmt.Errorf(`template data must be a JSON object, e.g. {"name":"Jane"}`)
			}
			personalization["dynamic_template_data"] = m
		}
		return payload, nil
	}

	if sub(d.SendGridSubject) == "" {
		return nil, fmt.Errorf("send_email needs a subject, or a template ID that supplies one")
	}
	payload["subject"] = sub(d.SendGridSubject)
	content := []any{}
	// SendGrid requires text/plain before text/html when both are present.
	if v := sub(d.SendGridText); v != "" {
		content = append(content, map[string]string{"type": "text/plain", "value": v})
	}
	if v := sub(d.SendGridHtml); v != "" {
		content = append(content, map[string]string{"type": "text/html", "value": v})
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("send_email needs an HTML body, a text body, or a template ID")
	}
	payload["content"] = content

	if v := sub(d.SendGridSendAt); v != "" {
		// send_at is a unix timestamp, and SendGrid refuses anything more than
		// 72 hours out.
		ts, err := atoiSafe(v)
		if err != nil {
			return nil, fmt.Errorf("send at must be a unix timestamp in seconds, at most 72 hours ahead")
		}
		payload["send_at"] = ts
	}
	return payload, nil
}

// sendgridFieldType normalizes a custom-field type to the exact casing SendGrid
// accepts; anything unrecognized becomes Text rather than failing the call.
func sendgridFieldType(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "number":
		return "Number"
	case "date":
		return "Date"
	default:
		return "Text"
	}
}
