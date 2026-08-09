package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	slackAgentProvider       = "slack"
	slackSignatureMaxAge     = 5 * time.Minute
	hostedAgentThreadTextCap = 16 << 10
	hostedAgentMessageCap    = 2000
	hostedAgentThreadMsgCap  = 40
)

var (
	slackAgentHTTPClient = &http.Client{Timeout: 20 * time.Second}
	slackMentionPattern  = regexp.MustCompile(`<@[A-Z0-9]+>\s*`)
	hostedWorkerOnce     sync.Once
	errHostedThreadStale = errors.New("Slack thread belongs to another agent deployment")
)

type slackAgentEventEnvelope struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge,omitempty"`
	TeamID    string `json:"team_id"`
	EventID   string `json:"event_id"`
	EventTime int64  `json:"event_time"`
	Event     struct {
		Type     string `json:"type"`
		User     string `json:"user"`
		Text     string `json:"text"`
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
		Channel  string `json:"channel"`
		BotID    string `json:"bot_id"`
		Subtype  string `json:"subtype"`
	} `json:"event"`
}

type slackAgentInteraction struct {
	Type      string `json:"type"`
	TriggerID string `json:"trigger_id"`
	ActionTS  string `json:"action_ts"`
	Team      struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container struct {
		ThreadTS  string `json:"thread_ts"`
		MessageTS string `json:"message_ts"`
	} `json:"container"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
		ActionTS string `json:"action_ts"`
	} `json:"actions"`
}

type hostedSlackEventPayload struct {
	WorkspaceID string `json:"workspaceId"`
	ChannelID   string `json:"channelId"`
	ThreadID    string `json:"threadId"`
	MessageID   string `json:"messageId"`
	RequesterID string `json:"requesterId"`
	Text        string `json:"text"`
}

type hostedSlackInteractionPayload struct {
	WorkspaceID string `json:"workspaceId"`
	ChannelID   string `json:"channelId"`
	ThreadID    string `json:"threadId"`
	RequesterID string `json:"requesterId"`
	ApprovalID  string `json:"approvalId"`
	Action      string `json:"action"`
}

func verifySlackAgentSignature(header http.Header, body []byte, now time.Time) error {
	secret := os.Getenv("SLACK_SIGNING_SECRET")
	if secret == "" {
		return errors.New("Slack signing secret is not configured")
	}
	timestampRaw := header.Get("X-Slack-Request-Timestamp")
	timestamp, err := strconv.ParseInt(timestampRaw, 10, 64)
	if err != nil {
		return errors.New("invalid Slack request timestamp")
	}
	requestTime := time.Unix(timestamp, 0)
	if now.Sub(requestTime) > slackSignatureMaxAge || requestTime.Sub(now) > slackSignatureMaxAge {
		return errors.New("stale Slack request")
	}
	provided := header.Get("X-Slack-Signature")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + timestampRaw + ":"))
	_, _ = mac.Write(body)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		return errors.New("invalid Slack signature")
	}
	return nil
}

// ReceiveSlackAgentEvent verifies, resolves, deduplicates, and durably queues a
// mention before acknowledging Slack. No model or tool work occurs inline.
func (h *WorkflowHandler) ReceiveSlackAgentEvent(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxHookBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "could not read body"})
		return
	}
	if err := verifySlackAgentSignature(c.Request.Header, body, time.Now()); err != nil {
		slog.WarnContext(c.Request.Context(), "slack agent signature rejected", "reason", err.Error())
		c.JSON(http.StatusUnauthorized, gin.H{"error": "signature verification failed"})
		return
	}
	var envelope slackAgentEventEnvelope
	if json.Unmarshal(body, &envelope) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unreadable Slack event"})
		return
	}
	if envelope.Type == "url_verification" {
		c.JSON(http.StatusOK, gin.H{"challenge": envelope.Challenge})
		return
	}
	if envelope.Type != "event_callback" || envelope.Event.Type != "app_mention" ||
		envelope.Event.User == "" || envelope.Event.BotID != "" || envelope.Event.Subtype != "" {
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
		return
	}
	if envelope.TeamID == "" || envelope.Event.Channel == "" || envelope.Event.TS == "" || envelope.EventID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Slack mention is missing routing fields"})
		return
	}
	if !h.activeSlackAgentTargetExists(envelope.TeamID, envelope.Event.Channel) {
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
		return
	}
	threadID := envelope.Event.ThreadTS
	if threadID == "" {
		threadID = envelope.Event.TS
	}
	payload, _ := json.Marshal(hostedSlackEventPayload{
		WorkspaceID: envelope.TeamID, ChannelID: envelope.Event.Channel,
		ThreadID: threadID, MessageID: envelope.Event.TS, RequesterID: envelope.Event.User,
		Text: truncate(envelope.Event.Text, hostedAgentMessageCap),
	})
	delivery := models.HostedAgentDelivery{
		Provider: slackAgentProvider, ExternalDeliveryID: envelope.EventID,
		ExternalWorkspaceID: envelope.TeamID, EventKind: "mention", Payload: models.JSONB(payload),
		Status: models.HostedAgentDeliveryPending, AvailableAt: time.Now().UTC(),
	}
	result := h.db.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&delivery)
	if result.Error != nil {
		slog.ErrorContext(c.Request.Context(), "could not queue Slack agent event", "error", result.Error)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not queue event"})
		return
	}
	status := "queued"
	if result.RowsAffected == 0 {
		status = "duplicate"
	}
	c.JSON(http.StatusOK, gin.H{"status": status})
}

// ReceiveSlackAgentInteraction handles approval/rejection button clicks through
// the same signed, deduplicated queue as mentions.
func (h *WorkflowHandler) ReceiveSlackAgentInteraction(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxHookBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "could not read body"})
		return
	}
	if err := verifySlackAgentSignature(c.Request.Header, body, time.Now()); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "signature verification failed"})
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unreadable Slack interaction"})
		return
	}
	var interaction slackAgentInteraction
	if json.Unmarshal([]byte(form.Get("payload")), &interaction) != nil || len(interaction.Actions) != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unreadable Slack interaction"})
		return
	}
	action := interaction.Actions[0]
	decision := ""
	switch action.ActionID {
	case "fernary_agent_approve":
		decision = "approve"
	case "fernary_agent_reject":
		decision = "reject"
	default:
		c.Status(http.StatusOK)
		return
	}
	if interaction.Team.ID == "" || interaction.User.ID == "" || action.Value == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Slack interaction is missing identity fields"})
		return
	}
	threadID := interaction.Container.ThreadTS
	if threadID == "" {
		threadID = interaction.Container.MessageTS
	}
	payload, _ := json.Marshal(hostedSlackInteractionPayload{
		WorkspaceID: interaction.Team.ID, ChannelID: interaction.Channel.ID, ThreadID: threadID,
		RequesterID: interaction.User.ID, ApprovalID: action.Value, Action: decision,
	})
	externalID := interaction.TriggerID + ":" + action.ActionTS
	if action.ActionTS == "" {
		externalID = interaction.TriggerID + ":" + interaction.ActionTS
	}
	delivery := models.HostedAgentDelivery{
		Provider: slackAgentProvider, ExternalDeliveryID: "interaction:" + externalID,
		ExternalWorkspaceID: interaction.Team.ID, EventKind: "interaction", Payload: models.JSONB(payload),
		Status: models.HostedAgentDeliveryPending, AvailableAt: time.Now().UTC(),
	}
	if err := h.db.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&delivery).Error; err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not queue interaction"})
		return
	}
	c.Status(http.StatusOK)
}

func (h *WorkflowHandler) activeSlackAgentTargetExists(workspaceID, channelID string) bool {
	var count int64
	h.db.DB.Table("agent_deployment_targets AS target").
		Joins("JOIN agent_deployments AS deployment ON deployment.id = target.deployment_id AND deployment.deleted_at IS NULL").
		Joins("JOIN agent_host_installations AS host ON host.id = deployment.host_installation_id AND host.deleted_at IS NULL").
		Where("target.deleted_at IS NULL AND target.provider = ? AND target.external_workspace_id = ? AND target.external_channel_id = ? AND target.enabled = true",
			slackAgentProvider, workspaceID, channelID).
		Where("deployment.status = ? AND host.status = ?", models.AgentDeploymentActive, models.AgentHostActive).
		Count(&count)
	return count > 0
}

func (h *WorkflowHandler) StartHostedAgentWorker() {
	hostedWorkerOnce.Do(func() { go h.hostedAgentWorkerLoop() })
}

func (h *WorkflowHandler) hostedAgentWorkerLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		h.processHostedAgentQueue()
		<-ticker.C
	}
}

func (h *WorkflowHandler) processHostedAgentQueue() {
	for processed := 0; processed < 20; processed++ {
		delivery, err := h.claimHostedAgentDelivery()
		if err != nil {
			slog.Error("hosted agent delivery claim failed", "error", err)
			return
		}
		if delivery == nil {
			return
		}
		err = h.processHostedAgentDelivery(context.Background(), delivery)
		h.finishHostedAgentDelivery(delivery, err)
	}
}

func (h *WorkflowHandler) claimHostedAgentDelivery() (*models.HostedAgentDelivery, error) {
	var delivery models.HostedAgentDelivery
	now := time.Now().UTC()
	// A turn can span several provider/tool rounds and a human-approved node can
	// itself be slow. Reclaim only after a wide crash window; thread advisory
	// locks provide the stronger concurrency guarantee while a worker is alive.
	stale := now.Add(-30 * time.Minute)
	err := h.db.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("(status = ? AND available_at <= ?) OR (status = ? AND claimed_at < ?)",
				models.HostedAgentDeliveryPending, now, models.HostedAgentDeliveryProcessing, stale).
			Order("available_at ASC").First(&delivery).Error
		if err != nil {
			return err
		}
		return tx.Model(&delivery).Updates(map[string]any{
			"status": models.HostedAgentDeliveryProcessing, "claimed_at": now,
			"attempt_count": gorm.Expr("attempt_count + 1"),
		}).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	delivery.AttemptCount++
	return &delivery, nil
}

func (h *WorkflowHandler) finishHostedAgentDelivery(delivery *models.HostedAgentDelivery, processErr error) {
	now := time.Now().UTC()
	if processErr == nil {
		h.db.DB.Model(delivery).Updates(map[string]any{
			"status": models.HostedAgentDeliveryCompleted, "completed_at": now, "last_error": "",
		})
		return
	}
	slog.Warn("hosted agent delivery failed", "delivery_id", delivery.ID, "attempt", delivery.AttemptCount, "error", processErr)
	if delivery.AttemptCount >= 5 {
		h.db.DB.Model(delivery).Updates(map[string]any{
			"status": models.HostedAgentDeliveryFailed, "completed_at": now,
			"last_error": truncate(processErr.Error(), 2000),
		})
		return
	}
	backoff := time.Duration(1<<min(delivery.AttemptCount, 6)) * time.Second
	h.db.DB.Model(delivery).Updates(map[string]any{
		"status": models.HostedAgentDeliveryPending, "available_at": now.Add(backoff),
		"last_error": truncate(processErr.Error(), 2000),
	})
}

func (h *WorkflowHandler) processHostedAgentDelivery(ctx context.Context, delivery *models.HostedAgentDelivery) error {
	switch delivery.EventKind {
	case "mention":
		var payload hostedSlackEventPayload
		if err := json.Unmarshal(delivery.Payload, &payload); err != nil {
			return err
		}
		return h.withHostedThreadLock(ctx, payload.WorkspaceID+":"+payload.ChannelID+":"+payload.ThreadID, func() error {
			return h.processSlackAgentMention(ctx, delivery, payload)
		})
	case "interaction":
		var payload hostedSlackInteractionPayload
		if err := json.Unmarshal(delivery.Payload, &payload); err != nil {
			return err
		}
		return h.withHostedThreadLock(ctx, payload.WorkspaceID+":"+payload.ChannelID+":"+payload.ThreadID, func() error {
			return h.processSlackAgentApproval(ctx, payload)
		})
	default:
		return fmt.Errorf("unknown hosted event kind %q", delivery.EventKind)
	}
}

// withHostedThreadLock serializes session reads/writes across server replicas.
// The PostgreSQL advisory lock is connection-scoped and is released even when a
// turn fails or the process loses its database connection.
func (h *WorkflowHandler) withHostedThreadLock(ctx context.Context, key string, run func() error) error {
	if key == "::" {
		return errors.New("hosted thread lock key is empty")
	}
	return h.db.DB.WithContext(ctx).Connection(func(connection *gorm.DB) error {
		if err := connection.Exec("SELECT pg_advisory_lock(hashtextextended(?, 0))", key).Error; err != nil {
			return err
		}
		defer connection.Exec("SELECT pg_advisory_unlock(hashtextextended(?, 0))", key)
		return run()
	})
}

type resolvedSlackDeployment struct {
	Deployment models.AgentDeployment
	Target     models.AgentDeploymentTarget
	Host       models.AgentHostInstallation
}

func (h *WorkflowHandler) resolveSlackDeployment(workspaceID, channelID string) (*resolvedSlackDeployment, error) {
	var target models.AgentDeploymentTarget
	if err := h.db.DB.Where("provider = ? AND external_workspace_id = ? AND external_channel_id = ? AND enabled = true",
		slackAgentProvider, workspaceID, channelID).First(&target).Error; err != nil {
		return nil, err
	}
	var deployment models.AgentDeployment
	if err := h.db.DB.Where("id = ? AND status = ?", target.DeploymentID, models.AgentDeploymentActive).First(&deployment).Error; err != nil {
		return nil, err
	}
	var deployerMembership int64
	h.db.DB.Model(&models.OrgMember{}).
		Where("organization_id = ? AND user_id = ?", deployment.OrganizationID, deployment.DeployedByUserID).
		Count(&deployerMembership)
	if deployerMembership == 0 {
		h.db.DB.Model(&deployment).Update("status", models.AgentDeploymentPaused)
		return nil, gorm.ErrRecordNotFound
	}
	var host models.AgentHostInstallation
	if err := h.db.DB.Where("id = ? AND status = ? AND external_workspace_id = ?",
		deployment.HostInstallationID, models.AgentHostActive, workspaceID).First(&host).Error; err != nil {
		return nil, err
	}
	if target.OrganizationID != deployment.OrganizationID || host.OrganizationID != deployment.OrganizationID {
		return nil, errors.New("hosted agent tenant boundary mismatch")
	}
	return &resolvedSlackDeployment{Deployment: deployment, Target: target, Host: host}, nil
}

func (h *WorkflowHandler) loadOrCreateHostedThread(resolved *resolvedSlackDeployment, payload hostedSlackEventPayload) (*models.HostedAgentThread, *models.ChatSession, error) {
	var thread models.HostedAgentThread
	err := h.db.DB.Where("provider = ? AND external_workspace_id = ? AND external_channel_id = ? AND external_thread_id = ?",
		slackAgentProvider, payload.WorkspaceID, payload.ChannelID, payload.ThreadID).First(&thread).Error
	if err == nil {
		if thread.DeploymentID != resolved.Deployment.ID.String() || thread.OrganizationID != resolved.Deployment.OrganizationID {
			return nil, nil, errHostedThreadStale
		}
		var session models.ChatSession
		if err := h.db.DB.First(&session, "id = ?", thread.ChatSessionID).Error; err != nil {
			return nil, nil, err
		}
		if session.OrganizationID != resolved.Deployment.OrganizationID ||
			session.WorkflowID != resolved.Deployment.WorkflowID ||
			session.UserID != resolved.Deployment.DeployedByUserID {
			return nil, nil, errors.New("hosted agent session boundary mismatch")
		}
		h.db.DB.Model(&thread).Update("latest_external_message_id", payload.MessageID)
		return &thread, &session, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, err
	}

	session := models.ChatSession{
		UserID: resolved.Deployment.DeployedByUserID, OrganizationID: resolved.Deployment.OrganizationID,
		WorkflowID: resolved.Deployment.WorkflowID, Title: "Slack · " + resolved.Deployment.Name,
		Messages: models.JSONB(`[]`), State: models.JSONB(`{}`),
	}
	err = h.db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		thread = models.HostedAgentThread{
			OrganizationID: resolved.Deployment.OrganizationID, DeploymentID: resolved.Deployment.ID.String(),
			ChatSessionID: session.ID.String(), Provider: slackAgentProvider,
			ExternalWorkspaceID: payload.WorkspaceID, ExternalChannelID: payload.ChannelID,
			ExternalThreadID: payload.ThreadID, LatestExternalMessageID: payload.MessageID,
		}
		return tx.Create(&thread).Error
	})
	if err != nil {
		return nil, nil, err
	}
	return &thread, &session, nil
}

func (h *WorkflowHandler) processSlackAgentMention(ctx context.Context, delivery *models.HostedAgentDelivery, payload hostedSlackEventPayload) error {
	resolved, err := h.resolveSlackDeployment(payload.WorkspaceID, payload.ChannelID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !auth.Allow(ctx, h.redis, "rl:hosted-agent-deployment:"+resolved.Deployment.ID.String(), 60, time.Minute) ||
		!auth.Allow(ctx, h.redis, "rl:hosted-agent-requester:"+resolved.Deployment.ID.String()+":"+payload.RequesterID, 12, time.Minute) {
		return slackAgentPostText(ctx, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID,
			"This agent is receiving too many requests right now. Please try again in a minute.", resolved.Deployment.Name)
	}
	thread, session, err := h.loadOrCreateHostedThread(resolved, payload)
	if errors.Is(err, errHostedThreadStale) {
		return slackAgentPostText(ctx, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID,
			"This thread belongs to an older agent deployment. Start a new top-level mention to use the current agent.", resolved.Deployment.Name)
	}
	if err != nil {
		return err
	}

	var existingApproval models.HostedAgentApproval
	if err := h.db.DB.Where("source_delivery_id = ? AND status = ?", delivery.ID.String(), models.HostedAgentApprovalPending).
		First(&existingApproval).Error; err == nil {
		return slackAgentPostApproval(ctx, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID, &existingApproval, resolved.Deployment.Name)
	}

	plan, err := h.bill.CheckBalance(resolved.Deployment.OrganizationID, resolved.Deployment.DeployedByUserID)
	if err != nil {
		message := "This organization's Fernary credits are exhausted, so the agent cannot run right now."
		if !errors.Is(err, billing.ErrOverCap) {
			message = "The agent could not check its Fernary credits. Please try again shortly."
		}
		if postErr := slackAgentPostText(ctx, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID, message, resolved.Deployment.Name); postErr != nil {
			return postErr
		}
		return nil
	}
	ctx = telemetry.WithSurface(ctx, telemetry.SurfaceAgent)
	ctx = telemetry.WithBilling(ctx, billing.BillingContextFor(resolved.Deployment.OrganizationID, resolved.Deployment.DeployedByUserID, plan))

	threadContext, err := slackAgentThreadContext(ctx, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID, payload.MessageID)
	if err != nil {
		slog.WarnContext(ctx, "could not load Slack thread context", "error", err)
	}
	requestText := slackAgentStripSelfMention(payload.Text, resolved.Host.BotUserID)
	if requestText == "" {
		requestText = "Help me use this agent."
	}
	message := requestText
	if threadContext != "" {
		message = "Slack thread context (text only, oldest to newest):\n" + threadContext +
			"\n\nCurrent request from Slack user " + payload.RequesterID + ":\n" + requestText
	}

	var ast executor.WorkflowAST
	ast.Name = resolved.Deployment.Name
	if json.Unmarshal(resolved.Deployment.SnapshotNodes, &ast.Nodes) != nil || json.Unmarshal(resolved.Deployment.SnapshotEdges, &ast.Edges) != nil {
		return errors.New("deployment snapshot is unreadable")
	}
	var policy AgentCapabilityPolicy
	if json.Unmarshal(resolved.Deployment.CapabilityPolicy, &policy) != nil {
		return errors.New("deployment policy is unreadable")
	}
	var analysis map[string]any
	_ = json.Unmarshal(resolved.Deployment.PermissionAnalysis, &analysis)
	goal, _ := analysis["goal"].(string)
	runtimeModel, err := prepareAgentRuntimeModel(resolved.Deployment.ModelID)
	if err != nil {
		_ = slackAgentPostText(ctx, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID,
			"This agent's model is not configured on Fernary. Ask the deployment owner to update it.", resolved.Deployment.Name)
		return nil
	}

	var approvals []*AgentApprovalActivity
	var streamedText strings.Builder
	var streamedErrors []string
	sink := func(event AgentTurnEvent) {
		switch event.Type {
		case AgentTurnText:
			streamedText.WriteString(event.Text)
		case AgentTurnError:
			streamedErrors = append(streamedErrors, event.Text)
		case AgentTurnApproval:
			if event.Approval != nil {
				approvals = append(approvals, event.Approval)
			}
		}
	}
	requestApproval := func(ctx context.Context, call AgentAuthorizedCall) (*AgentApprovalActivity, error) {
		return h.createHostedAgentApproval(ctx, delivery, resolved, thread, session, payload.RequesterID, call)
	}
	result, turnErr := h.RunAgentTurn(ctx, AgentTurnInput{
		Session: session, Workflow: ast, Policy: &policy,
		OwnerUserID: resolved.Deployment.DeployedByUserID, OrganizationID: resolved.Deployment.OrganizationID,
		Message: message, StoredMessage: requestText, HistoryLimit: 20, HistoryTextCap: 6000,
		Goal: goal, Model: runtimeModel, RequestApproval: requestApproval,
	}, sink)
	if len(approvals) > 0 {
		for _, activity := range approvals {
			var approval models.HostedAgentApproval
			if err := h.db.DB.First(&approval, "id = ?", activity.ApprovalID).Error; err != nil {
				return err
			}
			if err := slackAgentPostApproval(ctx, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID, &approval, resolved.Deployment.Name); err != nil {
				return err
			}
		}
		return nil
	}
	if turnErr != nil && !errors.Is(turnErr, ErrAgentApprovalPending) {
		message := "The agent could not finish that request."
		if len(streamedErrors) > 0 {
			message += " " + truncate(streamedErrors[len(streamedErrors)-1], 1000)
		}
		return slackAgentPostText(ctx, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID, message, resolved.Deployment.Name)
	}
	answer := strings.TrimSpace(result.Text)
	if answer == "" {
		answer = strings.TrimSpace(streamedText.String())
	}
	if answer == "" {
		answer = "I couldn't produce an answer for that request."
	}
	return slackAgentPostText(ctx, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID, answer, resolved.Deployment.Name)
}

func slackAgentStripSelfMention(message, botUserID string) string {
	botUserID = strings.TrimSpace(botUserID)
	if botUserID != "" {
		selfMention := regexp.MustCompile(`<@` + regexp.QuoteMeta(botUserID) + `>\s*`)
		return strings.TrimSpace(selfMention.ReplaceAllString(message, ""))
	}
	// Legacy installations did not persist bot_user_id. Remove only the first
	// mention (the app_mention event target), preserving teammates mentioned in
	// the actual instruction. Reconnecting Slack fills the durable ID.
	location := slackMentionPattern.FindStringIndex(message)
	if location == nil {
		return strings.TrimSpace(message)
	}
	return strings.TrimSpace(message[:location[0]] + message[location[1]:])
}

func (h *WorkflowHandler) createHostedAgentApproval(_ context.Context, delivery *models.HostedAgentDelivery, resolved *resolvedSlackDeployment, thread *models.HostedAgentThread, session *models.ChatSession, requesterID string, call AgentAuthorizedCall) (*AgentApprovalActivity, error) {
	effective, err := executor.ResolveSingleNodeData(call.Node, call.Overrides)
	if err != nil {
		return nil, err
	}
	effectiveJSON, _ := json.Marshal(effective)
	hash := sha256.Sum256(effectiveJSON)
	var details map[string]any
	if json.Unmarshal([]byte(agentSafeSavedConfig(effective)), &details) != nil {
		return nil, errors.New("could not render approval details")
	}
	detailJSON, _ := json.Marshal(details)
	if len(detailJSON) > 20<<10 {
		return nil, errors.New("the exact approval details are too large to display safely")
	}
	overrideJSON, _ := json.Marshal(call.Overrides)
	approval := models.HostedAgentApproval{
		OrganizationID: resolved.Deployment.OrganizationID, DeploymentID: resolved.Deployment.ID.String(),
		DeploymentVersion: resolved.Deployment.Version, ThreadID: thread.ID.String(), ChatSessionID: session.ID.String(),
		RequesterExternalID: requesterID, SourceDeliveryID: delivery.ID.String(),
		NodeID: call.Node.ID, Operation: call.Operation.ID, Reason: call.Reason,
		EffectiveOverrides: models.JSONB(overrideJSON), EffectiveConfigHash: hex.EncodeToString(hash[:]),
		DisplayDetails: models.JSONB(detailJSON), Status: models.HostedAgentApprovalPending,
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute),
	}
	if err := h.db.DB.Create(&approval).Error; err != nil {
		var existing models.HostedAgentApproval
		if loadErr := h.db.DB.Where("source_delivery_id = ?", delivery.ID.String()).First(&existing).Error; loadErr != nil {
			return nil, err
		}
		approval = existing
	}
	return &AgentApprovalActivity{
		ApprovalID: approval.ID.String(), Node: call.Node.Data.Label, NodeID: call.Node.ID,
		Operation: call.Operation.Label, Effect: call.Operation.Effect, Reason: call.Reason, Details: details,
	}, nil
}

func slackAgentThreadContext(ctx context.Context, token, channelID, threadID, currentMessageID string) (string, error) {
	var response struct {
		Messages []struct {
			User  string `json:"user"`
			BotID string `json:"bot_id"`
			Text  string `json:"text"`
			TS    string `json:"ts"`
		} `json:"messages"`
	}
	if err := slackAgentAPICall(ctx, token, "conversations.replies", map[string]any{
		"channel": channelID, "ts": threadID, "limit": 100,
	}, &response); err != nil {
		return "", err
	}
	messages := response.Messages
	if len(messages) > hostedAgentThreadMsgCap {
		messages = messages[len(messages)-hostedAgentThreadMsgCap:]
	}
	lines := make([]string, 0, len(messages))
	total := 0
	for _, message := range messages {
		if message.TS == currentMessageID || strings.TrimSpace(message.Text) == "" {
			continue
		}
		author := "Slack user " + message.User
		if message.BotID != "" {
			author = "Agent"
		}
		line := author + ": " + truncate(strings.TrimSpace(message.Text), hostedAgentMessageCap)
		if total+len(line) > hostedAgentThreadTextCap {
			break
		}
		lines = append(lines, line)
		total += len(line)
	}
	return strings.Join(lines, "\n"), nil
}

func slackAgentAPICall(ctx context.Context, token, method string, payload map[string]any, out any) error {
	body, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/"+method, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response, err := slackAgentHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Slack API %s returned %d", method, response.StatusCode)
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &status) != nil || !status.OK {
		return fmt.Errorf("Slack API %s failed: %s", method, status.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func slackAgentPostText(ctx context.Context, token, channelID, threadID, message string, agentName ...string) error {
	payload := map[string]any{
		"channel": channelID, "thread_ts": threadID, "text": truncate(message, 35000),
	}
	if len(agentName) > 0 && strings.TrimSpace(agentName[0]) != "" {
		payload["username"] = truncate(strings.TrimSpace(agentName[0]), 80)
	}
	return slackAgentAPICall(ctx, token, "chat.postMessage", payload, nil)
}

func slackAgentPostApproval(ctx context.Context, token, channelID, threadID string, approval *models.HostedAgentApproval, agentName ...string) error {
	var details map[string]any
	if json.Unmarshal(approval.DisplayDetails, &details) != nil {
		return errors.New("approval display details are unreadable")
	}
	detailJSON, _ := json.MarshalIndent(details, "", "  ")
	blocks := []map[string]any{
		{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": fmt.Sprintf(
				"*Approval required*\n*Operation:* %s\n*Why:* %s\n*Requested by:* <@%s>",
				slackAgentEscape(humanizeAgentOperation(approval.Operation)), slackAgentEscape(approval.Reason), approval.RequesterExternalID)},
		},
	}
	for start := 0; start < len(detailJSON); start += 2500 {
		end := min(start+2500, len(detailJSON))
		blocks = append(blocks, map[string]any{
			"type": "section", "text": map[string]any{
				"type": "mrkdwn", "text": "```" + slackAgentEscape(string(detailJSON[start:end])) + "```",
			},
		})
	}
	blocks = append(blocks, map[string]any{
		"type": "actions",
		"elements": []map[string]any{
			{"type": "button", "action_id": "fernary_agent_approve", "text": map[string]string{"type": "plain_text", "text": "Approve"}, "style": "primary", "value": approval.ID.String()},
			{"type": "button", "action_id": "fernary_agent_reject", "text": map[string]string{"type": "plain_text", "text": "Reject"}, "style": "danger", "value": approval.ID.String()},
		},
	})
	payload := map[string]any{
		"channel": channelID, "thread_ts": threadID,
		"text":   fmt.Sprintf("Approval required for %s: %s", approval.Operation, approval.Reason),
		"blocks": blocks,
	}
	if len(agentName) > 0 && strings.TrimSpace(agentName[0]) != "" {
		payload["username"] = truncate(strings.TrimSpace(agentName[0]), 80)
	}
	return slackAgentAPICall(ctx, token, "chat.postMessage", payload, nil)
}

func slackAgentEscape(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	return strings.ReplaceAll(value, ">", "&gt;")
}

func (h *WorkflowHandler) processSlackAgentApproval(ctx context.Context, payload hostedSlackInteractionPayload) error {
	var approval models.HostedAgentApproval
	if err := h.db.DB.First(&approval, "id = ?", payload.ApprovalID).Error; err != nil {
		return nil
	}
	var deployment models.AgentDeployment
	if err := h.db.DB.First(&deployment, "id = ?", approval.DeploymentID).Error; err != nil {
		return err
	}
	var host models.AgentHostInstallation
	if err := h.db.DB.First(&host, "id = ?", deployment.HostInstallationID).Error; err != nil {
		return err
	}
	var thread models.HostedAgentThread
	if err := h.db.DB.First(&thread, "id = ?", approval.ThreadID).Error; err != nil {
		return err
	}
	if host.ExternalWorkspaceID != payload.WorkspaceID || thread.ExternalChannelID != payload.ChannelID ||
		(payload.ThreadID != "" && thread.ExternalThreadID != payload.ThreadID) ||
		host.OrganizationID != deployment.OrganizationID || thread.OrganizationID != deployment.OrganizationID {
		return errors.New("approval interaction does not match its Slack thread")
	}
	if payload.RequesterID != approval.RequesterExternalID {
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"Only the teammate who requested this action can approve or reject it.", deployment.Name)
	}
	if approval.Status != models.HostedAgentApprovalPending {
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"That approval has already been resolved.", deployment.Name)
	}
	if time.Now().After(approval.ExpiresAt) {
		now := time.Now().UTC()
		h.db.DB.Model(&approval).Updates(map[string]any{"status": models.HostedAgentApprovalExpired, "resolved_at": now})
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"That approval expired. Mention the agent again to request a fresh action.", deployment.Name)
	}
	if payload.Action == "reject" {
		now := time.Now().UTC()
		result := h.db.DB.Model(&approval).Where("status = ?", models.HostedAgentApprovalPending).
			Updates(map[string]any{"status": models.HostedAgentApprovalRejected, "resolved_at": now})
		if result.Error != nil {
			return result.Error
		}
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"<@"+payload.RequesterID+"> rejected the action. Nothing was changed.", deployment.Name)
	}
	return h.executeSlackAgentApproval(ctx, &approval, &deployment, &host, &thread)
}

func (h *WorkflowHandler) executeSlackAgentApproval(ctx context.Context, approval *models.HostedAgentApproval, deployment *models.AgentDeployment, host *models.AgentHostInstallation, thread *models.HostedAgentThread) error {
	if approval.DeploymentVersion != deployment.Version || deployment.Status != models.AgentDeploymentActive {
		err := errors.New("deployment changed or is no longer active")
		h.failHostedApproval(approval, err)
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID, err.Error(), deployment.Name)
	}
	var ast executor.WorkflowAST
	ast.Name = deployment.SnapshotName
	if json.Unmarshal(deployment.SnapshotNodes, &ast.Nodes) != nil || json.Unmarshal(deployment.SnapshotEdges, &ast.Edges) != nil {
		return errors.New("deployment snapshot is unreadable")
	}
	var node *executor.WorkflowASTNode
	for i := range ast.Nodes {
		if ast.Nodes[i].ID == approval.NodeID {
			node = &ast.Nodes[i]
			break
		}
	}
	if node == nil {
		err := errors.New("approved node is absent from the deployment snapshot")
		h.failHostedApproval(approval, err)
		return err
	}
	var policy AgentCapabilityPolicy
	var overrides map[string]any
	if json.Unmarshal(deployment.CapabilityPolicy, &policy) != nil || json.Unmarshal(approval.EffectiveOverrides, &overrides) != nil {
		return errors.New("approved call is unreadable")
	}
	authorizationInput := map[string]any{"reason": approval.Reason}
	for field, value := range overrides {
		authorizationInput[field] = value
	}
	authorized, err := authorizeAgentToolCall(policy, *node, authorizationInput)
	if err != nil || authorized.Operation.ID != approval.Operation {
		if err == nil {
			err = errors.New("approved operation no longer matches its policy")
		}
		h.failHostedApproval(approval, err)
		return err
	}
	effective, err := executor.ResolveSingleNodeData(*node, authorized.Overrides)
	if err != nil {
		h.failHostedApproval(approval, err)
		return err
	}
	effectiveJSON, _ := json.Marshal(effective)
	hash := sha256.Sum256(effectiveJSON)
	if hex.EncodeToString(hash[:]) != approval.EffectiveConfigHash {
		err = errors.New("approved call hash does not match the pinned operation")
		h.failHostedApproval(approval, err)
		return err
	}

	var session models.ChatSession
	if err := h.db.DB.First(&session, "id = ?", approval.ChatSessionID).Error; err != nil {
		return err
	}
	plan, billErr := h.bill.CheckBalance(deployment.OrganizationID, deployment.DeployedByUserID)
	if billErr != nil {
		message := "This organization's Fernary credits are exhausted, so the approved action cannot run right now."
		if !errors.Is(billErr, billing.ErrOverCap) {
			message = "Fernary could not check the organization's credits. Please try approving again shortly."
		}
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID, message, deployment.Name)
	}
	ctx = telemetry.WithSurface(ctx, telemetry.SurfaceAgent)
	ctx = telemetry.WithBilling(ctx, billing.BillingContextFor(deployment.OrganizationID, deployment.DeployedByUserID, plan))

	now := time.Now().UTC()
	claim := h.db.DB.Model(approval).Where("status = ?", models.HostedAgentApprovalPending).
		Updates(map[string]any{"status": models.HostedAgentApprovalExecuting, "resolved_at": now})
	if claim.Error != nil {
		return claim.Error
	}
	if claim.RowsAffected != 1 {
		return nil
	}
	state := map[string]string{}
	_ = json.Unmarshal(session.State, &state)
	keys := executor.APIKeys{
		Anthropic: os.Getenv("ANTHROPIC_API_KEY"), OpenAI: os.Getenv("OPENAI_API_KEY"),
		Brave: os.Getenv("BRAVE_API_KEY"), Jina: os.Getenv("JINA_API_KEY"),
	}
	out, execErr := executor.ExecuteSingleNode(ctx, *node, authorized.Overrides, state, ast.Edges, keys,
		"approval-"+approval.ID.String(), deployment.DeployedByUserID, deployment.OrganizationID, nil)
	if execErr != nil {
		h.failHostedApproval(approval, execErr)
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"The approved action failed: "+truncate(execErr.Error(), 1500), deployment.Name)
	}
	state[node.ID] = truncate(out, agentStateCap)
	stateJSON, _ := json.Marshal(state)
	var history []agentStoredMessage
	_ = json.Unmarshal(session.Messages, &history)
	history = append(history, agentStoredMessage{
		Role: "assistant",
		Content: fmt.Sprintf("The requester approved %s and it completed successfully. Result: %s",
			humanizeAgentOperation(approval.Operation), truncate(out, agentResultCap)),
		ToolCalls: []agentToolCallRecord{{
			Node: node.Data.Label, NodeID: node.ID, Op: humanizeAgentOperation(approval.Operation), Status: "ok",
		}},
	})
	historyJSON, _ := json.Marshal(history)
	executedAt := time.Now().UTC()
	if err := h.db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&session).Updates(map[string]any{
			"state": models.JSONB(stateJSON), "messages": models.JSONB(historyJSON),
		}).Error; err != nil {
			return err
		}
		return tx.Model(approval).Updates(map[string]any{
			"status": models.HostedAgentApprovalExecuted, "executed_at": executedAt, "last_error": "",
		}).Error
	}); err != nil {
		return err
	}
	return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
		fmt.Sprintf("Approved action completed: *%s*\n```%s```", slackAgentEscape(approval.Operation), slackAgentEscape(truncate(out, 3000))), deployment.Name)
}

func (h *WorkflowHandler) failHostedApproval(approval *models.HostedAgentApproval, err error) {
	h.db.DB.Model(approval).Updates(map[string]any{
		"status": models.HostedAgentApprovalFailed, "last_error": truncate(err.Error(), 2000),
	})
}
