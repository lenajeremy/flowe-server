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
	"time"
)

// Slack Web API. Responses are always HTTP 200; success is signalled by the
// {"ok":bool} field, with the failure reason in "error".

const slackBase = "https://slack.com/api"

// isSlackAccessErr reports whether an error means the bot token can't see the
// conversation (it isn't a member, or lacks the history scope) — the cases
// where retrying with the user's own grant can succeed.
func isSlackAccessErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "channel_not_found") ||
		strings.Contains(msg, "not_in_channel") ||
		strings.Contains(msg, "missing_scope")
}

func slackCall(ctx context.Context, token, method, path string, jsonBody any) ([]byte, error) {
	var reader io.Reader
	if jsonBody != nil {
		b, _ := json.Marshal(jsonBody)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, slackBase+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if jsonBody != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var head struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &head) == nil && !head.OK {
		return nil, fmt.Errorf("Slack API error: %s", firstNonEmpty(head.Error, "unknown"))
	}
	return raw, nil
}

// runSlack executes a Slack node. token is the workspace bot token (xoxb-);
// userToken is the connecting human's grant (xoxp-), "" when the connection
// predates user scopes. Reads use the bot token; sends pick per SlackSendAs,
// and DMs always go out as the user — a bot DM'ing on someone's behalf reads
// as spam, so that op has no bot mode.
func runSlack(ctx context.Context, token, userToken string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	// asUser resolves the sending identity, failing with a reconnect hint when
	// the user grant is missing rather than silently falling back to the bot.
	asUser := func() (string, error) {
		if userToken == "" {
			return "", fmt.Errorf("Slack: sending as you requires re-connecting Slack (the stored connection has no user grant)")
		}
		return userToken, nil
	}

	switch d.IntegrationOp {
	case "send_message":
		sendToken := token
		payload := map[string]any{
			"channel": sub(d.SlackChannel),
			"text":    sub(d.SlackText),
		}
		if d.SlackSendAs == "user" {
			t, err := asUser()
			if err != nil {
				return "", err
			}
			sendToken = t
		} else if d.SlackBotName != "" {
			// Display-name override — bot sends only (chat:write.customize);
			// user-identity messages always show as the human.
			payload["username"] = sub(d.SlackBotName)
		}
		raw, err := slackCall(ctx, sendToken, http.MethodPost, "/chat.postMessage", payload)
		if err != nil {
			return "", err
		}
		var res struct {
			Channel string `json:"channel"`
			TS      string `json:"ts"`
		}
		_ = json.Unmarshal(raw, &res)
		b, _ := json.Marshal(map[string]any{"status": "sent", "channel": res.Channel, "ts": res.TS, "as": firstNonEmpty(d.SlackSendAs, "bot")})
		return string(b), nil

	case "send_dm":
		t, err := asUser()
		if err != nil {
			return "", err
		}
		recipient := sub(d.SlackUserId)
		if recipient == "" {
			return "", fmt.Errorf("Slack: choose a recipient for the direct message")
		}
		// Open (or fetch) the user↔user DM conversation, then post into it.
		raw, err := slackCall(ctx, t, http.MethodPost, "/conversations.open", map[string]any{
			"users": recipient,
		})
		if err != nil {
			return "", err
		}
		var opened struct {
			Channel struct {
				ID string `json:"id"`
			} `json:"channel"`
		}
		_ = json.Unmarshal(raw, &opened)
		if opened.Channel.ID == "" {
			return "", fmt.Errorf("Slack: could not open a DM with %s", recipient)
		}
		raw, err = slackCall(ctx, t, http.MethodPost, "/chat.postMessage", map[string]any{
			"channel": opened.Channel.ID,
			"text":    sub(d.SlackText),
		})
		if err != nil {
			return "", err
		}
		var res struct {
			TS string `json:"ts"`
		}
		_ = json.Unmarshal(raw, &res)
		b, _ := json.Marshal(map[string]any{"status": "sent", "to": recipient, "channel": opened.Channel.ID, "ts": res.TS, "as": "user"})
		return string(b), nil

	case "list_channels":
		q := url.Values{}
		q.Set("limit", fmt.Sprint(intOr(d.SlackLimit, 100)))
		q.Set("exclude_archived", "true")
		q.Set("types", "public_channel,private_channel")
		raw, err := slackCall(ctx, token, http.MethodGet, "/conversations.list?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Channels []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"channels"`
		}
		_ = json.Unmarshal(raw, &res)
		out := make([]map[string]any, 0, len(res.Channels))
		for _, ch := range res.Channels {
			out = append(out, map[string]any{"id": ch.ID, "name": "#" + ch.Name})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "get_channel_history":
		q := url.Values{}
		q.Set("channel", sub(d.SlackChannel))
		q.Set("limit", fmt.Sprint(intOr(d.SlackLimit, 20)))
		raw, err := slackCall(ctx, token, http.MethodGet, "/conversations.history?"+q.Encode(), nil)
		if err != nil && userToken != "" && isSlackAccessErr(err) {
			// DMs and group chats belong to the user, not the bot — retry with
			// the user grant (im:history / mpim:history).
			raw, err = slackCall(ctx, userToken, http.MethodGet, "/conversations.history?"+q.Encode(), nil)
		}
		if err != nil {
			return "", err
		}
		var res struct {
			Messages []struct {
				User string `json:"user"`
				Text string `json:"text"`
				TS   string `json:"ts"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &res)
		out := make([]map[string]any, 0, len(res.Messages))
		for _, m := range res.Messages {
			out = append(out, map[string]any{"user": m.User, "text": m.Text, "ts": m.TS})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "reply_in_thread":
		sendToken := token
		if d.SlackSendAs == "user" {
			t, err := asUser()
			if err != nil {
				return "", err
			}
			sendToken = t
		}
		raw, err := slackCall(ctx, sendToken, http.MethodPost, "/chat.postMessage", map[string]any{
			"channel":   sub(d.SlackChannel),
			"thread_ts": sub(d.SlackThreadTs),
			"text":      sub(d.SlackText),
		})
		if err != nil {
			return "", err
		}
		var res struct {
			TS string `json:"ts"`
		}
		_ = json.Unmarshal(raw, &res)
		b, _ := json.Marshal(map[string]any{"status": "sent", "thread_ts": sub(d.SlackThreadTs), "ts": res.TS})
		return string(b), nil

	case "update_message":
		raw, err := slackCall(ctx, token, http.MethodPost, "/chat.update", map[string]any{
			"channel": sub(d.SlackChannel),
			"ts":      sub(d.SlackMessageTs),
			"text":    sub(d.SlackText),
		})
		if err != nil {
			return "", err
		}
		var res struct {
			TS string `json:"ts"`
		}
		_ = json.Unmarshal(raw, &res)
		b, _ := json.Marshal(map[string]any{"status": "updated", "ts": res.TS})
		return string(b), nil

	case "delete_message":
		if _, err := slackCall(ctx, token, http.MethodPost, "/chat.delete", map[string]any{
			"channel": sub(d.SlackChannel),
			"ts":      sub(d.SlackMessageTs),
		}); err != nil {
			return "", err
		}
		return `{"status":"deleted"}`, nil

	case "add_reaction":
		emoji := strings.Trim(sub(d.SlackEmoji), ": ")
		if _, err := slackCall(ctx, token, http.MethodPost, "/reactions.add", map[string]any{
			"channel":   sub(d.SlackChannel),
			"timestamp": sub(d.SlackMessageTs),
			"name":      emoji,
		}); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": "reacted", "emoji": emoji})
		return string(b), nil

	case "pin_message":
		if _, err := slackCall(ctx, token, http.MethodPost, "/pins.add", map[string]any{
			"channel":   sub(d.SlackChannel),
			"timestamp": sub(d.SlackMessageTs),
		}); err != nil {
			return "", err
		}
		return `{"status":"pinned"}`, nil

	case "schedule_message":
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(sub(d.SlackPostAt)))
		if err != nil {
			return "", fmt.Errorf("Slack: post time must be RFC3339 (e.g. 2026-07-20T15:00:00Z): %w", err)
		}
		raw, err := slackCall(ctx, token, http.MethodPost, "/chat.scheduleMessage", map[string]any{
			"channel": sub(d.SlackChannel),
			"post_at": at.Unix(),
			"text":    sub(d.SlackText),
		})
		if err != nil {
			return "", err
		}
		var res struct {
			ScheduledMessageID string `json:"scheduled_message_id"`
		}
		_ = json.Unmarshal(raw, &res)
		b, _ := json.Marshal(map[string]any{"status": "scheduled", "id": res.ScheduledMessageID, "post_at": at.Format(time.RFC3339)})
		return string(b), nil

	case "create_channel":
		raw, err := slackCall(ctx, token, http.MethodPost, "/conversations.create", map[string]any{
			"name":       strings.TrimPrefix(sub(d.SlackChannelName), "#"),
			"is_private": sub(d.SlackPrivate) == "true",
		})
		if err != nil {
			return "", err
		}
		var res struct {
			Channel struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"channel"`
		}
		_ = json.Unmarshal(raw, &res)
		b, _ := json.Marshal(map[string]any{"status": "created", "id": res.Channel.ID, "name": "#" + res.Channel.Name})
		return string(b), nil

	case "archive_channel":
		if _, err := slackCall(ctx, token, http.MethodPost, "/conversations.archive", map[string]any{
			"channel": sub(d.SlackChannel),
		}); err != nil {
			return "", err
		}
		return `{"status":"archived"}`, nil

	case "join_channel":
		raw, err := slackCall(ctx, token, http.MethodPost, "/conversations.join", map[string]any{
			"channel": sub(d.SlackChannel),
		})
		if err != nil {
			return "", err
		}
		var res struct {
			Channel struct {
				ID string `json:"id"`
			} `json:"channel"`
		}
		_ = json.Unmarshal(raw, &res)
		b, _ := json.Marshal(map[string]any{"status": "joined", "id": res.Channel.ID})
		return string(b), nil

	case "invite_to_channel":
		users := strings.Join(splitCSV(sub(d.SlackUserId)), ",")
		if _, err := slackCall(ctx, token, http.MethodPost, "/conversations.invite", map[string]any{
			"channel": sub(d.SlackChannel),
			"users":   users,
		}); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": "invited", "users": users})
		return string(b), nil

	case "set_channel_topic":
		if _, err := slackCall(ctx, token, http.MethodPost, "/conversations.setTopic", map[string]any{
			"channel": sub(d.SlackChannel),
			"topic":   sub(d.SlackTopic),
		}); err != nil {
			return "", err
		}
		return `{"status":"topic_set"}`, nil

	case "upload_file":
		return slackUploadFile(ctx, token, sub(d.SlackChannel), sub(d.SlackFileName), sub(d.SlackFileContent))

	case "list_users":
		q := url.Values{}
		q.Set("limit", fmt.Sprint(intOr(d.SlackLimit, 100)))
		raw, err := slackCall(ctx, token, http.MethodGet, "/users.list?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Members []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Deleted bool   `json:"deleted"`
				IsBot   bool   `json:"is_bot"`
				Profile struct {
					RealName    string `json:"real_name"`
					DisplayName string `json:"display_name"`
					Email       string `json:"email"`
				} `json:"profile"`
			} `json:"members"`
		}
		_ = json.Unmarshal(raw, &res)
		out := make([]map[string]any, 0, len(res.Members))
		for _, m := range res.Members {
			if m.Deleted || m.IsBot || m.ID == "USLACKBOT" {
				continue
			}
			out = append(out, map[string]any{
				"id":    m.ID,
				"name":  firstNonEmpty(m.Profile.DisplayName, m.Profile.RealName, m.Name),
				"email": m.Profile.Email,
			})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "get_user_by_email":
		q := url.Values{}
		q.Set("email", strings.TrimSpace(sub(d.SlackEmail)))
		raw, err := slackCall(ctx, token, http.MethodGet, "/users.lookupByEmail?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var res struct {
			User struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Profile struct {
					RealName    string `json:"real_name"`
					DisplayName string `json:"display_name"`
					Email       string `json:"email"`
				} `json:"profile"`
			} `json:"user"`
		}
		_ = json.Unmarshal(raw, &res)
		b, _ := json.Marshal(map[string]any{
			"id":    res.User.ID,
			"name":  firstNonEmpty(res.User.Profile.DisplayName, res.User.Profile.RealName, res.User.Name),
			"email": res.User.Profile.Email,
		})
		return string(b), nil

	case "search_messages":
		// Search runs as the connecting user — bots can't search a workspace.
		t, err := asUser()
		if err != nil {
			return "", err
		}
		q := url.Values{}
		q.Set("query", sub(d.SlackText))
		q.Set("count", fmt.Sprint(intOr(d.SlackLimit, 20)))
		raw, err := slackCall(ctx, t, http.MethodGet, "/search.messages?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Messages struct {
				Matches []struct {
					User      string `json:"user"`
					Username  string `json:"username"`
					Text      string `json:"text"`
					TS        string `json:"ts"`
					Permalink string `json:"permalink"`
					Channel   struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"channel"`
				} `json:"matches"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &res)
		out := make([]map[string]any, 0, len(res.Messages.Matches))
		for _, m := range res.Messages.Matches {
			out = append(out, map[string]any{
				"user": firstNonEmpty(m.Username, m.User), "text": m.Text, "ts": m.TS,
				"channel": "#" + m.Channel.Name, "channel_id": m.Channel.ID, "permalink": m.Permalink,
			})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	default:
		return "", fmt.Errorf("unknown Slack operation: %s", d.IntegrationOp)
	}
}

// slackUploadFile drives the three-step external upload flow:
// getUploadURLExternal → POST the bytes → completeUploadExternal.
func slackUploadFile(ctx context.Context, token, channel, filename, content string) (string, error) {
	if filename == "" {
		filename = "upload.txt"
	}
	q := url.Values{}
	q.Set("filename", filename)
	q.Set("length", fmt.Sprint(len(content)))
	raw, err := slackCall(ctx, token, http.MethodGet, "/files.getUploadURLExternal?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	var ticket struct {
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := json.Unmarshal(raw, &ticket); err != nil || ticket.UploadURL == "" {
		return "", fmt.Errorf("Slack upload: no upload URL returned")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ticket.UploadURL, strings.NewReader(content))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("Slack upload failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("Slack upload failed: %d %s", resp.StatusCode, body)
	}

	complete := map[string]any{
		"files": []map[string]string{{"id": ticket.FileID, "title": filename}},
	}
	if channel != "" {
		complete["channel_id"] = channel
	}
	if _, err := slackCall(ctx, token, http.MethodPost, "/files.completeUploadExternal", complete); err != nil {
		return "", err
	}
	b, _ := json.Marshal(map[string]any{"status": "uploaded", "file_id": ticket.FileID, "name": filename})
	return string(b), nil
}
