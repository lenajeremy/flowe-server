package triggers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workflow-ai/server/internal/database/models"

	"github.com/google/uuid"
)

type asanaTestRoundTripFunc func(*http.Request) (*http.Response, error)

func (f asanaTestRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestMondayHandshakeEchoesJSONChallenge(t *testing.T) {
	status, body, headers, handled := (mondayAdapter{}).Handshake(
		httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"challenge":"prove-it"}`))
	if !handled || status != http.StatusOK || headers.Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected handshake: handled=%v status=%d headers=%v", handled, status, headers)
	}
	if string(body) != `{"challenge":"prove-it"}` {
		t.Fatalf("challenge response = %s", body)
	}
}

func TestMondayVerifiesJWTAndNormalizesItemEvent(t *testing.T) {
	t.Setenv("MONDAY_SIGNING_SECRET", "test-signing-secret")
	token := signedMondayJWT(t, "test-signing-secret", map[string]any{
		"accountId": 42, "userId": 7, "exp": time.Now().Add(time.Minute).Unix(),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/monday/trigger", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	trigger := &models.IntegrationTrigger{ScopeID: "42"}
	if err := (mondayAdapter{}).Verify(req, nil, trigger); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	payload := []byte(`{"event":{"type":"create_pulse","boardId":123,"pulseId":456,"pulseName":"Ship it","groupId":"topics","userId":7,"triggerTime":"2026-08-06T10:00:00Z","subscriptionId":99,"triggerUuid":"delivery-1"}}`)
	events, err := (mondayAdapter{}).Parse(req, payload)
	if err != nil || len(events) != 1 {
		t.Fatalf("Parse = %#v, %v", events, err)
	}
	event := events[0]
	if event.Type != "item.created" || event.ResourceID != "123" || event.ScopeID != "42" || event.Key != "99:delivery-1" {
		t.Fatalf("normalized event = %#v", event)
	}
	if event.Data["item_id"] != "456" || event.Data["item_name"] != "Ship it" {
		t.Fatalf("event data = %#v", event.Data)
	}
}

func TestMondayRejectsWebhookFromDifferentAccount(t *testing.T) {
	t.Setenv("MONDAY_SIGNING_SECRET", "test-signing-secret")
	token := signedMondayJWT(t, "test-signing-secret", map[string]any{
		"accountId": "other", "exp": time.Now().Add(time.Minute).Unix(),
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", token)
	if err := (mondayAdapter{}).Verify(req, nil, &models.IntegrationTrigger{ScopeID: "expected"}); err == nil {
		t.Fatal("expected account mismatch to fail")
	}
}

func TestAsanaHandshakeSignatureAndCompletionEvent(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Header.Set("X-Hook-Secret", "asana-secret")
	status, body, headers, handled := (asanaAdapter{}).Handshake(request, nil)
	if !handled || status != http.StatusOK || len(body) != 0 || headers.Get("X-Hook-Secret") != "asana-secret" {
		t.Fatalf("unexpected handshake: handled=%v status=%d headers=%v body=%q", handled, status, headers, body)
	}

	payload := []byte(`{"events":[{"action":"changed","resource":{"gid":"123","resource_type":"task"},"change":{"field":"completed","action":"changed","new_value":true},"created_at":"2026-08-06T10:00:00Z"}]}`)
	mac := hmac.New(sha256.New, []byte("asana-secret"))
	_, _ = mac.Write(payload)
	delivery := httptest.NewRequest(http.MethodPost, "/", nil)
	delivery.Header.Set("X-Hook-Signature", hex.EncodeToString(mac.Sum(nil)))
	trigger := &models.IntegrationTrigger{Secret: "asana-secret"}
	if err := (asanaAdapter{}).Verify(delivery, payload, trigger); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	events, err := (asanaAdapter{}).Parse(delivery, payload)
	if err != nil || len(events) != 1 {
		t.Fatalf("Parse = %#v, %v", events, err)
	}
	if events[0].Type != "task.completed" || events[0].Data["task_id"] != "123" || events[0].Data["completed"] != true {
		t.Fatalf("normalized event = %#v", events[0])
	}
}

func TestAsanaRegisterStoresSecretFromCreationBody(t *testing.T) {
	t.Setenv("PUBLIC_BASE_URL", "https://api.example.test")
	oldClient := httpClient
	oldBase := asanaHooksAPIBase
	defer func() { httpClient, asanaHooksAPIBase = oldClient, oldBase }()
	asanaHooksAPIBase = "https://asana.test/api/1.0"
	httpClient = &http.Client{Transport: asanaTestRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/1.0/webhooks" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		var payload struct {
			Data struct {
				Resource string            `json:"resource"`
				Target   string            `json:"target"`
				Filters  []asanaHookFilter `json:"filters"`
			} `json:"data"`
		}
		_ = json.NewDecoder(request.Body).Decode(&payload)
		if payload.Data.Resource != "123" || !strings.HasPrefix(payload.Data.Target, "https://api.example.test/api/hooks/asana/") {
			t.Errorf("payload = %#v", payload)
		}
		if len(payload.Data.Filters) != 1 || len(payload.Data.Filters[0].Fields) != 1 || payload.Data.Filters[0].Fields[0] != "completed" {
			t.Errorf("filters = %#v", payload.Data.Filters)
		}
		body := `{"data":{"gid":"456"},"X-Hook-Secret":"created-secret"}`
		return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	trigger := &models.IntegrationTrigger{
		BaseModel: models.BaseModel{ID: uuid.New()}, Event: "task.completed", ResourceID: "123",
	}
	registration, err := (asanaAdapter{}).Register(t.Context(), Conn{AccessToken: "token", WorkspaceID: "workspace"}, trigger)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if registration.RemoteID != "456" || registration.Secret != "created-secret" || registration.ScopeID != "workspace" {
		t.Fatalf("registration = %#v", registration)
	}
}

func signedMondayJWT(t *testing.T, secret string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
