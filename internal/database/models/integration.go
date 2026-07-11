package models

import "time"

// IntegrationConnection stores a user's OAuth connection to a third-party
// provider. One connection per user per provider. RefreshToken/ExpiresAt are
// set only for providers with expiring tokens (gmail, gitlab); a nil
// ExpiresAt means the access token does not expire.
type IntegrationConnection struct {
	BaseModel
	UserID        string     `json:"user_id"        gorm:"type:uuid;not null;uniqueIndex:idx_integration_user_provider"`
	Provider      string     `json:"provider"       gorm:"not null;uniqueIndex:idx_integration_user_provider"`
	AccessToken   string     `json:"-"              gorm:"not null"`
	RefreshToken  string     `json:"-"`
	ExpiresAt     *time.Time `json:"-"`
	WorkspaceName string     `json:"workspace_name"`
	WorkspaceID   string     `json:"workspace_id"`
	Scope         string     `json:"scope"`
}
