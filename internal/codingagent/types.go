// Package codingagent owns durable coding-agent jobs and the provider-neutral
// contracts between workflow execution, isolated sandboxes, and agent runtimes.
package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrSandboxNotFound           = errors.New("coding agent sandbox not found")
	ErrSandboxExecutionUncertain = errors.New("coding agent sandbox process termination could not be confirmed")
)

func RecoveryMarkerPath(jobID string) string {
	return "/tmp/fernary-coding-agent-result-" + jobID + ".json"
}

const (
	RuntimeCodex           = "codex"
	ProviderDaytona        = "daytona"
	DefaultCodexCLIVersion = "0.149.1"
)

type WorkspaceMode string

const (
	WorkspacePersistent WorkspaceMode = "persistent"
	WorkspaceEphemeral  WorkspaceMode = "ephemeral"
)

// ExecutionPolicy is frozen into the job before provider code executes. The
// runtime may narrow this policy, but must never broaden it.
type ExecutionPolicy struct {
	WorkspaceMode       WorkspaceMode `json:"workspaceMode"`
	Repository          string        `json:"repository"`
	Branch              string        `json:"branch,omitempty"`
	Model               string        `json:"model,omitempty"`
	MaxDurationSeconds  int           `json:"maxDurationSeconds"`
	AutoStopMinutes     int           `json:"autoStopMinutes"`
	AutoDeleteMinutes   int           `json:"autoDeleteMinutes"`
	NetworkBlockAll     bool          `json:"networkBlockAll"`
	AllowedDomains      []string      `json:"allowedDomains,omitempty"`
	AllowWorkspaceWrite bool          `json:"allowWorkspaceWrite"`
}

type SubmitRequest struct {
	OrganizationID  string
	UserID          string
	WorkflowID      string
	WorkflowRunID   string
	NodeID          string
	ConversationKey string
	Runtime         string
	Task            string
	Input           map[string]any
	Policy          ExecutionPolicy
}

type SandboxSpec struct {
	Name              string
	Snapshot          string
	Labels            map[string]string
	Environment       map[string]string
	AutoStopMinutes   int
	AutoDeleteMinutes int
	NetworkBlockAll   bool
	AllowedDomains    []string
	Ephemeral         bool
}

type CommandSpec struct {
	Command     string
	WorkingDir  string
	Environment map[string]string
	Timeout     time.Duration
}

type CommandResult struct {
	ExecutionID string
	ExitCode    int
	Stdout      string
	Stderr      string
}

// Sandbox is an already-provisioned isolated filesystem and process boundary.
type Sandbox interface {
	ID() string
	Start(context.Context) error
	Stop(context.Context) error
	Delete(context.Context) error
	Upload(context.Context, string, []byte, uint32) error
	Download(context.Context, string) ([]byte, error)
	Run(context.Context, CommandSpec, func(StreamEvent)) (CommandResult, error)
}

type SandboxProvider interface {
	Name() string
	Create(context.Context, SandboxSpec) (Sandbox, error)
	Get(context.Context, string) (Sandbox, error)
}

type StreamEvent struct {
	Type    string
	Message string
	Payload map[string]any
}

type RuntimeRequest struct {
	JobID            string
	SessionID        string
	Task             string
	Model            string
	WorkingDirectory string
	AuthBundle       []byte
	ExternalThreadID string
	AllowWrite       bool
	Timeout          time.Duration
}

type RuntimeResult struct {
	ExternalThreadID    string         `json:"externalThreadId,omitempty"`
	Summary             string         `json:"summary"`
	Output              map[string]any `json:"output,omitempty"`
	Artifacts           []Artifact     `json:"artifacts,omitempty"`
	RefreshedAuthBundle []byte         `json:"-"`
}

type Artifact struct {
	Kind       string `json:"kind"`
	Path       string `json:"path,omitempty"`
	MediaType  string `json:"mediaType,omitempty"`
	Content    string `json:"content,omitempty"`
	StorageKey string `json:"-"`
	SizeBytes  int64  `json:"sizeBytes"`
	SHA256     string `json:"sha256,omitempty"`
}

type Runtime interface {
	Name() string
	Run(context.Context, Sandbox, RuntimeRequest, func(StreamEvent)) (RuntimeResult, error)
}

func jsonObject(value any) ([]byte, error) {
	if value == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(value)
}
