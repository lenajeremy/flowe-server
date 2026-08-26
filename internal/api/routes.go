package api

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"workflow-ai/server/internal/api/handlers"
	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/codingagent"
	codexruntime "workflow-ai/server/internal/codingagent/codex"
	daytonaprovider "workflow-ai/server/internal/codingagent/daytona"
	"workflow-ai/server/internal/cryptobox"
	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/executor"
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
	codingAgents := configureCodingAgents(db)
	wh.SetCodingAgentService(codingAgents)

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

		// Integration triggers. Two shapes because providers disagree about who
		// owns the URL: a hook registered per subscription carries its trigger id,
		// while an app-level provider (Slack) posts every workspace's events to
		// the bare path and is routed from the payload. Authenticated by the
		// provider's signature over the raw body — see hooks.go.
		public.POST("/hooks/:provider", wh.ReceiveProviderHook)
		public.POST("/hooks/:provider/:triggerID", wh.ReceiveProviderHook)

		public.GET("/integrations/:provider/callback", wh.CallbackIntegration)
		// GitHub sends installation completion here when the App uses a Setup
		// URL. One-time state authenticates it; the handler then chains into the
		// ordinary user OAuth callback above.
		public.GET("/integrations/github/setup/callback", wh.GitHubSetupCallback)

		// The pricing page is public, and it renders from the same limits table the
		// server enforces so it cannot advertise something a handler will refuse.
		public.GET("/billing/plans", wh.PublicPlans)
		// The invite landing page must be able to say who invited you and to which
		// org BEFORE you sign in — otherwise the flow reads as "log in to discover
		// what this link does". Returns only the org name, role and invited address.
		public.GET("/org/invites/info", wh.InviteInfo)
		// Authenticated by Stripe's signature over the raw body, not by a session.
		public.POST("/billing/stripe/webhook", wh.StripeWebhook)

		// Signed Slack Events API and Block Kit interaction callbacks for hosted agents.
		public.POST("/agent-hosts/slack/events", wh.ReceiveSlackAgentEvent)
		public.POST("/agent-hosts/slack/interactions", wh.ReceiveSlackAgentInteraction)
	}

	// Everything else requires a session.
	api := r.Group("/api", auth.RequireAuth(rdb))
	{
		// In-app annotated screenshots sent to the private product-feedback inbox.
		api.POST("/feedback", wh.SubmitFeedback)

		api.POST("/run", wh.Run)
		api.POST("/run/node", wh.RunNode)

		// Organization members and invitations. Seats are the Team billing unit, so
		// these are billing endpoints in all but name.
		api.GET("/org/members", wh.ListMembers)
		api.DELETE("/org/members/:userId", wh.RemoveMember)
		api.POST("/org/invites", wh.InviteMember)
		api.DELETE("/org/invites/:id", wh.RevokeInvite)
		api.POST("/org/invites/accept", wh.AcceptInvite)
		api.POST("/org/seats", wh.SetSeats)
		api.POST("/org/members/:userId/limit", wh.SetMemberLimit)

		// Usage: the itemised ledger, and the CSV someone forwards to whoever asks
		// about the bill.
		api.GET("/usage", wh.GetUsage)
		api.GET("/usage/export.csv", wh.ExportUsage)

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

		// Integration triggers (provider events)
		api.GET("/trigger-catalog", wh.TriggerCatalog)
		api.GET("/workflows/:id/triggers", wh.ListTriggers)
		api.POST("/workflows/:id/triggers", wh.CreateTrigger)
		api.DELETE("/triggers/:id", wh.DeleteTrigger)

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

		// Deploy workflow agents into team chat hosts.
		registerAgentHostRoutes(api, wh)
		api.POST("/workflows/:id/agent-deployments/analyze", wh.AnalyzeAgentDeployment)
		api.GET("/workflows/:id/agent-deployments/capabilities", wh.AgentDeploymentCapabilities)
		api.POST("/workflows/:id/agent-deployments", wh.CreateAgentDeployment)
		api.GET("/workflows/:id/agent-deployments", wh.ListAgentDeployments)
		api.GET("/agent-deployments", wh.ListAllAgentDeployments)
		api.GET("/agent-deployments/:id", wh.GetAgentDeployment)
		api.PATCH("/agent-deployments/:id", wh.PatchAgentDeployment)
		api.DELETE("/agent-deployments/:id", wh.DeleteAgentDeployment)

		// Durable coding agents running in isolated Daytona environments.
		api.GET("/coding-agents/runtimes", wh.CodingAgentRuntimes)
		api.POST("/coding-agents/codex/connect", wh.StartCodexConnection)
		api.GET("/coding-agents/auth-attempts/:id", wh.GetCodingAgentAuthAttempt)
		api.DELETE("/coding-agents/auth-attempts/:id", wh.CancelCodingAgentAuthAttempt)
		api.DELETE("/coding-agents/credentials/:runtime", wh.DisconnectCodingAgent)
		api.GET("/coding-agent-jobs", wh.ListCodingAgentJobs)
		api.GET("/coding-agent-jobs/:id", wh.GetCodingAgentJob)
		api.GET("/coding-agent-jobs/:id/events", wh.ListCodingAgentJobEvents)
		api.POST("/coding-agent-jobs/:id/cancel", wh.CancelCodingAgentJob)
		api.GET("/coding-agent-environments", wh.ListCodingAgentEnvironments)
		api.DELETE("/coding-agent-environments/:id", wh.ResetCodingAgentEnvironment)

		// Workflow versions
		api.GET("/workflows/:id/versions", wh.ListVersions)
		api.POST("/workflows/:id/versions", wh.SaveVersion)
		api.POST("/workflows/:id/versions/:versionId/restore", wh.RestoreVersion)

		// Integration OAuth connections (Notion, Linear)
		api.GET("/integrations", wh.ListIntegrations)
		api.GET("/integrations/:provider/connect", wh.ConnectIntegration)
		api.GET("/integrations/:provider/resources", wh.IntegrationResources)
		api.GET("/integrations/github/setup", wh.GitHubIntegrationSetup)
		api.PUT("/integrations/:provider/key", wh.SetIntegrationKey)
		api.DELETE("/integrations/:provider", wh.DisconnectIntegration)
	}

	wh.StartHostedAgentWorker()
	wh.StartScheduler()
	s.Start(port)
}

func configureCodingAgents(db *database.DBClient) *codingagent.Service {
	if !cryptobox.Configured() {
		slog.Warn("coding agents disabled: TOKEN_ENC_KEY must be a base64-encoded 32-byte key")
		executor.CodingAgentRun = nil
		return nil
	}
	provider, err := daytonaprovider.NewProvider()
	if err != nil {
		slog.Warn("coding agents disabled: Daytona is not configured", "error", err)
		executor.CodingAgentRun = nil
		return nil
	}
	runtime, err := codexruntime.NewRuntime(os.Getenv("CODEX_CLI_VERSION"))
	if err != nil {
		slog.Error("coding agents disabled: Codex runtime is invalid", "error", err)
		executor.CodingAgentRun = nil
		return nil
	}
	service := codingagent.NewService(db.DB, provider, []codingagent.Runtime{runtime}, codingagent.ServiceConfig{
		WorkerCount: 2, PollInterval: time.Second, StaleAfter: 2 * time.Minute,
		SandboxSnapshot: os.Getenv("DAYTONA_CODING_AGENT_SNAPSHOT"),
		CodexCLIVersion: os.Getenv("CODEX_CLI_VERSION"),
		RepositoryToken: func(_ context.Context, orgID, userID, provider string) (string, error) {
			token, _ := handlers.FreshAccessTokenForOrg(db.DB, orgID, userID, provider)
			return token, nil
		},
	})
	if err := service.Start(context.Background()); err != nil {
		slog.Error("coding agents disabled: worker could not start", "error", err)
		executor.CodingAgentRun = nil
		return nil
	}
	executor.CodingAgentRun = func(ctx context.Context, req codingagent.SubmitRequest, emit func(codingagent.StreamEvent)) (string, string, []byte, string, string, error) {
		job, err := service.SubmitAndWait(ctx, req, emit)
		if err != nil {
			if job != nil {
				return job.ID.String(), string(job.Status), job.Result, job.Summary, job.LastError, err
			}
			return "", "", nil, "", "", err
		}
		return job.ID.String(), string(job.Status), job.Result, job.Summary, job.LastError, nil
	}
	slog.Info("coding agent workers started", "provider", provider.Name(), "runtime", runtime.Name())
	return service
}

// registerAgentHostRoutes keeps the provider-specific OAuth entry point static.
// Gin requires sibling wildcard segments to use the same name, so a dynamic
// /:provider/connect route cannot coexist with the installation /:id routes.
func registerAgentHostRoutes(api *gin.RouterGroup, wh *handlers.WorkflowHandler) {
	api.GET("/agent-hosts", wh.ListAgentHosts)
	api.GET("/agent-hosts/slack/connect", wh.ConnectAgentHost)
	api.GET("/agent-hosts/:id/channels", wh.ListAgentHostChannels)
	api.POST("/agent-deployments/:id/slack-channels/:channelId/join", wh.JoinAgentDeploymentSlackChannel)
	api.DELETE("/agent-hosts/:id", wh.DeleteAgentHost)
}
