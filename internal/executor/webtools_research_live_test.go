package executor

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"workflow-ai/server/internal/telemetry"
)

// Opt-in live check, same convention as the other LIVE_* tests in this repo.
// Stub tests prove the request shape we send; only the real API can prove it is
// accepted and that the cache actually engages.
//
//	RESEARCH_ENV_FILE=/abs/path/to/.env LIVE_RESEARCH=1 \
//	  go test ./internal/executor/ -run TestLiveResearchLoop -v
//
// Keys are read from the file rather than the environment so they never pass
// through a shell command line.

// usageRecorder tees model-provider response bodies to read their usage block.
type usageRecorder struct {
	base http.RoundTripper
	// host selects which provider to record, and parse reads that provider's
	// usage shape.
	host  string
	parse func([]byte) telemetry.Usage

	mu sync.Mutex
	// statuses and calls are parallel: one entry per recorded request.
	statuses []int
	calls    []telemetry.Usage
}

func (u *usageRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := u.base.RoundTrip(r)
	if err != nil || resp == nil || !strings.Contains(r.URL.Host, u.host) {
		return resp, err
	}
	raw, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))

	u.mu.Lock()
	u.statuses = append(u.statuses, resp.StatusCode)
	u.calls = append(u.calls, u.parse(raw))
	u.mu.Unlock()
	return resp, nil
}

// install swaps the recorder in for the duration of the test.
func (u *usageRecorder) install(t *testing.T) {
	t.Helper()
	u.base = http.DefaultTransport
	previous := http.DefaultClient.Transport
	http.DefaultClient.Transport = u
	t.Cleanup(func() { http.DefaultClient.Transport = previous })
}

// loadEnvFile reads KEY=value lines, ignoring comments and blanks.
func loadEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return out
}

func TestLiveResearchLoopReusesItsPrefix(t *testing.T) {
	if os.Getenv("LIVE_RESEARCH") == "" {
		t.Skip("set LIVE_RESEARCH=1 and RESEARCH_ENV_FILE=/abs/path/.env to run")
	}
	envFile := os.Getenv("RESEARCH_ENV_FILE")
	if envFile == "" {
		t.Fatal("RESEARCH_ENV_FILE must point at a file holding ANTHROPIC_API_KEY and BRAVE_API_KEY")
	}
	env := loadEnvFile(t, envFile)
	keys := APIKeys{
		Anthropic: env["ANTHROPIC_API_KEY"],
		Brave:     env["BRAVE_API_KEY"],
		Jina:      env["JINA_API_KEY"],
	}
	if keys.Anthropic == "" || keys.Brave == "" {
		t.Fatal("ANTHROPIC_API_KEY and BRAVE_API_KEY are both required for this test")
	}

	rec := &usageRecorder{host: "anthropic", parse: usageFromAnthropic}
	rec.install(t)

	out, err := callAnthropicWithTools(context.Background(),
		"claude-opus-4-8",
		"You are a research assistant. Cite the URLs you read.",
		"Find the current stable version of the Go programming language. "+
			"Search, then read two separate sources to confirm it, then answer in one sentence.",
		2048, keys.Anthropic, nil, keys)
	if err != nil {
		t.Fatalf("live research loop failed: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

	for i, status := range rec.statuses {
		if status != http.StatusOK {
			t.Errorf("call %d returned HTTP %d — the API rejected the request shape", i+1, status)
		}
	}
	if len(rec.calls) < 2 {
		t.Fatalf("only %d model call(s); the loop never built a prefix to reuse", len(rec.calls))
	}

	var totalRead, totalWrite int
	for i, u := range rec.calls {
		t.Logf("call %d: input=%d cache_write=%d cache_read=%d",
			i+1, u.InputTokens, u.CacheWriteTokens, u.CacheReadTokens)
		totalRead += u.CacheReadTokens
		totalWrite += u.CacheWriteTokens
	}
	t.Logf("answer: %s", strings.TrimSpace(out))
	t.Logf("totals across %d calls: cache_write=%d cache_read=%d", len(rec.calls), totalWrite, totalRead)

	if totalWrite == 0 {
		t.Error("nothing was ever written to the cache — check the breakpoint and the model's minimum prefix")
	}
	// The point of the change: later iterations must re-read the accumulated
	// conversation from cache instead of paying full input price for it again.
	if totalRead == 0 {
		t.Error("no cached tokens were ever read back; the prefix is being invalidated between iterations")
	}
}

// The OpenAI-compatible path has no explicit cache markers — caching there is
// automatic on the longest common prefix. What needs proving is that the shape
// changed on this path is still accepted: two system messages instead of one,
// tool_choice "none" on the final turn, and concurrently-executed tool results
// appended in request order.
func TestLiveOpenAIResearchLoopIsAccepted(t *testing.T) {
	if os.Getenv("LIVE_RESEARCH") == "" {
		t.Skip("set LIVE_RESEARCH=1 and RESEARCH_ENV_FILE=/abs/path/.env to run")
	}
	envFile := os.Getenv("RESEARCH_ENV_FILE")
	if envFile == "" {
		t.Fatal("RESEARCH_ENV_FILE must point at a file holding OPENAI_API_KEY and BRAVE_API_KEY")
	}
	env := loadEnvFile(t, envFile)
	keys := APIKeys{
		OpenAI: env["OPENAI_API_KEY"],
		Brave:  env["BRAVE_API_KEY"],
		Jina:   env["JINA_API_KEY"],
	}
	if keys.OpenAI == "" || keys.Brave == "" {
		t.Fatal("OPENAI_API_KEY and BRAVE_API_KEY are both required for this test")
	}

	rec := &usageRecorder{host: "openai", parse: usageFromOpenAI}
	rec.install(t)

	model := env["LIVE_OPENAI_MODEL"]
	if model == "" {
		model = "gpt-4o"
	}
	out, err := callOpenAIWithTools(context.Background(), model,
		"You are a research assistant. Cite the URLs you read.",
		"Find the current stable version of the Go programming language. "+
			"Search, then read two separate sources to confirm it, then answer in one sentence.",
		2048, keys.OpenAI, nil, keys)
	if err != nil {
		t.Fatalf("live research loop failed: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

	for i, status := range rec.statuses {
		if status != http.StatusOK {
			t.Errorf("call %d returned HTTP %d — the API rejected the request shape", i+1, status)
		}
	}
	if len(rec.calls) < 2 {
		t.Fatalf("only %d model call(s); the tool loop never ran a second turn", len(rec.calls))
	}
	var totalRead int
	for i, u := range rec.calls {
		t.Logf("call %d: input=%d cached=%d", i+1, u.InputTokens, u.CacheReadTokens)
		totalRead += u.CacheReadTokens
	}
	t.Logf("answer: %s", strings.TrimSpace(out))
	// Informational: this provider decides on its own whether to cache, so a
	// zero here is not a defect in anything this change controls.
	t.Logf("cached tokens read back across %d calls: %d", len(rec.calls), totalRead)
}
