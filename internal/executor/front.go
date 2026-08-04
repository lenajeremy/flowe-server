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

// Front's Core API.
//
// The idea to hold onto is that Front distinguishes a *message* from a *comment*.
// A message goes to the customer; a comment is an internal note visible only to
// teammates. They are separate endpoints with near-identical shapes, so mixing
// them up sends an internal remark to the person you were talking about. The op
// names here say which is which rather than leaving it to a flag.
//
// Sending also splits by intent: a new outbound conversation posts to a channel,
// while a reply posts to the conversation. Front rejects the wrong one rather than
// guessing.

const frontAPI = "https://api2.frontapp.com"

func frontCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, frontAPI+path, reader)
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
		return "", fmt.Errorf("front request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Title   string   `json:"title"`
				Message string   `json:"message"`
				Details []string `json:"details"`
			} `json:"_error"`
		}
		var parts []string
		if json.Unmarshal(raw, &e) == nil {
			if e.Error.Message != "" {
				parts = append(parts, e.Error.Message)
			} else if e.Error.Title != "" {
				parts = append(parts, e.Error.Title)
			}
			parts = append(parts, e.Error.Details...)
		}
		msg := strings.Join(parts, "; ")
		if msg == "" {
			msg = truncateStr(string(raw), 300)
		}
		switch resp.StatusCode {
		case http.StatusForbidden:
			msg += " — the connection is missing a scope for this resource; reconnect Front to grant it"
		case http.StatusUnprocessableEntity:
			// Almost always a channel that cannot send, or a missing author.
			msg += " — check the channel can send and that the author is a teammate of it"
		}
		return "", fmt.Errorf("Front API error (%d): %s", resp.StatusCode, msg)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

func runFront(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.FrontLimit, 25)
	conv := func() string { return url.PathEscape(sub(d.FrontConversationId)) }
	need := func(label, v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("this operation needs %s", label)
		}
		return nil
	}
	page := func(path string) string {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		p := fmt.Sprintf("%s%slimit=%d", path, sep, limit)
		if v := sub(d.FrontPageToken); v != "" {
			p += "&page_token=" + url.QueryEscape(v)
		}
		return p
	}

	switch d.IntegrationOp {
	// ---- conversations ----
	case "list_conversations":
		path := "/conversations"
		// An inbox or tag scopes the listing; otherwise it is everything visible.
		if v := sub(d.FrontInboxId); v != "" {
			path = "/inboxes/" + url.PathEscape(v) + "/conversations"
		} else if v := sub(d.FrontTagId); v != "" {
			path = "/tags/" + url.PathEscape(v) + "/conversations"
		}
		if q := sub(d.FrontQuery); q != "" {
			path += "?q=" + url.QueryEscape(q)
		}
		raw, err := frontCall(ctx, token, http.MethodGet, page(path), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "search_conversations":
		// Front's search takes its query inside the path, not as a parameter.
		if err := need("a search query", sub(d.FrontQuery)); err != nil {
			return "", err
		}
		raw, err := frontCall(ctx, token, http.MethodGet,
			page("/conversations/search/"+url.PathEscape(sub(d.FrontQuery))), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "get_conversation":
		if err := need("a conversation ID", sub(d.FrontConversationId)); err != nil {
			return "", err
		}
		return frontCall(ctx, token, http.MethodGet, "/conversations/"+conv(), nil)

	case "update_conversation":
		if err := need("a conversation ID", sub(d.FrontConversationId)); err != nil {
			return "", err
		}
		body := map[string]any{}
		if v := sub(d.FrontStatus); v != "" {
			// archived, open, deleted or spam.
			body["status"] = v
		}
		if v := sub(d.FrontAssigneeId); v != "" {
			body["assignee_id"] = v
		}
		if v := sub(d.FrontInboxId); v != "" {
			body["inbox_id"] = v
		}
		if len(body) == 0 {
			return "", fmt.Errorf("update_conversation needs a status, assignee or inbox to change")
		}
		if _, err := frontCall(ctx, token, http.MethodPatch, "/conversations/"+conv(), body); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"conversation":%q}`, sub(d.FrontConversationId)), nil

	case "assign_conversation":
		if err := need("a conversation ID", sub(d.FrontConversationId)); err != nil {
			return "", err
		}
		// An explicit null unassigns, which an empty string would not.
		var assignee any
		if v := sub(d.FrontAssigneeId); v != "" {
			assignee = v
		}
		if _, err := frontCall(ctx, token, http.MethodPatch, "/conversations/"+conv(),
			map[string]any{"assignee_id": assignee}); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"conversation":%q,"assignee":%q}`,
			sub(d.FrontConversationId), sub(d.FrontAssigneeId)), nil

	case "list_conversation_messages":
		if err := need("a conversation ID", sub(d.FrontConversationId)); err != nil {
			return "", err
		}
		raw, err := frontCall(ctx, token, http.MethodGet,
			page("/conversations/"+conv()+"/messages"), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	// ---- sending: customer-facing ----
	case "send_message":
		// A new outbound conversation, which must go through a channel.
		if err := need("a channel ID", sub(d.FrontChannelId)); err != nil {
			return "", err
		}
		to := splitCSV(sub(d.FrontTo))
		if len(to) == 0 {
			return "", fmt.Errorf("send_message needs at least one recipient")
		}
		body := map[string]any{"to": to, "body": sub(d.FrontBody)}
		if v := sub(d.FrontSubject); v != "" {
			body["subject"] = v
		}
		if v := splitCSV(sub(d.FrontCc)); len(v) > 0 {
			body["cc"] = v
		}
		if v := splitCSV(sub(d.FrontBcc)); len(v) > 0 {
			body["bcc"] = v
		}
		if v := sub(d.FrontAuthorId); v != "" {
			body["author_id"] = v
		}
		return frontCall(ctx, token, http.MethodPost,
			"/channels/"+url.PathEscape(sub(d.FrontChannelId))+"/messages", body)

	case "reply_to_conversation":
		// A reply threads into the existing conversation and reaches the customer.
		if err := need("a conversation ID", sub(d.FrontConversationId)); err != nil {
			return "", err
		}
		if err := need("a message body", sub(d.FrontBody)); err != nil {
			return "", err
		}
		body := map[string]any{"body": sub(d.FrontBody)}
		if v := sub(d.FrontAuthorId); v != "" {
			body["author_id"] = v
		}
		if v := sub(d.FrontChannelId); v != "" {
			body["channel_id"] = v
		}
		if v := splitCSV(sub(d.FrontTo)); len(v) > 0 {
			body["to"] = v
		}
		return frontCall(ctx, token, http.MethodPost,
			"/conversations/"+conv()+"/messages", body)

	case "create_draft":
		if err := need("a conversation ID", sub(d.FrontConversationId)); err != nil {
			return "", err
		}
		if err := need("an author ID", sub(d.FrontAuthorId)); err != nil {
			return "", err
		}
		body := map[string]any{
			"body":      sub(d.FrontBody),
			"author_id": sub(d.FrontAuthorId),
		}
		if v := sub(d.FrontChannelId); v != "" {
			body["channel_id"] = v
		}
		return frontCall(ctx, token, http.MethodPost, "/conversations/"+conv()+"/drafts", body)

	// ---- commenting: internal only ----
	case "add_comment":
		// Internal. Never delivered to the customer, unlike reply_to_conversation.
		if err := need("a conversation ID", sub(d.FrontConversationId)); err != nil {
			return "", err
		}
		if err := need("comment text", sub(d.FrontBody)); err != nil {
			return "", err
		}
		body := map[string]any{"body": sub(d.FrontBody)}
		if v := sub(d.FrontAuthorId); v != "" {
			body["author_id"] = v
		}
		return frontCall(ctx, token, http.MethodPost, "/conversations/"+conv()+"/comments", body)

	case "list_comments":
		if err := need("a conversation ID", sub(d.FrontConversationId)); err != nil {
			return "", err
		}
		return frontCall(ctx, token, http.MethodGet, page("/conversations/"+conv()+"/comments"), nil)

	// ---- tags ----
	case "list_tags":
		return frontCall(ctx, token, http.MethodGet, page("/tags"), nil)

	case "add_tags":
		ids := splitCSV(sub(d.FrontTagId))
		if err := need("a conversation ID", sub(d.FrontConversationId)); err != nil {
			return "", err
		}
		if len(ids) == 0 {
			return "", fmt.Errorf("add_tags needs at least one tag ID")
		}
		if _, err := frontCall(ctx, token, http.MethodPost, "/conversations/"+conv()+"/tags",
			map[string]any{"tag_ids": ids}); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"tagged":%d}`, len(ids)), nil

	case "remove_tags":
		ids := splitCSV(sub(d.FrontTagId))
		if err := need("a conversation ID", sub(d.FrontConversationId)); err != nil {
			return "", err
		}
		if len(ids) == 0 {
			return "", fmt.Errorf("remove_tags needs at least one tag ID")
		}
		if _, err := frontCall(ctx, token, http.MethodDelete, "/conversations/"+conv()+"/tags",
			map[string]any{"tag_ids": ids}); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"untagged":%d}`, len(ids)), nil

	case "create_tag":
		if err := need("a tag name", sub(d.FrontName)); err != nil {
			return "", err
		}
		return frontCall(ctx, token, http.MethodPost, "/tags",
			map[string]any{"name": sub(d.FrontName)})

	// ---- contacts ----
	case "list_contacts":
		raw, err := frontCall(ctx, token, http.MethodGet, page("/contacts"), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "get_contact":
		if err := need("a contact ID", sub(d.FrontContactId)); err != nil {
			return "", err
		}
		return frontCall(ctx, token, http.MethodGet,
			"/contacts/"+url.PathEscape(sub(d.FrontContactId)), nil)

	case "create_contact":
		body := map[string]any{}
		if v := sub(d.FrontName); v != "" {
			body["name"] = v
		}
		if v := sub(d.FrontDescription); v != "" {
			body["description"] = v
		}
		// A contact is identified by its handles — an address on some channel.
		handles := []any{}
		for _, h := range splitCSV(sub(d.FrontHandle)) {
			handles = append(handles, map[string]any{
				"handle": h,
				"source": firstNonEmpty(sub(d.FrontHandleSource), "email"),
			})
		}
		if len(handles) == 0 {
			return "", fmt.Errorf("create_contact needs at least one handle, e.g. an email address")
		}
		body["handles"] = handles
		return frontCall(ctx, token, http.MethodPost, "/contacts", body)

	case "update_contact":
		if err := need("a contact ID", sub(d.FrontContactId)); err != nil {
			return "", err
		}
		body := map[string]any{}
		if v := sub(d.FrontName); v != "" {
			body["name"] = v
		}
		if v := sub(d.FrontDescription); v != "" {
			body["description"] = v
		}
		if len(body) == 0 {
			return "", fmt.Errorf("update_contact needs a name or description to change")
		}
		if _, err := frontCall(ctx, token, http.MethodPatch,
			"/contacts/"+url.PathEscape(sub(d.FrontContactId)), body); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"contact":%q}`, sub(d.FrontContactId)), nil

	case "delete_contact":
		if err := need("a contact ID", sub(d.FrontContactId)); err != nil {
			return "", err
		}
		return frontCall(ctx, token, http.MethodDelete,
			"/contacts/"+url.PathEscape(sub(d.FrontContactId)), nil)

	case "add_contact_handle":
		if err := need("a contact ID and a handle",
			sub(d.FrontContactId)+sub(d.FrontHandle)); err != nil {
			return "", err
		}
		return frontCall(ctx, token, http.MethodPost,
			"/contacts/"+url.PathEscape(sub(d.FrontContactId))+"/handles",
			map[string]any{
				"handle": sub(d.FrontHandle),
				"source": firstNonEmpty(sub(d.FrontHandleSource), "email"),
			})

	// ---- workspace ----
	case "list_inboxes":
		return frontCall(ctx, token, http.MethodGet, page("/inboxes"), nil)

	case "list_channels":
		return frontCall(ctx, token, http.MethodGet, page("/channels"), nil)

	case "list_teammates":
		return frontCall(ctx, token, http.MethodGet, page("/teammates"), nil)

	case "get_teammate":
		if err := need("a teammate ID", sub(d.FrontTeammateId)); err != nil {
			return "", err
		}
		return frontCall(ctx, token, http.MethodGet,
			"/teammates/"+url.PathEscape(sub(d.FrontTeammateId)), nil)

	case "list_teams":
		return frontCall(ctx, token, http.MethodGet, "/teams", nil)

	case "list_accounts":
		return frontCall(ctx, token, http.MethodGet, page("/accounts"), nil)

	case "list_events":
		// The audit stream, useful for reacting to what changed since a last run.
		path := "/events"
		if v := sub(d.FrontQuery); v != "" {
			path += "?q[types]=" + url.QueryEscape(v)
		}
		raw, err := frontCall(ctx, token, http.MethodGet, page(path), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "list_links":
		return frontCall(ctx, token, http.MethodGet, page("/links"), nil)

	case "create_link":
		if err := need("a URL", sub(d.FrontUrl)); err != nil {
			return "", err
		}
		body := map[string]any{"external_url": sub(d.FrontUrl)}
		if v := sub(d.FrontName); v != "" {
			body["name"] = v
		}
		return frontCall(ctx, token, http.MethodPost, "/links", body)

	case "link_conversation":
		if err := need("a conversation ID and a link ID",
			sub(d.FrontConversationId)+sub(d.FrontLinkId)); err != nil {
			return "", err
		}
		if _, err := frontCall(ctx, token, http.MethodPost, "/conversations/"+conv()+"/links",
			map[string]any{"link_ids": splitCSV(sub(d.FrontLinkId))}); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"conversation":%q}`, sub(d.FrontConversationId)), nil

	case "":
		return "", fmt.Errorf("no Front operation selected")
	}
	return "", fmt.Errorf("unsupported Front operation: %s", d.IntegrationOp)
}
