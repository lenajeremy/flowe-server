package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
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
