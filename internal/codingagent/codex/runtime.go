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
	"unicode"
	"unicode/utf8"

	"workflow-ai/server/internal/codingagent"
)

const (
	maxAuthBundleSize = 2 << 20
	maxArtifactSize   = 2 << 20
	maxCommandLogSize = 4 << 10
	maxOutputLogSize  = 8 << 10
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
	if !regexp.MustCompile(`^[0-9a-fA-F]{40,64}$`).MatchString(req.BaselineSHA) {
		return result, errors.New("Codex repository baseline is missing or invalid")
	}
	if (strings.TrimSpace(req.ToolEndpoint) == "") != (strings.TrimSpace(req.ToolToken) == "") {
		return result, errors.New("Codex workflow tool endpoint and token must be configured together")
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
		cleanupCommand := "rm -f -- " + shellQuote(authPath) + " " + shellQuote(configPath) + " " + shellQuote(promptPath) + " " + shellQuote(schemaPath) + " " + shellQuote(resultPath) + " " + shellQuote(secretDir+"/mcp-token") + " && rmdir -- " + shellQuote(secretDir)
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
	config := runtimeConfig(req.ToolEndpoint)
	// Point Codex at this workflow's own nodes when the job was granted any.
	// The token travels in the process environment rather than the config file
	// or the command line: config.toml is uploaded to the workspace, and argv
	// is readable by anything else running in the sandbox.
	// The token is written as a mode-0600 file and exported by the command that
	// reads it, never passed as a command environment variable: Daytona records
	// the rendered command AND its environment in process session metadata, so
	// a secret placed there is retained by the provider. Only the path travels.
	commandEnvironment := map[string]string{"CODEX_HOME": secretDir}
	toolTokenPath := ""
	if _, ok := mcpServerConfig(req.ToolEndpoint); ok && req.ToolToken != "" {
		toolTokenPath = secretDir + "/mcp-token"
	}
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

	if toolTokenPath != "" {
		if err := sandbox.Upload(ctx, toolTokenPath, []byte(req.ToolToken), 0o600); err != nil {
			return result, fmt.Errorf("prepare Codex tool token: %w", err)
		}
	}

	command := buildCommand(req, secretDir, promptPath, schemaPath, resultPath, toolTokenPath)
	taskTimeout := req.Timeout
	if taskTimeout <= 0 || taskTimeout > 2*time.Hour {
		taskTimeout = 2 * time.Hour
	}
	jsonStream := &jsonLineStream{}
	execution, err := sandbox.Run(ctx, codingagent.CommandSpec{
		Command:     command,
		WorkingDir:  req.WorkingDirectory,
		Environment: commandEnvironment,
		Timeout:     taskTimeout,
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

	artifacts, artifactErr := collectGitArtifacts(ctx, sandbox, req.WorkingDirectory, req.BaselineSHA)
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

func runtimeConfig(toolEndpoint string) []byte {
	config := []byte(`cli_auth_credentials_store = "file"

[shell_environment_policy]
inherit = "core"
exclude = ["FERNARY_MCP_TOKEN"]
`)
	if toolServer, ok := mcpServerConfig(toolEndpoint); ok {
		config = append(config, toolServer...)
	}
	return config
}

func ensureCodexCommand(version string) string {
	packageName := shellQuote("@openai/codex@" + version)
	if version == "latest" {
		return "if ! command -v codex >/dev/null 2>&1; then npm install -g " + packageName + "; fi"
	}
	expected := shellQuote("codex-cli " + version)
	return "if ! command -v codex >/dev/null 2>&1 || ! codex --version | grep -Fqx -- " + expected + "; then npm install -g " + packageName + "; fi"
}

func buildCommand(req codingagent.RuntimeRequest, secretDir, promptPath, schemaPath, resultPath, toolTokenPath string) string {
	parts := []string{
		"codex", "--ask-for-approval", "never", "--search", "exec", "--json", "--color", "never", "--strict-config",
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
	command := strings.Join(parts, " ")
	if toolTokenPath != "" {
		// Read at run time from the file, so the value never appears in the
		// command string the provider stores — only the path does.
		command = "export " + toolTokenEnvVar + "=\"$(cat " + shellQuote(toolTokenPath) + ")\"; " + command
	}
	return command
}

func buildPrompt(task string) string {
	return strings.TrimSpace(task) + `

Fernary execution requirements:
- Work only inside the current repository workspace.
- Never read, print, copy, or expose authentication files, tokens, environment secrets, or files outside the repository.
- Use live web search when recent or authoritative information would improve the result. Never include repository contents, customer data, credentials, or private identifiers in a search query.
- Make the requested changes and run the most relevant verification available.
- Do not use repository credentials or direct shell commands to push, publish, deploy, open a pull request, or contact external people.
- You may perform explicitly granted external actions only through Fernary workflow tools. Read their saved scope carefully. Writes pause for the workflow owner's approval; explain the exact action and why it is needed in the tool's reason field.
- If an external action is rejected or its outcome is reported as unknown, do not retry an equivalent action. Report it clearly for human reconciliation.
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
	if item, ok := event["item"].(map[string]any); ok {
		if itemType, _ := item["type"].(string); itemType == "command_execution" {
			if commandEvent, ok := codexCommandEvent(eventType, item); ok && emit != nil {
				emit(commandEvent)
			}
			return
		}
	}
	if emit != nil {
		message := ""
		if item, ok := event["item"].(map[string]any); ok && item["type"] == "agent_message" {
			message = "Codex produced an update"
		}
		emit(codingagent.StreamEvent{Type: "codex_event", Message: message, Payload: scrubCodexEvent(event)})
	}
}

func codexCommandEvent(eventType string, item map[string]any) (codingagent.StreamEvent, bool) {
	phase := ""
	switch eventType {
	case "item.started":
		phase = "started"
	case "item.completed":
		phase = "completed"
	default:
		return codingagent.StreamEvent{}, false
	}
	payload := map[string]any{"kind": "command", "phase": phase}
	if value, ok := item["id"].(string); ok && value != "" {
		payload["itemId"] = safeLogText(value, 256)
	}
	if value, ok := item["command"].(string); ok && value != "" {
		payload["command"] = safeLogText(value, maxCommandLogSize)
	}
	if value, ok := item["status"].(string); ok && value != "" {
		payload["status"] = safeLogText(value, 64)
	}
	if value, exists := item["exit_code"]; exists {
		payload["exitCode"] = value
	}
	if phase == "completed" {
		if value, ok := item["aggregated_output"].(string); ok && value != "" {
			command, _ := item["command"].(string)
			if commandMayExposeSecrets(command) {
				payload["result"] = "Result hidden because this command may expose credentials or environment secrets."
				payload["resultRedacted"] = true
			} else {
				payload["result"] = safeLogText(value, maxOutputLogSize)
			}
		}
		message := "Codex completed a command"
		if exitCode, ok := numericInt(item["exit_code"]); ok && exitCode != 0 {
			message = "Codex command exited with an error"
		}
		return codingagent.StreamEvent{Type: "command_completed", Message: message, Payload: payload}, true
	}
	return codingagent.StreamEvent{Type: "command_started", Message: "Codex started a command", Payload: payload}, true
}

var secretReadingCommand = regexp.MustCompile(`(?i)(?:^|[\s/'"])(?:printenv|env|set|export|declare\s+-x)(?:\s|$)|(?:process\.env|os\.environ|auth\.json|\.env(?:\.|\s|$)|\.npmrc|\.pypirc|\.netrc|git-credentials|credentials?[/.'"]|secrets?[/.'"])`)

func commandMayExposeSecrets(command string) bool {
	return secretReadingCommand.MatchString(command)
}

func numericInt(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), true
	case int:
		return number, true
	case json.Number:
		parsed, err := strconv.Atoi(number.String())
		return parsed, err == nil
	default:
		return 0, false
	}
}

func safeLogText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	value = redactDiagnostic(value)
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "\n… output truncated by Fernary"
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

func collectGitArtifacts(ctx context.Context, sandbox codingagent.Sandbox, workingDirectory, baselineSHA string) ([]codingagent.Artifact, error) {
	status, err := sandbox.Run(ctx, codingagent.CommandSpec{
		Command: "git status --short", WorkingDir: workingDirectory, Timeout: time.Minute,
	}, nil)
	if err != nil || status.ExitCode != 0 {
		return nil, errors.New("Codex finished, but Git status could not be collected")
	}
	diff, err := sandbox.Run(ctx, codingagent.CommandSpec{
		Command: "git diff --binary --no-ext-diff " + shellQuote(baselineSHA) + " --", WorkingDir: workingDirectory, Timeout: time.Minute,
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

	// Tracked files the agent changed. The patch above describes them as a diff,
	// which is right for a human reading the run but useless to a caller that
	// has to write the result somewhere: committing through an API needs the
	// file's full post-change content, not the delta. Deletions are carried as
	// an empty-content artifact of their own kind, since "no content" and
	// "removed" are different instructions to whatever applies this.
	tracked, trackedErr := sandbox.Run(ctx, codingagent.CommandSpec{
		Command: "git diff --name-status -z --no-renames " + shellQuote(baselineSHA) + " --", WorkingDir: workingDirectory, Timeout: time.Minute,
	}, nil)
	if trackedErr != nil || tracked.ExitCode != 0 {
		return artifacts, errors.New("Codex finished, but its changed files could not be collected")
	}
	// -z emits status and path as separate NUL-terminated fields.
	fields := strings.Split(tracked.Stdout, "\x00")
	for index := 0; index+1 < len(fields); index += 2 {
		status, path := strings.TrimSpace(fields[index]), fields[index+1]
		if status == "" || !safeRepositoryPath(path) {
			continue
		}
		if strings.HasPrefix(status, "D") {
			artifacts = append(artifacts, inlineArtifact("file_deleted", path, "text/plain", ""))
			continue
		}
		if remaining <= 0 {
			break
		}
		content, downloadErr := readRepositoryFile(ctx, sandbox, workingDirectory, path, remaining)
		if downloadErr != nil {
			continue
		}
		artifacts = append(artifacts, inlineArtifact("file", path, "text/plain", content))
		remaining -= len(content)
	}

	for index, path := range strings.Split(untracked.Stdout, "\x00") {
		if index >= 50 || remaining <= 0 || !safeRepositoryPath(path) {
			break
		}
		content, downloadErr := readRepositoryFile(ctx, sandbox, workingDirectory, path, remaining)
		if downloadErr != nil {
			continue
		}
		artifacts = append(artifacts, inlineArtifact("file", path, "text/plain", content))
		remaining -= len(content)
	}
	return artifacts, nil
}

// readRepositoryFile downloads one file from the workspace, refusing anything
// over the remaining budget or that is not valid UTF-8. Size is checked in the
// sandbox first so an oversized file is never transferred at all, and the
// downloaded length is compared against it: a mismatch means the file changed
// under us, and a truncated read written back to a repository is worse than a
// skipped one.
func readRepositoryFile(ctx context.Context, sandbox codingagent.Sandbox, workingDirectory, path string, remaining int) (string, error) {
	sized, err := sandbox.Run(ctx, codingagent.CommandSpec{
		Command: "wc -c < " + shellQuote(path), WorkingDir: workingDirectory, Timeout: time.Minute,
	}, nil)
	if err != nil || sized.ExitCode != 0 {
		return "", errors.New("could not size " + path)
	}
	size, parseErr := strconv.ParseInt(strings.TrimSpace(sized.Stdout), 10, 64)
	if parseErr != nil || size < 0 || size > int64(remaining) {
		return "", errors.New("file does not fit the artifact budget")
	}
	content, downloadErr := sandbox.Download(ctx, strings.TrimSuffix(workingDirectory, "/")+"/"+path)
	if downloadErr != nil || int64(len(content)) != size || !utf8.Valid(content) {
		return "", errors.New("could not read " + path)
	}
	return string(content), nil
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
	regexp.MustCompile(`(?i)["']?(authorization|api[_-]?key|password|secret|token)["']?\s*[:=]\s*(?:bearer\s+)?[^\s]+`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{10,}\b`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|glpat-[A-Za-z0-9_-]{20,})\b`),
	regexp.MustCompile(`(?i)([?&](?:access_token|token|api_key|signature)=)[^&\s]+`),
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

// toolTokenEnvVar is the name Codex is told to read the bearer token from.
const toolTokenEnvVar = "FERNARY_MCP_TOKEN"

// mcpServerConfig renders the streamable-HTTP server block Codex expects.
//
// The endpoint is refused unless it is a plain https URL: it is interpolated
// into TOML, so a value carrying a quote or a newline could close the string
// and inject configuration of its own choosing into the agent's runtime.
func mcpServerConfig(endpoint string) ([]byte, bool) {
	endpoint = strings.TrimSpace(endpoint)
	if !strings.HasPrefix(endpoint, "https://") || strings.ContainsAny(endpoint, "\"'\n\r\\") {
		return nil, false
	}
	return []byte("\n[mcp_servers.fernary]\nurl = \"" + endpoint + "\"\nbearer_token_env_var = \"" + toolTokenEnvVar + "\"\n"), true
}
