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

// Slack Web API. Responses are always HTTP 200; success is signalled by the
// {"ok":bool} field, with the failure reason in "error".

const slackBase = "https://slack.com/api"

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

func runSlack(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	switch d.IntegrationOp {
	case "send_message":
		raw, err := slackCall(ctx, token, http.MethodPost, "/chat.postMessage", map[string]any{
			"channel": sub(d.SlackChannel),
			"text":    sub(d.SlackText),
		})
		if err != nil {
			return "", err
		}
		var res struct {
			Channel string `json:"channel"`
			TS      string `json:"ts"`
		}
		_ = json.Unmarshal(raw, &res)
		b, _ := json.Marshal(map[string]any{"status": "sent", "channel": res.Channel, "ts": res.TS})
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
