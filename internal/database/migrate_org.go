package database

import (
	"fmt"
	"log/slog"

	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database/models"

	"gorm.io/gorm"
)

// The tenancy migration.
//
// Ten tables gain organization_id. Every existing user gets a personal org of
// one, and every row they own is reassigned to it.
//
// Ordering is the whole difficulty, and it is not the order AutoMigrate would
// pick:
//
//  1. Add organization_id as NULLABLE by hand. AutoMigrate would emit
//     "ADD COLUMN … NOT NULL" with no default, which Postgres rejects outright on
//     any table that already has rows.
//  2. Drop the two unique indexes whose column list changed. GORM creates an
//     index when the NAME is absent and otherwise leaves it alone, so a
//     redefined index is silently kept at its old definition forever.
//  3. AutoMigrate — creates the new tables and indexes.
//  4. Backfill orgs, memberships, and every organization_id.
//  5. Only then apply NOT NULL, which fails loudly if the backfill missed a row.
//
// The whole backfill is idempotent: an org's id is derived from its owner's id
// (md5('org:'||user_id)::uuid), so re-running it recomputes the same values
// instead of creating a second org. That also means step 4 needs no "have I run
// yet" flag, which matters because hot reload can restart this mid-flight.

// orgTables are the ten tables that carry user_id directly and therefore need
// their own tenant column. DataKV and DataRecord inherit scope through store_id;
// LoginCode is keyed by email and needs nothing.
var orgTables = []string{
	"workflow_runs",
	"workflows",
	"api_keys",
	"workflow_versions",
	"webhook_triggers",
	"scheduled_triggers",
	"workflow_chats",
	"chat_sessions",
	"integration_connections",
	"data_stores",
}

// orgIDFromUser is the deterministic org id expression, used identically in every
// statement below. Changing it after a deployment would orphan every row, so it
// lives in exactly one place.
const orgIDFromUser = `md5('org:' || %s::text)::uuid`

// prepareOrgColumns runs before AutoMigrate: it adds the tenant column as
// nullable and clears away indexes whose definition changed.
func prepareOrgColumns(db *gorm.DB) error {
	for _, t := range orgTables {
		if !db.Migrator().HasTable(t) {
			continue // fresh database; AutoMigrate will create it correctly
		}
		if db.Migrator().HasColumn(t, "organization_id") {
			continue
		}
		if err := db.Exec(fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN organization_id uuid`, t)).Error; err != nil {
			return fmt.Errorf("add organization_id to %s: %w", t, err)
		}
		slog.Info("tenancy migration: added nullable organization_id", "table", t)
	}

	// idx_store_owner_name moves from (user_id, workflow_id, name) to
	// (organization_id, workflow_id, name). Account-scoped stores becoming
	// org-shared is the intended semantics for teams, not a regression.
	//
	// idx_integration_user_provider moves from (user_id, provider) to
	// (organization_id, user_id, provider).
	//
	// Dropping them before the column is populated is safe: Postgres treats NULLs
	// in a unique index as distinct, so the recreated index tolerates the gap
	// until the backfill lands. And because each user maps to their own org, no
	// two backfilled rows can collide on the new key.
	for _, idx := range []struct{ table, name string }{
		{"data_stores", "idx_store_owner_name"},
		{"integration_connections", "idx_integration_user_provider"},
	} {
		if !db.Migrator().HasTable(idx.table) || !db.Migrator().HasIndex(idx.table, idx.name) {
			continue
		}
		if !indexNeedsRebuild(db, idx.name) {
			continue
		}
		if err := db.Migrator().DropIndex(idx.table, idx.name); err != nil {
			return fmt.Errorf("drop %s: %w", idx.name, err)
		}
		slog.Info("tenancy migration: dropped index for rebuild on organization_id", "index", idx.name)
	}
	return nil
}

// indexNeedsRebuild reports whether an index is still on its pre-tenancy
// definition. Without this check every restart would drop and rebuild the index,
// which on a large table is an avoidable lock.
func indexNeedsRebuild(db *gorm.DB, name string) bool {
	var def string
	if err := db.Raw(
		`SELECT indexdef FROM pg_indexes WHERE indexname = ?`, name).Scan(&def).Error; err != nil {
		return true // cannot tell — rebuilding is the safe direction
	}
	return def != "" && !containsOrgColumn(def)
}

func containsOrgColumn(def string) bool {
	for i := 0; i+15 <= len(def); i++ {
		if def[i:i+15] == "organization_id" {
			return true
		}
	}
	return false
}

// backfillOrganizations assigns every pre-existing row to its owner's personal
// org, then locks the column down.
func backfillOrganizations(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		// One personal org per user. Name prefers the user's own name and falls
		// back to the email local part; slug is the local part plus a short hash
		// suffix, because two people can be alex@ at different domains.
		res := tx.Exec(`
			INSERT INTO organizations (id, created_at, updated_at, name, slug, plan, personal)
			SELECT ` + fmt.Sprintf(orgIDFromUser, "u.id") + `,
			       now(), now(),
			       COALESCE(NULLIF(u.name, ''), split_part(u.email, '@', 1)),
			       regexp_replace(lower(split_part(u.email, '@', 1)), '[^a-z0-9]+', '-', 'g')
			         || '-' || substr(md5(u.id::text), 1, 6),
			       'free', true
			FROM users u
			WHERE u.deleted_at IS NULL
			ON CONFLICT (id) DO NOTHING`)
		if res.Error != nil {
			return fmt.Errorf("create personal orgs: %w", res.Error)
		}
		if res.RowsAffected > 0 {
			slog.Info("tenancy migration: created personal organizations", "count", res.RowsAffected)
		}

		if err := tx.Exec(`
			INSERT INTO org_members (id, created_at, updated_at, organization_id, user_id, role)
			SELECT gen_random_uuid(), now(), now(),
			       ` + fmt.Sprintf(orgIDFromUser, "u.id") + `, u.id, 'owner'
			FROM users u
			WHERE u.deleted_at IS NULL
			ON CONFLICT DO NOTHING`).Error; err != nil {
			return fmt.Errorf("create memberships: %w", err)
		}

		// Reassign owned rows. Scoped to NULL so a row deliberately moved between
		// orgs later is never dragged back to its author's personal org.
		for _, t := range orgTables {
			res := tx.Exec(fmt.Sprintf(
				`UPDATE %s SET organization_id = %s WHERE organization_id IS NULL AND user_id IS NOT NULL`,
				t, fmt.Sprintf(orgIDFromUser, "user_id")))
			if res.Error != nil {
				return fmt.Errorf("backfill %s: %w", t, res.Error)
			}
			if res.RowsAffected > 0 {
				slog.Info("tenancy migration: reassigned rows", "table", t, "count", res.RowsAffected)
			}
		}

		// Opening credit balance, so an existing account is not locked out the
		// moment enforcement turns on. The matching ledger row keeps the balance
		// derivable from history rather than appearing from nowhere.
		grant := credits.PlanGrant(models.PlanFree)
		if err := tx.Exec(`
			INSERT INTO credit_balances (organization_id, balance, reserved, lifetime_spent, updated_at, last_grant_at)
			SELECT o.id, ?, 0, 0, now(), now() FROM organizations o
			ON CONFLICT (organization_id) DO NOTHING`, grant).Error; err != nil {
			return fmt.Errorf("seed credit balances: %w", err)
		}
		if err := tx.Exec(`
			INSERT INTO credit_ledger (id, created_at, updated_at, organization_id, delta, reason, external_ref)
			SELECT gen_random_uuid(), now(), now(), o.id, ?, ?, 'signup:' || o.id::text
			FROM organizations o
			ON CONFLICT (external_ref) DO NOTHING`, grant, models.ReasonSignupGrant).Error; err != nil {
			return fmt.Errorf("seed signup ledger: %w", err)
		}

		// The column can only be locked down once nothing is NULL. If a row slipped
		// through — a user row soft-deleted while owning live rows, say — this
		// fails here rather than letting an untenanted row exist indefinitely.
		for _, t := range orgTables {
			var orphans int64
			if err := tx.Raw(fmt.Sprintf(
				`SELECT count(*) FROM %s WHERE organization_id IS NULL`, t)).Scan(&orphans).Error; err != nil {
				return err
			}
			if orphans > 0 {
				return fmt.Errorf("%s still has %d rows with no organization — refusing to "+
					"apply NOT NULL; these rows are unreachable by any tenant-scoped query", t, orphans)
			}
			if err := tx.Exec(fmt.Sprintf(
				`ALTER TABLE %s ALTER COLUMN organization_id SET NOT NULL`, t)).Error; err != nil {
				return fmt.Errorf("set not null on %s: %w", t, err)
			}
		}
		return nil
	})
}
