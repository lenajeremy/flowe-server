package main

import (
	"context"
	"log"
	"log/slog"

	"workflow-ai/server/config"
	"workflow-ai/server/internal/api"
	"workflow-ai/server/internal/api/handlers"
	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
	rdb "workflow-ai/server/internal/database/redis"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/telemetry"

	"github.com/redis/go-redis/extra/redisotel/v9"
	gormtracing "gorm.io/plugin/opentelemetry/tracing"
)

func main() {
	otelShutdown, otelLogHandler := telemetry.Setup(context.Background())
	defer func() { _ = otelShutdown(context.Background()) }()

	config.SetupLogger(otelLogHandler)
	slog.Info("starting workflow-ai server")

	dbClient, err := database.NewDBClient()
	if err != nil {
		log.Fatal("failed to connect to database: ", err)
	}
	// WithoutQueryVariables: statements land in spans, bound values (tokens,
	// emails, encrypted secrets) never do.
	if err := dbClient.DB.Use(gormtracing.NewPlugin(
		gormtracing.WithoutMetrics(),
		gormtracing.WithoutQueryVariables(),
	)); err != nil {
		slog.Warn("failed to enable gorm tracing", "error", err)
	}

	database.InstrumentQueries(dbClient.DB)

	conn, err := dbClient.DB.DB()
	if err != nil {
		log.Fatal("failed to get db connection: ", err)
	}
	telemetry.ObserveDBPool(conn)
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
	if err := redisotel.InstrumentTracing(redisClient); err != nil {
		slog.Warn("failed to enable redis tracing", "error", err)
	}
	if err := redisotel.InstrumentMetrics(redisClient); err != nil {
		slog.Warn("failed to enable redis metrics", "error", err)
	}

	// Integration nodes fall back to the workflow owner's stored OAuth
	// connection when the node config carries no manual token. FreshAccessToken
	// transparently refreshes expiring tokens (gmail, gitlab).
	executor.IntegrationCredsLookup = func(userID, provider string) (string, string) {
		if userID == "" {
			return "", ""
		}
		return handlers.FreshAccessToken(dbClient.DB, userID, provider)
	}
	executor.IntegrationUserTokenLookup = func(userID, provider string) string {
		if userID == "" {
			return ""
		}
		return handlers.UserGrantToken(dbClient.DB, userID, provider)
	}

	const port = 8080
	api.InitServer(port, dbClient, redisClient)
}
