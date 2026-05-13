package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
	return tools
}

// ── Tool execution ─────────────────────────────────────────────

func executeTool(ctx context.Context, name string, input json.RawMessage, keys APIKeys) string {
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
		result, err := jinaRead(ctx, params.URL, keys.Jina)
		if err != nil {
			return "error: " + err.Error()
		}
		return result
	}
	return "error: unknown tool " + name
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.search.brave.com/res/v1/web/search", nil)
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
	jinaURL := "https://r.jina.ai/" + pageURL
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

func callAnthropicWithTools(ctx context.Context, model, system, user string, maxTok int, key string, imgs []imageRef, keys APIKeys) (string, error) {
	tools := anthropicWebTools(keys.Brave != "")

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

	const maxIter = 10
	for range maxIter {
		body, _ := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": maxTok,
			"system":     system,
			"tools":      tools,
			"messages":   messages,
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
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

		// Execute each tool and collect results
		toolResults := make([]map[string]any, 0)
		for _, b := range r.Content {
			if b.Type != "tool_use" {
				continue
			}
			result := executeTool(ctx, b.Name, b.Input, keys)
			toolResults = append(toolResults, map[string]any{
				"type":        "tool_result",
				"tool_use_id": b.ID,
				"content":     result,
			})
		}
		messages = append(messages, map[string]any{"role": "user", "content": toolResults})
	}

	return "", fmt.Errorf("tool loop exceeded max iterations")
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

func callOpenAIWithTools(ctx context.Context, model, system, user string, maxTok int, key string, imgs []imageRef, keys APIKeys) (string, error) {
	tools := openAIWebTools(keys.Brave != "")

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

	messages := []map[string]any{
		{"role": "system", "content": system},
		{"role": "user", "content": userContent},
	}

	const maxIter = 10
	for range maxIter {
		body, _ := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": maxTok,
			"messages":   messages,
			"tools":      tools,
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
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

		// Execute each tool and add results
		for _, tc := range choice.Message.ToolCalls {
			result := executeTool(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments), keys)
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": tc.ID,
				"content":      result,
			})
		}
	}

	return "", fmt.Errorf("tool loop exceeded max iterations")
}
