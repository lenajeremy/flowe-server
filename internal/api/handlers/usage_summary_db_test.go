package handlers

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
)

// The usage summary against a real Postgres. Opt-in via TEST_DATABASE_URL:
//
//	TEST_DATABASE_URL="host=localhost user=postgres password=postgres dbname=workflow_ai port=5434 sslmode=disable"
//
// DB-backed because every bug these pin lived in the SQL — a WHERE clause applied
// to one figure and not its neighbour, and a COALESCE that reached for the wrong
// column. None of them are reachable through a fake.
//
// What they defend is a page that adds up. A member reading "used 698 of 160,000,
// 79,302 left" cannot tell which of the three numbers to believe, and the answer
// was that 698 was theirs while 160,000 belonged to the whole organization.

func summaryDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run usage summary tests")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.CreditLedger{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedLedger writes one org's window: a grant that belongs to nobody, and spend
// belonging to two different people.
func seedLedger(t *testing.T, db *gorm.DB) (h *WorkflowHandler, orgID, alice, bob string) {
	t.Helper()
	orgID = uuid.NewString()
	alice, bob = uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		db.Unscoped().Where("organization_id = ?", orgID).Delete(&models.CreditLedger{})
	})

	rows := []models.CreditLedger{
		{OrganizationID: orgID, Delta: 100_000, Reason: models.ReasonMonthlyGrant},
		{OrganizationID: orgID, Delta: -600, Reason: models.ReasonLLMUsage,
			UserID: &alice, Provider: "openai", Model: "gpt-5.5", WorkflowName: "Digest"},
		// A non-LLM charge: the node type lands in provider, where a model would go.
		{OrganizationID: orgID, Delta: -2, Reason: models.ReasonEmail,
			UserID: &alice, Provider: "emailSend", WorkflowName: "Digest"},
		{OrganizationID: orgID, Delta: -400, Reason: models.ReasonLLMUsage,
			UserID: &bob, Provider: "openai", Model: "gpt-5.5"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	return &WorkflowHandler{db: &database.DBClient{DB: db}, bill: billing.New(db)}, orgID, alice, bob
}

func creditsFor(t *testing.T, s gin1H, key, label string) int64 {
	t.Helper()
	for _, b := range s[key].([]usageBreakdown) {
		if b.Label == label {
			return b.Credits
		}
	}
	return -1
}

// gin1H keeps the assertions readable without importing gin into every line.
type gin1H = map[string]any

func window() (time.Time, time.Time) {
	return time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
}

func TestAMembersCreditsAddedIsNotTheWholeOrgs(t *testing.T) {
	db := summaryDB(t)
	h, orgID, alice, _ := seedLedger(t, db)
	from, to := window()

	s, err := h.usageSummary(orgID, from, to, alice, "", false)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got := s["spent"].(int64); got != 602 {
		t.Fatalf("spent = %d, want 602 — Alice's rows only", got)
	}
	// The bug: this returned the organization's 100,000 next to Alice's 602, while
	// the Credits tab underneath listed no rows at all, because grants carry no user.
	if got := s["granted"].(int64); got != 0 {
		t.Fatalf("granted = %d, want 0 — grants belong to the org, and this view is one person", got)
	}
}

func TestAnOrgWideViewStillSeesItsGrants(t *testing.T) {
	db := summaryDB(t)
	h, orgID, _, _ := seedLedger(t, db)
	from, to := window()

	s, err := h.usageSummary(orgID, from, to, "", "", false)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got := s["granted"].(int64); got != 100_000 {
		t.Fatalf("granted = %d, want 100000 — scoping must not hide grants from the org view", got)
	}
	if got := s["spent"].(int64); got != 1002 {
		t.Fatalf("spent = %d, want 1002", got)
	}
}

func TestBreakdownsFollowTheCreditsFilter(t *testing.T) {
	db := summaryDB(t)
	h, orgID, _, _ := seedLedger(t, db)
	from, to := window()

	s, err := h.usageSummary(orgID, from, to, "", "grant", false)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	// The bug: these described spend regardless of the filter, so selecting Credits
	// put an itemisation of charges directly above the words "no credits added".
	if got := creditsFor(t, s, "by_reason", "Monthly allowance"); got != 100_000 {
		t.Fatalf("by_reason[Monthly allowance] = %d, want 100000", got)
	}
	if got := creditsFor(t, s, "by_reason", "AI"); got != -1 {
		t.Fatalf("by_reason still lists AI (%d) while filtered to credits", got)
	}
}

func TestBreakdownsSumToTheirHeadlineFigure(t *testing.T) {
	db := summaryDB(t)
	h, orgID, _, _ := seedLedger(t, db)
	from, to := window()

	for _, tc := range []struct{ kind, total string }{{"", "spent"}, {"grant", "granted"}} {
		s, err := h.usageSummary(orgID, from, to, "", tc.kind, false)
		if err != nil {
			t.Fatalf("summary %q: %v", tc.kind, err)
		}
		want := s[tc.total].(int64)
		for _, key := range []string{"by_reason", "by_workflow", "by_model"} {
			var sum int64
			for _, b := range s[key].([]usageBreakdown) {
				sum += b.Credits
			}
			// A card whose parts do not add up to its own total leaves the reader with
			// no way to tell which number is the wrong one.
			if sum != want {
				t.Fatalf("kind=%q %s sums to %d, want %d", tc.kind, key, sum, want)
			}
		}
	}
}

func TestNodeTypesAreNotReportedAsModels(t *testing.T) {
	db := summaryDB(t)
	h, orgID, _, _ := seedLedger(t, db)
	from, to := window()

	s, err := h.usageSummary(orgID, from, to, "", "", false)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	// "emailSend" is a node type stored where a model would go. Under a heading that
	// says "By model" it reads as the name of a model we bill against.
	if got := creditsFor(t, s, "by_model", "emailSend"); got != -1 {
		t.Fatalf("by_model lists the node type emailSend (%d credits)", got)
	}
	if got := creditsFor(t, s, "by_model", "Other steps"); got != 2 {
		t.Fatalf("by_model[Other steps] = %d, want 2 — the charge must still be counted", got)
	}
	if got := creditsFor(t, s, "by_model", "gpt-5.5"); got != 1000 {
		t.Fatalf("by_model[gpt-5.5] = %d, want 1000", got)
	}
}
