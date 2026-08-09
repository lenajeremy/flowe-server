package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readerStub stands in for the page reader. It records how many times each page
// was actually fetched and how many fetches overlapped.
type readerStub struct {
	mu       sync.Mutex
	fetches  map[string]int
	inFlight int32
	peak     int32
	delay    time.Duration
	fail     map[string]int // page → remaining failures before it starts working
}

func newReaderStub(delay time.Duration) *readerStub {
	return &readerStub{fetches: map[string]int{}, fail: map[string]int{}, delay: delay}
}

func (s *readerStub) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := strings.TrimPrefix(r.URL.Path, "/")

		now := atomic.AddInt32(&s.inFlight, 1)
		for {
			peak := atomic.LoadInt32(&s.peak)
			if now <= peak || atomic.CompareAndSwapInt32(&s.peak, peak, now) {
				break
			}
		}
		defer atomic.AddInt32(&s.inFlight, -1)

		s.mu.Lock()
		s.fetches[page]++
		remaining := s.fail[page]
		if remaining > 0 {
			s.fail[page] = remaining - 1
		}
		s.mu.Unlock()

		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		if remaining > 0 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("reader is down"))
			return
		}
		_, _ = fmt.Fprintf(w, "contents of %s", page)
	}))
	t.Cleanup(srv.Close)

	original := jinaReadBase
	jinaReadBase = srv.URL + "/"
	t.Cleanup(func() { jinaReadBase = original })
}

func (s *readerStub) fetchCount(page string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches[page]
}

func readCall(page string) toolCall {
	return toolCall{
		ID:    "call_" + page,
		Name:  "read_url",
		Input: json.RawMessage(`{"url":"` + page + `"}`),
	}
}

// A page the model comes back to is a normal part of research — two searches on
// related queries return overlapping links. Re-fetching it costs latency, a
// reader call and the tokens to carry the same text through the loop again.
func TestARepeatedReadCostsOneFetch(t *testing.T) {
	stub := newReaderStub(0)
	stub.serve(t)
	cache := newReadCache()

	first := executeTool(context.Background(), "read_url", json.RawMessage(`{"url":"alpha"}`), APIKeys{}, cache)
	second := executeTool(context.Background(), "read_url", json.RawMessage(`{"url":"alpha"}`), APIKeys{}, cache)

	if first != "contents of alpha" || second != first {
		t.Fatalf("both reads should return the page: %q then %q", first, second)
	}
	if got := stub.fetchCount("alpha"); got != 1 {
		t.Errorf("wanted 1 upstream fetch for a repeated URL, got %d", got)
	}
}

// A read that fails must stay retryable. Caching the failure would let one
// timeout blacklist a source for the rest of the run.
func TestAFailedReadIsRetriedNotRemembered(t *testing.T) {
	stub := newReaderStub(0)
	stub.fail["flaky"] = 1
	stub.serve(t)
	cache := newReadCache()

	if got := executeTool(context.Background(), "read_url", json.RawMessage(`{"url":"flaky"}`), APIKeys{}, cache); !strings.HasPrefix(got, "error:") {
		t.Fatalf("first read should report the failure, got %q", got)
	}
	if got := executeTool(context.Background(), "read_url", json.RawMessage(`{"url":"flaky"}`), APIKeys{}, cache); got != "contents of flaky" {
		t.Errorf("second read should retry and succeed, got %q", got)
	}
	if got := stub.fetchCount("flaky"); got != 2 {
		t.Errorf("wanted the failed read retried (2 fetches), got %d", got)
	}
}

// The model asks for several reads in one turn because they are independent.
// Running them in sequence multiplied a multi-source turn by the number of
// sources, against a 30-second timeout on each.
func TestABatchOfReadsRunsConcurrentlyInOrder(t *testing.T) {
	stub := newReaderStub(60 * time.Millisecond)
	stub.serve(t)

	calls := []toolCall{readCall("a"), readCall("b"), readCall("c")}
	start := time.Now()
	results := runToolBatch(context.Background(), calls, APIKeys{}, newReadCache())
	elapsed := time.Since(start)

	for i, page := range []string{"a", "b", "c"} {
		if want := "contents of " + page; results[i] != want {
			t.Errorf("result %d out of order: got %q, want %q", i, results[i], want)
		}
	}
	if peak := atomic.LoadInt32(&stub.peak); peak < 2 {
		t.Errorf("reads did not overlap: peak concurrency %d", peak)
	}
	// Serial execution would take at least 3x the per-read delay.
	if elapsed >= 3*60*time.Millisecond {
		t.Errorf("batch took %v, consistent with serial execution", elapsed)
	}
}

// Concurrency is capped because every read goes through one upstream reader.
func TestConcurrencyStaysUnderTheCap(t *testing.T) {
	stub := newReaderStub(40 * time.Millisecond)
	stub.serve(t)

	calls := make([]toolCall, 0, maxParallelTools*3)
	for i := range maxParallelTools * 3 {
		calls = append(calls, readCall(fmt.Sprintf("p%d", i)))
	}
	runToolBatch(context.Background(), calls, APIKeys{}, newReadCache())

	if peak := atomic.LoadInt32(&stub.peak); peak > int32(maxParallelTools) {
		t.Errorf("peak concurrency %d exceeded the cap of %d", peak, maxParallelTools)
	}
}

// The clock carries a Unix second. Concatenated onto the prompt it makes the
// whole prefix unique per request, so nothing ahead of it can ever be reused.
func TestTheClockStaysOutOfTheCachedPrefix(t *testing.T) {
	blocks := anthropicSystem("You are a research assistant.")
	if len(blocks) != 2 {
		t.Fatalf("wanted a static block and a clock block, got %d", len(blocks))
	}
	if got := blocks[0]["text"].(string); got != "You are a research assistant." {
		t.Errorf("static instructions should come first, got %q", got)
	}
	if clock := blocks[1]["text"].(string); !strings.Contains(clock, "Current date and time") {
		t.Errorf("clock should be the trailing block, got %q", clock)
	}
	if strings.Contains(blocks[0]["text"].(string), "Current date and time") {
		t.Error("clock leaked into the static block, which cannot then be cached")
	}

	// An empty text block is rejected by the API, so a caller with no system
	// prompt must get the clock alone.
	if bare := anthropicSystem("   "); len(bare) != 1 {
		t.Errorf("wanted only the clock for a blank system prompt, got %d blocks", len(bare))
	}
}

// anthropicStub records every request body and answers according to script.
type anthropicStub struct {
	mu       sync.Mutex
	requests []map[string]any
}

// serve stands up a stub that keeps asking for a tool until tool use is
// forbidden, at which point it writes an answer — the contract the real API
// follows for tool_choice "none".
func (s *anthropicStub) serve(t *testing.T, answer string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("undecodable request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, body)
		n := len(s.requests)
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if _, forbidden := body["tool_choice"]; forbidden {
			_, _ = fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}],"stop_reason":"end_turn"}`, answer)
			return
		}
		_, _ = fmt.Fprintf(w, `{"content":[{"type":"tool_use","id":"tu_%d","name":"read_url","input":{"url":"page%d"}}],"stop_reason":"tool_use"}`, n, n)
	}))
	t.Cleanup(srv.Close)

	original := anthropicToolsURL
	anthropicToolsURL = srv.URL
	t.Cleanup(func() { anthropicToolsURL = original })
}

func (s *anthropicStub) at(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[i]
}

func (s *anthropicStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// lastUserBlocks returns the content blocks of the final message in a recorded
// request body, which is the tool-result turn the loop just appended.
func lastUserBlocks(t *testing.T, body map[string]any) []any {
	t.Helper()
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("request had no messages: %v", body["messages"])
	}
	last, ok := msgs[len(msgs)-1].(map[string]any)
	if !ok {
		t.Fatalf("final message was not an object: %v", msgs[len(msgs)-1])
	}
	if last["role"] != "user" {
		t.Fatalf("final message should be the tool-result turn, got role %v", last["role"])
	}
	blocks, ok := last["content"].([]any)
	if !ok {
		t.Fatalf("final user content was not a block list: %v", last["content"])
	}
	return blocks
}

// Page text is the bulk of a research conversation and it is resent on every
// iteration. Without a breakpoint the loop pays full input price for the same
// pages ten-plus times.
func TestEachTurnMarksACacheBreakpoint(t *testing.T) {
	readers := newReaderStub(0)
	readers.serve(t)
	stub := &anthropicStub{}
	stub.serve(t, "here is what I found")

	if _, err := callAnthropicWithTools(context.Background(),
		"claude-opus-4-8", "system prompt", "research this", 1024, "key", nil, APIKeys{}); err != nil {
		t.Fatalf("loop returned an error: %v", err)
	}

	if stub.count() < 3 {
		t.Fatalf("wanted several iterations to compare, got %d", stub.count())
	}

	// The opening request has nothing to reuse yet.
	if blocks, ok := stub.at(0)["messages"].([]any); !ok || len(blocks) != 1 {
		t.Errorf("first request should carry only the user turn, got %v", stub.at(0)["messages"])
	}

	// Every later request must carry exactly one breakpoint, on the last block
	// of the turn just appended.
	for i := 1; i < stub.count(); i++ {
		blocks := lastUserBlocks(t, stub.at(i))
		marked := 0
		for j, raw := range blocks {
			block := raw.(map[string]any)
			if _, ok := block["cache_control"]; !ok {
				continue
			}
			marked++
			if j != len(blocks)-1 {
				t.Errorf("request %d: breakpoint on block %d, not the last of %d", i, j, len(blocks))
			}
		}
		if marked != 1 {
			t.Errorf("request %d: wanted 1 cache breakpoint on the appended turn, got %d", i, marked)
		}
	}

	// The clock must stay out of the cached prefix, and must not itself be
	// marked — it changes every request.
	system, ok := stub.at(1)["system"].([]any)
	if !ok || len(system) != 2 {
		t.Fatalf("system should be two blocks, got %v", stub.at(1)["system"])
	}
	for i, raw := range system {
		if _, marked := raw.(map[string]any)["cache_control"]; marked {
			t.Errorf("system block %d is marked cacheable; the volatile clock lives here", i)
		}
	}
}

// Running out of iterations used to return an error and discard everything the
// loop had paid to gather. It has to spend its last turn writing instead.
func TestTheCeilingProducesAnAnswerNotAnError(t *testing.T) {
	readers := newReaderStub(0)
	readers.serve(t)
	stub := &anthropicStub{}
	stub.serve(t, "partial but useful findings")

	out, err := callAnthropicWithTools(context.Background(),
		"claude-opus-4-8", "system prompt", "research this", 1024, "key", nil, APIKeys{})
	if err != nil {
		t.Fatalf("a loop that hits its ceiling must still answer, got error: %v", err)
	}
	if out != "partial but useful findings" {
		t.Errorf("wanted the model's write-up, got %q", out)
	}
	if stub.count() != maxToolIters {
		t.Errorf("wanted %d iterations, got %d", maxToolIters, stub.count())
	}

	final := stub.at(maxToolIters - 1)
	choice, ok := final["tool_choice"].(map[string]any)
	if !ok || choice["type"] != "none" {
		t.Errorf("final request should forbid tool use, got tool_choice %v", final["tool_choice"])
	}
	if _, ok := final["tools"]; !ok {
		t.Error("tool definitions must stay — the history's tool_use blocks need them to validate")
	}
	// Only the last request forbids tools; forbidding earlier would cut research short.
	for i := 0; i < maxToolIters-1; i++ {
		if _, forbidden := stub.at(i)["tool_choice"]; forbidden {
			t.Errorf("request %d forbade tool use before the ceiling", i)
		}
	}
}

// Being cut off mid-sentence is worse than being warned. The model gets a few
// turns' notice so it can choose to stop reading and start writing.
func TestTheModelIsWarnedBeforeTheCeiling(t *testing.T) {
	readers := newReaderStub(0)
	readers.serve(t)
	stub := &anthropicStub{}
	stub.serve(t, "done")

	if _, err := callAnthropicWithTools(context.Background(),
		"claude-opus-4-8", "system prompt", "research this", 1024, "key", nil, APIKeys{}); err != nil {
		t.Fatalf("loop returned an error: %v", err)
	}

	carriesNotice := func(i int) bool {
		body, _ := json.Marshal(stub.at(i)["messages"])
		return strings.Contains(string(body), "nearly out of research budget")
	}
	if carriesNotice(maxToolIters - wrapUpAt - 1) {
		t.Error("warned too early — that spends research budget on wrapping up")
	}
	if !carriesNotice(maxToolIters - wrapUpAt) {
		t.Errorf("no warning by iteration %d; the model gets cut off with no notice",
			maxToolIters-wrapUpAt)
	}
	if !carriesNotice(maxToolIters - 1) {
		t.Error("warning missing from the final iteration")
	}
}
