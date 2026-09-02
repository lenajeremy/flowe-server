package executor

import (
	"net/url"
	"strings"
	"testing"
)

// Where a model's traffic actually goes.
//
// Routing used to be "does the id start with claude" — everything else went to
// api.openai.com with the OpenAI key. Picking Gemini in the model dropdown
// therefore posted a Gemini id to OpenAI, which rejected it. These assert on the
// host, because that is the thing that was wrong and a plausible-looking route
// struct proves nothing.

func TestEachProviderGetsItsOwnEndpointAndKey(t *testing.T) {
	keys := APIKeys{OpenAI: "sk-openai", Gemini: "gm-key", XAI: "xai-key"}

	for _, tc := range []struct {
		model, host, provider, key string
	}{
		{"gemini-3.5-flash", "generativelanguage.googleapis.com", "google", "gm-key"},
		{"gemini-3.1-pro-preview", "generativelanguage.googleapis.com", "google", "gm-key"},
		{"grok-4.5", "api.x.ai", "xai", "xai-key"},
		{"gpt-5.5", "api.openai.com", "openai", "sk-openai"},
		// An id nobody has taught us about keeps going where every id went
		// before, rather than failing closed on a model that might work.
		{"some-new-model", "api.openai.com", "openai", "sk-openai"},
	} {
		route, err := routeForModel(tc.model, keys)
		if err != nil {
			t.Errorf("%s: %v", tc.model, err)
			continue
		}
		if !strings.Contains(route.URL, tc.host) {
			t.Errorf("%s routed to %s, want host %s", tc.model, route.URL, tc.host)
		}
		if route.Provider != tc.provider {
			t.Errorf("%s reported provider %q, want %q — telemetry attributes cost by this",
				tc.model, route.Provider, tc.provider)
		}
		if route.Key != tc.key {
			t.Errorf("%s would send the wrong provider's key", tc.model)
		}
	}
}

// The failure that matters most: sending a Gemini id to OpenAI. It does not error
// at the routing layer, it errors at the provider, which reads as "Gemini is
// broken" rather than "we never called Gemini".
func TestAGeminiModelIsNeverSentToOpenAI(t *testing.T) {
	route, err := routeForModel("gemini-3.5-flash", APIKeys{OpenAI: "sk-openai", Gemini: "gm-key"})
	if err != nil {
		t.Fatal(err)
	}
	// Matching on the host, not the URL: Google's compatibility endpoint has
	// "openai" in its own path (/v1beta/openai/chat/completions), so a substring
	// check on the whole URL reports a false positive.
	parsed, parseErr := url.Parse(route.URL)
	if parseErr != nil {
		t.Fatalf("unparseable route URL %q: %v", route.URL, parseErr)
	}
	if parsed.Host == "api.openai.com" {
		t.Fatalf("gemini model routed to %s", route.URL)
	}
	if route.Key == "sk-openai" {
		t.Fatal("gemini call would carry the OpenAI key")
	}
}

// A missing key has to name the variable to set. "OpenAI API key not set" while
// running a Gemini model sent people to the wrong dashboard.
func TestAMissingKeyNamesItsOwnEnvVar(t *testing.T) {
	_, err := routeForModel("gemini-3.5-flash", APIKeys{OpenAI: "sk-openai"})
	if err == nil {
		t.Fatal("expected an error when GEMINI_API_KEY is unset")
	}
	if !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Errorf("error was %q; it must name GEMINI_API_KEY", err)
	}
	if strings.Contains(err.Error(), "OpenAI") {
		t.Errorf("error was %q; it points at the wrong provider", err)
	}
}

// The default is the whole point of the change: an LLM node with no model chosen
// must land on the cheap tier, and it must be a model the router can place.
func TestTheDefaultLLMModelIsCheapAndRoutable(t *testing.T) {
	if !strings.Contains(DefaultLLMModel, "flash") {
		t.Errorf("DefaultLLMModel = %q, which is not a small-tier model", DefaultLLMModel)
	}
	route, err := routeForModel(DefaultLLMModel, APIKeys{Gemini: "gm-key"})
	if err != nil {
		t.Fatalf("the default model does not route: %v", err)
	}
	if route.Provider != "google" {
		t.Errorf("default routed to %q", route.Provider)
	}
}

// KeysFromEnv exists so a new provider cannot be forgotten at one of the five
// entry points. If a field is left unread the executor silently cannot reach that
// provider from whichever path built the struct.
func TestKeysFromEnvReadsEveryProvider(t *testing.T) {
	for _, env := range []string{
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY",
		"XAI_API_KEY", "BRAVE_API_KEY", "JINA_API_KEY",
	} {
		t.Setenv(env, "value-of-"+env)
	}
	keys := KeysFromEnv()
	for name, got := range map[string]string{
		"ANTHROPIC_API_KEY": keys.Anthropic,
		"OPENAI_API_KEY":    keys.OpenAI,
		"GEMINI_API_KEY":    keys.Gemini,
		"XAI_API_KEY":       keys.XAI,
		"BRAVE_API_KEY":     keys.Brave,
		"JINA_API_KEY":      keys.Jina,
	} {
		if got != "value-of-"+name {
			t.Errorf("%s was not read into APIKeys (got %q)", name, got)
		}
	}
}

// Gemini 3.x thinks with the same token budget it answers from, so a small
// max_completion_tokens produced an empty response with finish_reason "length".
// Branch conditions ask for 8 tokens, so every branch node would have evaluated
// as false. Verified live: 32 tokens returned "" before this, "Paris" after.
func TestGeminiCallsDisableReasoningSoSmallBudgetsStillAnswer(t *testing.T) {
	route, err := routeForModel("gemini-3.5-flash", APIKeys{Gemini: "gm-key"})
	if err != nil {
		t.Fatal(err)
	}
	if got := route.Body["reasoning_effort"]; got != "none" {
		t.Errorf("reasoning_effort = %v, want \"none\" — \"low\" still exhausts a small budget", got)
	}

	// And it has to survive into the request body, which is built from a typed
	// struct that has no field for it.
	merged := withRouteBody([]byte(`{"model":"gemini-3.5-flash","max_tokens":8}`), route.Body)
	if !strings.Contains(string(merged), `"reasoning_effort":"none"`) {
		t.Errorf("body lost the provider field: %s", merged)
	}
	if !strings.Contains(string(merged), `"max_tokens":8`) {
		t.Errorf("merge dropped an existing field: %s", merged)
	}
}

// OpenAI must not receive a Google-only field.
func TestOpenAIRequestsCarryNoProviderSpecificFields(t *testing.T) {
	route, err := routeForModel("gpt-5.5", APIKeys{OpenAI: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	if len(route.Body) != 0 {
		t.Errorf("openai route carries %v, which it does not understand", route.Body)
	}
	original := []byte(`{"model":"gpt-5.5"}`)
	if got := withRouteBody(original, route.Body); string(got) != string(original) {
		t.Errorf("body was rewritten with nothing to add: %s", got)
	}
}

// Every handler that assembles its own chat-completions body needs the same
// provider fields the router applies. The permission analyzer, the agent chat
// loop and the builder all do, and each was found missing them: on Gemini that
// means a request that thinks away its budget and returns nothing.
func TestOpenAICompatibleBodyIsAvailableToHandlers(t *testing.T) {
	got := OpenAICompatibleBody("gemini-3.5-flash")
	if got["reasoning_effort"] != "none" {
		t.Errorf("gemini body = %v, want reasoning_effort none", got)
	}
	if fields := OpenAICompatibleBody("gpt-5.5"); len(fields) != 0 {
		t.Errorf("openai body = %v, want nothing", fields)
	}

	// A copy, so a caller merging into its payload cannot mutate the table and
	// change every later request.
	got["reasoning_effort"] = "tampered"
	if again := OpenAICompatibleBody("gemini-3.5-flash"); again["reasoning_effort"] != "none" {
		t.Error("the returned map aliases the route table")
	}
}
