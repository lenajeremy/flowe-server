package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Microsoft Graph (Outlook mail + calendar).

const graphBase = "https://graph.microsoft.com/v1.0"

func graphCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, graphBase+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("outlook request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			return "", fmt.Errorf("Outlook API error (%d): %s", resp.StatusCode, e.Error.Message)
		}
		return "", fmt.Errorf("Outlook API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return string(raw), nil
}

func graphRecipients(csv string) []map[string]any {
	addrs := splitCSV(csv)
	out := make([]map[string]any, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, map[string]any{"emailAddress": map[string]string{"address": a}})
	}
	return out
}

func runOutlook(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	switch d.IntegrationOp {
	case "send_email":
		msg := map[string]any{
			"subject":      sub(d.OutlookSubject),
			"body":         map[string]string{"contentType": "HTML", "content": sub(d.OutlookBody)},
			"toRecipients": graphRecipients(sub(d.OutlookTo)),
		}
		if cc := sub(d.OutlookCc); cc != "" {
			msg["ccRecipients"] = graphRecipients(cc)
		}
		// sendMail returns 202 with an empty body.
		if _, err := graphCall(ctx, token, http.MethodPost, "/me/sendMail",
			map[string]any{"message": msg, "saveToSentItems": true}); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"status":"sent","to":%s}`, jsonString(sub(d.OutlookTo))), nil

	case "list_messages":
		q := url.Values{}
		q.Set("$top", fmt.Sprint(intOr(d.OutlookLimit, 10)))
		q.Set("$select", "id,subject,from,receivedDateTime,bodyPreview")
		if query := sub(d.OutlookQuery); query != "" {
			q.Set("$search", `"`+query+`"`)
		} else {
			q.Set("$orderby", "receivedDateTime desc")
		}
		raw, err := graphCall(ctx, token, http.MethodGet, "/me/messages?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Value []struct {
				ID      string `json:"id"`
				Subject string `json:"subject"`
				Preview string `json:"bodyPreview"`
				From    struct {
					EmailAddress struct {
						Address string `json:"address"`
					} `json:"emailAddress"`
				} `json:"from"`
			} `json:"value"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		out := make([]map[string]any, 0, len(res.Value))
		for _, m := range res.Value {
			out = append(out, map[string]any{
				"id":      m.ID,
				"subject": m.Subject,
				"from":    m.From.EmailAddress.Address,
				"preview": m.Preview,
			})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "get_message":
		raw, err := graphCall(ctx, token, http.MethodGet,
			"/me/messages/"+url.PathEscape(sub(d.OutlookMessageId))+"?$select=id,subject,from,body,receivedDateTime", nil)
		if err != nil {
			return "", err
		}
		var m struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
			From    struct {
				EmailAddress struct {
					Address string `json:"address"`
				} `json:"emailAddress"`
			} `json:"from"`
			Body struct {
				Content string `json:"content"`
			} `json:"body"`
		}
		_ = json.Unmarshal([]byte(raw), &m)
		b, _ := json.Marshal(map[string]any{"id": m.ID, "subject": m.Subject, "from": m.From.EmailAddress.Address, "body": m.Body.Content})
		return string(b), nil

	case "create_event":
		event := map[string]any{
			"subject": sub(d.OutlookSubject),
			"body":    map[string]string{"contentType": "HTML", "content": sub(d.OutlookBody)},
			"start":   map[string]string{"dateTime": sub(d.OutlookStart), "timeZone": "UTC"},
			"end":     map[string]string{"dateTime": sub(d.OutlookEnd), "timeZone": "UTC"},
		}
		raw, err := graphCall(ctx, token, http.MethodPost, "/me/events", event)
		if err != nil {
			return "", err
		}
		var created struct {
			ID      string `json:"id"`
			WebLink string `json:"webLink"`
		}
		_ = json.Unmarshal([]byte(raw), &created)
		b, _ := json.Marshal(map[string]any{"status": "created", "id": created.ID, "link": created.WebLink})
		return string(b), nil

	default:
		return "", fmt.Errorf("unknown Outlook operation: %s", d.IntegrationOp)
	}
}
