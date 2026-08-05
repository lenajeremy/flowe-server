package database

import (
	"fmt"
	"log/slog"

	"workflow-ai/server/config"
	"workflow-ai/server/internal/database/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type DBClient struct {
	DB *gorm.DB
}

func NewDBClient() (*DBClient, error) {
	dsn := fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s port=%s sslmode=disable",
		config.GetEnv("POSTGRES_HOST"),
		config.GetEnv("POSTGRES_USER"),
		config.GetEnv("POSTGRES_PASSWORD"),
		config.GetEnv("POSTGRES_DB"),
		config.GetEnv("POSTGRES_PORT"),
	)

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	slog.Info("connected to PostgreSQL")
	return &DBClient{DB: db}, nil
}

func (c *DBClient) Setup() error {
	slog.Info("running database migrations")
	// Early builds keyed integration connections by provider alone; the index
	// must go before AutoMigrate adds the per-user composite one.
	if c.DB.Migrator().HasIndex(&models.IntegrationConnection{}, "idx_integration_connections_provider") {
		_ = c.DB.Migrator().DropIndex(&models.IntegrationConnection{}, "idx_integration_connections_provider")
	}
	// Purge soft-deleted connections — they still occupy the unique index and
	// block reconnects (early builds soft-deleted on disconnect).
	if c.DB.Migrator().HasTable(&models.IntegrationConnection{}) {
		c.DB.Unscoped().Where("deleted_at IS NOT NULL").Delete(&models.IntegrationConnection{})
	}
	// Publishing gates scheduled runs. Workflows that were already scheduling
	// before the flag existed must stay live — detect that here, BEFORE
	// AutoMigrate adds the column, so the backfill runs exactly once and never
	// re-publishes something the user has since unpublished.
	backfillPublished := c.DB.Migrator().HasTable(&models.Workflow{}) &&
		!c.DB.Migrator().HasColumn(&models.Workflow{}, "published")

	// Tenancy has to touch the schema both before and after AutoMigrate — see
	// migrate_org.go for why the order is what it is.
	if err := prepareOrgColumns(c.DB); err != nil {
		return err
	}

	if err := c.DB.AutoMigrate(
		&models.User{},
		&models.LoginCode{},
		&models.Organization{},
		&models.OrgMember{},
		&models.CreditLedger{},
		&models.CreditBalance{},
		&models.CreditHold{},
		&models.WorkflowRun{},
		&models.Workflow{},
		&models.ApiKey{},
		&models.WorkflowVersion{},
		&models.WebhookTrigger{},
		&models.ScheduledTrigger{},
		&models.WorkflowChat{},
		&models.ChatSession{},
		&models.IntegrationConnection{},
		&models.DataStore{},
		&models.DataKV{},
		&models.DataRecord{},
	); err != nil {
		return err
	}

	if err := backfillOrganizations(c.DB); err != nil {
		return err
	}

	if backfillPublished {
		res := c.DB.Exec(`UPDATE workflows SET published = true WHERE id::text IN (
			SELECT workflow_id FROM scheduled_triggers WHERE enabled = true)`)
		if res.Error != nil {
			return res.Error
		}
		slog.Info("published backfill: kept already-scheduled workflows live", "count", res.RowsAffected)
	}
	return nil
}
