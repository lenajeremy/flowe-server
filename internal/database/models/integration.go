package models

// DefaultUserID is the placeholder owner used while the app is single-user.
// When multi-user auth lands, handlers resolve the real user from the session
// (see currentUserID in handlers/integrations.go) — the schema is already
// keyed per user so no data migration is needed.
const DefaultUserID = "local"

// IntegrationConnection stores a user's OAuth connection to a third-party
// provider (notion, linear). One connection per user per provider.
type IntegrationConnection struct {
	BaseModel
	UserID        string `json:"user_id"        gorm:"not null;default:'local';uniqueIndex:idx_integration_user_provider"`
	Provider      string `json:"provider"       gorm:"not null;uniqueIndex:idx_integration_user_provider"`
	AccessToken   string `json:"-"              gorm:"not null"`
	WorkspaceName string `json:"workspace_name"`
	WorkspaceID   string `json:"workspace_id"`
	Scope         string `json:"scope"`
}
