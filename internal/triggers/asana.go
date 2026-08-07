package triggers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
)

func init() { Register(asanaAdapter{}) }

type asanaAdapter struct{}

var asanaHooksAPIBase = "https://app.asana.com/api/1.0"

type asanaHookFilter struct {
	ResourceType    string   `json:"resource_type"`
	Action          string   `json:"action"`
	ResourceSubtype string   `json:"resource_subtype,omitempty"`
	Fields          []string `json:"fields,omitempty"`
}

var asanaWebhookFilters = map[string][]asanaHookFilter{
	"task.added":     {{ResourceType: "task", Action: "added"}},
	"task.changed":   {{ResourceType: "task", Action: "changed"}},
	"task.completed": {{ResourceType: "task", Action: "changed", Fields: []string{"completed"}}},
	"task.deleted":   {{ResourceType: "task", Action: "deleted"}},
	"comment.added":  {{ResourceType: "story", Action: "added", ResourceSubtype: "comment_added"}},
}

func (asanaAdapter) Provider() string   { return "asana" }
func (asanaAdapter) Delivery() Delivery { return Push }

func (asanaAdapter) Events() []EventSpec {
	return []EventSpec{
		{ID: "task.added", Label: "Task added to project", ResourceKind: "project", Sample: map[string]any{"task_id": "123", "action": "added"}},
		{ID: "task.changed", Label: "Task changed", ResourceKind: "project", Sample: map[string]any{"task_id": "123", "action": "changed", "field": "name"}},
		{ID: "task.completed", Label: "Task completed", ResourceKind: "project", Sample: map[string]any{"task_id": "123", "completed": true}},
		{ID: "task.deleted", Label: "Task deleted", ResourceKind: "project", Sample: map[string]any{"task_id": "123", "action": "deleted"}},
		{ID: "comment.added", Label: "Comment added", ResourceKind: "project", Sample: map[string]any{"story_id": "456", "task_id": "123", "action": "added"}},
	}
}

func (asanaAdapter) Register(ctx context.Context, conn Conn, t *models.IntegrationTrigger) (Registration, error) {
	filters, ok := asanaWebhookFilters[t.Event]
	if !ok {
		return Registration{}, fmt.Errorf("Asana: unknown event %q", t.Event)
	}
	projectID, err := positiveDecimal(t.ResourceID)
	if err != nil {
		return Registration{}, fmt.Errorf("Asana: select a valid project: %w", err)
	}
	callback := HookURL("asana", t.ID.String())
	parsed, err := url.Parse(callback)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return Registration{}, fmt.Errorf("Asana: PUBLIC_BASE_URL must be a public HTTPS URL before webhooks can be registered")
	}
	nonce, err := randomSecret()
	if err != nil {
		return Registration{}, err
	}
	callback += "?registration=" + nonce[:16]
	payload := map[string]any{"data": map[string]any{"resource": projectID, "target": callback, "filters": filters}}
	raw, headers, err := asanaHookAPI(ctx, conn.AccessToken, http.MethodPost, "/webhooks", payload)
	if err != nil {
		return Registration{}, fmt.Errorf("Asana: could not create the project webhook: %w", err)
	}
	var response struct {
		Data struct {
			GID string `json:"gid"`
		} `json:"data"`
		HookSecret string `json:"X-Hook-Secret"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Data.GID == "" {
		return Registration{}, fmt.Errorf("Asana: created a webhook without an id")
	}
	secret := strings.TrimSpace(response.HookSecret)
	if secret == "" {
		secret = strings.TrimSpace(headers.Get("X-Hook-Secret"))
	}
	if secret == "" {
		// Asana returns the secret from the synchronous handshake on the webhook
		// creation response. Without it subsequent deliveries cannot be trusted.
		_, _, _ = asanaHookAPI(ctx, conn.AccessToken, http.MethodDelete, "/webhooks/"+url.PathEscape(response.Data.GID), nil)
		return Registration{}, fmt.Errorf("Asana: webhook creation returned no signing secret")
	}
	return Registration{RemoteID: response.Data.GID, Secret: secret, ScopeID: conn.WorkspaceID}, nil
}

func (asanaAdapter) Unregister(ctx context.Context, conn Conn, t *models.IntegrationTrigger) error {
	if _, err := positiveDecimal(t.RemoteID); err != nil {
		return fmt.Errorf("Asana: invalid webhook id: %w", err)
	}
	_, _, err := asanaHookAPI(ctx, conn.AccessToken, http.MethodDelete, "/webhooks/"+url.PathEscape(t.RemoteID), nil)
	return err
}

func (asanaAdapter) Renew(context.Context, Conn, *models.IntegrationTrigger) (*time.Time, error) {
	return nil, nil
}

func (asanaAdapter) Handshake(r *http.Request, _ []byte) (int, []byte, http.Header, bool) {
	secret := strings.TrimSpace(r.Header.Get("X-Hook-Secret"))
	if secret == "" {
		return 0, nil, nil, false
	}
	return http.StatusOK, nil, http.Header{"X-Hook-Secret": {secret}}, true
}

func (asanaAdapter) Verify(r *http.Request, body []byte, t *models.IntegrationTrigger) error {
	if t == nil || strings.TrimSpace(t.Secret) == "" {
		return fmt.Errorf("Asana: webhook has no signing secret")
	}
	received, err := hex.DecodeString(strings.TrimSpace(r.Header.Get("X-Hook-Signature")))
	if err != nil || len(received) == 0 {
		return fmt.Errorf("Asana: request is not signed")
	}
	mac := hmac.New(sha256.New, []byte(t.Secret))
	_, _ = mac.Write(body)
	if !hmac.Equal(received, mac.Sum(nil)) {
		return fmt.Errorf("Asana: signature does not match")
	}
	return nil
}

func (asanaAdapter) Parse(_ *http.Request, body []byte) ([]Event, error) {
	var payload struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("Asana: unreadable payload: %w", err)
	}
	events := make([]Event, 0, len(payload.Events))
	for _, raw := range payload.Events {
		var p struct {
			Action   string `json:"action"`
			Resource struct {
				GID             string `json:"gid"`
				ResourceType    string `json:"resource_type"`
				ResourceSubtype string `json:"resource_subtype"`
			} `json:"resource"`
			Parent struct {
				GID          string `json:"gid"`
				ResourceType string `json:"resource_type"`
			} `json:"parent"`
			Change struct {
				Field    string `json:"field"`
				Action   string `json:"action"`
				NewValue any    `json:"new_value"`
			} `json:"change"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("Asana: unreadable event: %w", err)
		}
		eventType := ""
		switch {
		case p.Resource.ResourceType == "task" && p.Action == "added":
			eventType = "task.added"
		case p.Resource.ResourceType == "task" && p.Action == "changed" && p.Change.Field == "completed" && asanaTruthy(p.Change.NewValue):
			eventType = "task.completed"
		case p.Resource.ResourceType == "task" && p.Action == "changed":
			eventType = "task.changed"
		case p.Resource.ResourceType == "task" && p.Action == "deleted":
			eventType = "task.deleted"
		case p.Resource.ResourceType == "story" && p.Action == "added" && (p.Resource.ResourceSubtype == "comment_added" || p.Resource.ResourceSubtype == ""):
			eventType = "comment.added"
		default:
			continue
		}
		when := time.Now().UTC()
		if parsed, err := time.Parse(time.RFC3339Nano, p.CreatedAt); err == nil {
			when = parsed
		}
		hash := sha256.Sum256(raw)
		data := map[string]any{
			"action": p.Action, "resource_id": p.Resource.GID, "resource_type": p.Resource.ResourceType,
			"resource_subtype": p.Resource.ResourceSubtype, "parent_id": p.Parent.GID,
			"field": p.Change.Field, "change_action": p.Change.Action, "new_value": p.Change.NewValue,
		}
		if p.Resource.ResourceType == "task" {
			data["task_id"] = p.Resource.GID
			if eventType == "task.completed" {
				data["completed"] = true
			}
		} else {
			data["story_id"] = p.Resource.GID
			data["task_id"] = p.Parent.GID
		}
		events = append(events, Event{Key: hex.EncodeToString(hash[:]), Type: eventType, OccurredAt: when, Data: data})
	}
	return events, nil
}

func asanaTruthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	case map[string]any:
		if completed, ok := v["completed"].(bool); ok {
			return completed
		}
	}
	return false
}

func asanaHookAPI(ctx context.Context, token, method, path string, body any) ([]byte, http.Header, error) {
	var reader io.Reader
	if body != nil {
		payload, _ := json.Marshal(body)
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(asanaHooksAPIBase, "/")+path, reader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, resp.Header, fmt.Errorf("API returned %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return raw, resp.Header, nil
}
