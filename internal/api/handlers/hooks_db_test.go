package handlers

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/triggers"
)

// What happens between "a signed event arrived" and "a workflow ran".
//
// Opt-in via TEST_DATABASE_URL, and DB-backed for the same reason the usage
// tests are: the guarantees live in a unique index and a WHERE clause, and a
// fake would pass while production double-fires.
//
//	TEST_DATABASE_URL="host=localhost user=postgres password=postgres dbname=workflow_ai port=5434 sslmode=disable"
//
// Three rules, each of which costs real money when it breaks: a redelivered
// event runs once, a draft workflow does not run at all, and a chatty provider
// cannot spend an org's whole allowance.

func hooksDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run trigger dispatch tests")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.IntegrationTrigger{}, &models.WebhookDelivery{},
		&models.Workflow{}, &models.WorkflowRun{}, &models.Organization{},
		&models.CreditBalance{}, &models.CreditLedger{}, &models.CreditHold{},
		&models.IntegrationConnection{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedTrigger creates a published workflow with a GitHub trigger on it.
func seedTrigger(t *testing.T, db *gorm.DB, published bool) (*WorkflowHandler, *models.IntegrationTrigger) {
	t.Helper()
	userID := uuid.NewString()

	// A real org with real credit. Admission is not a formality — it refuses a
	// run for an org it cannot find or one that is out of credit, and a test
	// without both would be asserting against a refusal rather than the rule it
	// means to check.
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "Trigger tests", Slug: "trigger-tests-" + uuid.NewString()[:8],
		Plan: models.PlanFree, Personal: true, Seats: 1,
	}
	if err := db.Create(&org).Error; err != nil {
		t.Fatalf("seed org: %v", err)
	}
	orgID := org.ID.String()
	if err := credits.Grant(db, orgID, 500_000, models.ReasonAdjustment,
		"test-"+orgID); err != nil {
		t.Fatalf("seed credits: %v", err)
	}

	wf := models.Workflow{
		BaseModel: models.BaseModel{ID: uuid.New()}, UserID: userID,
		OrganizationID: orgID, Name: "PR digest", Published: published,
		Nodes: models.JSONB(`[]`), Edges: models.JSONB(`[]`),
	}
	if err := db.Create(&wf).Error; err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	trig := models.IntegrationTrigger{
		OrganizationID: orgID, UserID: userID, WorkflowID: wf.ID.String(),
		Provider: "github", Event: "pull_request.opened", ResourceID: "acme/widgets",
		ScopeID:  "1000001",
		Delivery: models.DeliveryPush, Enabled: true, MaxRunsPerHour: 60,
		Secret: "s3cret",
	}
	if err := db.Create(&trig).Error; err != nil {
		t.Fatalf("seed trigger: %v", err)
	}

	t.Cleanup(func() {
		db.Unscoped().Where("trigger_id = ?", trig.ID.String()).Delete(&models.WebhookDelivery{})
		db.Unscoped().Where("workflow_id = ?", wf.ID.String()).Delete(&models.WorkflowRun{})
		db.Unscoped().Delete(&trig)
		db.Unscoped().Delete(&wf)
	})

	return &WorkflowHandler{db: &database.DBClient{DB: db}, bill: billing.New(db)}, &trig
}

func prEvent(key string) triggers.Event {
	return triggers.Event{
		Key: key, Type: "pull_request.opened", ResourceID: "acme/widgets",
		ScopeID:    "1000001",
		OccurredAt: time.Now().UTC(),
		Data:       map[string]any{"number": 42, "title": "Fix it", "base": "main"},
	}
}

func TestAnotherInstallationsEventIsNotOurs(t *testing.T) {
	// The reason the installation id is stored at all. Repository names are not
	// unique across accounts, and one app-level webhook hears every installation
	// the app is on — so a stranger who names a repository "acme/widgets" and
	// installs the app would otherwise be able to start this workflow.
	db := hooksDB(t)
	h, trig := seedTrigger(t, db, true)

	foreign := prEvent("delivery-from-elsewhere")
	foreign.ScopeID = "9999999" // same repo name, different installation

	if h.dispatch(context.Background(), trig, foreign) {
		t.Error("an event from another installation started a run")
	}
	if n := runCount(t, db, trig.WorkflowID); n != 0 {
		t.Errorf("want no runs, got %d", n)
	}

	// And the trigger still works for its own installation — a guard that blocks
	// everything is just as broken.
	if !h.dispatch(context.Background(), trig, prEvent("delivery-ours")) {
		t.Error("the trigger's own installation was blocked too")
	}
}

func runCount(t *testing.T, db *gorm.DB, workflowID string) int64 {
	t.Helper()
	var n int64
	db.Model(&models.WorkflowRun{}).Where("workflow_id = ?", workflowID).Count(&n)
	return n
}

func TestARedeliveredEventRunsOnce(t *testing.T) {
	// GitHub has a "Redeliver" button, Slack retries three times on any non-200,
	// and a poll that crashes mid-window re-reads it. Each of those is the same
	// event arriving twice; each duplicate run would send the email again.
	db := hooksDB(t)
	h, trig := seedTrigger(t, db, true)

	ev := prEvent("delivery-abc")
	first := h.dispatch(context.Background(), trig, ev)
	second := h.dispatch(context.Background(), trig, ev)

	if !first {
		t.Fatal("the first delivery did not run")
	}
	if second {
		t.Error("the same delivery ran a second time")
	}
	if n := runCount(t, db, trig.WorkflowID); n != 1 {
		t.Errorf("want 1 run, got %d", n)
	}
}

func TestTwoDifferentEventsBothRun(t *testing.T) {
	// The other half of dedupe: keying too broadly would collapse genuine events
	// into one and silently drop work.
	db := hooksDB(t)
	h, trig := seedTrigger(t, db, true)

	h.dispatch(context.Background(), trig, prEvent("delivery-1"))
	h.dispatch(context.Background(), trig, prEvent("delivery-2"))

	if n := runCount(t, db, trig.WorkflowID); n != 2 {
		t.Errorf("want 2 runs for 2 distinct deliveries, got %d", n)
	}
}

func TestADraftWorkflowDoesNotRunOnRealEvents(t *testing.T) {
	// The publish gate. Someone wiring up a trigger should not have their
	// half-built workflow start acting on live pull requests while they edit it.
	db := hooksDB(t)
	h, trig := seedTrigger(t, db, false)

	if h.dispatch(context.Background(), trig, prEvent("delivery-draft")) {
		t.Error("an unpublished workflow ran")
	}
	if n := runCount(t, db, trig.WorkflowID); n != 0 {
		t.Errorf("want no runs, got %d", n)
	}
}

func TestPublishingLetsTheSameTriggerThrough(t *testing.T) {
	// Guards the gate against being stuck shut — a trigger that never fires even
	// when published is the same bug seen from the other side.
	db := hooksDB(t)
	h, trig := seedTrigger(t, db, false)

	h.dispatch(context.Background(), trig, prEvent("before-publish"))
	db.Model(&models.Workflow{}).Where("id::text = ?", trig.WorkflowID).Update("published", true)
	if !h.dispatch(context.Background(), trig, prEvent("after-publish")) {
		t.Error("a published workflow still did not run")
	}
}

func TestAFilteredEventCostsNothing(t *testing.T) {
	db := hooksDB(t)
	h, trig := seedTrigger(t, db, true)
	db.Model(trig).Update("filters", models.JSONB(`{"base":"release"}`))
	db.First(trig, "id = ?", trig.ID)

	if h.dispatch(context.Background(), trig, prEvent("filtered-out")) {
		t.Error("an event the filter excludes started a run")
	}
	if n := runCount(t, db, trig.WorkflowID); n != 0 {
		t.Errorf("a filtered event created %d runs", n)
	}
}

func TestATriggerStopsAtItsHourlyCap(t *testing.T) {
	// Slack permits 30,000 events per workspace per hour. Without a ceiling the
	// first chatty account empties the org's credit and every other workflow in
	// it stops too.
	db := hooksDB(t)
	h, trig := seedTrigger(t, db, true)
	db.Model(trig).Update("max_runs_per_hour", 3)
	db.First(trig, "id = ?", trig.ID)

	fired := 0
	for i := 0; i < 6; i++ {
		if h.dispatch(context.Background(), trig, prEvent("burst-"+strconvI(i))) {
			fired++
		}
	}
	// The cap is checked after the delivery is claimed, so the run that crosses
	// it is the one that trips the guard: at most cap+1 get through, never all six.
	if fired > 4 {
		t.Errorf("cap of 3 let %d runs through", fired)
	}
	if fired == 0 {
		t.Error("the cap blocked everything, including the first event")
	}

	var reloaded models.IntegrationTrigger
	db.First(&reloaded, "id = ?", trig.ID)
	if reloaded.LastError == "" {
		t.Error("hitting the cap left no explanation on the trigger")
	}
}

func TestInstallationSuspensionPausesOnlyThatInstallationAndRestoreResumesIt(t *testing.T) {
	db := hooksDB(t)
	h, trig := seedTrigger(t, db, true)

	h.applyTriggerLifecycle("github", triggers.Event{
		ScopeID:   trig.ScopeID,
		Lifecycle: &triggers.LifecycleEvent{Action: triggers.LifecycleScopeSuspended},
	})
	db.First(trig, "id = ?", trig.ID)
	if trig.Enabled || trig.LastError != installationSuspendedError {
		t.Fatalf("suspended trigger = enabled %v, error %q", trig.Enabled, trig.LastError)
	}

	// A different installation's restoration cannot repair this trigger.
	h.applyTriggerLifecycle("github", triggers.Event{
		ScopeID:   "another-installation",
		Lifecycle: &triggers.LifecycleEvent{Action: triggers.LifecycleScopeRestored},
	})
	db.First(trig, "id = ?", trig.ID)
	if trig.Enabled {
		t.Fatal("another installation's restore resumed this trigger")
	}

	h.applyTriggerLifecycle("github", triggers.Event{
		ScopeID:   trig.ScopeID,
		Lifecycle: &triggers.LifecycleEvent{Action: triggers.LifecycleScopeRestored},
	})
	db.First(trig, "id = ?", trig.ID)
	if !trig.Enabled || trig.LastError != "" {
		t.Fatalf("restored trigger = enabled %v, error %q", trig.Enabled, trig.LastError)
	}
}

func TestSelectedRepositoryRemovalPausesOnlyThatRepository(t *testing.T) {
	db := hooksDB(t)
	h, trig := seedTrigger(t, db, true)

	h.applyTriggerLifecycle("github", triggers.Event{
		ScopeID: trig.ScopeID,
		Lifecycle: &triggers.LifecycleEvent{
			Action: triggers.LifecycleResourcesRemoved, ResourceIDs: []string{"acme/other"},
		},
	})
	db.First(trig, "id = ?", trig.ID)
	if !trig.Enabled {
		t.Fatal("removing another repository paused this trigger")
	}

	// GitHub repository names are case-insensitive. The lifecycle update must
	// use the same rule as exact installation verification.
	h.applyTriggerLifecycle("github", triggers.Event{
		ScopeID: trig.ScopeID,
		Lifecycle: &triggers.LifecycleEvent{
			Action: triggers.LifecycleResourcesRemoved, ResourceIDs: []string{" ACME/WIDGETS "},
		},
	})
	db.First(trig, "id = ?", trig.ID)
	if trig.Enabled || trig.LastError != repositoryRemovedError {
		t.Fatalf("removed repository trigger = enabled %v, error %q", trig.Enabled, trig.LastError)
	}

	h.applyTriggerLifecycle("github", triggers.Event{
		ScopeID: trig.ScopeID,
		Lifecycle: &triggers.LifecycleEvent{
			Action: triggers.LifecycleResourcesAdded, ResourceIDs: []string{"acme/widgets"},
		},
	})
	db.First(trig, "id = ?", trig.ID)
	if !trig.Enabled || trig.LastError != "" {
		t.Fatalf("re-added repository trigger = enabled %v, error %q", trig.Enabled, trig.LastError)
	}
}

func TestRevokedGitHubAuthorizationDeletesTheGrantAndPausesItsTriggers(t *testing.T) {
	db := hooksDB(t)
	h, trig := seedTrigger(t, db, true)
	connection := models.IntegrationConnection{
		OrganizationID: trig.OrganizationID,
		UserID:         trig.UserID,
		Provider:       "github",
		WorkspaceID:    "583231",
		WorkspaceName:  "octocat",
		AccessToken:    "revoked-token",
	}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&connection) })

	h.applyTriggerLifecycle("github", triggers.Event{
		Lifecycle: &triggers.LifecycleEvent{
			Action: triggers.LifecycleAuthorizationRevoked, AccountID: "583231", AccountName: "renamed-octocat",
		},
	})
	db.First(trig, "id = ?", trig.ID)
	if trig.Enabled || trig.LastError != authorizationRevokedError {
		t.Fatalf("revoked authorization trigger = enabled %v, error %q", trig.Enabled, trig.LastError)
	}
	var count int64
	db.Unscoped().Model(&models.IntegrationConnection{}).Where("id = ?", connection.ID).Count(&count)
	if count != 0 {
		t.Fatal("revoked GitHub authorization remained stored")
	}
}

func strconvI(i int) string { return string(rune('0' + i)) }
