package codingagent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type toolchainTestSandbox struct {
	result CommandResult
	err    error
	last   CommandSpec
}

func (s *toolchainTestSandbox) ID() string                                           { return "sandbox-test" }
func (s *toolchainTestSandbox) Start(context.Context) error                          { return nil }
func (s *toolchainTestSandbox) Stop(context.Context) error                           { return nil }
func (s *toolchainTestSandbox) Delete(context.Context) error                         { return nil }
func (s *toolchainTestSandbox) Upload(context.Context, string, []byte, uint32) error { return nil }
func (s *toolchainTestSandbox) Download(context.Context, string) ([]byte, error)     { return nil, nil }
func (s *toolchainTestSandbox) Run(_ context.Context, spec CommandSpec, _ func(StreamEvent)) (CommandResult, error) {
	s.last = spec
	return s.result, s.err
}

func TestVerifySandboxToolchainChecksCompletePinnedContract(t *testing.T) {
	sandbox := &toolchainTestSandbox{result: CommandResult{Stdout: sandboxToolchainReadyMessage}}
	if err := VerifySandboxToolchain(context.Background(), sandbox, DefaultCodexCLIVersion); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"zsh", "git", "python3", "go", "node", "npm", "tmux", "brew", "codex", "codex-cli " + DefaultCodexCLIVersion} {
		if !strings.Contains(sandbox.last.Command, required) {
			t.Fatalf("toolchain check omitted %q: %s", required, sandbox.last.Command)
		}
	}
	if sandbox.last.WorkingDir != "/tmp" {
		t.Fatalf("verification working directory = %q", sandbox.last.WorkingDir)
	}
}

func TestVerifySandboxToolchainReportsMissingImageDependency(t *testing.T) {
	sandbox := &toolchainTestSandbox{result: CommandResult{ExitCode: 10, Stderr: "missing required command: npm\n"}}
	err := VerifySandboxToolchain(context.Background(), sandbox, DefaultCodexCLIVersion)
	if err == nil || !strings.Contains(err.Error(), "missing required command: npm") || !strings.Contains(err.Error(), "DAYTONA_CODING_AGENT_SNAPSHOT") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifySandboxToolchainReportsProviderFailure(t *testing.T) {
	sandbox := &toolchainTestSandbox{err: errors.New("provider unavailable")}
	err := VerifySandboxToolchain(context.Background(), sandbox, DefaultCodexCLIVersion)
	if err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}
