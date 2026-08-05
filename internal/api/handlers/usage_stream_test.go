package handlers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"workflow-ai/server/internal/telemetry"
)

// Streaming usage is the easiest thing in the billing path to lose, because both
// providers report it somewhere a naive reader skips: Anthropic splits it across
// the first and last event, and OpenAI puts it in a terminal chunk that carries no
// choices at all. These tests feed synthetic streams through the real consumers.

// captureUsage installs a UsageSink for the duration of one test.
func captureUsage(t *testing.T) *[]telemetry.Usage {
	t.Helper()
	var got []telemetry.Usage
	prev := telemetry.UsageSink
	telemetry.UsageSink = func(_ context.Context, _, _ string, u telemetry.Usage) {
		got = append(got, u)
	}
	t.Cleanup(func() { telemetry.UsageSink = prev })
	return &got
}

// streamCtx returns a gin context whose writer discards, plus the response a
// consumer will read.
func streamCtx(sse string) (*gin.Context, *http.Response, http.Flusher) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	return c, resp, rec
}

func TestAnthropicStreamUsageSpansMessageStartAndDelta(t *testing.T) {
	got := captureUsage(t)

	// Input counts arrive on message_start, output on the terminal message_delta.
	// Reading only one of them was the bug this guards.
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1200,"cache_read_input_tokens":800,"cache_creation_input_tokens":300,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":450}}`,
		`data: {"type":"message_stop"}`,
	}, "\n") + "\n"

	c, resp, flusher := streamCtx(stream)
	if _, _, err := consumeStream(c, resp, flusher, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("consumeStream: %v", err)
	}

	if len(*got) != 1 {
		t.Fatalf("expected exactly one usage record, got %d", len(*got))
	}
	u := (*got)[0]
	// output_tokens appears on both events (1 then 450) and Anthropic means the
	// running total on message_delta, so the sum is what the API reports.
	if u.InputTokens != 1200 || u.OutputTokens != 451 ||
		u.CacheReadTokens != 800 || u.CacheWriteTokens != 300 {
		t.Fatalf("wrong usage: %+v", u)
	}
}

func TestOpenAIStreamUsageSurvivesTheChoicelessTerminalChunk(t *testing.T) {
	got := captureUsage(t)

	// The last chunk has "choices":[] — any guard that skips choice-less chunks
	// before reading usage silently makes the whole builder surface free.
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hel"},"finish_reason":""}]}`,
		`data: {"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":900,"completion_tokens":120,"total_tokens":1020,"prompt_tokens_details":{"cached_tokens":400}}}`,
		`data: [DONE]`,
	}, "\n") + "\n"

	c, resp, flusher := streamCtx(stream)
	content, _, err := consumeOpenAIStream(c, resp, flusher, "openai", "gpt-4o")
	if err != nil {
		t.Fatalf("consumeOpenAIStream: %v", err)
	}
	if content != "hello" {
		t.Fatalf("content = %q, want %q", content, "hello")
	}

	if len(*got) != 1 {
		t.Fatalf("expected exactly one usage record, got %d", len(*got))
	}
	u := (*got)[0]
	// 900 prompt tokens include the 400 cached ones, so uncached input is 500.
	// Billing both at full rate would overcharge every cached call.
	if u.InputTokens != 500 || u.OutputTokens != 120 || u.CacheReadTokens != 400 {
		t.Fatalf("wrong usage: %+v", u)
	}
}

func TestStreamThatReportsNoUsageIsFlaggedNotSilent(t *testing.T) {
	got := captureUsage(t)

	// A provider that reports nothing must not reach the billing sink — it has to
	// land on the unmeasured counter instead, where it can be alerted on.
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\ndata: [DONE]\n"
	c, resp, flusher := streamCtx(stream)
	if _, _, err := consumeOpenAIStream(c, resp, flusher, "openai", "gpt-4o"); err != nil {
		t.Fatalf("consumeOpenAIStream: %v", err)
	}
	if len(*got) != 0 {
		t.Fatalf("zero usage must not be billed, got %+v", *got)
	}
}

func TestStreamUsageIsRecordedEvenWhenTheStreamErrors(t *testing.T) {
	got := captureUsage(t)

	// Tokens spent before a mid-flight failure were still spent. The response
	// here is a non-200, which is the one case that must NOT bill, followed by a
	// truncated 200 stream, which must.
	c, resp, flusher := streamCtx("")
	resp.StatusCode = http.StatusInternalServerError
	resp.Body = io.NopCloser(bytes.NewReader([]byte(`{"error":"boom"}`)))
	if _, _, err := consumeStream(c, resp, flusher, "claude-sonnet-4-6"); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
	if len(*got) != 0 {
		t.Fatalf("a rejected request bills nothing, got %+v", *got)
	}

	// Truncated after message_start: input tokens were consumed, output never
	// arrived, and the sink must still see the input.
	truncated := `data: {"type":"message_start","message":{"usage":{"input_tokens":700,"output_tokens":0}}}` + "\n"
	c2, resp2, flusher2 := streamCtx(truncated)
	if _, _, err := consumeStream(c2, resp2, flusher2, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("consumeStream: %v", err)
	}
	if len(*got) != 1 || (*got)[0].InputTokens != 700 {
		t.Fatalf("truncated stream should still bill its input, got %+v", *got)
	}
}
