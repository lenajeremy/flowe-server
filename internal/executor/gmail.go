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

	default:
		return "", fmt.Errorf("unknown Gmail operation: %s", d.IntegrationOp)
	}
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
