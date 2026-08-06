package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"workflow-ai/server/internal/executor"
)

// Live proof that the builder's prompt actually caches.
//
// Opt-in, because it spends real money against a real provider:
//
//	LIVE_PROMPT_CACHE=1 go test ./internal/api/handlers/ -run TestLiveOpenAICachesTheBuilderPrefix -v
//
// The static tests next door pin the request *shape*; only the provider can say
// whether that shape earns a cache hit. This sends the genuine builder prompt
// and tool set — not a stand-in — twice, the way a two-turn conversation does,
// and reads the cached-token count off the second response.
func TestLiveOpenAICachesTheBuilderPrefix(t *testing.T) {
	if os.Getenv("LIVE_PROMPT_CACHE") == "" {
		t.Skip("set LIVE_PROMPT_CACHE=1 to run (spends real tokens)")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}

	// Each turn is its own HTTP request, so each one rebuilds the system
	// messages and reads the clock again. Reusing one build across both turns
	// would freeze the clock and prove nothing — the whole question is what
	// happens to the prefix when the clock moves.
	turn := func(tail ...map[string]any) []map[string]any {
		return append(cachedSystemMessages(workflowSystemPrompt), tail...)
	}

	call := func(msgs []map[string]any) (prompt, cached int) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"model":                 "gpt-5.5",
			"messages":              msgs,
			"tools":                 openAIToolDefs(),
			"max_completion_tokens": 16,
		})
		req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Usage struct {
				PromptTokens        int `json:"prompt_tokens"`
				PromptTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Error.Message != "" {
			t.Fatalf("provider error (HTTP %d): %s", resp.StatusCode, out.Error.Message)
		}
		return out.Usage.PromptTokens, out.Usage.PromptTokensDetails.CachedTokens
	}

	p1, c1 := call(turn(map[string]any{"role": "user", "content": "Say READY and nothing else."}))
	t.Logf("turn 1: prompt_tokens=%d cached_tokens=%d", p1, c1)

	// The clock has second granularity — wait past a tick so turn 2 genuinely
	// reads a different time, as a real second turn would.
	time.Sleep(1500 * time.Millisecond)

	p2, c2 := call(turn(
		map[string]any{"role": "user", "content": "Say READY and nothing else."},
		map[string]any{"role": "assistant", "content": "READY"},
		map[string]any{"role": "user", "content": "Say READY again."}))
	t.Logf("turn 2: prompt_tokens=%d cached_tokens=%d", p2, c2)

	if c2 == 0 {
		t.Fatalf("turn 2 read nothing from cache (prompt_tokens=%d) — something in the prefix "+
			"is still varying per request; the builder is re-reading its whole prompt every turn", p2)
	}
	if c2 < p1/2 {
		t.Errorf("only %d of ~%d prefix tokens were cached — the breakpoint is in the wrong place", c2, p1)
	}
}

// The same proof on Anthropic, where the stakes are higher.
//
// OpenAI caches on its own and the breakpoint is implicit; Anthropic caches only
// what a cache_control marker covers, and the marked prefix has to match
// exactly — there is no partial credit for a prefix that drifts at the tail. A
// clock inside the marked block therefore costs the entire hit, tool schemas
// included, on every single turn.
//
//	LIVE_PROMPT_CACHE=1 go test ./internal/api/handlers/ -run TestLiveAnthropicCachesTheBuilderPrefix -v
func TestLiveAnthropicCachesTheBuilderPrefix(t *testing.T) {
	if os.Getenv("LIVE_PROMPT_CACHE") == "" {
		t.Skip("set LIVE_PROMPT_CACHE=1 to run (spends real tokens)")
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	call := func(msgs []map[string]any) (input, read, write int) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"model":      "claude-sonnet-4-6",
			"max_tokens": 16,
			"system":     cachedSystem(workflowSystemPrompt),
			"tools":      builderTools(),
			"messages":   msgs,
		})
		req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Error.Message != "" {
			t.Fatalf("provider error (HTTP %d, %s): %s", resp.StatusCode, out.Error.Type, out.Error.Message)
		}
		return out.Usage.InputTokens, out.Usage.CacheReadInputTokens, out.Usage.CacheCreationInputTokens
	}

	i1, r1, w1 := call([]map[string]any{{"role": "user", "content": "Say READY and nothing else."}})
	t.Logf("turn 1: input=%d cache_read=%d cache_creation=%d", i1, r1, w1)

	time.Sleep(1500 * time.Millisecond)

	i2, r2, w2 := call([]map[string]any{
		{"role": "user", "content": "Say READY and nothing else."},
		{"role": "assistant", "content": "READY"},
		{"role": "user", "content": "Say READY again."},
	})
	t.Logf("turn 2: input=%d cache_read=%d cache_creation=%d", i2, r2, w2)

	if r2 == 0 {
		t.Fatalf("turn 2 read nothing from cache (input=%d) — the marked prefix is still "+
			"changing between turns", i2)
	}
	// input_tokens on Anthropic is the uncached remainder, so a working cache
	// makes it collapse to the tail while the read count carries the prefix.
	if i2 > r2 {
		t.Errorf("more tokens billed uncached (%d) than served from cache (%d) — the breakpoint "+
			"is covering the wrong part of the prompt", i2, r2)
	}
}

// How big the agent chat's cacheable prefix actually is.
//
// Caching has a floor: a prefix under the provider's minimum caches nothing, in
// silence. The builder clears it easily (its prompt inlines the whole
// integration catalogue); the agent chat's prompt is a few paragraphs plus one
// schema per node, so whether it clears anything is a question about the
// workflow, not about this code. Measured, not assumed.
func TestLiveAgentChatPrefixSize(t *testing.T) {
	if os.Getenv("LIVE_PROMPT_CACHE") == "" {
		t.Skip("set LIVE_PROMPT_CACHE=1 to run (spends real tokens)")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}

	system := agentSystemPrompt(
		executor.WorkflowAST{Name: "Daily standup digest"},
		[]agentTool{{Schema: map[string]any{
			"name": "summarize", "description": "Summarize the new Linear issues",
			"input_schema": executor.ClockToolSchema(),
		}}},
		map[string]string{"last_run": "2026-08-05"},
	)
	msgs := append(cachedSystemMessages(system),
		map[string]any{"role": "user", "content": "Say READY."})

	body, _ := json.Marshal(map[string]any{
		"model": "gpt-5.5", "messages": msgs, "max_completion_tokens": 16,
	})
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	json.NewDecoder(resp.Body).Decode(&out)

	t.Logf("agent-chat prefix for a one-node workflow: %d prompt tokens", out.Usage.PromptTokens)
	if out.Usage.PromptTokens < 1024 {
		t.Logf("BELOW the 1024-token floor OpenAI and Opus 4.8/Sonnet 4.6 need, and well below "+
			"Haiku 4.5's 4096 — a workflow this small will cache nothing whatever this code does. "+
			"Bigger canvases (more nodes, more schemas) clear it. (%d tokens)", out.Usage.PromptTokens)
	}
}
