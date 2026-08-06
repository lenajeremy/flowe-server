package billing

import (
	"errors"
	"testing"

	"workflow-ai/server/internal/database/models"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPublishScheduleLimitOnlyAppliesToWorkflowsWithLiveSchedules(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Workflow{}, &models.ScheduledTrigger{}); err != nil {
		t.Fatal(err)
	}

	orgID := uuid.NewString()
	userID := uuid.NewString()
	existing := models.Workflow{
		UserID: userID, OrganizationID: orgID, Name: "Existing schedule", Published: true,
	}
	target := models.Workflow{
		UserID: userID, OrganizationID: orgID, Name: "GitHub issue workflow",
	}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ScheduledTrigger{
		UserID: userID, OrganizationID: orgID, WorkflowID: existing.ID.String(), Enabled: true,
	}).Error; err != nil {
		t.Fatal(err)
	}

	gate := New(db)
	if err := gate.CheckPublishSchedule(orgID, target.ID.String(), models.PlanFree); err != nil {
		t.Fatalf("integration-only workflow was charged a schedule slot: %v", err)
	}

	if err := db.Create(&models.ScheduledTrigger{
		UserID: userID, OrganizationID: orgID, WorkflowID: target.ID.String(), Enabled: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gate.CheckPublishSchedule(orgID, target.ID.String(), models.PlanFree); !errors.Is(err, ErrLimit) {
		t.Fatalf("second live scheduled workflow got error %v, want plan limit", err)
	}
}
