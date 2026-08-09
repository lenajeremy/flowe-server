package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Where a model's requests go.
//
// Provider routing here used to be a single boolean: anything whose id did not
// begin with "claude" was sent to OpenAI, with OpenAI's key, at OpenAI's URL.
// That was fine while those were the only two providers the executor could
// reach, but it silently mis-sent every other model in the picker — a Gemini or
// Grok id went to api.openai.com and came back rejected.
//
// Google and xAI both publish an OpenAI-compatible chat-completions endpoint, so
// the entire request and response path is reused; only the host, the key and the
// telemetry label differ. Anthropic keeps its own path because its wire format
// genuinely differs.
//
// Adding a provider is one row, and a model id that matches no row keeps going
// to OpenAI, which is what every id did before.
var llmRoutes = []struct {
	prefix   string
	provider string
	url      string
	key      func(APIKeys) string
	keyEnv   string
	body     map[string]any
}{
	{
		prefix: "gemini", provider: "google",
		url: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		key: func(k APIKeys) string { return k.Gemini }, keyEnv: "GEMINI_API_KEY",
		// Gemini 3.x spends output tokens thinking before it writes anything, from
		// the same budget. A branch condition asking for 8 tokens came back empty
		// with finish_reason "length" — 0 content tokens, 29 spent reasoning — so
		// every branch node would have silently evaluated as false.
		//
		// It is also most of the saving: a one-word answer costs 80 total tokens
		// with thinking on and 9 with it off. "low" is not enough — it still
		// exhausts a small budget — so only "none" is safe as a default. Anyone
		// who wants deliberation picks the Pro model, which is a priced choice.
		body: map[string]any{"reasoning_effort": "none"},
	},
	{
		prefix: "grok", provider: "xai",
		url: "https://api.x.ai/v1/chat/completions",
		key: func(k APIKeys) string { return k.XAI }, keyEnv: "XAI_API_KEY",
	},
}

const openAIChatCompletionsURL = "https://api.openai.com/v1/chat/completions"

// llmRoute is a resolved destination for one call.
type llmRoute struct {
	// Provider is the telemetry label, so cost and latency stay attributable
	// per provider rather than all reporting as "openai".
	Provider string
	URL      string
	Key      string
	// Body holds provider-specific request fields merged into the payload.
	Body map[string]any
}

// routeForModel resolves an OpenAI-compatible model id to its endpoint and key.
//
// Anthropic ids never reach here — isAnthropicModel is checked first, because
// that path speaks a different protocol.
func routeForModel(model string, keys APIKeys) (llmRoute, error) {
	id := strings.ToLower(strings.TrimSpace(model))
	for _, candidate := range llmRoutes {
		if !strings.HasPrefix(id, candidate.prefix) {
			continue
		}
		key := candidate.key(keys)
		if key == "" {
			// Named after the variable rather than the provider: the fix is to set
			// it, and the error should say which one.
			return llmRoute{}, fmt.Errorf("%s is not set, so %s models cannot run", candidate.keyEnv, candidate.prefix)
		}
		return llmRoute{Provider: candidate.provider, URL: candidate.url, Key: key, Body: candidate.body}, nil
	}
	if keys.OpenAI == "" {
		return llmRoute{}, fmt.Errorf("OpenAI API key not set")
	}
	return llmRoute{Provider: "openai", URL: openAIChatCompletionsURL, Key: keys.OpenAI}, nil
}

// The models the executor reaches for when nothing was chosen.
//
// Google's Flash tier is the default because it is the cheapest thing that still
// answers these well, and workflow runs are the volume: an LLM node fires on
// every run of every published workflow, where the builder fires once while
// someone is designing. Credit billing agrees — "-flash" is treated as
// small-tier, so a Flash call costs 1x against 4x for gpt-4o.
//
// Saved workflows are unaffected: these only apply when no model was chosen.
const (
	DefaultLLMModel   = "gemini-3.5-flash"
	DefaultSmallModel = "gemini-3.5-flash"
)

// KeysFromEnv reads every provider credential the executor can use.
//
// One constructor rather than a struct literal per caller. There are five places
// that build these, in three different formatting styles, and adding a provider
// meant editing all five — miss one and that entry point silently cannot reach
// the new provider, with no compile error to catch it.
func KeysFromEnv() APIKeys {
	return APIKeys{
		Anthropic: os.Getenv("ANTHROPIC_API_KEY"),
		OpenAI:    os.Getenv("OPENAI_API_KEY"),
		Gemini:    os.Getenv("GEMINI_API_KEY"),
		XAI:       os.Getenv("XAI_API_KEY"),
		Brave:     os.Getenv("BRAVE_API_KEY"),
		Jina:      os.Getenv("JINA_API_KEY"),
	}
}

// withRouteBody merges a provider's extra request fields into a marshalled body.
//
// The request is built from a typed struct, which cannot carry a field only one
// provider understands. Round-tripping through a map is the least invasive way to
// add one without making every provider's payload generic.
func withRouteBody(body []byte, extra map[string]any) []byte {
	if len(extra) == 0 {
		return body
	}
	var merged map[string]any
	if json.Unmarshal(body, &merged) != nil {
		return body
	}
	for key, value := range extra {
		merged[key] = value
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return body
	}
	return out
}

// OpenAICompatibleBody returns the provider-specific request fields a model needs,
// for callers that build their own chat-completions payload instead of going
// through callOpenAI.
//
// The API handlers do exactly that in a few places — the permission analyzer, the
// agent chat loops — and a Gemini request assembled without these silently comes
// back empty, because the model spends its whole token budget thinking. Exported
// so those callers can apply the same fields rather than each rediscovering the
// failure.
func OpenAICompatibleBody(model string) map[string]any {
	id := strings.ToLower(strings.TrimSpace(model))
	for _, candidate := range llmRoutes {
		if strings.HasPrefix(id, candidate.prefix) {
			out := make(map[string]any, len(candidate.body))
			for key, value := range candidate.body {
				out[key] = value
			}
			return out
		}
	}
	return nil
}
