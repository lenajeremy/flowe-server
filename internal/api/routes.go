package api

import (
	"net/http"

	"workflow-ai/server/internal/api/handlers"
	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

func InitServer(port int, db *database.DBClient, rdb *redis.Client) {
	s := NewServer(port, db, rdb)
	r := s.routerEngine

	// otelgin emits the server span + http.server.request.duration metric per
	// request; /health is filtered so uptime probes don't drown the data.
	// AccessLog and Recovery run inside the span so their log lines carry
	// trace ids (they replace gin.Logger/gin.Recovery, which only ever wrote
	// to stdout and were invisible to Loki).
	r.Use(otelgin.Middleware("fernary-server", otelgin.WithGinFilter(func(c *gin.Context) bool {
		return c.FullPath() != "/health"
	})))
	r.Use(telemetry.AccessLog())
	r.Use(telemetry.Recovery())
	r.Use(telemetry.GinActiveRequests())
	r.Use(BodyLimit(10 << 20)) // 10 MiB request-body cap
	r.Use(CorsMiddleware())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	wh := handlers.NewWorkflowHandler(db, rdb)

	// Auth — public by nature (they establish the session)
	authGroup := r.Group("/api/auth")
	{
		authGroup.POST("/email/start", wh.AuthEmailStart)
		authGroup.POST("/email/verify", wh.AuthEmailVerify)
		authGroup.GET("/google/connect", wh.AuthGoogleConnect)
		authGroup.GET("/google/callback", wh.AuthGoogleCallback)
		authGroup.GET("/me", wh.AuthMe)
		authGroup.POST("/logout", wh.AuthLogout)
	}

	// Public endpoints: reachable without a session.
	// - webhooks + trigger authenticate by token / API key
	// - runs + approve/reject are capability URLs (unguessable UUIDv4 run IDs)
	//   because approval emails link non-users straight to /run/<id>
	// - the integrations OAuth callback arrives via provider redirect with no
	//   cookie guarantee; its CSRF state (bound to the initiating user) is the guard
	public := r.Group("/api")
	{
		public.GET("/runs/:id", wh.GetRun)
		public.GET("/runs/:id/stream", wh.StreamRun)
		public.POST("/runs/:runId/node/:nodeId/approve", wh.ApproveRun)
		public.POST("/runs/:runId/node/:nodeId/reject", wh.RejectRun)

		public.POST("/trigger/:workflowId", wh.TriggerWorkflow)

		public.GET("/webhooks/:token", wh.WebhookInfo)
		public.POST("/webhooks/:token", wh.ReceiveWebhook)

		public.GET("/integrations/:provider/callback", wh.CallbackIntegration)

		// The pricing page is public, and it renders from the same limits table the
		// server enforces so it cannot advertise something a handler will refuse.
		public.GET("/billing/plans", wh.PublicPlans)
		// The invite landing page must be able to say who invited you and to which
		// org BEFORE you sign in — otherwise the flow reads as "log in to discover
		// what this link does". Returns only the org name, role and invited address.
		public.GET("/org/invites/info", wh.InviteInfo)
		// Authenticated by Stripe's signature over the raw body, not by a session.
		public.POST("/billing/stripe/webhook", wh.StripeWebhook)
	}

	// Everything else requires a session.
	api := r.Group("/api", auth.RequireAuth(rdb))
	{
		api.POST("/run", wh.Run)

		// Organization members and invitations. Seats are the Team billing unit, so
		// these are billing endpoints in all but name.
		api.GET("/org/members", wh.ListMembers)
		api.DELETE("/org/members/:userId", wh.RemoveMember)
		api.POST("/org/invites", wh.InviteMember)
		api.DELETE("/org/invites/:id", wh.RevokeInvite)
		api.POST("/org/invites/accept", wh.AcceptInvite)

		// Billing
		api.GET("/billing", wh.GetBilling)
		api.POST("/billing/checkout", wh.StartCheckout)
		api.POST("/billing/portal", wh.OpenPortal)

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

		// API keys
		api.GET("/apikeys", wh.ListApiKeys)
		api.POST("/apikeys", wh.CreateApiKey)
		api.DELETE("/apikeys/:id", wh.DeleteApiKey)

		// Webhook trigger management
		api.GET("/workflows/:id/webhook", wh.GetWebhook)
		api.DELETE("/workflows/:id/webhook", wh.DeleteWebhook)

		// Publishing (gates scheduled runs only)
		api.POST("/workflows/:id/publish", wh.SetPublished(true))
		api.POST("/workflows/:id/unpublish", wh.SetPublished(false))

		// Scheduled triggers
		api.GET("/workflows/:id/schedule", wh.GetSchedule)
		api.POST("/workflows/:id/schedule", wh.SetSchedule)
		api.DELETE("/workflows/:id/schedule", wh.DeleteSchedule)

		// Persistence (Data stores)
		api.GET("/data-stores", wh.ListDataStores)
		api.GET("/data-stores/events", wh.DataStoreEvents)
		api.POST("/data-stores", wh.CreateDataStore)
		api.GET("/data-stores/:id", wh.GetDataStore)
		api.PATCH("/data-stores/:id", wh.UpdateDataStore)
		api.DELETE("/data-stores/:id", wh.DeleteDataStore)
		api.GET("/data-stores/:id/entries", wh.ListDataEntries)
		api.PUT("/data-stores/:id/entries", wh.PutDataEntry)
		api.DELETE("/data-stores/:id/entries/:entry", wh.DeleteDataEntry)
		api.POST("/data-stores/:id/clear", wh.ClearDataStore)

		// AI workflow generation
		api.POST("/ai/generate-workflow", wh.AIGenerate)
		api.POST("/ai/data-store-proposals/:id/resolve", wh.ResolveDataStoreProposal)
		api.GET("/ai/models", wh.AIModels)

		// AI chat history per workflow
		api.GET("/workflows/:id/chat", wh.GetWorkflowChat)
		api.PUT("/workflows/:id/chat", wh.SaveWorkflowChat)

		// Chat-with-workflow (agent mode)
		api.POST("/workflows/:id/chat-sessions", wh.CreateChatSession)
		api.GET("/workflows/:id/chat-sessions", wh.ListChatSessions)
		api.GET("/chat-sessions/:id", wh.GetChatSession)
		api.DELETE("/chat-sessions/:id", wh.DeleteChatSession)
		api.POST("/chat-sessions/:id/message", wh.AgentChatTurn)

		// Workflow versions
		api.GET("/workflows/:id/versions", wh.ListVersions)
		api.POST("/workflows/:id/versions", wh.SaveVersion)
		api.POST("/workflows/:id/versions/:versionId/restore", wh.RestoreVersion)

		// Integration OAuth connections (Notion, Linear)
		api.GET("/integrations", wh.ListIntegrations)
		api.GET("/integrations/:provider/connect", wh.ConnectIntegration)
		api.GET("/integrations/:provider/resources", wh.IntegrationResources)
		api.PUT("/integrations/:provider/key", wh.SetIntegrationKey)
		api.DELETE("/integrations/:provider", wh.DisconnectIntegration)
	}

	wh.StartScheduler()
	s.Start(port)
}
