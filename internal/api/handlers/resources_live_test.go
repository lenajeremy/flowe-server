package handlers

import (
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
)

// The dependent-resource lookup, against the real provider.
//
// Opt-in twice over — it needs a database AND a live connected account, and it
// spends someone's real GitHub rate limit:
//
//	LIVE_RESOURCES=1 TEST_DATABASE_URL="…" go test ./internal/api/handlers/ -run TestLiveChildResources -v
//
// Read-only: it lists repositories, then the branches and collaborators of one.
// It exists because the interesting failure is not JSON parsing — it is the
// chain around it. A wrong path, a missing Accept header, or a token that never
// got refreshed all produce the same symptom in the UI (an empty dropdown), and
// only a real call tells them apart.
func TestLiveChildResources(t *testing.T) {
	if os.Getenv("LIVE_RESOURCES") == "" {
		t.Skip("set LIVE_RESOURCES=1 to run (calls the real GitHub API)")
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run")
	}
	db, openErr := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if openErr != nil {
		t.Fatalf("open: %v", openErr)
	}
	h := &WorkflowHandler{db: &database.DBClient{DB: db}}

	// Try every connected account: a dev database accumulates dead tokens, and
	// one stale connection should not read as a broken feature.
	var conns []models.IntegrationConnection
	db.Where("provider = ? AND deleted_at IS NULL", "github").Find(&conns)
	if len(conns) == 0 {
		t.Skip("no github connection in this database")
	}
	var conn models.IntegrationConnection
	var repos []integrationResource
	var err error
	for _, c := range conns {
		repos, err = h.listProviderResources(c.OrganizationID, c.UserID, "github")
		if err == nil {
			conn = c
			break
		}
		t.Logf("connection %s unusable: %v", c.ID.String()[:8], truncate(err.Error(), 80))
	}
	if err != nil {
		t.Skipf("no usable github token in this database (%d tried) — reconnect GitHub to run this", len(conns))
	}
	if len(repos) == 0 {
		t.Skip("the connected account has no repositories")
	}
	t.Logf("repositories: %d (first: %s)", len(repos), repos[0].ID)
	if repos[0].Type != "repo" {
		t.Errorf("repository resource typed %q — the picker filters on this", repos[0].Type)
	}

	children, childErr := h.listChildResources(conn.OrganizationID, conn.UserID, "github", repos[0].ID)
	if childErr != nil {
		t.Fatalf("listing inside %s: %v", repos[0].ID, childErr)
	}

	kinds := map[string]int{}
	for _, c := range children {
		kinds[c.Type]++
		if c.ID == "" || c.Name == "" {
			t.Errorf("resource with an empty id or name: %+v", c)
		}
	}
	t.Logf("inside %s: %d branches, %d people", repos[0].ID, kinds["branch"], kinds["user"])

	// A repository always has at least one branch. Zero means the call worked
	// and returned nothing useful, which is the silent-empty-dropdown failure.
	if kinds["branch"] == 0 {
		t.Errorf("no branches came back for %s — the picker would be empty", repos[0].ID)
	}
}
