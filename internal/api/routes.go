package api

import (
	"net/http"

	"workflow-ai/server/internal/api/handlers"
	"workflow-ai/server/internal/database"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

func InitServer(port int, db *database.DBClient, rdb *redis.Client) {
	s := NewServer(port, db, rdb)
	r := s.routerEngine

	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(CorsMiddleware())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	wh := handlers.NewWorkflowHandler(db, rdb)
	api := r.Group("/api")
	{
		api.POST("/run", wh.Run)

		// Workflow persistence
		api.POST("/workflows", wh.Create)
		api.GET("/workflows", wh.List)
		api.GET("/workflows/:id", wh.GetOne)
		api.PUT("/workflows/:id", wh.Update)
		api.DELETE("/workflows/:id", wh.Delete)

		// Runs
		api.GET("/workflows/:id/events", wh.WorkflowEvents)
		api.GET("/workflows/:id/runs/active", wh.GetActiveRun)
		api.GET("/workflows/:id/runs", wh.ListRuns)
		api.GET("/runs/:id", wh.GetRun)
		api.GET("/runs/:id/stream", wh.StreamRun)
		api.POST("/runs/:runId/node/:nodeId/approve", wh.ApproveRun)
		api.POST("/runs/:runId/node/:nodeId/reject", wh.RejectRun)

		// Programmatic trigger
		api.POST("/trigger/:workflowId", wh.TriggerWorkflow)

		// API keys
		api.GET("/apikeys", wh.ListApiKeys)
		api.POST("/apikeys", wh.CreateApiKey)
		api.DELETE("/apikeys/:id", wh.DeleteApiKey)

		// Webhook triggers
		api.GET("/workflows/:id/webhook", wh.GetWebhook)
		api.DELETE("/workflows/:id/webhook", wh.DeleteWebhook)
		api.GET("/webhooks/:token", wh.WebhookInfo)
		api.POST("/webhooks/:token", wh.ReceiveWebhook)

		// Scheduled triggers
		api.GET("/workflows/:id/schedule", wh.GetSchedule)
		api.POST("/workflows/:id/schedule", wh.SetSchedule)
		api.DELETE("/workflows/:id/schedule", wh.DeleteSchedule)

		// Workflow versions
		api.GET("/workflows/:id/versions", wh.ListVersions)
		api.POST("/workflows/:id/versions", wh.SaveVersion)
		api.POST("/workflows/:id/versions/:versionId/restore", wh.RestoreVersion)
	}

	wh.StartScheduler()
	s.Start(port)
}
