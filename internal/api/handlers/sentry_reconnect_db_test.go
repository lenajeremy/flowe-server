package handlers

import (
	"testing"

	"workflow-ai/server/internal/database/models"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Reinstalling the Sentry app mints a new installation uuid. Triggers are
// pinned to that uuid — one webhook URL receives every customer's events and
// the uuid is what tells them apart — so a reconnect that leaves them on the
// old value produces triggers that match nothing and never fire again.
func TestSentryReconnectMovesTriggersToTheNewInstallation(t *testing.T) {
	db := sentryReconnectDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()

	trigger := models.IntegrationTrigger{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID, UserID: userID, Provider: "sentry",
		Event: "issue.created", ResourceID: "backend", ScopeID: "install-old", Enabled: true,
	}
	if err := db.Create(&trigger).Error; err != nil {
		t.Fatal(err)
	}

	previous := models.IntegrationConnection{WorkspaceID: "acme", InstallationID: "install-old"}
	current := models.IntegrationConnection{WorkspaceID: "acme", InstallationID: "install-new"}
	if err := resentryTriggers(db, orgID, userID, previous, current); err != nil {
		t.Fatalf("resentryTriggers: %v", err)
	}

	var reloaded models.IntegrationTrigger
	if err := db.First(&reloaded, "id = ?", trigger.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.ScopeID != "install-new" {
		t.Fatalf("scope_id = %q, want the new installation — the trigger would never match a delivery", reloaded.ScopeID)
	}
	if !reloaded.Enabled {
		t.Error("a same-organization reconnect disabled a trigger that is still valid")
	}
}

// A different organization is not a reconnect. Project slugs can collide across
// Sentry organizations while meaning different things, so retargeting would
// quietly point someone's workflow at a stranger's errors.
func TestSentryReconnectToAnotherOrganizationDisablesTriggers(t *testing.T) {
	db := sentryReconnectDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()

	trigger := models.IntegrationTrigger{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID, UserID: userID, Provider: "sentry",
		Event: "issue.created", ResourceID: "backend", ScopeID: "install-old", Enabled: true,
	}
	if err := db.Create(&trigger).Error; err != nil {
		t.Fatal(err)
	}

	previous := models.IntegrationConnection{WorkspaceID: "acme", InstallationID: "install-old"}
	current := models.IntegrationConnection{WorkspaceID: "other-co", InstallationID: "install-new"}
	if err := resentryTriggers(db, orgID, userID, previous, current); err != nil {
		t.Fatalf("resentryTriggers: %v", err)
	}

	var reloaded models.IntegrationTrigger
	if err := db.First(&reloaded, "id = ?", trigger.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.Enabled {
		t.Fatal("a trigger was left live against a different Sentry organization")
	}
	if reloaded.ScopeID == "install-new" {
		t.Error("the trigger was retargeted at another organization's installation")
	}
	if reloaded.LastError == "" {
		t.Error("the trigger was disabled with no reason, so nobody can tell why it stopped")
	}
}

func sentryReconnectDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.IntegrationTrigger{}); err != nil {
		t.Fatal(err)
	}
	return db
}
