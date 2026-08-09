package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"workflow-ai/server/internal/telemetry"
)

// ── Tool definitions ──────────────────────────────────────────

// anthropicWebTools returns Anthropic-format tool definitions.
// web_search is omitted when no Brave API key is configured.
func anthropicWebTools(hasSearch bool) []map[string]any {
	tools := []map[string]any{}
	if hasSearch {
		tools = append(tools, map[string]any{
			"name":        "web_search",
			"description": "Search the web for current information, articles, news, and resources. Returns a list of results with titles, URLs, and descriptions. Use this first to find relevant links, then call read_url to get the full content.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "The search query",
					},
				},
				"required": []string{"query"},
			},
		})
	}
	tools = append(tools, map[string]any{
		"name":        "read_url",
		"description": "Fetch and read the full content of a webpage as markdown. Works on JavaScript-rendered pages, blogs, documentation, and any public URL. Use this to read the actual content of links found via web_search.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The full URL of the webpage to read",
				},
			},
			"required": []string{"url"},
		},
	})
	tools = append(tools, map[string]any{
		"name":         ClockToolName,
		"description":  ClockToolDesc,
		"input_schema": ClockToolSchema(),
	})
	return tools
}

// openAIWebTools returns OpenAI function-calling format tool definitions.
func openAIWebTools(hasSearch bool) []map[string]any {
	tools := []map[string]any{}
	if hasSearch {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "web_search",
				"description": "Search the web for current information, articles, news, and resources. Returns a list of results with titles, URLs, and descriptions. Use this first to find relevant links, then call read_url to get the full content.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "The search query",
						},
					},
					"required": []string{"query"},
				},
			},
		})
	}
	tools = append(tools, map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "read_url",
			"description": "Fetch and read the full content of a webpage as markdown. Works on JavaScript-rendered pages, blogs, documentation, and any public URL. Use this to read the actual content of links found via web_search.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "The full URL of the webpage to read",
					},
				},
				"required": []string{"url"},
			},
		},
	})
	tools = append(tools, map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        ClockToolName,
			"description": ClockToolDesc,
			"parameters":  ClockToolSchema(),
		},
	})
	return tools
}

// Endpoints, as vars so tests can drive the loop against a stub. Same seam the
// rest of the package already uses (asanaAPIURL, mondayAPIURL).
var (
	anthropicToolsURL = "https://api.anthropic.com/v1/messages"
	openAIToolsURL    = "https://api.openai.com/v1/chat/completions"
	braveSearchURL    = "https://api.search.brave.com/res/v1/web/search"
	jinaReadBase      = "https://r.jina.ai/"
)

// ── Research budget ────────────────────────────────────────────

const (
	// maxToolIters bounds the loop. Every iteration is another billable model
	// call, so this is a cost ceiling as much as a safety one. Real research —
	// search, read several sources, search again on what those turned up, write
	// it up — routinely needs more than the ten rounds this used to allow.
	maxToolIters = 16
	// wrapUpAt is how many iterations before the ceiling the model is told to
	// stop gathering. Hitting the ceiling with nothing written is the failure
	// this exists to prevent: the work is paid for and then discarded.
	wrapUpAt = 3
	// maxParallelTools caps concurrent tool execution. Reads go through one
	// upstream reader, so unbounded fan-out turns a single research turn into a
	// burst against it.
	maxParallelTools = 4
)

// wrapUpNotice is appended to the tool results once the loop is close to its
// ceiling. It is our own text in our own message, not model or page content.
const wrapUpNotice = "Note: you are nearly out of research budget. Stop gathering " +
	"and write your final answer from what you already have."

// ── Tool execution ─────────────────────────────────────────────

// readCache memoizes read_url for the life of one tool loop.
//
// Research doubles back: two searches on related queries return overlapping
// links, and the model re-reads a page it looked at three rounds ago. Without
// this, each re-read pays the fetch latency, another reader call, and the tokens
// to carry the same 20k characters through the conversation again.
//
// A duplicate URL inside a single parallel batch can still be fetched twice —
// the lock is only held around map access, not across the fetch. That is a
// deliberate trade: holding it across a 30-second fetch would serialize the
// batch, and the same model turn asking for one URL twice is not a real case.
type readCache struct {
	mu    sync.Mutex
	pages map[string]string
}

func newReadCache() *readCache { return &readCache{pages: map[string]string{}} }

func (c *readCache) get(url string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	page, ok := c.pages[url]
	return page, ok
}

// put stores a successful read. Failures are never cached — a timeout should
// not permanently poison a URL for the rest of the run.
func (c *readCache) put(url, page string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pages[url] = page
}

func executeTool(ctx context.Context, name string, input json.RawMessage, keys APIKeys, cache *readCache) string {
	switch name {
	case "web_search":
		var params struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(input, &params); err != nil {
			return "error: invalid tool input"
		}
		result, err := braveSearch(ctx, params.Query, keys.Brave)
		if err != nil {
			return "error: " + err.Error()
		}
		return result

	case "read_url":
		var params struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(input, &params); err != nil {
			return "error: invalid tool input"
		}
		if page, ok := cache.get(params.URL); ok {
			return page
		}
		result, err := jinaRead(ctx, params.URL, keys.Jina)
		if err != nil {
			return "error: " + err.Error()
		}
		cache.put(params.URL, result)
		return result

	case ClockToolName:
		// No input at all is valid — the timezone is optional.
		var params struct {
			Timezone string `json:"timezone"`
		}
		_ = json.Unmarshal(input, &params)
		return CurrentTime(params.Timezone)
	}
	return "error: unknown tool " + name
}

// toolCall is one requested invocation, in neither provider's wire shape so the
// two loops can share the runner below.
type toolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// runToolBatch executes one turn's tool calls concurrently, returning results in
// request order.
//
// A model that asks for four reads in one turn is telling you they are
// independent. Running them in sequence made that turn take four times as long
// as it needed to, against a 30-second per-read timeout.
func runToolBatch(ctx context.Context, calls []toolCall, keys APIKeys, cache *readCache) []string {
	results := make([]string, len(calls))
	if len(calls) < 2 {
		for i, call := range calls {
			results[i] = executeTool(ctx, call.Name, call.Input, keys, cache)
		}
		return results
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelTools)
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = executeTool(ctx, call.Name, call.Input, keys, cache)
		}()
	}
	wg.Wait()
	return results
}

// anthropicSystem renders the system prompt as blocks with the clock in its own
// trailing block.
//
// The clock carries a Unix second, so glued onto the end of the prompt it makes
// the whole prefix unique per request and nothing in front of it can ever be
// reused. Held apart and placed last, the static instructions stay
// byte-identical between runs. Anthropic rejects an empty text block, so a
// caller with no system prompt gets the clock alone.
func anthropicSystem(system string) []map[string]any {
	blocks := make([]map[string]any, 0, 2)
	if strings.TrimSpace(system) != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": system})
	}
	return append(blocks, map[string]any{"type": "text", "text": ClockBlock(true)})
}

// ── Brave Search ──────────────────────────────────────────────

type braveSearchResp struct {
	Web struct {
		Results []struct {
			Title         string   `json:"title"`
			URL           string   `json:"url"`
			Description   string   `json:"description"`
			ExtraSnippets []string `json:"extra_snippets,omitempty"`
		} `json:"results"`
	} `json:"web"`
}

func braveSearch(ctx context.Context, query, apiKey string) (string, error) {
	if apiKey == "" {
		return "", fmt.Errorf("BRAVE_API_KEY not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, braveSearchURL, nil)
	if err != nil {
		return "", err
	}
	q := req.URL.Query()
	q.Set("q", query)
	q.Set("count", "8")
	q.Set("extra_snippets", "true")
	req.URL.RawQuery = q.Encode()
	req.Header.Set("X-Subscription-Token", apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("brave search %d: %s", resp.StatusCode, raw)
	}
	var result braveSearchResp
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, r := range result.Web.Results {
		sb.WriteString(fmt.Sprintf("[%d] %s\nURL: %s\n%s\n", i+1, r.Title, r.URL, r.Description))
		for _, snippet := range r.ExtraSnippets {
			sb.WriteString("  > " + snippet + "\n")
		}
		sb.WriteString("\n")
	}
	if sb.Len() == 0 {
		return "No results found.", nil
	}
	return sb.String(), nil
}

// ── Jina Reader ───────────────────────────────────────────────

func jinaRead(ctx context.Context, pageURL, apiKey string) (string, error) {
	jinaURL := jinaReadBase + pageURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jinaURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("X-Return-Format", "markdown")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(raw)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return "", fmt.Errorf("jina read %d: %s", resp.StatusCode, snippet)
	}
	content := string(raw)
	const maxLen = 20_000
	if len(content) > maxLen {
		content = content[:maxLen] + "\n\n[Content truncated to 20 000 characters]"
	}
	return content, nil
}

// ── Anthropic with tool loop ──────────────────────────────────

type anthropicToolResp struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text,omitempty"`
		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
}

func callAnthropicWithTools(ctx context.Context, model, system, user string, maxTok int, key string, imgs []imageRef, keys APIKeys) (out string, err error) {
	ctx, llmDone := telemetry.StartLLM(ctx, "anthropic", model)
	defer func() { llmDone(len(out), err) }()

	systemBlocks := anthropicSystem(system)
	tools := anthropicWebTools(keys.Brave != "")
	cache := newReadCache()

	// Build initial user message
	var userContent any
	if len(imgs) > 0 {
		blocks := make([]map[string]any, 0, len(imgs)+1)
		for _, img := range imgs {
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": img.MediaType,
					"data":       img.Data,
				},
			})
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": user})
		userContent = blocks
	} else {
		userContent = user
	}

	messages := []map[string]any{
		{"role": "user", "content": userContent},
	}

	for i := range maxToolIters {
		payload := map[string]any{
			"model":      model,
			"max_tokens": maxTok,
			"system":     systemBlocks,
			"tools":      tools,
			"messages":   messages,
		}
		if i == maxToolIters-1 {
			// Out of budget. The tool definitions have to stay — the history is
			// full of tool_use blocks that need them to validate — but forbid
			// another call so this turn has to be the write-up. Without this the
			// loop fell off the end and returned an error, throwing away every
			// page it had paid to read.
			payload["tool_choice"] = map[string]any{"type": "none"}
		}
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, anthropicToolsURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, raw)
		}
		// Each round of the tool loop is its own billable call, so usage is
		// recorded per iteration rather than once for the whole conversation.
		telemetry.LLMTokens(ctx, "anthropic", model, usageFromAnthropic(raw))

		var r anthropicToolResp
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", err
		}

		if r.StopReason != "tool_use" {
			// end_turn or other terminal — return text
			for _, b := range r.Content {
				if b.Type == "text" {
					return b.Text, nil
				}
			}
			return "", nil
		}

		// Build assistant message from all content blocks
		assistantBlocks := make([]map[string]any, 0, len(r.Content))
		for _, b := range r.Content {
			switch b.Type {
			case "text":
				assistantBlocks = append(assistantBlocks, map[string]any{"type": "text", "text": b.Text})
			case "tool_use":
				assistantBlocks = append(assistantBlocks, map[string]any{
					"type":  "tool_use",
					"id":    b.ID,
					"name":  b.Name,
					"input": b.Input,
				})
			}
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": assistantBlocks})

		calls := make([]toolCall, 0, len(r.Content))
		for _, b := range r.Content {
			if b.Type == "tool_use" {
				calls = append(calls, toolCall{ID: b.ID, Name: b.Name, Input: b.Input})
			}
		}
		results := runToolBatch(ctx, calls, keys, cache)

		resultBlocks := make([]map[string]any, 0, len(calls)+1)
		for j, call := range calls {
			resultBlocks = append(resultBlocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": call.ID,
				"content":     results[j],
			})
		}
		if i+1 >= maxToolIters-wrapUpAt {
			resultBlocks = append(resultBlocks, map[string]any{"type": "text", "text": wrapUpNotice})
		}
		// One moving cache breakpoint, on the last block of the turn just
		// appended. Everything ahead of it — tools, system, and every page
		// already read — is a stable prefix, so the next iteration re-reads it at
		// cache rates instead of paying full price for the same text again. Page
		// text is the bulk of a research conversation, which is what makes this
		// worth doing here.
		//
		// A breakpoint looks back at most 20 content blocks for the previous
		// entry. One iteration adds 2N+2 blocks for N tool calls, so a turn
		// asking for nine or more tools at once exceeds the window and simply
		// pays full price — a silent miss, not a failure.
		if len(resultBlocks) > 0 {
			resultBlocks[len(resultBlocks)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		messages = append(messages, map[string]any{"role": "user", "content": resultBlocks})
	}

	// Unreachable while the final iteration forbids tool use: that turn cannot
	// stop on tool_use, so it always returns above.
	return "", fmt.Errorf("tool loop ended without a final answer")
}

// ── OpenAI with tool loop ─────────────────────────────────────

type openAIToolResp struct {
	Choices []struct {
		Message struct {
			Role      string  `json:"role"`
			Content   *string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

func callOpenAIWithTools(ctx context.Context, model, system, user string, maxTok int, key string, imgs []imageRef, keys APIKeys) (out string, err error) {
	ctx, llmDone := telemetry.StartLLM(ctx, "openai", model)
	defer func() { llmDone(len(out), err) }()

	tools := openAIWebTools(keys.Brave != "")
	cache := newReadCache()

	// Build initial user message content
	var userContent any
	if len(imgs) > 0 {
		blocks := make([]map[string]any, 0, len(imgs)+1)
		for _, img := range imgs {
			blocks = append(blocks, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:" + img.MediaType + ";base64," + img.Data},
			})
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": user})
		userContent = blocks
	} else {
		userContent = user
	}

	// The clock sits in its own trailing system message so the static prompt in
	// front of it is a stable prefix. OpenAI-compatible caching is automatic on
	// the longest common prefix, so a per-second timestamp in the first message
	// is by itself enough to make every run a cold start.
	messages := make([]map[string]any, 0, 3)
	if strings.TrimSpace(system) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	messages = append(messages,
		map[string]any{"role": "system", "content": ClockBlock(true)},
		map[string]any{"role": "user", "content": userContent},
	)

	for i := range maxToolIters {
		payload := map[string]any{
			"model": model,
			// max_completion_tokens replaced max_tokens; gpt-5.x hard-errors on
			// the old name (openAIRequest was fixed for this, this path wasn't).
			"max_completion_tokens": maxTok,
			"messages":              messages,
			"tools":                 tools,
		}
		if i == maxToolIters-1 {
			payload["tool_choice"] = "none"
		}
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, openAIToolsURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("openai %d: %s", resp.StatusCode, raw)
		}
		telemetry.LLMTokens(ctx, "openai", model, usageFromOpenAI(raw))

		var r openAIToolResp
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", err
		}
		if len(r.Choices) == 0 {
			return "", fmt.Errorf("openai: empty response")
		}

		choice := r.Choices[0]
		if choice.FinishReason != "tool_calls" {
			if choice.Message.Content != nil {
				return *choice.Message.Content, nil
			}
			return "", nil
		}

		// Add assistant message with tool_calls
		messages = append(messages, map[string]any{
			"role":       "assistant",
			"content":    choice.Message.Content,
			"tool_calls": choice.Message.ToolCalls,
		})

		calls := make([]toolCall, 0, len(choice.Message.ToolCalls))
		for _, tc := range choice.Message.ToolCalls {
			calls = append(calls, toolCall{
				ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(tc.Function.Arguments),
			})
		}
		results := runToolBatch(ctx, calls, keys, cache)

		// One tool message per call, in request order — a provider that sees
		// results out of order relative to the tool_calls array rejects the turn.
		for j, call := range calls {
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": call.ID,
				"content":      results[j],
			})
		}
		if i+1 >= maxToolIters-wrapUpAt {
			messages = append(messages, map[string]any{"role": "user", "content": wrapUpNotice})
		}
	}

	// Unreachable while the final iteration forbids tool use.
	return "", fmt.Errorf("tool loop ended without a final answer")
}
