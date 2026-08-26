package daytona

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"workflow-ai/server/internal/codingagent"

	sdk "github.com/daytona/clients/sdk-go/pkg/daytona"
	sdkerrors "github.com/daytona/clients/sdk-go/pkg/errors"
	"github.com/daytona/clients/sdk-go/pkg/options"
	daytonatypes "github.com/daytona/clients/sdk-go/pkg/types"
	"github.com/google/uuid"
)

const defaultImage = "node:22-bookworm"

type Provider struct {
	client *sdk.Client
	image  string
}

func NewProvider() (*Provider, error) {
	client, err := sdk.NewClient()
	if err != nil {
		return nil, fmt.Errorf("configure Daytona: %w", err)
	}
	return NewProviderWithClient(client, defaultImage), nil
}

func NewProviderWithClient(client *sdk.Client, image string) *Provider {
	if image == "" {
		image = defaultImage
	}
	return &Provider{client: client, image: image}
}

func (p *Provider) Name() string { return codingagent.ProviderDaytona }

func (p *Provider) Create(ctx context.Context, spec codingagent.SandboxSpec) (codingagent.Sandbox, error) {
	if p == nil || p.client == nil {
		return nil, errors.New("Daytona client is not configured")
	}
	if strings.TrimSpace(spec.Name) == "" {
		return nil, errors.New("sandbox name is required")
	}
	if spec.AutoStopMinutes < 1 {
		return nil, errors.New("sandbox auto-stop must be at least one minute")
	}
	if !spec.Ephemeral && spec.AutoDeleteMinutes < spec.AutoStopMinutes {
		return nil, errors.New("sandbox auto-delete cannot precede auto-stop")
	}
	if len(spec.AllowedDomains) > 0 {
		if err := codingagent.ValidateAllowedDomains(spec.AllowedDomains); err != nil {
			return nil, err
		}
	}

	domains := normalizeDomains(spec.AllowedDomains)
	networkBlockAll, domainAllowList := sandboxNetworkSettings(spec.NetworkBlockAll, domains)
	base := daytonatypes.SandboxBaseParams{
		Name:               spec.Name,
		Language:           daytonatypes.CodeLanguageJavaScript,
		EnvVars:            cloneStrings(spec.Environment),
		Labels:             cloneStrings(spec.Labels),
		Public:             false,
		AutoStopInterval:   intPointer(spec.AutoStopMinutes),
		AutoDeleteInterval: intPointer(spec.AutoDeleteMinutes),
		NetworkBlockAll:    networkBlockAll,
		DomainAllowList:    domainAllowList,
		Ephemeral:          spec.Ephemeral,
	}

	var params any
	if strings.TrimSpace(spec.Snapshot) != "" {
		params = daytonatypes.SnapshotParams{SandboxBaseParams: base, Snapshot: strings.TrimSpace(spec.Snapshot)}
	} else {
		params = daytonatypes.ImageParams{SandboxBaseParams: base, Image: p.image}
	}
	created, err := p.client.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("create Daytona sandbox: %w", err)
	}
	return &sandbox{sandbox: created}, nil
}

func sandboxNetworkSettings(blockAll bool, domains []string) (bool, *string) {
	if len(domains) > 0 {
		joined := strings.Join(domains, ",")
		// Daytona rejects domainAllowList + networkBlockAll as mutually
		// exclusive. A domain allow list is itself deny-by-default.
		return false, &joined
	}
	return blockAll, nil
}

func (p *Provider) Get(ctx context.Context, externalID string) (codingagent.Sandbox, error) {
	if p == nil || p.client == nil {
		return nil, errors.New("Daytona client is not configured")
	}
	if strings.TrimSpace(externalID) == "" {
		return nil, errors.New("Daytona sandbox ID is required")
	}
	got, err := p.client.Get(ctx, externalID)
	if err != nil {
		if errors.Is(err, sdkerrors.ErrNotFound) {
			return nil, fmt.Errorf("%w: %v", codingagent.ErrSandboxNotFound, err)
		}
		return nil, fmt.Errorf("get Daytona sandbox: %w", err)
	}
	return &sandbox{sandbox: got}, nil
}

type sandbox struct {
	sandbox *sdk.Sandbox
}

func (s *sandbox) ID() string { return s.sandbox.ID }

func (s *sandbox) Start(ctx context.Context) error {
	if err := s.sandbox.Start(ctx); err != nil {
		return fmt.Errorf("start Daytona sandbox: %w", err)
	}
	return nil
}

func (s *sandbox) Stop(ctx context.Context) error {
	if err := s.sandbox.Stop(ctx); err != nil {
		return fmt.Errorf("stop Daytona sandbox: %w", err)
	}
	return nil
}

func (s *sandbox) Delete(ctx context.Context) error {
	if err := s.sandbox.Delete(ctx); err != nil {
		if errors.Is(err, sdkerrors.ErrNotFound) {
			return fmt.Errorf("%w: %v", codingagent.ErrSandboxNotFound, err)
		}
		return fmt.Errorf("delete Daytona sandbox: %w", err)
	}
	return nil
}

func (s *sandbox) Upload(ctx context.Context, destination string, content []byte, mode uint32) error {
	if !safeRemotePath(destination) {
		return errors.New("upload destination must be an absolute normalized path")
	}
	if err := s.sandbox.FileSystem.UploadFile(ctx, content, destination); err != nil {
		return fmt.Errorf("upload file to Daytona sandbox: %w", err)
	}
	if mode != 0 {
		permission := fmt.Sprintf("%04o", mode&0o777)
		if err := s.sandbox.FileSystem.SetFilePermissions(ctx, destination, options.WithPermissionMode(permission)); err != nil {
			return fmt.Errorf("set Daytona file permissions: %w", err)
		}
	}
	return nil
}

func (s *sandbox) Download(ctx context.Context, source string) ([]byte, error) {
	if !safeRemotePath(source) {
		return nil, errors.New("download source must be an absolute normalized path")
	}
	content, err := s.sandbox.FileSystem.DownloadFile(ctx, source, nil)
	if err != nil {
		return nil, fmt.Errorf("download file from Daytona sandbox: %w", err)
	}
	return content, nil
}

func (s *sandbox) Run(ctx context.Context, spec codingagent.CommandSpec, emit func(codingagent.StreamEvent)) (codingagent.CommandResult, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return codingagent.CommandResult{}, errors.New("sandbox command is required")
	}
	if spec.WorkingDir != "" && !safeRemotePath(spec.WorkingDir) {
		return codingagent.CommandResult{}, errors.New("sandbox working directory must be an absolute normalized path")
	}
	if spec.Timeout <= 0 || spec.Timeout > 2*time.Hour {
		return codingagent.CommandResult{}, errors.New("sandbox command timeout must be between one nanosecond and two hours")
	}
	if err := validateCommandEnvironment(spec.Environment); err != nil {
		return codingagent.CommandResult{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	sessionID := "fernary-" + uuid.NewString()
	if err := s.sandbox.Process.CreateSession(runCtx, sessionID); err != nil {
		return codingagent.CommandResult{}, fmt.Errorf("create Daytona process session: %w", err)
	}
	var cleanupOnce sync.Once
	var cleanupErr error
	deleteSession := func() error {
		cleanupOnce.Do(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cleanupCancel()
			cleanupErr = s.sandbox.Process.DeleteSession(cleanupCtx, sessionID)
		})
		return cleanupErr
	}
	defer func() { _ = deleteSession() }()

	command := composeCommand(spec.WorkingDir, spec.Environment, spec.Command)
	started, err := s.sandbox.Process.ExecuteSessionCommand(runCtx, sessionID, command, true, true)
	if err != nil {
		return codingagent.CommandResult{}, fmt.Errorf("start Daytona command: %w", err)
	}
	commandID, ok := started["id"].(string)
	if !ok || commandID == "" {
		return codingagent.CommandResult{}, errors.New("Daytona command did not return an execution ID")
	}
	if emit != nil {
		emit(codingagent.StreamEvent{Type: "execution_started", Payload: map[string]any{"executionId": commandID}})
	}

	stdout := make(chan string, 64)
	stderr := make(chan string, 64)
	streamErr := make(chan error, 1)
	go func() {
		streamErr <- s.sandbox.Process.GetSessionCommandLogsStream(runCtx, sessionID, commandID, stdout, stderr)
	}()

	var stdoutBuffer, stderrBuffer cappedBuffer
	stdoutBuffer.limit = 2 << 20
	stderrBuffer.limit = 2 << 20
	streamDone := streamErr
	for stdout != nil || stderr != nil {
		select {
		case <-runCtx.Done():
			if err := deleteSession(); err != nil {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer stopCancel()
				if stopErr := s.sandbox.Stop(stopCtx); stopErr != nil {
					return codingagent.CommandResult{ExecutionID: commandID}, fmt.Errorf(
						"%w: delete process session: %v; stop sandbox: %v", codingagent.ErrSandboxExecutionUncertain, err, stopErr,
					)
				}
			}
			return codingagent.CommandResult{ExecutionID: commandID}, runCtx.Err()
		case err := <-streamDone:
			streamDone = nil
			if err != nil {
				return codingagent.CommandResult{ExecutionID: commandID, Stdout: stdoutBuffer.String(), Stderr: stderrBuffer.String()}, fmt.Errorf("stream Daytona command: %w", err)
			}
		case chunk, open := <-stdout:
			if !open {
				stdout = nil
				continue
			}
			stdoutBuffer.WriteString(chunk)
			if emit != nil {
				emit(codingagent.StreamEvent{Type: "stdout", Message: chunk})
			}
		case chunk, open := <-stderr:
			if !open {
				stderr = nil
				continue
			}
			stderrBuffer.WriteString(chunk)
			if emit != nil {
				emit(codingagent.StreamEvent{Type: "stderr", Message: chunk})
			}
		}
	}
	if streamDone != nil {
		if err := <-streamDone; err != nil {
			return codingagent.CommandResult{ExecutionID: commandID, Stdout: stdoutBuffer.String(), Stderr: stderrBuffer.String()}, fmt.Errorf("stream Daytona command: %w", err)
		}
	}
	status, err := s.sandbox.Process.GetSessionCommand(runCtx, sessionID, commandID)
	if err != nil {
		return codingagent.CommandResult{ExecutionID: commandID, Stdout: stdoutBuffer.String(), Stderr: stderrBuffer.String()}, fmt.Errorf("read Daytona command status: %w", err)
	}
	exitCode, ok := numericInt(status["exitCode"])
	if !ok {
		return codingagent.CommandResult{ExecutionID: commandID, Stdout: stdoutBuffer.String(), Stderr: stderrBuffer.String()}, errors.New("Daytona command ended without an exit code")
	}
	return codingagent.CommandResult{
		ExecutionID: commandID,
		ExitCode:    exitCode,
		Stdout:      stdoutBuffer.String(),
		Stderr:      stderrBuffer.String(),
	}, nil
}

// Daytona process session metadata includes the rendered command. Credentials
// therefore travel as short-lived mode-0600 files, never environment prefixes.
func validateCommandEnvironment(environment map[string]string) error {
	for key := range environment {
		upper := strings.ToUpper(key)
		for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "API_KEY", "APIKEY", "CREDENTIAL"} {
			if strings.Contains(upper, marker) {
				return fmt.Errorf("sensitive value %q must be injected as a temporary file, not a command environment variable", key)
			}
		}
	}
	return nil
}

func intPointer(value int) *int { return &value }

func cloneStrings(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func normalizeDomains(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || strings.ContainsAny(value, " /:@,") {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func safeRemotePath(value string) bool {
	if !strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') {
		return false
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == ".." {
			return false
		}
	}
	return true
}

func environmentPrefix(environment map[string]string) string {
	if len(environment) == 0 {
		return ""
	}
	var result strings.Builder
	result.WriteString("env")
	for key, value := range environment {
		if !validEnvironmentName(key) {
			continue
		}
		result.WriteByte(' ')
		result.WriteString(key)
		result.WriteByte('=')
		result.WriteString(shellQuote(value))
	}
	result.WriteByte(' ')
	return result.String()
}

func composeCommand(workingDirectory string, environment map[string]string, command string) string {
	result := ""
	if workingDirectory != "" {
		result = "cd -- " + shellQuote(workingDirectory) + " && "
	}
	return result + environmentPrefix(environment) + command
}

func validEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func numericInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), typed == float64(int(typed))
	case string:
		got, err := strconv.Atoi(typed)
		return got, err == nil
	default:
		return 0, false
	}
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) WriteString(value string) {
	if b.limit <= 0 {
		return
	}
	if len(value) >= b.limit {
		b.Buffer.Reset()
		b.Buffer.WriteString(value[len(value)-b.limit:])
		return
	}
	overflow := b.Len() + len(value) - b.limit
	if overflow > 0 {
		current := append([]byte(nil), b.Bytes()[overflow:]...)
		b.Buffer.Reset()
		_, _ = b.Buffer.Write(current)
	}
	_, _ = b.Buffer.WriteString(value)
}
