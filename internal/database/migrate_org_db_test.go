package database

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/tenancy"
)

// The tenancy migration against a real Postgres. Opt-in via TEST_DATABASE_URL,
// same as the datastore tests.
//
// What matters here is not that the SQL runs — it is that the org id computed in
// Go agrees with the one computed in SQL. The backfill assigns rows using the SQL
// expression; every request afterwards resolves the org using the Go function. If
// those two ever disagree, every backfilled row becomes invisible to its owner and
// nothing fails loudly. That agreement is what this test pins.

func TestPersonalOrgIDMatchesTheBackfillSQL(t *testing.T) {
	db := dbForTest(t)

	for _, userID := range []string{
		uuid.NewString(),
		uuid.NewString(),
		"00000000-0000-0000-0000-000000000001",
	} {
		var fromSQL string
		if err := db.Raw(
			`SELECT md5('org:' || ?::uuid::text)::uuid::text`, userID).Scan(&fromSQL).Error; err != nil {
			t.Fatalf("sql: %v", err)
		}
		if got := tenancy.PersonalOrgID(userID).String(); got != fromSQL {
			t.Fatalf("org id disagrees for user %s:\n  Go:  %s\n  SQL: %s\n"+
				"the backfill and every later request would resolve different orgs", userID, got, fromSQL)
		}
	}
}

func TestBackfillAssignsRowsAndLocksTheColumn(t *testing.T) {
	db := dbForTest(t)
	migrateForTest(t, db)

	// A user who predates tenancy, owning one row in each of two tables. Written
	// with raw SQL so the org column is left NULL exactly as a pre-migration row
	// would be — the model would otherwise fill it in.
	user := models.User{Email: uuid.NewString() + "@example.test", Name: "Old Account"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	uid := user.ID.String()
	wfID := uuid.New()
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", uid).Delete(&models.Workflow{})
		db.Unscoped().Where("user_id = ?", uid).Delete(&models.WorkflowRun{})
		db.Unscoped().Where("user_id = ?", uid).Delete(&models.OrgMember{})
		db.Exec(`DELETE FROM credit_ledger WHERE organization_id = ?`, tenancy.PersonalOrgID(uid))
		db.Exec(`DELETE FROM credit_balances WHERE organization_id = ?`, tenancy.PersonalOrgID(uid))
		db.Unscoped().Where("id = ?", tenancy.PersonalOrgID(uid)).Delete(&models.Organization{})
		db.Unscoped().Delete(&models.User{}, "id = ?", uid)
	})

	// The column has to be nullable for this insert to be possible at all, which is
	// itself the thing prepareOrgColumns guarantees before AutoMigrate runs.
	if err := db.Exec(`ALTER TABLE workflows ALTER COLUMN organization_id DROP NOT NULL`).Error; err != nil {
		t.Fatalf("relax constraint: %v", err)
	}
	if err := db.Exec(`ALTER TABLE workflow_runs ALTER COLUMN organization_id DROP NOT NULL`).Error; err != nil {
		t.Fatalf("relax constraint: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO workflows (id, created_at, updated_at, user_id, name, nodes, edges, published)
		VALUES (?, now(), now(), ?, 'legacy', '[]'::jsonb, '[]'::jsonb, false)`,
		wfID, uid).Error; err != nil {
		t.Fatalf("insert legacy workflow: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO workflow_runs (id, created_at, updated_at, user_id, workflow_id, workflow_name, status)
		VALUES (?, now(), now(), ?, ?, 'legacy', 'completed')`,
		uuid.New(), uid, wfID.String()).Error; err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}

	if err := backfillOrganizations(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	want := tenancy.PersonalOrgID(uid).String()

	var org models.Organization
	if err := db.Where("id = ?", want).First(&org).Error; err != nil {
		t.Fatalf("personal org was not created: %v", err)
	}
	if !org.Personal || org.Plan != models.PlanFree {
		t.Fatalf("personal org should start on the free plan: %+v", org)
	}
	if org.Name != "Old Account" {
		t.Fatalf("org name should come from the user, got %q", org.Name)
	}

	var role string
	if err := db.Raw(`SELECT role FROM org_members WHERE user_id = ? AND organization_id = ?`,
		uid, want).Scan(&role).Error; err != nil || role != string(models.RoleOwner) {
		t.Fatalf("membership missing or not owner: role=%q err=%v", role, err)
	}

	for _, table := range []string{"workflows", "workflow_runs"} {
		var got string
		if err := db.Raw(
			`SELECT organization_id::text FROM `+table+` WHERE user_id = ? LIMIT 1`, uid).
			Scan(&got).Error; err != nil {
			t.Fatalf("read back %s: %v", table, err)
		}
		if got != want {
			t.Fatalf("%s row was not reassigned: got %q want %q", table, got, want)
		}
	}

	// Opening balance, and a ledger row that explains where it came from.
	var bal models.CreditBalance
	if err := db.Where("organization_id = ?", want).First(&bal).Error; err != nil {
		t.Fatalf("no opening balance: %v", err)
	}
	if bal.Balance <= 0 {
		t.Fatalf("opening balance should be the free grant, got %d", bal.Balance)
	}
	var ledgerSum int64
	db.Raw(`SELECT COALESCE(sum(delta),0) FROM credit_ledger WHERE organization_id = ?`, want).
		Scan(&ledgerSum)
	if ledgerSum != bal.Balance {
		t.Fatalf("balance %d is not derivable from the ledger (%d)", bal.Balance, ledgerSum)
	}

	// And the column is locked down afterwards, so a future insert that forgets the
	// org fails loudly instead of creating an untenanted row.
	err := db.Exec(`
		INSERT INTO workflows (id, created_at, updated_at, user_id, name, nodes, edges, published)
		VALUES (?, now(), now(), ?, 'no org', '[]'::jsonb, '[]'::jsonb, false)`,
		uuid.New(), uid).Error
	if err == nil {
		t.Fatal("a workflow with no organization was accepted — the NOT NULL was not applied")
	}
}

func TestBackfillIsIdempotent(t *testing.T) {
	db := dbForTest(t)
	migrateForTest(t, db)

	user := models.User{Email: uuid.NewString() + "@example.test"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	uid := user.ID.String()
	orgID := tenancy.PersonalOrgID(uid)
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", uid).Delete(&models.OrgMember{})
		db.Exec(`DELETE FROM credit_ledger WHERE organization_id = ?`, orgID)
		db.Exec(`DELETE FROM credit_balances WHERE organization_id = ?`, orgID)
		db.Unscoped().Where("id = ?", orgID).Delete(&models.Organization{})
		db.Unscoped().Delete(&models.User{}, "id = ?", uid)
	})

	// Hot reload can restart Setup mid-flight, so running it repeatedly must not
	// create a second org, a second membership, or a second grant.
	for i := 0; i < 3; i++ {
		if err := backfillOrganizations(db); err != nil {
			t.Fatalf("backfill run %d: %v", i+1, err)
		}
	}

	var orgs, members, grants int64
	db.Model(&models.Organization{}).Where("id = ?", orgID).Count(&orgs)
	db.Model(&models.OrgMember{}).Where("user_id = ?", uid).Count(&members)
	db.Raw(`SELECT count(*) FROM credit_ledger WHERE organization_id = ?`, orgID).Scan(&grants)
	if orgs != 1 || members != 1 || grants != 1 {
		t.Fatalf("re-running the backfill duplicated rows: orgs=%d members=%d grants=%d",
			orgs, members, grants)
	}
}

func TestProvisionAndBackfillAgreeOnTheSameOrg(t *testing.T) {
	db := dbForTest(t)
	migrateForTest(t, db)

	// Signup provisions in Go; the migration backfills in SQL. Both must land on
	// one org for the same user, or a user would end up with two.
	user := models.User{Email: uuid.NewString() + "@example.test", Name: "Both Paths"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	uid := user.ID.String()
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", uid).Delete(&models.OrgMember{})
		db.Exec(`DELETE FROM credit_ledger WHERE organization_id = ?`, tenancy.PersonalOrgID(uid))
		db.Exec(`DELETE FROM credit_balances WHERE organization_id = ?`, tenancy.PersonalOrgID(uid))
		db.Unscoped().Where("id = ?", tenancy.PersonalOrgID(uid)).Delete(&models.Organization{})
		db.Unscoped().Delete(&models.User{}, "id = ?", uid)
	})

	org, err := tenancy.Provision(db, &user)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := backfillOrganizations(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var count int64
	db.Model(&models.Organization{}).
		Joins("JOIN org_members m ON m.organization_id = organizations.id").
		Where("m.user_id = ?", uid).Count(&count)
	if count != 1 {
		t.Fatalf("user ended up in %d orgs, want 1", count)
	}

	resolved, err := tenancy.OrgForUser(db, uid)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ID != org.ID {
		t.Fatalf("resolution disagrees with provisioning: %s vs %s", resolved.ID, org.ID)
	}
}

// migrateForTest brings the test database up to the current schema, exercising the
// same pre/post ordering Setup uses.
func migrateForTest(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := prepareOrgColumns(db); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := db.AutoMigrate(
		&models.User{}, &models.Organization{}, &models.OrgMember{},
		&models.CreditLedger{}, &models.CreditBalance{}, &models.CreditHold{},
		&models.Workflow{}, &models.WorkflowRun{}, &models.DataStore{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// A leftover NULL from an earlier failed run would make every later assertion
	// confusing, so clear the ground first.
	db.Exec(`DELETE FROM workflows WHERE organization_id IS NULL`)
	db.Exec(`DELETE FROM workflow_runs WHERE organization_id IS NULL`)
	_ = time.Now
}
