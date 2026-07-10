package main

import (
	"log"
	"log/slog"

	"workflow-ai/server/config"
	"workflow-ai/server/internal/api"
	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
	rdb "workflow-ai/server/internal/database/redis"
	"workflow-ai/server/internal/executor"
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

	// Notion/Linear nodes fall back to stored OAuth connections when the
	// node config carries no manual token. Connections are keyed per user;
	// until multi-user auth lands every workflow belongs to DefaultUserID —
	// afterwards, resolve the run's owner here instead.
	executor.IntegrationTokenLookup = func(provider string) string {
		var conn models.IntegrationConnection
		if err := dbClient.DB.Where("user_id = ? AND provider = ?", models.DefaultUserID, provider).
			First(&conn).Error; err != nil {
			return ""
		}
		return conn.AccessToken
	}

	const port = 8080
	api.InitServer(port, dbClient, redisClient)
}
