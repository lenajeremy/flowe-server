package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"workflow-ai/server/internal/database/models"

	"github.com/google/uuid"
)

type slackRoundTripFunc func(*http.Request) (*http.Response, error)

func (f slackRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func signedSlackHeaders(secret string, body []byte, timestamp int64) http.Header {
	timestampRaw := strconv.FormatInt(timestamp, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + timestampRaw + ":"))
	_, _ = mac.Write(body)
	header := http.Header{}
	header.Set("X-Slack-Request-Timestamp", timestampRaw)
	header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return header
}

func TestVerifySlackAgentSignatureAuthenticatesBodyAndRejectsReplay(t *testing.T) {
	t.Setenv("SLACK_SIGNING_SECRET", "signing-secret")
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"type":"event_callback","event_id":"Ev1"}`)
	header := signedSlackHeaders("signing-secret", body, now.Unix())
	if err := verifySlackAgentSignature(header, body, now); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := verifySlackAgentSignature(header, append(body, 'x'), now); err == nil {
		t.Fatal("tampered body passed signature verification")
	}
	stale := signedSlackHeaders("signing-secret", body, now.Add(-slackSignatureMaxAge-time.Second).Unix())
	if err := verifySlackAgentSignature(stale, body, now); err == nil {
		t.Fatal("stale Slack request was accepted")
	}
}

func TestOAuthScopeContainsUsesWholeScopeNames(t *testing.T) {
	t.Parallel()
	if !oauthScopeContains("chat:write,app_mentions:read channels:history", "app_mentions:read") {
		t.Fatal("expected scope was not found")
	}
	if oauthScopeContains("chat:write.public", "chat:write") {
		t.Fatal("partial scope name matched")
	}
}

func TestSlackAgentHostScopesRequireThreadReadAccess(t *testing.T) {
	t.Parallel()
	complete := "app_mentions:read chat:write chat:write.customize channels:read channels:history groups:read groups:history"
	if !slackAgentHostScopesReady(complete) {
		t.Fatal("complete hosted-agent scope set was rejected")
	}
	if slackAgentHostScopesReady(strings.ReplaceAll(complete, "groups:history", "")) {
		t.Fatal("host without private-channel thread history was accepted")
	}
}

func TestSlackAgentHostRequestsOptionalPublicChannelJoin(t *testing.T) {
	t.Parallel()
	if !slices.Contains(slackAgentHostRequestedScopes, "channels:join") {
		t.Fatal("hosted-agent OAuth does not request channels:join")
	}
	// Existing installations without the convenience scope must remain usable;
	// they fall back to opening Slack for a manual invite.
	completeRequired := "app_mentions:read chat:write chat:write.customize channels:read channels:history groups:read groups:history"
	if !slackAgentHostScopesReady(completeRequired) {
		t.Fatal("optional channels:join scope was incorrectly made deployment-critical")
	}
}

func TestSlackAgentOAuthRequestsHostedIdentityScopes(t *testing.T) {
	t.Parallel()
	scopes := oauthProviders["slack"].extraAuthQ.Get("scope")
	for _, want := range []string{"app_mentions:read", "chat:write", "chat:write.customize", "groups:history"} {
		if !oauthScopeContains(scopes, want) {
			t.Errorf("Slack OAuth does not request %q: %s", want, scopes)
		}
	}
}

func TestAgentHostOAuthStateIsDistinctFromActionConnection(t *testing.T) {
	state := newAgentHostOAuthState("user-1", "org-1", "https://app.example", "")
	entry, ok := consumeOAuthState(state)
	if !ok || !entry.agentHost {
		t.Fatal("agent-host OAuth intent was not preserved through state")
	}
	actionState := newOAuthState("user-1", "org-1", "https://app.example")
	actionEntry, ok := consumeOAuthState(actionState)
	if !ok || actionEntry.agentHost {
		t.Fatal("ordinary integration OAuth was marked as an agent-host install")
	}
}

func TestSlackAgentAPIGetPreservesReadArguments(t *testing.T) {
	oldClient := slackAgentHTTPClient
	t.Cleanup(func() { slackAgentHTTPClient = oldClient })
	slackAgentHTTPClient = &http.Client{Transport: slackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatalf("Slack read method = %q, want GET", request.Method)
		}
		if got := request.URL.Query().Get("channel"); got != "C123" {
			t.Fatalf("channel query = %q, want C123", got)
		}
		if got := request.URL.Query().Get("limit"); got != "100" {
			t.Fatalf("limit query = %q, want 100", got)
		}
		if request.Header.Get("Authorization") != "Bearer xoxb-test" {
			t.Fatal("Slack bearer token was not attached")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"channel":{"id":"C123","name":"general","is_member":true}}`)),
			Header:     http.Header{},
		}, nil
	})}
	var result struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := slackAgentAPIGet(context.Background(), "xoxb-test", "conversations.info", map[string]any{
		"channel": "C123", "limit": 100,
	}, &result); err != nil {
		t.Fatalf("Slack GET failed: %v", err)
	}
	if result.Channel.ID != "C123" {
		t.Fatalf("decoded channel = %q, want C123", result.Channel.ID)
	}
}

func TestSlackAgentAPIErrorIncludesResponseMetadata(t *testing.T) {
	oldClient := slackAgentHTTPClient
	t.Cleanup(func() { slackAgentHTTPClient = oldClient })
	slackAgentHTTPClient = &http.Client{Transport: slackRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"ok":false,"error":"invalid_arguments","response_metadata":{"messages":["[ERROR] missing required field: channel"]}}`,
			)),
			Header: http.Header{},
		}, nil
	})}
	err := slackAgentAPIGet(context.Background(), "xoxb-test", "conversations.info", map[string]any{"channel": "C123"}, nil)
	if err == nil || !strings.Contains(err.Error(), "missing required field: channel") {
		t.Fatalf("Slack error = %v, want response metadata", err)
	}
}

func TestJoinSlackAgentPublicChannelUsesJoinAPIAndRejectsPrivateChannels(t *testing.T) {
	oldClient := slackAgentHTTPClient
	t.Cleanup(func() { slackAgentHTTPClient = oldClient })
	requests := 0
	slackAgentHTTPClient = &http.Client{Transport: slackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		switch requests {
		case 1:
			if request.Method != http.MethodGet || request.URL.Path != "/api/conversations.info" || request.URL.Query().Get("channel") != "C123" {
				t.Fatalf("unexpected channel inspection request: %s %s", request.Method, request.URL.String())
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
				`{"ok":true,"channel":{"id":"C123","name":"general","is_member":false,"is_private":false}}`,
			)), Header: http.Header{}}, nil
		case 2:
			if request.Method != http.MethodPost || request.URL.Path != "/api/conversations.join" {
				t.Fatalf("unexpected channel join request: %s %s", request.Method, request.URL.String())
			}
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload["channel"] != "C123" {
				t.Fatalf("join payload = %#v, error = %v", payload, err)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
				`{"ok":true,"channel":{"id":"C123","name":"general","is_member":true,"is_private":false}}`,
			)), Header: http.Header{}}, nil
		default:
			t.Fatalf("unexpected extra Slack request: %s", request.URL.String())
			return nil, nil
		}
	})}
	channel, err := joinSlackAgentPublicChannel(context.Background(), "xoxb-test", "C123")
	if err != nil || channel.IsMember == nil || !*channel.IsMember || channel.ID != "C123" || requests != 2 {
		t.Fatalf("public join = (%#v, %v, %d requests), want joined channel", channel, err, requests)
	}

	requests = 0
	slackAgentHTTPClient = &http.Client{Transport: slackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
			`{"ok":true,"channel":{"id":"G123","name":"private","is_member":false,"is_private":true}}`,
		)), Header: http.Header{}}, nil
	})}
	_, err = joinSlackAgentPublicChannel(context.Background(), "xoxb-test", "G123")
	if !errors.Is(err, errSlackAgentPrivateChannel) || requests != 1 {
		t.Fatalf("private join = (%v, %d requests), want rejection without join API", err, requests)
	}
}

func TestSlackAgentPostTextUsesDeploymentDisplayName(t *testing.T) {
	oldClient := slackAgentHTTPClient
	t.Cleanup(func() { slackAgentHTTPClient = oldClient })
	var payload map[string]any
	slackAgentHTTPClient = &http.Client{Transport: slackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://slack.com/api/chat.postMessage" {
			t.Fatalf("Slack endpoint = %q", request.URL.String())
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode Slack payload: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Header:     http.Header{},
		}, nil
	})}
	if err := slackAgentPostText(context.Background(), "xoxb-test", "C123", "100.1", "hello", "Sales Agent"); err != nil {
		t.Fatalf("post text: %v", err)
	}
	if payload["username"] != "Sales Agent" {
		t.Fatalf("Slack username = %#v, want deployment name", payload["username"])
	}
}

func TestSlackAgentPostTextUsesDeliveryIdempotencyKey(t *testing.T) {
	oldClient := slackAgentHTTPClient
	t.Cleanup(func() { slackAgentHTTPClient = oldClient })
	var payload map[string]any
	slackAgentHTTPClient = &http.Client{Transport: slackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode Slack payload: %v", err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: http.Header{}}, nil
	})}
	deliveryID := uuid.NewString()
	if err := slackAgentPostText(withSlackAgentDeliveryID(context.Background(), deliveryID), "xoxb-test", "C123", "100.1", "hello"); err != nil {
		t.Fatalf("post text: %v", err)
	}
	if payload["client_msg_id"] != deliveryID {
		t.Fatalf("client_msg_id = %#v, want %q", payload["client_msg_id"], deliveryID)
	}
}

func TestSlackAgentGeneratedTextCannotNotifyUsersOrChannel(t *testing.T) {
	t.Parallel()
	got := slackAgentSanitizeGeneratedText("notify <!channel> <!here> <!everyone> <!subteam^S123ABC|ops> and <@U123ABC>")
	if strings.Contains(got, "<!") || strings.Contains(got, "<@") {
		t.Fatalf("generated Slack notification markup survived: %q", got)
	}
	for _, want := range []string{"@channel", "@here", "@everyone", "@user-group", "@user-U123ABC"} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitized text omitted %q: %q", want, got)
		}
	}
}

func TestSlackAgentStripSelfMentionPreservesMentionedTeammates(t *testing.T) {
	t.Parallel()
	message := "<@UFERNARY> send the summary to <@UALICE> and <@UBOB>"
	got := slackAgentStripSelfMention(message, "UFERNARY")
	if got != "send the summary to <@UALICE> and <@UBOB>" {
		t.Fatalf("stripped message = %q", got)
	}
	legacy := slackAgentStripSelfMention(message, "")
	if legacy != got {
		t.Fatalf("legacy mention fallback = %q, want %q", legacy, got)
	}
}

func TestSlackThreadContextRetainsNewestMessagesWhenBounded(t *testing.T) {
	oldClient := slackAgentHTTPClient
	t.Cleanup(func() { slackAgentHTTPClient = oldClient })
	messages := make([]map[string]string, 0, 20)
	for index := 0; index < 20; index++ {
		messages = append(messages, map[string]string{
			"user": "U123", "ts": fmt.Sprintf("100.%06d", index),
			"text": fmt.Sprintf("message-%02d-%s", index, strings.Repeat("x", 1900)),
		})
	}
	response, _ := json.Marshal(map[string]any{"ok": true, "messages": messages})
	slackAgentHTTPClient = &http.Client{Transport: slackRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(response))), Header: http.Header{}}, nil
	})}

	contextText, err := slackAgentThreadContext(context.Background(), "xoxb-test", "C123", "100.000000", "999.999999")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contextText, "message-19-") {
		t.Fatal("bounded thread context dropped the newest message")
	}
	if strings.Contains(contextText, "message-00-") {
		t.Fatal("bounded thread context retained the oldest message instead of newer context")
	}
	if strings.Index(contextText, "message-12-") > strings.Index(contextText, "message-19-") {
		t.Fatal("bounded thread context is not ordered oldest to newest")
	}
}

func TestSuccessfulHostedApprovalOutcomeSurvivesForRetry(t *testing.T) {
	t.Parallel()
	handler := &WorkflowHandler{}
	approvalID := "approval-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { handler.forgetHostedApprovalOutcome(context.Background(), approvalID) })

	if err := handler.rememberHostedApprovalOutcome(context.Background(), approvalID, `{"id":"ISSUE-1"}`); err != nil {
		t.Fatalf("remember outcome: %v", err)
	}
	outcome, found, err := handler.recoverHostedApprovalOutcome(context.Background(), approvalID)
	if err != nil {
		t.Fatalf("recover outcome: %v", err)
	}
	if !found || outcome.Output != `{"id":"ISSUE-1"}` {
		t.Fatalf("recovered outcome = (%+v, %v)", outcome, found)
	}
}

func TestSlackUnknownOutcomeIncludesRequesterReconciliationControls(t *testing.T) {
	oldClient := slackAgentHTTPClient
	t.Cleanup(func() { slackAgentHTTPClient = oldClient })
	var payload map[string]any
	slackAgentHTTPClient = &http.Client{Transport: slackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode Slack payload: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Header:     http.Header{},
		}, nil
	})}
	approval := models.HostedAgentApproval{
		BaseModel: models.BaseModel{ID: uuid.New()}, RequesterExternalID: "U123",
		Operation: "create_issue", Reason: "The teammate requested a tracked issue.",
		ExecutionKey: uuid.NewString(), DisplayDetails: models.JSONB(`{"title":"Production incident"}`),
	}
	if err := slackAgentPostUnknownOutcome(
		context.Background(), "xoxb-test", "C123", "100.1", &approval, "Ops Agent",
	); err != nil {
		t.Fatalf("post unknown outcome: %v", err)
	}
	raw, _ := json.Marshal(payload)
	message := string(raw)
	for _, want := range []string{
		"fernary_agent_outcome_completed", "fernary_agent_outcome_not_run", "Production incident", "U123", approval.ExecutionKey,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("Slack reconciliation payload is missing %q: %s", want, message)
		}
	}
}

func TestHostedApprovalInteractionActionsIncludeReconciliation(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"fernary_agent_approve":           "approve",
		"fernary_agent_reject":            "reject",
		"fernary_agent_outcome_completed": "reconcile_completed",
		"fernary_agent_outcome_not_run":   "reconcile_not_run",
	}
	for actionID, want := range tests {
		if got, ok := hostedApprovalInteractionAction(actionID); !ok || got != want {
			t.Errorf("interaction action %q = (%q, %v), want (%q, true)", actionID, got, ok, want)
		}
	}
	if got, ok := hostedApprovalInteractionAction("untrusted_action"); ok || got != "" {
		t.Fatalf("unsupported interaction action = (%q, %v), want rejected", got, ok)
	}
}
