package models

import (
	"time"

	"workflow-ai/server/internal/cryptobox"

	"gorm.io/gorm"
)

// Delivery is how we learn an event happened.
//
// The distinction is ours, not the user's: a trigger says "when a PR opens",
// and whether the provider pushes that to us or we go asking is a property of
// the provider, not of the intent. Keeping it here means a provider can move
// from Poll to Push later without touching a single workflow.
const (
	DeliveryPush = "push"
	DeliveryPoll = "poll"
)

// IntegrationTrigger wakes a workflow when something happens in a connected
// tool. One row per trigger node on a canvas — a workflow may have several,
// so uniqueness is the WorkflowID + NodeID pair (WebhookTrigger's
// one-per-workflow rule is the thing this replaces).
//
// The row carries state for both delivery strategies rather than splitting into
// two tables: the columns a Poll trigger leaves empty are cheap, and one table
// means the lifecycle code (enable, disable, health, ownership, cascade delete)
// exists once instead of twice.
type IntegrationTrigger struct {
	BaseModel
	OrganizationID string `json:"organization_id" gorm:"type:uuid;not null;index"`
	UserID         string `json:"user_id"         gorm:"type:uuid;not null;index"`
	WorkflowID     string `json:"workflow_id" gorm:"not null;index;uniqueIndex:idx_integration_trigger_live_node,priority:1,where:deleted_at IS NULL"`
	// NodeID ties the trigger to the canvas node that configured it, so an
	// arriving event can be injected into that node alone rather than into every
	// trigger node on the graph.
	NodeID string `json:"node_id" gorm:"not null;uniqueIndex:idx_integration_trigger_live_node,priority:2,where:deleted_at IS NULL"`

	Provider string `json:"provider" gorm:"not null;index"`
	// Event is the registry's event id, e.g. "pull_request.opened".
	Event string `json:"event" gorm:"not null"`
	// ResourceID scopes the trigger within the account: a repo, a channel, a
	// mailbox. Empty for providers whose events are account-wide.
	ResourceID    string `json:"resource_id"`
	ResourceLabel string `json:"resource_label"`
	// ScopeID is the provider's own identifier for the installation or workspace
	// the events come from — a GitHub App installation id, a Slack team_id.
	//
	// It exists because an app-level webhook hears every installation at once,
	// and a repository's full name is not unique across them: two accounts can
	// each have "acme/widgets", and matching on the name alone would let one
	// installation's events wake the other's workflow. The installation id is
	// the thing that actually distinguishes them.
	ScopeID string `json:"scope_id" gorm:"index"`
	// Filters narrows within an event — {"base":"main"} for PRs into main.
	// Filtering here rather than in a branch node is what stops a busy repo from
	// buying a full workflow run per event.
	Filters JSONB `json:"filters" gorm:"type:jsonb"`

	Delivery string `json:"delivery" gorm:"not null;default:push"`
	Enabled  bool   `json:"enabled"  gorm:"not null;default:true"`

	// ── push state ──
	// RemoteID is the provider's own id for the hook, needed to delete or renew
	// it. Empty for app-level providers (Slack) that register nothing.
	RemoteID string `json:"remote_id"`
	// Secret signs inbound payloads. Encrypted at rest like OAuth tokens: it is
	// the only thing standing between a stranger's POST and a workflow run.
	Secret    string     `json:"-"`
	ExpiresAt *time.Time `json:"expires_at"`

	// ── poll state ──
	// Cursor is the provider's watermark — a Gmail historyId, a Drive pageToken.
	// It advances only after the events it produced have been admitted, so a
	// crash re-delivers rather than drops.
	Cursor              string     `json:"-"`
	PollIntervalSeconds int        `json:"poll_interval_seconds" gorm:"not null;default:60"`
	NextPollAt          *time.Time `json:"next_poll_at"`
	LastPolledAt        *time.Time `json:"last_polled_at"`

	// ── health ──
	LastEventAt *time.Time `json:"last_event_at"`
	// ConsecutiveFailures drives the poll backoff and, past a threshold, disables
	// the trigger with LastError explaining why — a trigger that has quietly
	// stopped working is worse than one that says it stopped.
	ConsecutiveFailures int    `json:"consecutive_failures" gorm:"not null;default:0"`
	LastError           string `json:"last_error"`

	// MaxRunsPerHour caps what one trigger can spend. Slack alone permits 30,000
	// events per workspace per hour; without a ceiling a chatty workspace turns
	// into an emptied credit balance overnight.
	MaxRunsPerHour int `json:"max_runs_per_hour" gorm:"not null;default:60"`
}

func (t *IntegrationTrigger) BeforeSave(_ *gorm.DB) error {
	t.Secret = cryptobox.Encrypt(t.Secret)
	return nil
}

func (t *IntegrationTrigger) AfterSave(_ *gorm.DB) error {
	t.Secret = cryptobox.Decrypt(t.Secret)
	return nil
}

func (t *IntegrationTrigger) AfterFind(_ *gorm.DB) error {
	t.Secret = cryptobox.Decrypt(t.Secret)
	return nil
}

// WebhookDelivery records that we have already seen an event.
//
// Every provider retries: Slack three times on any non-200, GitHub on request,
// and a poll that crashes mid-flight re-reads the same window. Without this
// table a redelivery is a second workflow run — a second email sent, a second
// row written, a second charge on the ledger. The unique index is the whole
// mechanism; see the insert helper for why it must be ON CONFLICT DO NOTHING
// rather than a check-then-insert.
type WebhookDelivery struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	Provider  string    `json:"provider"    gorm:"not null;uniqueIndex:idx_delivery_provider_key,priority:1"`
	Key       string    `json:"key"         gorm:"not null;uniqueIndex:idx_delivery_provider_key,priority:2"`
	TriggerID string    `json:"trigger_id"  gorm:"type:uuid;index"`
	CreatedAt time.Time `json:"created_at"  gorm:"index"`
}
