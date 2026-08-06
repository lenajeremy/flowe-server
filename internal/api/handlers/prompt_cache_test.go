package handlers

import (
	"strings"
	"testing"
)

// Where the cache breakpoint goes.
//
// A prompt cache matches on the exact bytes of everything up to the breakpoint,
// and the clock carries a Unix second. Concatenated onto the prompt — which is
// what WithClockAndTool does, and what these call sites used to send — the
// prefix is different on every request and the cache can never hit: the builder
// re-reads its whole prompt and integration catalogue on every turn of a
// five-round tool loop, and the cache columns in the ledger stay at zero.
//
// These pin the two halves of the fix: the cacheable block holds the prompt and
// nothing else, and the clock sits after it.

func TestTheCacheableBlockIsThePromptAndOnlyThePrompt(t *testing.T) {
	const prompt = "You are a workflow builder."
	blocks := cachedSystem(prompt)

	if len(blocks) != 2 {
		t.Fatalf("want a static block and a clock block, got %d", len(blocks))
	}
	// Byte-identical to the input: anything appended here — a clock, a date, a
	// request id — is what breaks caching, and it breaks it silently.
	if got := blocks[0]["text"]; got != prompt {
		t.Errorf("the cached block is not the prompt verbatim:\n want %q\n got  %q", prompt, got)
	}
	if strings.Contains(prompt, "Unix:") {
		t.Fatal("test fixture is wrong — the prompt itself must not contain a clock")
	}
	if text, _ := blocks[1]["text"].(string); !strings.Contains(text, "Unix:") {
		t.Errorf("the clock is missing from the second block: %q", text)
	}
}

func TestTheBreakpointSitsOnTheStaticBlockNotTheClock(t *testing.T) {
	blocks := cachedSystem("You are a workflow builder.")

	if blocks[0]["cache_control"] == nil {
		t.Error("no breakpoint on the static block — nothing is cached, including the tool schemas ahead of it")
	}
	// Marked here instead, the cached prefix would end after a value that
	// changes every second: every request writes a new entry and reads none.
	if blocks[1]["cache_control"] != nil {
		t.Error("breakpoint on the clock block — the cache key would change every second")
	}
}

func TestTheFirstSystemMessageHoldsNoClock(t *testing.T) {
	// OpenAI-compatible providers have no cache_control; they match the longest
	// common prefix, so message[0] is the whole mechanism. A clock in it means
	// the prefix differs from the first byte and nothing is ever reused.
	const prompt = "You are a workflow builder."
	msgs := cachedSystemMessages(prompt)

	if len(msgs) != 2 {
		t.Fatalf("want the prompt and the clock as separate messages, got %d", len(msgs))
	}
	if got := msgs[0]["content"]; got != prompt {
		t.Errorf("message[0] is not the prompt verbatim:\n want %q\n got  %q", prompt, got)
	}
	if msgs[0]["role"] != "system" || msgs[1]["role"] != "system" {
		t.Errorf("both must be system messages, got %v and %v", msgs[0]["role"], msgs[1]["role"])
	}
	if text, _ := msgs[1]["content"].(string); !strings.Contains(text, "Unix:") {
		t.Errorf("the clock is missing from message[1]: %q", text)
	}
}
