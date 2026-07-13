package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Gmail API (users.me). Messages are sent as base64url-encoded RFC 2822.

const gmailBase = "https://gmail.googleapis.com/gmail/v1/users/me"

func gmailCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, gmailBase+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("gmail request failed: %w", err)
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
			return "", fmt.Errorf("Gmail API error (%d): %s", resp.StatusCode, e.Error.Message)
		}
		return "", fmt.Errorf("Gmail API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return string(raw), nil
}

func runGmail(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	switch d.IntegrationOp {
	case "send_email":
		raw := gmailBuildRaw(sub(d.GmailTo), sub(d.GmailCc), sub(d.GmailSubject), sub(d.GmailBody))
		if _, err := gmailCall(ctx, token, http.MethodPost, "/messages/send", map[string]any{"raw": raw}); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"status":"sent","to":%s}`, jsonString(sub(d.GmailTo))), nil

	case "create_draft":
		raw := gmailBuildRaw(sub(d.GmailTo), sub(d.GmailCc), sub(d.GmailSubject), sub(d.GmailBody))
		if _, err := gmailCall(ctx, token, http.MethodPost, "/drafts", map[string]any{"message": map[string]any{"raw": raw}}); err != nil {
			return "", err
		}
		return `{"status":"draft_created"}`, nil

	case "list_messages":
		q := url.Values{"maxResults": {fmt.Sprint(intOr(d.GmailLimit, 10))}}
		if query := sub(d.GmailQuery); query != "" {
			q.Set("q", query)
		}
		listRaw, err := gmailCall(ctx, token, http.MethodGet, "/messages?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var list struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
		}
		_ = json.Unmarshal([]byte(listRaw), &list)
		out := make([]map[string]any, 0, len(list.Messages))
		for _, m := range list.Messages {
			metaRaw, err := gmailCall(ctx, token, http.MethodGet,
				"/messages/"+m.ID+"?format=metadata&metadataHeaders=From&metadataHeaders=Subject", nil)
			if err != nil {
				continue
			}
			from, subject := gmailHeaders(metaRaw)
			out = append(out, map[string]any{"id": m.ID, "from": from, "subject": subject, "snippet": gmailSnippet(metaRaw)})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "get_message":
		raw, err := gmailCall(ctx, token, http.MethodGet, "/messages/"+sub(d.GmailMessageId)+"?format=full", nil)
		if err != nil {
			return "", err
		}
		from, subject := gmailHeaders(raw)
		body := gmailPlainText(raw)
		if body == "" {
			body = gmailSnippet(raw)
		}
		b, _ := json.Marshal(map[string]any{"id": sub(d.GmailMessageId), "from": from, "subject": subject, "body": body})
		return string(b), nil

	case "reply_to_message":
		// Fetch the original for its thread id + Message-ID so the reply
		// threads correctly in every client.
		id := sub(d.GmailMessageId)
		origRaw, err := gmailCall(ctx, token, http.MethodGet,
			"/messages/"+id+"?format=metadata&metadataHeaders=From&metadataHeaders=Subject&metadataHeaders=Message-ID", nil)
		if err != nil {
			return "", err
		}
		var orig struct {
			ThreadID string `json:"threadId"`
			Payload  struct {
				Headers []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"headers"`
			} `json:"payload"`
		}
		_ = json.Unmarshal([]byte(origRaw), &orig)
		var from, subject, msgID string
		for _, h := range orig.Payload.Headers {
			switch h.Name {
			case "From":
				from = h.Value
			case "Subject":
				subject = h.Value
			case "Message-ID", "Message-Id":
				msgID = h.Value
			}
		}
		if !strings.HasPrefix(strings.ToLower(subject), "re:") {
			subject = "Re: " + subject
		}
		to := firstNonEmpty(sub(d.GmailTo), from)
		raw := gmailBuildRawReply(to, subject, sub(d.GmailBody), msgID)
		if _, err := gmailCall(ctx, token, http.MethodPost, "/messages/send",
			map[string]any{"raw": raw, "threadId": orig.ThreadID}); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": "replied", "to": to, "threadId": orig.ThreadID})
		return string(b), nil

	case "get_thread":
		raw, err := gmailCall(ctx, token, http.MethodGet, "/threads/"+sub(d.GmailThreadId)+"?format=full", nil)
		if err != nil {
			return "", err
		}
		var th struct {
			Messages []json.RawMessage `json:"messages"`
		}
		_ = json.Unmarshal([]byte(raw), &th)
		out := make([]map[string]any, 0, len(th.Messages))
		for _, m := range th.Messages {
			from, subject := gmailHeaders(string(m))
			body := gmailPlainText(string(m))
			if body == "" {
				body = gmailSnippet(string(m))
			}
			out = append(out, map[string]any{"from": from, "subject": subject, "body": truncateStr(body, 2000)})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "list_labels":
		raw, err := gmailCall(ctx, token, http.MethodGet, "/labels", nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Labels []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"labels"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		out := make([]map[string]any, 0, len(res.Labels))
		for _, l := range res.Labels {
			out = append(out, map[string]any{"id": l.ID, "name": l.Name, "type": l.Type})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "create_label":
		raw, err := gmailCall(ctx, token, http.MethodPost, "/labels", map[string]any{
			"name":                  sub(d.GmailLabelName),
			"labelListVisibility":   "labelShow",
			"messageListVisibility": "show",
		})
		if err != nil {
			return "", err
		}
		var l struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal([]byte(raw), &l)
		b, _ := json.Marshal(map[string]any{"status": "created", "id": l.ID, "name": l.Name})
		return string(b), nil

	case "add_label":
		return gmailModify(ctx, token, sub(d.GmailMessageId), []string{sub(d.GmailLabelId)}, nil, "label_added")
	case "remove_label":
		return gmailModify(ctx, token, sub(d.GmailMessageId), nil, []string{sub(d.GmailLabelId)}, "label_removed")
	case "mark_read":
		return gmailModify(ctx, token, sub(d.GmailMessageId), nil, []string{"UNREAD"}, "marked_read")
	case "mark_unread":
		return gmailModify(ctx, token, sub(d.GmailMessageId), []string{"UNREAD"}, nil, "marked_unread")
	case "archive_message":
		return gmailModify(ctx, token, sub(d.GmailMessageId), nil, []string{"INBOX"}, "archived")

	case "trash_message":
		if _, err := gmailCall(ctx, token, http.MethodPost, "/messages/"+sub(d.GmailMessageId)+"/trash", nil); err != nil {
			return "", err
		}
		return `{"status":"trashed"}`, nil

	case "list_drafts":
		q := url.Values{"maxResults": {fmt.Sprint(intOr(d.GmailLimit, 10))}}
		raw, err := gmailCall(ctx, token, http.MethodGet, "/drafts?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Drafts []struct {
				ID      string `json:"id"`
				Message struct {
					ID string `json:"id"`
				} `json:"message"`
			} `json:"drafts"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		out := make([]map[string]any, 0, len(res.Drafts))
		for _, dr := range res.Drafts {
			metaRaw, err := gmailCall(ctx, token, http.MethodGet,
				"/messages/"+dr.Message.ID+"?format=metadata&metadataHeaders=To&metadataHeaders=Subject", nil)
			if err != nil {
				out = append(out, map[string]any{"id": dr.ID})
				continue
			}
			_, subject := gmailHeaders(metaRaw)
			out = append(out, map[string]any{"id": dr.ID, "subject": subject, "snippet": gmailSnippet(metaRaw)})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "send_draft":
		raw, err := gmailCall(ctx, token, http.MethodPost, "/drafts/send", map[string]any{"id": sub(d.GmailDraftId)})
		if err != nil {
			return "", err
		}
		var res struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		b, _ := json.Marshal(map[string]any{"status": "sent", "messageId": res.ID})
		return string(b), nil

	default:
		return "", fmt.Errorf("unknown Gmail operation: %s", d.IntegrationOp)
	}
}

// gmailModify adds/removes label ids on a message (read state, inbox, custom
// labels all ride the same endpoint).
func gmailModify(ctx context.Context, token, messageID string, add, remove []string, status string) (string, error) {
	body := map[string]any{}
	if len(add) > 0 {
		body["addLabelIds"] = add
	}
	if len(remove) > 0 {
		body["removeLabelIds"] = remove
	}
	if _, err := gmailCall(ctx, token, http.MethodPost, "/messages/"+messageID+"/modify", body); err != nil {
		return "", err
	}
	b, _ := json.Marshal(map[string]any{"status": status, "id": messageID})
	return string(b), nil
}

// gmailBuildRawReply is gmailBuildRaw plus threading headers.
func gmailBuildRawReply(to, subject, body, inReplyTo string) string {
	to, subject = stripHeader(to), stripHeader(subject)
	var b strings.Builder
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	if inReplyTo != "" {
		b.WriteString("In-Reply-To: " + stripHeader(inReplyTo) + "\r\n")
		b.WriteString("References: " + stripHeader(inReplyTo) + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n\r\n")
	b.WriteString(body)
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(b.String()))
}

// stripHeader removes CR/LF so a template-supplied value can't inject extra
// SMTP headers (Bcc, etc.) or a premature body — header (CRLF) injection.
func stripHeader(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// gmailBuildRaw assembles a UTF-8 text/plain RFC 2822 message and base64url-
// encodes it (no padding) as the Gmail API expects.
func gmailBuildRaw(to, cc, subject, body string) string {
	to, cc, subject = stripHeader(to), stripHeader(cc), stripHeader(subject)
	var b strings.Builder
	b.WriteString("To: " + to + "\r\n")
	if cc != "" {
		b.WriteString("Cc: " + cc + "\r\n")
	}
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n\r\n")
	b.WriteString(body)
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(b.String()))
}

func gmailHeaders(raw string) (from, subject string) {
	var m struct {
		Payload struct {
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
		} `json:"payload"`
	}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return "", ""
	}
	for _, h := range m.Payload.Headers {
		switch h.Name {
		case "From":
			from = h.Value
		case "Subject":
			subject = h.Value
		}
	}
	return from, subject
}

func gmailSnippet(raw string) string {
	var m struct {
		Snippet string `json:"snippet"`
	}
	_ = json.Unmarshal([]byte(raw), &m)
	return m.Snippet
}

// gmailPlainText walks the MIME parts and decodes the first text/plain body.
func gmailPlainText(raw string) string {
	var m struct {
		Payload gmailPart `json:"payload"`
	}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return ""
	}
	return gmailWalk(m.Payload)
}

type gmailPart struct {
	MimeType string `json:"mimeType"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

func gmailWalk(p gmailPart) string {
	if p.MimeType == "text/plain" && p.Body.Data != "" {
		if dec, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(p.Body.Data); err == nil {
			return string(dec)
		}
	}
	for _, part := range p.Parts {
		if t := gmailWalk(part); t != "" {
			return t
		}
	}
	return ""
}
