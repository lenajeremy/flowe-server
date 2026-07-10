package models

// IntegrationConnection stores a user's OAuth connection to a third-party
// provider (notion, linear). One connection per user per provider.
type IntegrationConnection struct {
	BaseModel
	UserID        string `json:"user_id"        gorm:"type:uuid;not null;uniqueIndex:idx_integration_user_provider"`
	Provider      string `json:"provider"       gorm:"not null;uniqueIndex:idx_integration_user_provider"`
	AccessToken   string `json:"-"              gorm:"not null"`
	WorkspaceName string `json:"workspace_name"`
	WorkspaceID   string `json:"workspace_id"`
	Scope         string `json:"scope"`
}
