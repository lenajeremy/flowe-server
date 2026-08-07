package triggers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
)

func init() { Register(mondayAdapter{}) }

type mondayAdapter struct{}

var mondayHooksGraphQLURL = "https://api.monday.com/v2"

var mondayWebhookEvents = map[string]string{
	"item.created":        "create_item",
	"item.column_changed": "change_column_value",
	"update.created":      "create_update",
}

func (mondayAdapter) Provider() string   { return "monday" }
func (mondayAdapter) Delivery() Delivery { return Push }

func (mondayAdapter) Events() []EventSpec {
	return []EventSpec{
		{
			ID: "item.created", Label: "Item created", ResourceKind: "board",
			Filters: []FilterSpec{{Key: "group_id", Label: "Group", ResourceKind: "group"}},
			Sample:  map[string]any{"board_id": "123", "item_id": "456", "item_name": "Prepare launch", "group_id": "topics"},
		},
		{
			ID: "item.column_changed", Label: "Column value changed", ResourceKind: "board",
			Filters: []FilterSpec{{Key: "column_id", Label: "Column", ResourceKind: "column"}},
			Sample:  map[string]any{"board_id": "123", "item_id": "456", "column_id": "status", "value": map[string]any{"label": "Done"}},
		},
		{
			ID: "update.created", Label: "Update added", ResourceKind: "board",
			Sample: map[string]any{"board_id": "123", "item_id": "456", "update_id": "789", "body": "Ready for review"},
		},
	}
}

func (mondayAdapter) Register(ctx context.Context, conn Conn, t *models.IntegrationTrigger) (Registration, error) {
	event, ok := mondayWebhookEvents[t.Event]
	if !ok {
		return Registration{}, fmt.Errorf("monday.com: unknown event %q", t.Event)
	}
	boardID, err := positiveDecimal(t.ResourceID)
	if err != nil {
		return Registration{}, fmt.Errorf("monday.com: select a valid board: %w", err)
	}
	if strings.TrimSpace(os.Getenv("MONDAY_SIGNING_SECRET")) == "" {
		return Registration{}, fmt.Errorf("monday.com: MONDAY_SIGNING_SECRET is not configured")
	}
	callback := HookURL("monday", t.ID.String())
	parsed, err := url.Parse(callback)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return Registration{}, fmt.Errorf("monday.com: PUBLIC_BASE_URL must be a public HTTPS URL before webhooks can be registered")
	}
	nonce, err := randomSecret()
	if err != nil {
		return Registration{}, err
	}
	callback += "?registration=" + nonce[:16]

	// WebhookEventType cannot be supplied as a string variable, so interpolate
	// only the value selected from the closed map above. IDs and URLs remain
	// GraphQL variables.
	query := fmt.Sprintf(`mutation ($board: ID!, $url: String!) { create_webhook(board_id: $board, url: $url, event: %s) { id board_id } }`, event)
	var response struct {
		CreateWebhook struct {
			ID      string `json:"id"`
			BoardID string `json:"board_id"`
		} `json:"create_webhook"`
	}
	if err := mondayHookGraphQL(ctx, conn.AccessToken, query, map[string]any{"board": boardID, "url": callback}, &response); err != nil {
		return Registration{}, fmt.Errorf("monday.com: could not create the board webhook: %w", err)
	}
	if response.CreateWebhook.ID == "" {
		return Registration{}, fmt.Errorf("monday.com: created a webhook without an id")
	}
	if response.CreateWebhook.BoardID != "" && response.CreateWebhook.BoardID != boardID {
		_ = mondayDeleteWebhook(ctx, conn.AccessToken, response.CreateWebhook.ID)
		return Registration{}, fmt.Errorf("monday.com: webhook was created on an unexpected board")
	}
	return Registration{RemoteID: response.CreateWebhook.ID, ScopeID: conn.WorkspaceID}, nil
}

func (mondayAdapter) Unregister(ctx context.Context, conn Conn, t *models.IntegrationTrigger) error {
	if _, err := positiveDecimal(t.RemoteID); err != nil {
		return fmt.Errorf("monday.com: invalid webhook id: %w", err)
	}
	return mondayDeleteWebhook(ctx, conn.AccessToken, t.RemoteID)
}

func mondayDeleteWebhook(ctx context.Context, token, webhookID string) error {
	var response struct {
		DeleteWebhook struct {
			ID string `json:"id"`
		} `json:"delete_webhook"`
	}
	return mondayHookGraphQL(ctx, token,
		`mutation ($id: ID!) { delete_webhook(id: $id) { id } }`,
		map[string]any{"id": webhookID}, &response)
}

func (mondayAdapter) Renew(context.Context, Conn, *models.IntegrationTrigger) (*time.Time, error) {
	return nil, nil
}

func (mondayAdapter) Handshake(_ *http.Request, body []byte) (int, []byte, http.Header, bool) {
	var challenge struct {
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(body, &challenge) != nil || challenge.Challenge == "" {
		return 0, nil, nil, false
	}
	response, _ := json.Marshal(challenge)
	return http.StatusOK, response, http.Header{"Content-Type": {"application/json"}}, true
}

func (mondayAdapter) Verify(r *http.Request, _ []byte, t *models.IntegrationTrigger) error {
	if t == nil {
		return fmt.Errorf("monday.com: webhook has no trigger")
	}
	token := strings.TrimSpace(r.Header.Get("Authorization"))
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("monday.com: request is not signed")
	}
	secret := strings.TrimSpace(os.Getenv("MONDAY_SIGNING_SECRET"))
	if secret == "" {
		return fmt.Errorf("monday.com: signing secret is not configured")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("monday.com: invalid JWT header")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(headerRaw, &header) != nil || header.Alg != "HS256" {
		return fmt.Errorf("monday.com: unsupported JWT algorithm")
	}
	received, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("monday.com: invalid JWT signature")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = io.WriteString(mac, parts[0]+"."+parts[1])
	if !hmac.Equal(received, mac.Sum(nil)) {
		return fmt.Errorf("monday.com: signature does not match")
	}
	claims, err := mondayClaims(token)
	if err != nil || claims.ExpiresAt <= time.Now().Unix() {
		return fmt.Errorf("monday.com: webhook JWT is expired or unreadable")
	}
	if t.ScopeID != "" && claims.AccountID != "" && t.ScopeID != claims.AccountID {
		return fmt.Errorf("monday.com: webhook belongs to a different account")
	}
	if claims.Audience != "" {
		base := strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/")
		if base == "" {
			base = strings.TrimRight(os.Getenv("OAUTH_REDIRECT_BASE"), "/")
		}
		if base != "" && claims.Audience != base+r.URL.RequestURI() {
			return fmt.Errorf("monday.com: webhook JWT has the wrong audience")
		}
	}
	return nil
}

type mondayJWTClaims struct {
	AccountID string `json:"accountId"`
	UserID    string `json:"userId"`
	Audience  string `json:"aud"`
	ExpiresAt int64  `json:"exp"`
}

func mondayClaims(token string) (mondayJWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return mondayJWTClaims{}, fmt.Errorf("not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return mondayJWTClaims{}, err
	}
	var wire struct {
		AccountID json.RawMessage `json:"accountId"`
		UserID    json.RawMessage `json:"userId"`
		Audience  string          `json:"aud"`
		ExpiresAt int64           `json:"exp"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return mondayJWTClaims{}, err
	}
	return mondayJWTClaims{
		AccountID: mondayJSONID(wire.AccountID), UserID: mondayJSONID(wire.UserID),
		Audience: wire.Audience, ExpiresAt: wire.ExpiresAt,
	}, nil
}

func (mondayAdapter) Parse(r *http.Request, body []byte) ([]Event, error) {
	var payload struct {
		Event struct {
			Type           string          `json:"type"`
			BoardID        json.RawMessage `json:"boardId"`
			PulseID        json.RawMessage `json:"pulseId"`
			PulseName      string          `json:"pulseName"`
			GroupID        string          `json:"groupId"`
			ColumnID       string          `json:"columnId"`
			ColumnTitle    string          `json:"columnTitle"`
			Value          any             `json:"value"`
			PreviousValue  any             `json:"previousValue"`
			UpdateID       json.RawMessage `json:"updateId"`
			Body           string          `json:"body"`
			UserID         json.RawMessage `json:"userId"`
			TriggerTime    string          `json:"triggerTime"`
			SubscriptionID json.RawMessage `json:"subscriptionId"`
			TriggerUUID    string          `json:"triggerUuid"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("monday.com: unreadable payload: %w", err)
	}
	p := payload.Event
	types := map[string]string{"create_pulse": "item.created", "update_column_value": "item.column_changed", "create_update": "update.created"}
	eventType := types[p.Type]
	if eventType == "" {
		return nil, nil
	}
	boardID := mondayJSONID(p.BoardID)
	if boardID == "" {
		return nil, fmt.Errorf("monday.com: event has no board id")
	}
	key := mondayJSONID(p.SubscriptionID) + ":" + p.TriggerUUID
	if strings.Trim(key, ":") == "" {
		return nil, fmt.Errorf("monday.com: event has no delivery identifier")
	}
	when := time.Now().UTC()
	if parsed, err := time.Parse(time.RFC3339Nano, p.TriggerTime); err == nil {
		when = parsed
	}
	claims, _ := mondayClaims(strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	data := map[string]any{
		"board_id": boardID, "item_id": mondayJSONID(p.PulseID), "item_name": p.PulseName,
		"group_id": p.GroupID, "column_id": p.ColumnID, "column_title": p.ColumnTitle,
		"value": p.Value, "previous_value": p.PreviousValue, "update_id": mondayJSONID(p.UpdateID),
		"body": p.Body, "user_id": mondayJSONID(p.UserID),
	}
	return []Event{{Key: key, Type: eventType, ResourceID: boardID, ScopeID: claims.AccountID, OccurredAt: when, Data: data}}, nil
}

func mondayJSONID(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if strings.HasPrefix(trimmed, `"`) {
		var value string
		_ = json.Unmarshal(raw, &value)
		return value
	}
	return trimmed
}

func mondayHookGraphQL(ctx context.Context, token, query string, variables map[string]any, target any) error {
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mondayHooksGraphQLURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("API-Version", "2026-04")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("API returned %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("unreadable API response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("API error: %s", envelope.Errors[0].Message)
	}
	if target != nil && json.Unmarshal(envelope.Data, target) != nil {
		return fmt.Errorf("unreadable API response data")
	}
	return nil
}
