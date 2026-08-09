package models

import (
	"time"

	"workflow-ai/server/internal/cryptobox"

	"gorm.io/gorm"
)

type AgentHostStatus string

const (
	AgentHostActive    AgentHostStatus = "active"
	AgentHostReconnect AgentHostStatus = "reconnect_required"
	AgentHostRevoked   AgentHostStatus = "revoked"
)

// AgentHostInstallation is the bot installation used to receive and answer
// team-chat messages. It is separate from workflow action credentials: the
// host token transports chat while tools act with the deploying user's grants.
type AgentHostInstallation struct {
	BaseModel
	OrganizationID        string          `json:"organization_id" gorm:"type:uuid;not null;index"`
	InstalledByUserID     string          `json:"installed_by_user_id" gorm:"type:uuid;not null;index"`
	Provider              string          `json:"provider" gorm:"not null;index;uniqueIndex:idx_agent_host_installation,priority:1"`
	ExternalWorkspaceID   string          `json:"external_workspace_id" gorm:"not null;uniqueIndex:idx_agent_host_installation,priority:2"`
	ExternalWorkspaceName string          `json:"external_workspace_name"`
	BotUserID             string          `json:"bot_user_id"`
	BotToken              string          `json:"-" gorm:"not null"`
	Scopes                string          `json:"scopes"`
	Status                AgentHostStatus `json:"status" gorm:"type:varchar(32);not null;default:'active';index"`
	LastVerifiedAt        *time.Time      `json:"last_verified_at,omitempty"`
	LastError             string          `json:"last_error,omitempty"`
}

func (i *AgentHostInstallation) BeforeSave(_ *gorm.DB) error {
	i.BotToken = cryptobox.Encrypt(i.BotToken)
	return nil
}

func (i *AgentHostInstallation) AfterSave(_ *gorm.DB) error {
	i.BotToken = cryptobox.Decrypt(i.BotToken)
	return nil
}

func (i *AgentHostInstallation) AfterFind(_ *gorm.DB) error {
	i.BotToken = cryptobox.Decrypt(i.BotToken)
	return nil
}

type AgentDeploymentStatus string

const (
	AgentDeploymentDraft   AgentDeploymentStatus = "draft"
	AgentDeploymentActive  AgentDeploymentStatus = "active"
	AgentDeploymentPaused  AgentDeploymentStatus = "paused"
	AgentDeploymentRevoked AgentDeploymentStatus = "revoked"
)

// AgentPermissionAnalysisRecord binds an AI-generated goal/recommendation to
// the exact workflow snapshot it analyzed. Deployment creation accepts this
// record ID instead of trusting goal text returned through the browser.
type AgentPermissionAnalysisRecord struct {
	BaseModel
	OrganizationID    string    `json:"organization_id" gorm:"type:uuid;not null;index"`
	WorkflowID        string    `json:"workflow_id" gorm:"type:uuid;not null;index"`
	RequestedByUserID string    `json:"requested_by_user_id" gorm:"type:uuid;not null;index"`
	SnapshotHash      string    `json:"snapshot_hash" gorm:"not null;index"`
	ModelID           string    `json:"model_id" gorm:"not null"`
	Goal              string    `json:"goal" gorm:"not null"`
	Summary           string    `json:"summary" gorm:"not null"`
	RecommendedPolicy JSONB     `json:"recommended_policy" gorm:"type:jsonb;not null"`
	Warnings          JSONB     `json:"warnings" gorm:"type:jsonb;not null;default:'[]'"`
	ExpiresAt         time.Time `json:"expires_at" gorm:"not null;index"`
}

// AgentDeployment is an immutable, reviewed workflow snapshot. Redeploying
// creates a new version rather than silently changing what an active agent can
// do when the canvas is edited.
type AgentDeployment struct {
	BaseModel
	OrganizationID     string                `json:"organization_id" gorm:"type:uuid;not null;index"`
	WorkflowID         string                `json:"workflow_id" gorm:"type:uuid;not null;index"`
	DeployedByUserID   string                `json:"deployed_by_user_id" gorm:"type:uuid;not null;index"`
	HostInstallationID string                `json:"host_installation_id" gorm:"type:uuid;not null;index"`
	Provider           string                `json:"provider" gorm:"not null;index"`
	Name               string                `json:"name" gorm:"not null"`
	Alias              string                `json:"alias" gorm:"not null;index"`
	ModelID            string                `json:"model_id"`
	Version            int                   `json:"version" gorm:"not null;default:1"`
	Status             AgentDeploymentStatus `json:"status" gorm:"type:varchar(20);not null;default:'draft';index"`

	SnapshotName       string    `json:"snapshot_name" gorm:"not null"`
	SnapshotNodes      JSONB     `json:"snapshot_nodes" gorm:"type:jsonb;not null"`
	SnapshotEdges      JSONB     `json:"snapshot_edges" gorm:"type:jsonb;not null"`
	SnapshotHash       string    `json:"snapshot_hash" gorm:"not null;index"`
	SourceUpdatedAt    time.Time `json:"source_updated_at"`
	CapabilityPolicy   JSONB     `json:"capability_policy" gorm:"type:jsonb;not null"`
	PermissionAnalysis JSONB     `json:"permission_analysis" gorm:"type:jsonb;not null;default:'{}'"`
}

// AgentDeploymentTarget is the closed-by-default channel allowlist. A row must
// exist and be enabled before a mention in that channel can invoke the model.
type AgentDeploymentTarget struct {
	BaseModel
	OrganizationID      string `json:"organization_id" gorm:"type:uuid;not null;index"`
	DeploymentID        string `json:"deployment_id" gorm:"type:uuid;not null;index"`
	Provider            string `json:"provider" gorm:"not null;uniqueIndex:idx_agent_target_live,priority:1,where:deleted_at IS NULL"`
	ExternalWorkspaceID string `json:"external_workspace_id" gorm:"not null;uniqueIndex:idx_agent_target_live,priority:2,where:deleted_at IS NULL"`
	ExternalChannelID   string `json:"external_channel_id" gorm:"not null;uniqueIndex:idx_agent_target_live,priority:3,where:deleted_at IS NULL"`
	ExternalChannelName string `json:"external_channel_name"`
	Enabled             bool   `json:"enabled" gorm:"not null;default:false;index"`
}

// HostedAgentThread maps one provider thread to the ChatSession that owns its
// bounded transcript and executor state.
type HostedAgentThread struct {
	BaseModel
	OrganizationID          string `json:"organization_id" gorm:"type:uuid;not null;index"`
	DeploymentID            string `json:"deployment_id" gorm:"type:uuid;not null;index"`
	ChatSessionID           string `json:"chat_session_id" gorm:"type:uuid;not null;index"`
	Provider                string `json:"provider" gorm:"not null;uniqueIndex:idx_hosted_agent_thread,priority:1"`
	ExternalWorkspaceID     string `json:"external_workspace_id" gorm:"not null;uniqueIndex:idx_hosted_agent_thread,priority:2"`
	ExternalChannelID       string `json:"external_channel_id" gorm:"not null;uniqueIndex:idx_hosted_agent_thread,priority:3"`
	ExternalThreadID        string `json:"external_thread_id" gorm:"not null;uniqueIndex:idx_hosted_agent_thread,priority:4"`
	LatestExternalMessageID string `json:"latest_external_message_id"`
}

type HostedAgentDeliveryStatus string

const (
	HostedAgentDeliveryPending    HostedAgentDeliveryStatus = "pending"
	HostedAgentDeliveryProcessing HostedAgentDeliveryStatus = "processing"
	HostedAgentDeliveryCompleted  HostedAgentDeliveryStatus = "completed"
	HostedAgentDeliveryFailed     HostedAgentDeliveryStatus = "failed"
)

// HostedAgentDelivery is both the durable ingress job and the idempotency key
// for provider retries.
type HostedAgentDelivery struct {
	BaseModel
	Provider            string `json:"provider" gorm:"not null;uniqueIndex:idx_hosted_delivery,priority:1"`
	ExternalDeliveryID  string `json:"external_delivery_id" gorm:"not null;uniqueIndex:idx_hosted_delivery,priority:2"`
	ExternalWorkspaceID string `json:"external_workspace_id" gorm:"index"`
	// ThreadKey lets concurrent workers preserve provider-thread turn order.
	// Legacy queued deliveries default to empty and retain the old behavior.
	ThreadKey            string                    `json:"-" gorm:"not null;default:'';index"`
	EventKind            string                    `json:"event_kind" gorm:"not null;index"`
	Payload              JSONB                     `json:"payload" gorm:"type:jsonb;not null"`
	Status               HostedAgentDeliveryStatus `json:"status" gorm:"type:varchar(20);not null;default:'pending';index"`
	AttemptCount         int                       `json:"attempt_count" gorm:"not null;default:0"`
	AvailableAt          time.Time                 `json:"available_at" gorm:"index"`
	ClaimedAt            *time.Time                `json:"claimed_at,omitempty"`
	CompletedAt          *time.Time                `json:"completed_at,omitempty"`
	LastError            string                    `json:"last_error,omitempty"`
	ResponseDeploymentID string                    `json:"-" gorm:"type:varchar(36);index"`
	ResponseText         string                    `json:"-" gorm:"type:text"`
	ResponseRecordedAt   *time.Time                `json:"-"`
}

type HostedAgentApprovalStatus string

const (
	HostedAgentApprovalPending   HostedAgentApprovalStatus = "pending"
	HostedAgentApprovalApproved  HostedAgentApprovalStatus = "approved"
	HostedAgentApprovalExecuting HostedAgentApprovalStatus = "executing"
	HostedAgentApprovalRejected  HostedAgentApprovalStatus = "rejected"
	HostedAgentApprovalExpired   HostedAgentApprovalStatus = "expired"
	HostedAgentApprovalExecuted  HostedAgentApprovalStatus = "executed"
	// OutcomeUnknown means execution was claimed but the worker stopped before
	// it could durably record whether the external side effect succeeded. It is
	// terminal and must never be retried automatically.
	HostedAgentApprovalOutcomeUnknown HostedAgentApprovalStatus = "outcome_unknown"
	// ReconciledDone and ReconciledVoid are requester-confirmed terminal states
	// for an indeterminate execution. The former records that the external
	// action completed; the latter records that it did not complete.
	HostedAgentApprovalReconciledDone HostedAgentApprovalStatus = "reconciled_done"
	HostedAgentApprovalReconciledVoid HostedAgentApprovalStatus = "reconciled_void"
	HostedAgentApprovalFailed         HostedAgentApprovalStatus = "failed"
)

// HostedAgentApproval pins the exact authorized call. Approval execution uses
// EffectiveOverrides directly; it never asks the model to regenerate them.
type HostedAgentApproval struct {
	BaseModel
	OrganizationID      string `json:"organization_id" gorm:"type:uuid;not null;index"`
	DeploymentID        string `json:"deployment_id" gorm:"type:uuid;not null;index"`
	DeploymentVersion   int    `json:"deployment_version" gorm:"not null"`
	ThreadID            string `json:"thread_id" gorm:"type:uuid;not null;index"`
	ChatSessionID       string `json:"chat_session_id" gorm:"type:uuid;not null;index"`
	RequesterExternalID string `json:"requester_external_id" gorm:"not null;index"`
	SourceDeliveryID    string `json:"source_delivery_id" gorm:"type:uuid;not null;uniqueIndex"`
	NodeID              string `json:"node_id" gorm:"not null"`
	Operation           string `json:"operation" gorm:"not null"`
	Reason              string `json:"reason" gorm:"not null"`
	EffectiveOverrides  JSONB  `json:"effective_overrides" gorm:"type:jsonb;not null"`
	EffectiveConfigHash string `json:"effective_config_hash" gorm:"not null"`
	// ExecutionConfig is the exact, template-resolved node configuration that
	// was reviewed. Storing only this value avoids copying unrelated session
	// outputs into the approval while ensuring a later turn cannot change it.
	ExecutionConfig        JSONB                     `json:"-" gorm:"type:jsonb;not null;default:'{}'"`
	ConfigRecordedAt       *time.Time                `json:"-"`
	ExecutionFingerprint   string                    `json:"-" gorm:"index"`
	CredentialProvider     string                    `json:"-" gorm:"index"`
	CredentialConnectionID string                    `json:"-" gorm:"type:varchar(36);index"`
	DisplayDetails         JSONB                     `json:"display_details" gorm:"type:jsonb;not null"`
	Status                 HostedAgentApprovalStatus `json:"status" gorm:"type:varchar(20);not null;default:'pending';index"`
	ExpiresAt              time.Time                 `json:"expires_at" gorm:"not null;index"`
	ResolvedAt             *time.Time                `json:"resolved_at,omitempty"`
	ExecutedAt             *time.Time                `json:"executed_at,omitempty"`
	// ExecutionKey is a stable, durable attempt identity written before any
	// external side effect. It follows the call through logs and billing, while
	// unresolved equivalent calls are blocked by the pinned call fields above.
	ExecutionKey        string     `json:"-" gorm:"index"`
	ExecutionStartedAt  *time.Time `json:"execution_started_at,omitempty"`
	OutcomeReconciledAt *time.Time `json:"outcome_reconciled_at,omitempty"`
	OutcomeReconciledBy string     `json:"-"`
	// ExecutionResult is written immediately after a successful tool call,
	// before the result is merged into the chat session. SessionRecordedAt makes
	// that merge idempotently retryable after a database or Slack failure.
	ExecutionResult           string     `json:"-" gorm:"type:text"`
	ExecutionResultRecordedAt *time.Time `json:"-"`
	SessionRecordedAt         *time.Time `json:"session_recorded_at,omitempty"`
	LastError                 string     `json:"last_error,omitempty"`
}
