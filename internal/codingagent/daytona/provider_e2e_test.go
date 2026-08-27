package daytona

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"workflow-ai/server/internal/codingagent"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
)

// TestProviderLiveLifecycle is deliberately opt-in: it creates a billable
// Daytona sandbox. It validates the real provider boundary without requiring a
// Fernary login or persisting any runtime credential.
func TestProviderLiveLifecycle(t *testing.T) {
	if os.Getenv("CODING_AGENT_E2E") != "1" {
		t.Skip("set CODING_AGENT_E2E=1 to run the live Daytona lifecycle test")
	}
	_ = godotenv.Load("../../../.env")
	if strings.TrimSpace(os.Getenv("DAYTONA_API_KEY")) == "" {
		t.Fatal("DAYTONA_API_KEY is required")
	}

	provider, err := NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sandbox, err := provider.Create(ctx, codingagent.SandboxSpec{
		Name:     "fernary-e2e-" + uuid.NewString()[:12],
		Snapshot: strings.TrimSpace(os.Getenv("DAYTONA_CODING_AGENT_SNAPSHOT")),
		Labels: map[string]string{
			"fernary": "coding-agent-e2e",
		},
		AutoStopMinutes: 5, AutoDeleteMinutes: 10, Ephemeral: true,
		NetworkBlockAll: true, AllowedDomains: []string{"api.openai.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := sandbox.Delete(cleanupCtx); err != nil && !strings.Contains(err.Error(), codingagent.ErrSandboxNotFound.Error()) {
			t.Errorf("delete live test sandbox: %v", err)
		}
	}()
	if strings.TrimSpace(os.Getenv("DAYTONA_CODING_AGENT_SNAPSHOT")) != "" {
		if err := codingagent.VerifySandboxToolchain(ctx, sandbox, os.Getenv("CODEX_CLI_VERSION")); err != nil {
			t.Fatal(err)
		}
	}

	content := []byte("fernary-daytona-e2e")
	path := "/tmp/fernary-e2e.txt"
	if err := sandbox.Upload(ctx, path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	downloaded, err := sandbox.Download(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if string(downloaded) != string(content) {
		t.Fatalf("downloaded content = %q", downloaded)
	}
	result, err := sandbox.Run(ctx, codingagent.CommandSpec{
		Command: "printf '%s' " + shellQuote(string(content)), WorkingDir: "/tmp", Timeout: time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != string(content) {
		t.Fatalf("command result = exit %d, stdout %q, stderr %q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if testing.Verbose() {
		fmt.Println("live Daytona sandbox lifecycle passed")
	}
}
