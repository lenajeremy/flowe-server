package models

import (
	"time"

	"workflow-ai/server/internal/cryptobox"

	"gorm.io/gorm"
)

type CodingAgentCredentialStatus string

const (
	CodingAgentCredentialConnected CodingAgentCredentialStatus = "connected"
	CodingAgentCredentialExpired   CodingAgentCredentialStatus = "expired"
	CodingAgentCredentialRevoked   CodingAgentCredentialStatus = "revoked"
	CodingAgentCredentialError     CodingAgentCredentialStatus = "error"
)

type CodingAgentAuthStatus string

const (
	CodingAgentAuthProvisioning CodingAgentAuthStatus = "provisioning"
	CodingAgentAuthWaiting      CodingAgentAuthStatus = "waiting"
	CodingAgentAuthConnected    CodingAgentAuthStatus = "connected"
	CodingAgentAuthFailed       CodingAgentAuthStatus = "failed"
	CodingAgentAuthCancelled    CodingAgentAuthStatus = "cancelled"
	CodingAgentAuthExpired      CodingAgentAuthStatus = "expired"
)

func (s CodingAgentAuthStatus) Terminal() bool {
	switch s {
	case CodingAgentAuthConnected, CodingAgentAuthFailed, CodingAgentAuthCancelled, CodingAgentAuthExpired:
		return true
	default:
		return false
	}
}

// CodingAgentAuthAttempt is the durable browser/device-code handoff. UserCode
// is short-lived and not a credential; the resulting auth bundle lives only in
// CodingAgentCredential. ExternalSandboxID is hidden from clients.
type CodingAgentAuthAttempt struct {
	BaseModel
	// ActiveKey is non-null only while this attempt can still be completed. A
	// nullable unique key closes the "two first requests" race that row locking
	// alone cannot prevent when there is no existing attempt to lock.
	ActiveKey         *string               `json:"-" gorm:"type:varchar(160);uniqueIndex"`
	OrganizationID    string                `json:"organization_id" gorm:"type:uuid;not null;index"`
	UserID            string                `json:"user_id" gorm:"type:uuid;not null;index"`
	Runtime           string                `json:"runtime" gorm:"type:varchar(32);not null;index"`
	Provider          string                `json:"provider" gorm:"type:varchar(32);not null"`
	Status            CodingAgentAuthStatus `json:"status" gorm:"type:varchar(24);not null;default:'provisioning';index"`
	VerificationURL   string                `json:"verification_url,omitempty"`
	UserCode          string                `json:"user_code,omitempty"`
	ExternalSandboxID string                `json:"-"`
	ClaimedBy         string                `json:"-" gorm:"index"`
	HeartbeatAt       *time.Time            `json:"-" gorm:"index"`
	ExpiresAt         time.Time             `json:"expires_at" gorm:"not null;index"`
	CompletedAt       *time.Time            `json:"completed_at,omitempty"`
	CancelRequestedAt *time.Time            `json:"cancel_requested_at,omitempty"`
	LastError         string                `json:"last_error,omitempty"`
}

// CodingAgentCredential stores a runtime's portable authentication bundle.
// For Codex this is the contents of CODEX_HOME/auth.json, captured after the
// user completes OpenAI's device-code login. The bundle is encrypted at rest,
// is never copied into a sandbox snapshot, and is never returned by an API.
type CodingAgentCredential struct {
	BaseModel
	OrganizationID string                      `json:"organization_id" gorm:"type:uuid;not null;uniqueIndex:idx_coding_agent_credential,priority:1;index"`
	UserID         string                      `json:"user_id" gorm:"type:uuid;not null;uniqueIndex:idx_coding_agent_credential,priority:2;index"`
	Runtime        string                      `json:"runtime" gorm:"type:varchar(32);not null;uniqueIndex:idx_coding_agent_credential,priority:3"`
	Status         CodingAgentCredentialStatus `json:"status" gorm:"type:varchar(24);not null;default:'connected';index"`
	AccountLabel   string                      `json:"account_label"`
	AuthBundle     string                      `json:"-" gorm:"type:text;not null"`
	ConnectedAt    time.Time                   `json:"connected_at" gorm:"not null"`
	LastVerifiedAt *time.Time                  `json:"last_verified_at,omitempty"`
	ExpiresAt      *time.Time                  `json:"expires_at,omitempty"`
	LastError      string                      `json:"last_error,omitempty"`
}

func (c *CodingAgentCredential) BeforeSave(_ *gorm.DB) error {
	c.AuthBundle = cryptobox.Encrypt(c.AuthBundle)
	return nil
}

func (c *CodingAgentCredential) AfterSave(_ *gorm.DB) error {
	c.AuthBundle = cryptobox.Decrypt(c.AuthBundle)
	return nil
}

func (c *CodingAgentCredential) AfterFind(_ *gorm.DB) error {
	c.AuthBundle = cryptobox.Decrypt(c.AuthBundle)
	return nil
}

type CodingAgentEnvironmentStatus string

const (
	CodingAgentEnvironmentProvisioning CodingAgentEnvironmentStatus = "provisioning"
	CodingAgentEnvironmentReady        CodingAgentEnvironmentStatus = "ready"
	CodingAgentEnvironmentBusy         CodingAgentEnvironmentStatus = "busy"
	CodingAgentEnvironmentStopped      CodingAgentEnvironmentStatus = "stopped"
	CodingAgentEnvironmentArchived     CodingAgentEnvironmentStatus = "archived"
	CodingAgentEnvironmentError        CodingAgentEnvironmentStatus = "error"
	CodingAgentEnvironmentDeleting     CodingAgentEnvironmentStatus = "deleting"
)

// CodingAgentEnvironment is the durable Fernary identity for an isolated
// provider sandbox. WorkspaceKey controls intentional reuse; external IDs are
// never accepted from a workflow node or browser request.
type CodingAgentEnvironment struct {
	BaseModel
	OrganizationID     string                       `json:"organization_id" gorm:"type:uuid;not null;index"`
	UserID             string                       `json:"user_id" gorm:"type:uuid;not null;index"`
	WorkflowID         string                       `json:"workflow_id" gorm:"type:uuid;index"`
	NodeID             string                       `json:"node_id" gorm:"index"`
	WorkspaceKey       string                       `json:"workspace_key" gorm:"not null;uniqueIndex"`
	Provider           string                       `json:"provider" gorm:"type:varchar(32);not null;index;uniqueIndex:idx_coding_agent_external_sandbox,priority:1,where:external_sandbox_id <> ''"`
	ExternalSandboxID  string                       `json:"-" gorm:"uniqueIndex:idx_coding_agent_external_sandbox,priority:2,where:external_sandbox_id <> ''"`
	Snapshot           string                       `json:"snapshot"`
	Region             string                       `json:"region"`
	Status             CodingAgentEnvironmentStatus `json:"status" gorm:"type:varchar(24);not null;default:'provisioning';index"`
	RepositoryProvider string                       `json:"repository_provider" gorm:"type:varchar(16);not null;default:'github'"`
	RepositoryID       string                       `json:"repository_id,omitempty"`
	Repository         string                       `json:"repository"`
	Branch             string                       `json:"branch"`
	CurrentJobID       string                       `json:"current_job_id,omitempty" gorm:"type:varchar(36);index"`
	LastActivityAt     *time.Time                   `json:"last_activity_at,omitempty" gorm:"index"`
	AutoStopMinutes    int                          `json:"auto_stop_minutes" gorm:"not null;default:15"`
	AutoDeleteMinutes  int                          `json:"auto_delete_minutes" gorm:"not null;default:10080"`
	Configuration      JSONB                        `json:"configuration" gorm:"type:jsonb;not null;default:'{}'"`
	LastError          string                       `json:"last_error,omitempty"`
	LifecycleVersion   int                          `json:"lifecycle_version" gorm:"not null;default:1"`
}

type CodingAgentSessionStatus string

const (
	CodingAgentSessionActive CodingAgentSessionStatus = "active"
	CodingAgentSessionClosed CodingAgentSessionStatus = "closed"
	CodingAgentSessionError  CodingAgentSessionStatus = "error"
)

// CodingAgentSession maps Fernary conversation continuity to the runtime's
// own thread/session identifier. It contains no transcript secrets itself.
type CodingAgentSession struct {
	BaseModel
	OrganizationID   string                   `json:"organization_id" gorm:"type:uuid;not null;index"`
	UserID           string                   `json:"user_id" gorm:"type:uuid;not null;index"`
	EnvironmentID    string                   `json:"environment_id" gorm:"type:uuid;not null;index"`
	Runtime          string                   `json:"runtime" gorm:"type:varchar(32);not null;index"`
	ConversationKey  string                   `json:"conversation_key" gorm:"not null;uniqueIndex"`
	ExternalThreadID string                   `json:"-"`
	Status           CodingAgentSessionStatus `json:"status" gorm:"type:varchar(24);not null;default:'active';index"`
	LastJobID        string                   `json:"last_job_id,omitempty" gorm:"type:varchar(36);index"`
	LastActivityAt   *time.Time               `json:"last_activity_at,omitempty"`
	LastError        string                   `json:"last_error,omitempty"`
}

type CodingAgentJobStatus string

const (
	CodingAgentJobPending   CodingAgentJobStatus = "pending"
	CodingAgentJobClaimed   CodingAgentJobStatus = "claimed"
	CodingAgentJobRunning   CodingAgentJobStatus = "running"
	CodingAgentJobSucceeded CodingAgentJobStatus = "succeeded"
	CodingAgentJobFailed    CodingAgentJobStatus = "failed"
	CodingAgentJobCancelled CodingAgentJobStatus = "cancelled"
	CodingAgentJobTimedOut  CodingAgentJobStatus = "timed_out"
)

func (s CodingAgentJobStatus) Terminal() bool {
	switch s {
	case CodingAgentJobSucceeded, CodingAgentJobFailed, CodingAgentJobCancelled, CodingAgentJobTimedOut:
		return true
	default:
		return false
	}
}

// CodingAgentJob is the durable command boundary. ExecutionPolicy and Input
// are fixed before the sandbox is touched. IdempotencyKey prevents a workflow
// retry or HTTP reconnect from launching the same non-idempotent task twice.
type CodingAgentJob struct {
	BaseModel
	OrganizationID      string               `json:"organization_id" gorm:"type:uuid;not null;index"`
	UserID              string               `json:"user_id" gorm:"type:uuid;not null;index"`
	WorkflowID          string               `json:"workflow_id" gorm:"type:uuid;not null;index"`
	WorkflowRunID       string               `json:"workflow_run_id" gorm:"type:varchar(36);index"`
	NodeID              string               `json:"node_id" gorm:"not null;index"`
	ConversationKey     string               `json:"conversation_key" gorm:"index"`
	EnvironmentID       string               `json:"environment_id" gorm:"type:varchar(36);index"`
	SessionID           string               `json:"session_id" gorm:"type:varchar(36);index"`
	CredentialID        string               `json:"-" gorm:"type:varchar(36);index"`
	IdempotencyKey      string               `json:"idempotency_key" gorm:"not null;uniqueIndex"`
	Runtime             string               `json:"runtime" gorm:"type:varchar(32);not null;index"`
	Task                string               `json:"task" gorm:"type:text;not null"`
	Input               JSONB                `json:"input" gorm:"type:jsonb;not null;default:'{}'"`
	ExecutionPolicy     JSONB                `json:"execution_policy" gorm:"type:jsonb;not null;default:'{}'"`
	Status              CodingAgentJobStatus `json:"status" gorm:"type:varchar(24);not null;default:'pending';index"`
	AttemptCount        int                  `json:"attempt_count" gorm:"not null;default:0"`
	MaxAttempts         int                  `json:"max_attempts" gorm:"not null;default:3"`
	AvailableAt         time.Time            `json:"available_at" gorm:"not null;index"`
	ClaimedAt           *time.Time           `json:"claimed_at,omitempty"`
	StartedAt           *time.Time           `json:"started_at,omitempty"`
	HeartbeatAt         *time.Time           `json:"heartbeat_at,omitempty" gorm:"index"`
	CancelRequestedAt   *time.Time           `json:"cancel_requested_at,omitempty" gorm:"index"`
	CompletedAt         *time.Time           `json:"completed_at,omitempty"`
	ClaimedBy           string               `json:"-" gorm:"index"`
	ProviderExecutionID string               `json:"-" gorm:"index"`
	RepositoryBaseSHA   string               `json:"repository_base_sha,omitempty" gorm:"type:varchar(64)"`
	WorkingDirectory    string               `json:"-" gorm:"type:text"`
	Result              JSONB                `json:"result" gorm:"type:jsonb;not null;default:'{}'"`
	Summary             string               `json:"summary" gorm:"type:text"`
	LastError           string               `json:"last_error,omitempty" gorm:"type:text"`
	NextEventSequence   int                  `json:"-" gorm:"not null;default:1"`
	// ToolTokenHash authenticates the sandbox when it calls back to use the
	// workflow's nodes as tools. Only the SHA-256 is stored: the plaintext
	// exists in the sandbox and nowhere else, so a database read cannot be
	// turned into the authority to act as this job. Cleared when the job
	// reaches a terminal state, which is what bounds the token's life — it is
	// the job, not a clock, that decides when the sandbox stops being trusted.
	ToolTokenHash string `json:"-" gorm:"type:varchar(64);index"`
	// ToolWorkflow and ToolPolicy are immutable authorization snapshots. The
	// callback must never resolve a job against the mutable saved workflow.
	ToolWorkflow JSONB `json:"-" gorm:"type:jsonb;not null;default:'{}'"`
	ToolPolicy   JSONB `json:"tool_policy" gorm:"type:jsonb;not null;default:'{\"version\":1,\"nodes\":[]}'"`
	// ToolNodeIDs remains for backwards compatibility with already-saved
	// canvases. Newly queued jobs also persist a normalized ToolPolicy.
	ToolNodeIDs JSONB `json:"tool_node_ids,omitempty" gorm:"type:jsonb;not null;default:'[]'"`
}

// CodingAgentEvent is an append-only audit and progress stream. Event payloads
// must be scrubbed before insertion; credentials and raw environment variables
// are forbidden here.
type CodingAgentEvent struct {
	BaseModel
	OrganizationID string `json:"organization_id" gorm:"type:uuid;not null;index"`
	JobID          string `json:"job_id" gorm:"type:uuid;not null;uniqueIndex:idx_coding_agent_job_sequence,priority:1;index"`
	Sequence       int    `json:"sequence" gorm:"not null;uniqueIndex:idx_coding_agent_job_sequence,priority:2"`
	Type           string `json:"type" gorm:"type:varchar(48);not null;index"`
	Message        string `json:"message" gorm:"type:text"`
	Payload        JSONB  `json:"payload" gorm:"type:jsonb;not null;default:'{}'"`
}

// CodingAgentArtifact retains the useful output after the environment stops.
// Large blobs live in object storage; small patches and reports may be inline.
type CodingAgentArtifact struct {
	BaseModel
	OrganizationID string `json:"organization_id" gorm:"type:uuid;not null;index"`
	JobID          string `json:"job_id" gorm:"type:uuid;not null;index"`
	Kind           string `json:"kind" gorm:"type:varchar(32);not null;index"`
	Path           string `json:"path"`
	MediaType      string `json:"media_type"`
	SizeBytes      int64  `json:"size_bytes" gorm:"not null;default:0"`
	SHA256         string `json:"sha256" gorm:"index"`
	StorageKey     string `json:"-"`
	InlineContent  string `json:"inline_content,omitempty" gorm:"type:text"`
}

type CodingAgentToolCallStatus string

const (
	CodingAgentToolCallPendingApproval CodingAgentToolCallStatus = "pending_approval"
	CodingAgentToolCallApproved        CodingAgentToolCallStatus = "approved"
	CodingAgentToolCallExecuting       CodingAgentToolCallStatus = "executing"
	CodingAgentToolCallSucceeded       CodingAgentToolCallStatus = "succeeded"
	CodingAgentToolCallFailed          CodingAgentToolCallStatus = "failed"
	CodingAgentToolCallOutcomeUnknown  CodingAgentToolCallStatus = "outcome_unknown"
	CodingAgentToolCallRejected        CodingAgentToolCallStatus = "rejected"
	CodingAgentToolCallCancelled       CodingAgentToolCallStatus = "cancelled"
)

func (s CodingAgentToolCallStatus) Terminal() bool {
	switch s {
	case CodingAgentToolCallSucceeded, CodingAgentToolCallFailed, CodingAgentToolCallOutcomeUnknown,
		CodingAgentToolCallRejected, CodingAgentToolCallCancelled:
		return true
	default:
		return false
	}
}

// CodingAgentToolCall is the durable idempotency and approval boundary for one
// sandbox callback. An executing or outcome-unknown mutation is never replayed
// automatically: the external side effect may already have happened.
type CodingAgentToolCall struct {
	BaseModel
	OrganizationID   string                    `json:"organization_id" gorm:"type:uuid;not null;index"`
	UserID           string                    `json:"user_id" gorm:"type:uuid;not null;index"`
	JobID            string                    `json:"job_id" gorm:"type:uuid;not null;uniqueIndex:idx_coding_agent_tool_request,priority:1;index"`
	RequestKey       string                    `json:"-" gorm:"type:varchar(64);not null;uniqueIndex:idx_coding_agent_tool_request,priority:2"`
	Fingerprint      string                    `json:"-" gorm:"type:varchar(64);not null;index"`
	NodeID           string                    `json:"node_id" gorm:"not null;index"`
	NodeLabel        string                    `json:"node_label"`
	ToolName         string                    `json:"tool_name" gorm:"not null"`
	Operation        string                    `json:"operation" gorm:"not null"`
	Effect           string                    `json:"effect" gorm:"type:varchar(16);not null;index"`
	Reason           string                    `json:"reason" gorm:"type:text"`
	Arguments        JSONB                     `json:"arguments" gorm:"type:jsonb;not null;default:'{}'"`
	EffectiveConfig  JSONB                     `json:"effective_config" gorm:"type:jsonb;not null;default:'{}'"`
	Status           CodingAgentToolCallStatus `json:"status" gorm:"type:varchar(24);not null;index"`
	Result           JSONB                     `json:"result,omitempty" gorm:"type:jsonb;not null;default:'{}'"`
	LastError        string                    `json:"last_error,omitempty" gorm:"type:text"`
	RequestedAt      time.Time                 `json:"requested_at" gorm:"not null;index"`
	ApprovedAt       *time.Time                `json:"approved_at,omitempty"`
	ApprovedByUserID string                    `json:"approved_by_user_id,omitempty" gorm:"type:uuid"`
	StartedAt        *time.Time                `json:"started_at,omitempty"`
	CompletedAt      *time.Time                `json:"completed_at,omitempty"`
}
