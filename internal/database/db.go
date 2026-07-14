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
	return c.DB.AutoMigrate(
		&models.User{},
		&models.LoginCode{},
		&models.WorkflowRun{},
		&models.Workflow{},
		&models.ApiKey{},
		&models.WorkflowVersion{},
		&models.WebhookTrigger{},
		&models.ScheduledTrigger{},
		&models.WorkflowChat{},
		&models.ChatSession{},
		&models.IntegrationConnection{},
	)
}
