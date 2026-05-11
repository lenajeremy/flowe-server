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
	return c.DB.AutoMigrate(
		&models.WorkflowRun{},
		&models.Workflow{},
		&models.ApiKey{},
		&models.WorkflowVersion{},
		&models.WebhookTrigger{},
		&models.ScheduledTrigger{},
		&models.WorkflowChat{},
	)
}
