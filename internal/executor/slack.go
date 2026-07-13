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

	default:
		return "", fmt.Errorf("unknown Slack operation: %s", d.IntegrationOp)
	}
}
