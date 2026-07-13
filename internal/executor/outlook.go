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

	case "reply_to_message":
		// createReply-and-send in one call; comment is prepended to the quoted
		// original by Graph.
		if _, err := graphCall(ctx, token, http.MethodPost,
			"/me/messages/"+url.PathEscape(sub(d.OutlookMessageId))+"/reply",
			map[string]any{"comment": sub(d.OutlookComment)}); err != nil {
			return "", err
		}
		return `{"status":"replied"}`, nil

	case "forward_message":
		if _, err := graphCall(ctx, token, http.MethodPost,
			"/me/messages/"+url.PathEscape(sub(d.OutlookMessageId))+"/forward",
			map[string]any{
				"comment":      sub(d.OutlookComment),
				"toRecipients": graphRecipients(sub(d.OutlookTo)),
			}); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": "forwarded", "to": sub(d.OutlookTo)})
		return string(b), nil

	case "create_draft":
		msg := map[string]any{
			"subject":      sub(d.OutlookSubject),
			"body":         map[string]string{"contentType": "HTML", "content": sub(d.OutlookBody)},
			"toRecipients": graphRecipients(sub(d.OutlookTo)),
		}
		if cc := sub(d.OutlookCc); cc != "" {
			msg["ccRecipients"] = graphRecipients(cc)
		}
		raw, err := graphCall(ctx, token, http.MethodPost, "/me/messages", msg)
		if err != nil {
			return "", err
		}
		var created struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &created)
		b, _ := json.Marshal(map[string]any{"status": "draft_created", "id": created.ID})
		return string(b), nil

	case "move_message":
		raw, err := graphCall(ctx, token, http.MethodPost,
			"/me/messages/"+url.PathEscape(sub(d.OutlookMessageId))+"/move",
			map[string]any{"destinationId": sub(d.OutlookFolderId)})
		if err != nil {
			return "", err
		}
		var moved struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &moved)
		b, _ := json.Marshal(map[string]any{"status": "moved", "id": moved.ID})
		return string(b), nil

	case "mark_read":
		if _, err := graphCall(ctx, token, http.MethodPatch,
			"/me/messages/"+url.PathEscape(sub(d.OutlookMessageId)),
			map[string]any{"isRead": true}); err != nil {
			return "", err
		}
		return `{"status":"marked_read"}`, nil

	case "flag_message":
		if _, err := graphCall(ctx, token, http.MethodPatch,
			"/me/messages/"+url.PathEscape(sub(d.OutlookMessageId)),
			map[string]any{"flag": map[string]string{"flagStatus": "flagged"}}); err != nil {
			return "", err
		}
		return `{"status":"flagged"}`, nil

	case "delete_message":
		// Graph DELETE moves to Deleted Items (soft delete).
		if _, err := graphCall(ctx, token, http.MethodDelete,
			"/me/messages/"+url.PathEscape(sub(d.OutlookMessageId)), nil); err != nil {
			return "", err
		}
		return `{"status":"deleted"}`, nil

	case "list_folders":
		raw, err := graphCall(ctx, token, http.MethodGet, "/me/mailFolders?$top=50&$select=id,displayName,totalItemCount", nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Value []struct {
				ID    string `json:"id"`
				Name  string `json:"displayName"`
				Count int    `json:"totalItemCount"`
			} `json:"value"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		out := make([]map[string]any, 0, len(res.Value))
		for _, f := range res.Value {
			out = append(out, map[string]any{"id": f.ID, "name": f.Name, "messages": f.Count})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "list_events":
		q := url.Values{}
		q.Set("$top", fmt.Sprint(intOr(d.OutlookLimit, 10)))
		q.Set("$select", "id,subject,start,end,organizer,webLink")
		q.Set("$orderby", "start/dateTime")
		path := "/me/events?" + q.Encode()
		if start, end := sub(d.OutlookStart), sub(d.OutlookEnd); start != "" && end != "" {
			// calendarView expands recurring events within the window
			path = "/me/calendarView?startDateTime=" + url.QueryEscape(start) +
				"&endDateTime=" + url.QueryEscape(end) + "&" + q.Encode()
		}
		raw, err := graphCall(ctx, token, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Value []struct {
				ID      string `json:"id"`
				Subject string `json:"subject"`
				WebLink string `json:"webLink"`
				Start   struct {
					DateTime string `json:"dateTime"`
				} `json:"start"`
				End struct {
					DateTime string `json:"dateTime"`
				} `json:"end"`
			} `json:"value"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		out := make([]map[string]any, 0, len(res.Value))
		for _, e := range res.Value {
			out = append(out, map[string]any{
				"id": e.ID, "subject": e.Subject,
				"start": e.Start.DateTime, "end": e.End.DateTime, "link": e.WebLink,
			})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "update_event":
		patch := map[string]any{}
		if v := sub(d.OutlookSubject); v != "" {
			patch["subject"] = v
		}
		if v := sub(d.OutlookBody); v != "" {
			patch["body"] = map[string]string{"contentType": "HTML", "content": v}
		}
		if v := sub(d.OutlookStart); v != "" {
			patch["start"] = map[string]string{"dateTime": v, "timeZone": "UTC"}
		}
		if v := sub(d.OutlookEnd); v != "" {
			patch["end"] = map[string]string{"dateTime": v, "timeZone": "UTC"}
		}
		if len(patch) == 0 {
			return "", fmt.Errorf("Outlook: nothing to update — set a subject, body, start, or end")
		}
		if _, err := graphCall(ctx, token, http.MethodPatch,
			"/me/events/"+url.PathEscape(sub(d.OutlookEventId)), patch); err != nil {
			return "", err
		}
		return `{"status":"updated"}`, nil

	case "delete_event":
		if _, err := graphCall(ctx, token, http.MethodDelete,
			"/me/events/"+url.PathEscape(sub(d.OutlookEventId)), nil); err != nil {
			return "", err
		}
		return `{"status":"deleted"}`, nil

	case "respond_to_event":
		resp := sub(d.OutlookResponse)
		switch resp {
		case "accept", "decline", "tentativelyAccept":
		default:
			return "", fmt.Errorf("Outlook: response must be accept, decline, or tentativelyAccept")
		}
		if _, err := graphCall(ctx, token, http.MethodPost,
			"/me/events/"+url.PathEscape(sub(d.OutlookEventId))+"/"+resp,
			map[string]any{"comment": sub(d.OutlookComment), "sendResponse": true}); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": resp})
		return string(b), nil

	case "list_contacts":
		q := url.Values{}
		q.Set("$top", fmt.Sprint(intOr(d.OutlookLimit, 25)))
		q.Set("$select", "id,displayName,emailAddresses")
		if query := sub(d.OutlookQuery); query != "" {
			q.Set("$search", `"`+query+`"`)
		}
		raw, err := graphCall(ctx, token, http.MethodGet, "/me/contacts?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Value []struct {
				ID     string `json:"id"`
				Name   string `json:"displayName"`
				Emails []struct {
					Address string `json:"address"`
				} `json:"emailAddresses"`
			} `json:"value"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		out := make([]map[string]any, 0, len(res.Value))
		for _, c := range res.Value {
			email := ""
			if len(c.Emails) > 0 {
				email = c.Emails[0].Address
			}
			out = append(out, map[string]any{"id": c.ID, "name": c.Name, "email": email})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "create_contact":
		name := sub(d.OutlookContactName)
		contact := map[string]any{
			"displayName": name,
			"emailAddresses": []map[string]any{
				{"address": sub(d.OutlookContactEmail), "name": name},
			},
		}
		raw, err := graphCall(ctx, token, http.MethodPost, "/me/contacts", contact)
		if err != nil {
			return "", err
		}
		var created struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &created)
		b, _ := json.Marshal(map[string]any{"status": "created", "id": created.ID, "name": name})
		return string(b), nil

	default:
		return "", fmt.Errorf("unknown Outlook operation: %s", d.IntegrationOp)
	}
}
