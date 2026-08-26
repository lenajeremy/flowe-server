package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"workflow-ai/server/internal/codingagent"
)

const (
	maxAuthBundleSize = 2 << 20
	maxArtifactSize   = 2 << 20
)

var safeIdentifier = regexp.MustCompile(`^[A-Za-z0-9._-]{1,160}$`)

type Runtime struct {
	cliVersion string
}

func NewRuntime(cliVersion string) (*Runtime, error) {
	cliVersion = strings.TrimSpace(cliVersion)
	if cliVersion == "" {
		cliVersion = codingagent.DefaultCodexCLIVersion
	}
	if !safeIdentifier.MatchString(cliVersion) {
		return nil, errors.New("Codex CLI version contains unsupported characters")
	}
	return &Runtime{cliVersion: cliVersion}, nil
}

func (r *Runtime) Name() string { return codingagent.RuntimeCodex }

func (r *Runtime) Run(ctx context.Context, sandbox codingagent.Sandbox, req codingagent.RuntimeRequest, emit func(codingagent.StreamEvent)) (result codingagent.RuntimeResult, runErr error) {
	if sandbox == nil {
		return result, errors.New("Codex runtime requires a sandbox")
	}
	if strings.TrimSpace(req.Task) == "" {
		return result, errors.New("Codex task is required")
	}
	if !safeIdentifier.MatchString(req.JobID) {
		return result, errors.New("Codex job ID is invalid")
	}
	if !safeIdentifier.MatchString(req.SessionID) {
		return result, errors.New("Codex session ID is invalid")
	}
	if len(req.AuthBundle) == 0 || len(req.AuthBundle) > maxAuthBundleSize || !json.Valid(req.AuthBundle) {
		return result, errors.New("Codex authentication bundle is missing or invalid")
	}
	if req.ExternalThreadID != "" && !safeIdentifier.MatchString(req.ExternalThreadID) {
		return result, errors.New("Codex thread ID is invalid")
	}
	if !strings.HasPrefix(req.WorkingDirectory, "/") || strings.Contains(req.WorkingDirectory, "..") {
		return result, errors.New("Codex working directory must be an absolute normalized path")
	}

	// Session state must survive across jobs for `codex exec resume` to work.
	// Only the short-lived auth/input files are removed; Codex rollouts remain in
	// this provider-isolated directory until the workspace is reset/deleted.
	secretDir := "/workspace/.fernary/codex/" + req.SessionID
	authPath := secretDir + "/auth.json"
	configPath := secretDir + "/config.toml"
	promptPath := secretDir + "/prompt.txt"
	schemaPath := secretDir + "/result.schema.json"
	resultPath := secretDir + "/result.json"

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if refreshed, err := sandbox.Download(cleanupCtx, authPath); err == nil && json.Valid(refreshed) && len(refreshed) <= maxAuthBundleSize {
			result.RefreshedAuthBundle = refreshed
		}
		cleanupCommand := "rm -f -- " + shellQuote(authPath) + " " + shellQuote(configPath) + " " + shellQuote(promptPath) + " " + shellQuote(schemaPath) + " " + shellQuote(resultPath) + " && rmdir -- " + shellQuote(secretDir)
		_, _ = sandbox.Run(cleanupCtx, codingagent.CommandSpec{Command: cleanupCommand, WorkingDir: "/tmp", Timeout: 30 * time.Second}, nil)
	}()

	prepareCommand := "mkdir -p -- " + shellQuote(secretDir) + " && " + ensureCodexCommand(r.cliVersion) + " && codex --version"
	prepared, err := sandbox.Run(ctx, codingagent.CommandSpec{Command: prepareCommand, WorkingDir: "/tmp", Timeout: 10 * time.Minute}, emit)
	if err != nil {
		return result, fmt.Errorf("prepare Codex runtime: %w", err)
	}
	if prepared.ExitCode != 0 {
		return result, fmt.Errorf("prepare Codex runtime: %s", conciseError(prepared.Stderr, prepared.Stdout))
	}
	if emit != nil {
		emit(codingagent.StreamEvent{Type: "runtime_ready", Message: strings.TrimSpace(prepared.Stdout)})
	}

	prompt := buildPrompt(req.Task)
	config := []byte("cli_auth_credentials_store = \"file\"\n")
	for path, file := range map[string]struct {
		content []byte
		mode    uint32
	}{
		authPath:   {content: req.AuthBundle, mode: 0o600},
		configPath: {content: config, mode: 0o600},
		promptPath: {content: []byte(prompt), mode: 0o600},
		schemaPath: {content: []byte(resultSchema), mode: 0o600},
	} {
		if err := sandbox.Upload(ctx, path, file.content, file.mode); err != nil {
			return result, fmt.Errorf("prepare Codex input: %w", err)
		}
	}

	command := buildCommand(req, secretDir, promptPath, schemaPath, resultPath)
	taskTimeout := req.Timeout
	if taskTimeout <= 0 || taskTimeout > 2*time.Hour {
		taskTimeout = 2 * time.Hour
	}
	jsonStream := &jsonLineStream{}
	execution, err := sandbox.Run(ctx, codingagent.CommandSpec{
		Command:    command,
		WorkingDir: req.WorkingDirectory,
		Environment: map[string]string{
			"CODEX_HOME": secretDir,
		},
		Timeout: taskTimeout,
	}, func(event codingagent.StreamEvent) {
		if event.Type == "stdout" {
			for _, parsed := range jsonStream.Feed(event.Message, false) {
				applyCodexEvent(&result, parsed, emit)
			}
			return
		}
		if event.Type == "stderr" {
			if emit != nil {
				emit(codingagent.StreamEvent{Type: "runtime_diagnostic", Message: "Codex emitted diagnostic output"})
			}
			return
		}
		if emit != nil {
			emit(event)
		}
	})
	for _, parsed := range jsonStream.Feed("", true) {
		applyCodexEvent(&result, parsed, emit)
	}
	if err != nil {
		return result, fmt.Errorf("run Codex: %w", err)
	}
	if execution.ExitCode != 0 {
		return result, fmt.Errorf("Codex exited with code %d: %s", execution.ExitCode, conciseError(execution.Stderr, execution.Stdout))
	}

	finalContent, err := sandbox.Download(ctx, resultPath)
	if err != nil {
		return result, fmt.Errorf("read Codex result: %w", err)
	}
	var output map[string]any
	if err := json.Unmarshal(finalContent, &output); err != nil {
		return result, fmt.Errorf("decode Codex result: %w", err)
	}
	result.Output = output
	if summary, ok := output["summary"].(string); ok {
		result.Summary = strings.TrimSpace(summary)
	}
	if result.Summary == "" {
		return result, errors.New("Codex completed without a summary")
	}

	artifacts, artifactErr := collectGitArtifacts(ctx, sandbox, req.WorkingDirectory)
	if artifactErr != nil {
		if emit != nil {
			emit(codingagent.StreamEvent{Type: "warning", Message: artifactErr.Error()})
		}
	} else {
		result.Artifacts = artifacts
	}
	completion, err := json.Marshal(result)
	if err != nil {
		return result, fmt.Errorf("encode Codex recovery marker: %w", err)
	}
	if err := sandbox.Upload(ctx, codingagent.RecoveryMarkerPath(req.JobID), completion, 0o600); err != nil {
		return result, fmt.Errorf("persist Codex recovery marker: %w", err)
	}
	return result, nil
}

func ensureCodexCommand(version string) string {
	packageName := shellQuote("@openai/codex@" + version)
	if version == "latest" {
		return "if ! command -v codex >/dev/null 2>&1; then npm install -g " + packageName + "; fi"
	}
	expected := shellQuote("codex-cli " + version)
	return "if ! command -v codex >/dev/null 2>&1 || ! codex --version | grep -Fqx -- " + expected + "; then npm install -g " + packageName + "; fi"
}

func buildCommand(req codingagent.RuntimeRequest, secretDir, promptPath, schemaPath, resultPath string) string {
	parts := []string{
		"codex", "--ask-for-approval", "never", "exec", "--json", "--color", "never", "--ignore-user-config",
		"--sandbox", map[bool]string{true: "workspace-write", false: "read-only"}[req.AllowWrite],
		"--output-schema", shellQuote(schemaPath), "--output-last-message", shellQuote(resultPath),
		"--cd", shellQuote(req.WorkingDirectory),
	}
	if strings.TrimSpace(req.Model) != "" {
		parts = append(parts, "--model", shellQuote(strings.TrimSpace(req.Model)))
	}
	if req.ExternalThreadID != "" {
		parts = append(parts, "resume", shellQuote(req.ExternalThreadID))
	}
	parts = append(parts, "-", "<", shellQuote(promptPath))
	return strings.Join(parts, " ")
}

func buildPrompt(task string) string {
	return strings.TrimSpace(task) + `

Fernary execution requirements:
- Work only inside the current repository workspace.
- Never read, print, copy, or expose authentication files, tokens, environment secrets, or files outside the repository.
- Make the requested changes and run the most relevant verification available.
- Do not push, publish, deploy, open a pull request, or contact external people.
- Return the final result using the provided JSON schema.`
}

type jsonLineStream struct {
	partial string
}

func (s *jsonLineStream) Feed(chunk string, flush bool) []map[string]any {
	combined := s.partial + chunk
	s.partial = ""
	if !flush && !strings.HasSuffix(combined, "\n") {
		if index := strings.LastIndexByte(combined, '\n'); index >= 0 {
			s.partial = combined[index+1:]
			combined = combined[:index+1]
		} else {
			s.partial = combined
			return nil
		}
	}
	var events []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(combined))
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1<<20)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) == nil {
			events = append(events, event)
		}
	}
	return events
}

func applyCodexEvent(result *codingagent.RuntimeResult, event map[string]any, emit func(codingagent.StreamEvent)) {
	eventType, _ := event["type"].(string)
	if eventType == "thread.started" {
		if threadID, ok := event["thread_id"].(string); ok && safeIdentifier.MatchString(threadID) {
			result.ExternalThreadID = threadID
		}
	}
	message := ""
	if item, ok := event["item"].(map[string]any); ok {
		switch item["type"] {
		case "command_execution":
			message = "Codex completed a command"
		case "agent_message":
			message = "Codex produced an update"
		}
	}
	if emit != nil {
		emit(codingagent.StreamEvent{Type: "codex_event", Message: message, Payload: scrubCodexEvent(event)})
	}
}

func scrubCodexEvent(event map[string]any) map[string]any {
	// JSONL is useful progress metadata, but arbitrary command output can contain
	// repository secrets. Keep event identity and safe summaries; final artifacts
	// remain organization-scoped and are handled separately.
	result := map[string]any{}
	for _, key := range []string{"type", "thread_id"} {
		if value, exists := event[key]; exists {
			result[key] = value
		}
	}
	if item, ok := event["item"].(map[string]any); ok {
		clean := map[string]any{}
		for _, key := range []string{"id", "type", "status", "exit_code"} {
			if value, exists := item[key]; exists {
				clean[key] = value
			}
		}
		result["item"] = clean
	}
	if usage, ok := event["usage"].(map[string]any); ok {
		result["usage"] = usage
	}
	return result
}

func collectGitArtifacts(ctx context.Context, sandbox codingagent.Sandbox, workingDirectory string) ([]codingagent.Artifact, error) {
	status, err := sandbox.Run(ctx, codingagent.CommandSpec{
		Command: "git status --short", WorkingDir: workingDirectory, Timeout: time.Minute,
	}, nil)
	if err != nil || status.ExitCode != 0 {
		return nil, errors.New("Codex finished, but Git status could not be collected")
	}
	diff, err := sandbox.Run(ctx, codingagent.CommandSpec{
		Command: "git diff --binary --no-ext-diff HEAD --", WorkingDir: workingDirectory, Timeout: time.Minute,
	}, nil)
	if err != nil || diff.ExitCode != 0 {
		return nil, errors.New("Codex finished, but its patch could not be collected")
	}
	artifacts := []codingagent.Artifact{inlineArtifact("git_status", "git-status.txt", "text/plain", status.Stdout)}
	if len(diff.Stdout) <= maxArtifactSize {
		artifacts = append(artifacts, inlineArtifact("patch", "changes.patch", "text/x-diff", diff.Stdout))
	} else {
		artifacts = append(artifacts, codingagent.Artifact{Kind: "patch", Path: "changes.patch", MediaType: "text/x-diff", SizeBytes: int64(len(diff.Stdout))})
	}
	untracked, err := sandbox.Run(ctx, codingagent.CommandSpec{
		Command: "git ls-files --others --exclude-standard -z", WorkingDir: workingDirectory, Timeout: time.Minute,
	}, nil)
	if err != nil || untracked.ExitCode != 0 {
		return artifacts, errors.New("Codex finished, but untracked files could not be collected")
	}
	remaining := maxArtifactSize - len(diff.Stdout)
	for index, path := range strings.Split(untracked.Stdout, "\x00") {
		if index >= 50 || remaining <= 0 || !safeRepositoryPath(path) {
			break
		}
		sized, sizeErr := sandbox.Run(ctx, codingagent.CommandSpec{
			Command: "wc -c < " + shellQuote(path), WorkingDir: workingDirectory, Timeout: time.Minute,
		}, nil)
		if sizeErr != nil || sized.ExitCode != 0 {
			continue
		}
		size, parseErr := strconv.ParseInt(strings.TrimSpace(sized.Stdout), 10, 64)
		if parseErr != nil || size < 0 || size > int64(remaining) {
			continue
		}
		content, downloadErr := sandbox.Download(ctx, strings.TrimSuffix(workingDirectory, "/")+"/"+path)
		if downloadErr != nil || int64(len(content)) != size || !utf8.Valid(content) {
			continue
		}
		artifacts = append(artifacts, inlineArtifact("file", path, "text/plain", string(content)))
		remaining -= len(content)
	}
	return artifacts, nil
}

func safeRepositoryPath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.ContainsRune(path, '\x00') {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func inlineArtifact(kind, path, mediaType, content string) codingagent.Artifact {
	digest := sha256.Sum256([]byte(content))
	return codingagent.Artifact{
		Kind: kind, Path: path, MediaType: mediaType, Content: content,
		SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]),
	}
}

func conciseError(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		value = redactDiagnostic(value)
		if len(value) > 1000 {
			value = value[len(value)-1000:]
		}
		return value
	}
	return "unknown error"
}

var diagnosticSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization|api[_-]?key|password|secret|token)\s*[:=]\s*[^\s]+`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{10,}\b`),
}

func redactDiagnostic(value string) string {
	for _, pattern := range diagnosticSecrets {
		value = pattern.ReplaceAllString(value, "[redacted]")
	}
	return value
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

const resultSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["summary", "changedFiles", "tests", "notes"],
  "properties": {
    "summary": {"type": "string"},
    "changedFiles": {"type": "array", "items": {"type": "string"}},
    "tests": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["command", "status"],
        "properties": {
          "command": {"type": "string"},
          "status": {"type": "string", "enum": ["passed", "failed", "not_run"]}
        }
      }
    },
    "notes": {"type": "array", "items": {"type": "string"}}
  }
}`
