package executor

import (
	"encoding/json"

	"workflow-ai/server/internal/telemetry"
)

// Reading token usage out of a provider response.
//
// Every provider reports usage, but each names the fields differently and each
// puts cached input somewhere else. These two shapes cover all call sites, and
// both are decoded from the raw body rather than added to the existing response
// structs — a call site that ignores usage should not compile away the fields.

// anthropicUsage matches the usage object on a Messages API response. Cache
// fields are absent unless the request used cache_control.
type anthropicUsage struct {
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// openAIUsage matches the usage object on a Chat Completions response. Cached
// input is nested a level deeper than the rest.
type openAIUsage struct {
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// usageFromAnthropic reads usage from a raw Anthropic response body.
func usageFromAnthropic(raw []byte) telemetry.Usage {
	var u anthropicUsage
	if json.Unmarshal(raw, &u) != nil {
		return telemetry.Usage{}
	}
	return telemetry.Usage{
		InputTokens:      u.Usage.InputTokens,
		OutputTokens:     u.Usage.OutputTokens,
		CacheReadTokens:  u.Usage.CacheReadInputTokens,
		CacheWriteTokens: u.Usage.CacheCreationInputTokens,
	}
}

// usageFromOpenAI reads usage from a raw OpenAI response body. OpenAI's
// prompt_tokens *includes* the cached ones, so the cached count is subtracted
// out — otherwise cached input would be billed twice, once at each rate.
func usageFromOpenAI(raw []byte) telemetry.Usage {
	var u openAIUsage
	if json.Unmarshal(raw, &u) != nil {
		return telemetry.Usage{}
	}
	cached := u.Usage.PromptTokensDetails.CachedTokens
	input := u.Usage.PromptTokens - cached
	if input < 0 {
		input = 0
	}
	return telemetry.Usage{
		InputTokens:     input,
		OutputTokens:    u.Usage.CompletionTokens,
		CacheReadTokens: cached,
	}
}
