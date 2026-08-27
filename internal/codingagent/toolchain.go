package codingagent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var sandboxToolVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)

const sandboxToolchainReadyMessage = "fernary sandbox toolchain ready"

// VerifySandboxToolchain enforces the contract provided by the configured
// Daytona snapshot before credentials, repository contents, or a coding task
// enter the sandbox. The image is intentionally immutable at runtime: a
// missing or mismatched tool is fixed by publishing a new snapshot, never by
// npm-installing software into an individual workspace.
func VerifySandboxToolchain(ctx context.Context, sandbox Sandbox, codexVersion string) error {
	if sandbox == nil {
		return errors.New("coding agent sandbox image is not ready: sandbox is unavailable")
	}
	codexVersion = strings.TrimSpace(codexVersion)
	if codexVersion == "" {
		codexVersion = DefaultCodexCLIVersion
	}
	if !sandboxToolVersionPattern.MatchString(codexVersion) {
		return errors.New("configured Codex CLI version is invalid")
	}

	command := `set -eu; ` +
		`for fernary_command in sh bash zsh git curl python3 go node npm tmux brew codex; do ` +
		`command -v "$fernary_command" >/dev/null 2>&1 || { printf 'missing required command: %s\n' "$fernary_command" >&2; exit 10; }; ` +
		`done; ` +
		`[ "$(id -u)" -ne 0 ] || { printf 'sandbox runtime user must not be root\n' >&2; exit 11; }; ` +
		`[ -w /workspace ] || { printf '/workspace is not writable by the sandbox runtime user\n' >&2; exit 12; }; `
	if codexVersion != "latest" {
		expected := "codex-cli " + codexVersion
		command += `fernary_codex_version="$(codex --version 2>/dev/null || true)"; ` +
			`[ "$fernary_codex_version" = ` + quoteShell(expected) + ` ] || { ` +
			`printf 'Codex CLI version mismatch: expected %s, got %s\n' ` + quoteShell(expected) + ` "$fernary_codex_version" >&2; exit 13; }; `
	}
	command += "printf '%s\\n' " + quoteShell(sandboxToolchainReadyMessage)

	result, err := sandbox.Run(ctx, CommandSpec{Command: command, WorkingDir: "/tmp", Timeout: 5 * time.Minute}, nil)
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf(
			"coding agent sandbox image is not ready: %s; rebuild and configure DAYTONA_CODING_AGENT_SNAPSHOT",
			commandFailure(result, err),
		)
	}
	if strings.TrimSpace(result.Stdout) != sandboxToolchainReadyMessage {
		return errors.New("coding agent sandbox image is not ready: toolchain verification returned an unexpected result")
	}
	return nil
}

func (s *Service) verifySandboxToolchain(ctx context.Context, sandbox Sandbox) error {
	return VerifySandboxToolchain(ctx, sandbox, s.config.CodexCLIVersion)
}
