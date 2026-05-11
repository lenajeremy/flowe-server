package models

import "time"

type RunStatus string

const (
	RunStatusRunning   RunStatus = "running"
	RunStatusCompleted RunStatus = "completed"
	RunStatusError     RunStatus = "error"
)

// WorkflowRun persists a record for every workflow execution.
type WorkflowRun struct {
	BaseModel
	WorkflowID   string    `json:"workflow_id"    gorm:"index"`
	WorkflowName string    `json:"workflow_name"  gorm:"not null"`
	Status       RunStatus `json:"status"         gorm:"type:varchar(20);not null;default:'running'"`
	ErrorMessage string    `json:"error_message,omitempty"`
	Events       JSONB     `json:"events"         gorm:"type:jsonb"`
	Input        JSONB     `json:"input"          gorm:"type:jsonb"`
}

// Workflow persists the full workflow definition (nodes + edges as JSONB).
type Workflow struct {
	BaseModel
	Name  string `json:"name"  gorm:"not null;index"`
	Nodes JSONB  `json:"nodes" gorm:"type:jsonb;not null;default:'[]'"`
	Edges JSONB  `json:"edges" gorm:"type:jsonb;not null;default:'[]'"`
}

// ApiKey stores hashed API keys for programmatic workflow triggers.
type ApiKey struct {
	BaseModel
	Name        string     `json:"name"          gorm:"not null"`
	KeyHash     string     `json:"key_hash"      gorm:"not null;uniqueIndex"`
	KeyPrefix   string     `json:"key_prefix"    gorm:"not null"`
	WorkspaceID string     `json:"workspace_id"`
	LastUsedAt  *time.Time `json:"last_used_at"`
}

// WorkflowChat stores the AI builder conversation for a workflow.
type WorkflowChat struct {
	BaseModel
	WorkflowID string `json:"workflow_id" gorm:"not null;uniqueIndex"`
	Messages   JSONB  `json:"messages"    gorm:"type:jsonb;not null;default:'[]'"`
}

// WorkflowVersion stores snapshots of workflow node/edge definitions.
type WorkflowVersion struct {
	BaseModel
	WorkflowID string `json:"workflow_id" gorm:"not null;index"`
	Version    int    `json:"version"     gorm:"not null"`
	Nodes      JSONB  `json:"nodes"       gorm:"type:jsonb;not null"`
	Edges      JSONB  `json:"edges"       gorm:"type:jsonb;not null"`
	Name       string `json:"name"`
}

// WebhookTrigger maps a random token to a workflow for inbound webhook triggering.
type WebhookTrigger struct {
	BaseModel
	WorkflowID  string `json:"workflow_id"  gorm:"not null;uniqueIndex"`
	Token       string `json:"token"        gorm:"not null;uniqueIndex"`
	Description string `json:"description"`
}

// ScheduledTrigger runs a workflow on a calendar-based schedule.
type ScheduledTrigger struct {
	BaseModel
	WorkflowID string     `json:"workflow_id"  gorm:"not null;uniqueIndex"`
	Frequency  string     `json:"frequency"`   // "hourly" | "daily" | "weekly" | "monthly"
	RunTime    string     `json:"run_time"`    // "HH:MM" (ignored for hourly)
	DayOfWeek  int        `json:"day_of_week"` // 0=Sun…6=Sat (weekly only)
	DayOfMonth int        `json:"day_of_month"`// 1–31 (monthly only)
	Repeat     bool       `json:"repeat"       gorm:"default:true"`
	Enabled    bool       `json:"enabled"      gorm:"default:true"`
	LastRunAt  *time.Time `json:"last_run_at"`
	NextRunAt  *time.Time `json:"next_run_at"`
}
