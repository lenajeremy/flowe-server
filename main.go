package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"workflow-ai/server/config"
	"workflow-ai/server/internal/api"
	"workflow-ai/server/internal/database"
	rdb "workflow-ai/server/internal/database/redis"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/n8n"
)

func main() {
	config.SetupLogger()
	slog.Info("starting workflow-ai server")

	dbClient, err := database.NewDBClient()
	if err != nil {
		log.Fatal("failed to connect to database: ", err)
	}

	conn, err := dbClient.DB.DB()
	if err != nil {
		log.Fatal("failed to get db connection: ", err)
	}
	defer func() {
		slog.Info("closing database connection")
		_ = conn.Close()
	}()

	if err := dbClient.Setup(); err != nil {
		log.Fatal("failed to run migrations: ", err)
	}

	// Mark any runs that were left in "running" state from a previous server
	// session as errored — they can never be resumed after a restart.
	if result := dbClient.DB.
		Model(&models.WorkflowRun{}).
		Where("status = ?", models.RunStatusRunning).
		Updates(map[string]any{
			"status":        models.RunStatusError,
			"error_message": "Server restarted — run was interrupted",
		}); result.Error != nil {
		slog.Warn("failed to clean up orphaned runs", "error", result.Error)
	} else if result.RowsAffected > 0 {
		slog.Info("marked orphaned runs as error", "count", result.RowsAffected)
	}

	redisClient := rdb.New()

	// Initialize n8n client and seed workflows in background
	n8nURL := os.Getenv("N8N_URL")
	if n8nURL == "" {
		n8nURL = "http://localhost:5678"
	}
	n8nClient := n8n.NewClient(n8nURL)
	executor.N8NClient = n8nClient

	go func() {
		const maxAttempts = 20
		for i := range maxAttempts {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := n8nClient.SeedWorkflows(ctx)
			cancel()
			if err == nil {
				slog.Info("n8n workflows seeded successfully")
				return
			}
			slog.Warn("n8n seed attempt failed, retrying",
				"attempt", i+1, "max", maxAttempts, "err", err)
			time.Sleep(3 * time.Second)
		}
		slog.Error("failed to seed n8n workflows after all attempts")
	}()

	const port = 8080
	api.InitServer(port, dbClient, redisClient)
}
