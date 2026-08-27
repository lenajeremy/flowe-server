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
	RepositoryGitHub       = "github"
	RepositoryGitLab       = "gitlab"
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
	RepositoryProvider  string        `json:"repositoryProvider"`
	RepositoryID        string        `json:"repositoryId,omitempty"`
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

// ToolGrant is the exact authority a coding-agent job receives for one
// workflow node. Both operations and fields are deny-by-default. The grant is
// frozen into the job together with the graph snapshot, so editing a workflow
// or expanding the integration catalog cannot broaden an already-running job.
type ToolGrant struct {
	NodeID                string   `json:"nodeId"`
	AllowedOperations     []string `json:"allowedOperations"`
	AllowedOverrideFields []string `json:"allowedOverrideFields"`
}

type ToolPolicy struct {
	Version int         `json:"version"`
	Nodes   []ToolGrant `json:"nodes"`
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
	// ToolWorkflow is the exact graph submitted for this run. ToolGrants is the
	// exact operation/field policy approved on the canvas. Both are persisted
	// before queueing and are the only source of authority at callback time.
	ToolWorkflow json.RawMessage
	ToolGrants   []ToolGrant
	// ToolNodeIDs is accepted only for workflows saved by older clients. New
	// submissions convert it to a pinned, read-only policy against ToolWorkflow.
	ToolNodeIDs []string
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
	BaselineSHA      string
	AuthBundle       []byte
	ExternalThreadID string
	AllowWrite       bool
	Timeout          time.Duration
	// ToolEndpoint and ToolToken let the runtime point the agent at this
	// workflow's nodes. Both empty means the job was granted no tools, and the
	// runtime must then configure none — an agent that can see a tool server it
	// has no grant for would spend turns discovering it can do nothing.
	ToolEndpoint string
	ToolToken    string
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
