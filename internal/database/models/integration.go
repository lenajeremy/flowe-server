package models

import (
	"time"

	"workflow-ai/server/internal/cryptobox"

	"gorm.io/gorm"
)

// IntegrationConnection stores a user's OAuth connection to a third-party
// provider. One connection per user per provider. RefreshToken/ExpiresAt are
// set only for providers with expiring tokens (gmail, gitlab); a nil
// ExpiresAt means the access token does not expire.
//
// AccessToken/RefreshToken are encrypted at rest (see cryptobox) via the hooks
// below and are never serialized to clients (json:"-").
type IntegrationConnection struct {
	BaseModel
	UserID        string     `json:"user_id"        gorm:"type:uuid;not null;uniqueIndex:idx_integration_user_provider"`
	Provider      string     `json:"provider"       gorm:"not null;uniqueIndex:idx_integration_user_provider"`
	AccessToken   string     `json:"-"              gorm:"not null"`
	RefreshToken  string     `json:"-"`
	// UserAccessToken is a second grant acting as the human who connected
	// (Slack xoxp- tokens) for providers where actions can run either as the
	// bot or on the user's behalf. Empty for providers without user grants.
	UserAccessToken string     `json:"-"`
	ExpiresAt       *time.Time `json:"-"`
	WorkspaceName   string     `json:"workspace_name"`
	WorkspaceID     string     `json:"workspace_id"`
	Scope           string     `json:"scope"`
}

// BeforeSave encrypts tokens on the way to the database. Encrypt is idempotent
// and a no-op when no key is configured.
func (c *IntegrationConnection) BeforeSave(_ *gorm.DB) error {
	c.AccessToken = cryptobox.Encrypt(c.AccessToken)
	c.RefreshToken = cryptobox.Encrypt(c.RefreshToken)
	c.UserAccessToken = cryptobox.Encrypt(c.UserAccessToken)
	return nil
}

// AfterSave restores plaintext on the in-memory struct so callers that keep
// using it after a write see the real token, not ciphertext.
func (c *IntegrationConnection) AfterSave(_ *gorm.DB) error {
	c.AccessToken = cryptobox.Decrypt(c.AccessToken)
	c.RefreshToken = cryptobox.Decrypt(c.RefreshToken)
	c.UserAccessToken = cryptobox.Decrypt(c.UserAccessToken)
	return nil
}

// AfterFind decrypts tokens loaded from the database (legacy plaintext rows are
// returned unchanged).
func (c *IntegrationConnection) AfterFind(_ *gorm.DB) error {
	c.AccessToken = cryptobox.Decrypt(c.AccessToken)
	c.RefreshToken = cryptobox.Decrypt(c.RefreshToken)
	c.UserAccessToken = cryptobox.Decrypt(c.UserAccessToken)
	return nil
}
