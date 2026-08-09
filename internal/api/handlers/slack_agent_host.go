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
	"sort"
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
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	slackAgentProvider       = "slack"
	slackSignatureMaxAge     = 5 * time.Minute
	hostedAgentThreadTextCap = 16 << 10
	hostedAgentMessageCap    = 2000
	hostedAgentThreadMsgCap  = 40
	hostedApprovalOutcomeTTL = 24 * time.Hour
	hostedAgentWorkerCount   = 4
)

var (
	slackAgentHTTPClient         = &http.Client{Timeout: 20 * time.Second}
	slackMentionPattern          = regexp.MustCompile(`<@[A-Z0-9]+>\s*`)
	slackSpecialMentionPattern   = regexp.MustCompile(`<!((?:channel|here|everyone))(?:\^[^>]*)?>`)
	slackSubteamMentionPattern   = regexp.MustCompile(`<!subteam\^[A-Z0-9]+(?:\|[^>]*)?>`)
	slackUserMentionPattern      = regexp.MustCompile(`<@([A-Z0-9]+)>`)
	slackRoutingIDPattern        = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,31}$`)
	slackTimestampPattern        = regexp.MustCompile(`^[0-9]{1,16}\.[0-9]{1,16}$`)
	hostedWorkerOnce             sync.Once
	errHostedThreadStale         = errors.New("Slack thread belongs to another agent deployment")
	errHostedAgentAuthorityEnded = errors.New("the deployment owner is no longer authorized to act for this organization")
	errHostedCredentialChanged   = errors.New("the deployment owner's integration connection changed after approval was requested")
	hostedApprovalOutcomeMemory  sync.Map
)

type hostedApprovalRecoverableOutcome struct {
	Output     string    `json:"output"`
	RecordedAt time.Time `json:"recordedAt"`
}

type hostedApprovalOutcomeUnresolvedError struct {
	ApprovalID string
	Operation  string
}

func (e *hostedApprovalOutcomeUnresolvedError) Error() string {
	return fmt.Sprintf(
		"an equivalent %s action has an unresolved outcome; its requester must verify and reconcile it in Slack before this action can run again",
		humanizeAgentOperation(e.Operation),
	)
}

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

type slackAgentDeliveryContextKey struct{}

func withSlackAgentDeliveryID(ctx context.Context, deliveryID string) context.Context {
	return context.WithValue(ctx, slackAgentDeliveryContextKey{}, deliveryID)
}

func slackAgentDeliveryID(ctx context.Context) string {
	value, _ := ctx.Value(slackAgentDeliveryContextKey{}).(string)
	return value
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
	if !slackRoutingIDPattern.MatchString(envelope.TeamID) ||
		!slackRoutingIDPattern.MatchString(envelope.Event.Channel) ||
		!slackRoutingIDPattern.MatchString(envelope.Event.User) ||
		!slackTimestampPattern.MatchString(envelope.Event.TS) ||
		(envelope.Event.ThreadTS != "" && !slackTimestampPattern.MatchString(envelope.Event.ThreadTS)) ||
		len(envelope.EventID) > 255 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Slack mention has invalid routing fields"})
		return
	}
	targetExists, err := h.activeSlackAgentTargetExists(envelope.TeamID, envelope.Event.Channel)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "could not resolve Slack agent destination", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not resolve agent destination"})
		return
	}
	if !targetExists {
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
		ExternalWorkspaceID: envelope.TeamID, ThreadKey: envelope.TeamID + ":" + envelope.Event.Channel + ":" + threadID,
		EventKind: "mention", Payload: models.JSONB(payload),
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
	if json.Unmarshal([]byte(form.Get("payload")), &interaction) != nil || interaction.Type != "block_actions" || len(interaction.Actions) != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unreadable Slack interaction"})
		return
	}
	action := interaction.Actions[0]
	decision, supported := hostedApprovalInteractionAction(action.ActionID)
	if !supported {
		c.Status(http.StatusOK)
		return
	}
	if interaction.Team.ID == "" || interaction.User.ID == "" || interaction.Channel.ID == "" || action.Value == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Slack interaction is missing identity fields"})
		return
	}
	threadID := interaction.Container.ThreadTS
	if threadID == "" {
		threadID = interaction.Container.MessageTS
	}
	actionTS := action.ActionTS
	if actionTS == "" {
		actionTS = interaction.ActionTS
	}
	if !slackRoutingIDPattern.MatchString(interaction.Team.ID) ||
		!slackRoutingIDPattern.MatchString(interaction.User.ID) ||
		!slackRoutingIDPattern.MatchString(interaction.Channel.ID) ||
		!slackTimestampPattern.MatchString(threadID) || interaction.TriggerID == "" || len(interaction.TriggerID) > 255 ||
		!slackTimestampPattern.MatchString(actionTS) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Slack interaction has invalid routing fields"})
		return
	}
	if _, err := uuid.Parse(action.Value); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Slack interaction has an invalid approval ID"})
		return
	}
	payload, _ := json.Marshal(hostedSlackInteractionPayload{
		WorkspaceID: interaction.Team.ID, ChannelID: interaction.Channel.ID, ThreadID: threadID,
		RequesterID: interaction.User.ID, ApprovalID: action.Value, Action: decision,
	})
	externalID := interaction.TriggerID + ":" + actionTS
	delivery := models.HostedAgentDelivery{
		Provider: slackAgentProvider, ExternalDeliveryID: "interaction:" + externalID,
		ExternalWorkspaceID: interaction.Team.ID,
		ThreadKey:           interaction.Team.ID + ":" + interaction.Channel.ID + ":" + threadID,
		EventKind:           "interaction", Payload: models.JSONB(payload),
		Status: models.HostedAgentDeliveryPending, AvailableAt: time.Now().UTC(),
	}
	if err := h.db.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&delivery).Error; err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not queue interaction"})
		return
	}
	c.Status(http.StatusOK)
}

func hostedApprovalInteractionAction(actionID string) (string, bool) {
	switch actionID {
	case "fernary_agent_approve":
		return "approve", true
	case "fernary_agent_reject":
		return "reject", true
	case "fernary_agent_outcome_completed":
		return "reconcile_completed", true
	case "fernary_agent_outcome_not_run":
		return "reconcile_not_run", true
	default:
		return "", false
	}
}

func (h *WorkflowHandler) activeSlackAgentTargetExists(workspaceID, channelID string) (bool, error) {
	var count int64
	err := h.db.DB.Table("agent_deployment_targets AS target").
		Joins("JOIN agent_deployments AS deployment ON deployment.id = target.deployment_id AND deployment.deleted_at IS NULL").
		Joins("JOIN agent_host_installations AS host ON host.id = deployment.host_installation_id AND host.deleted_at IS NULL").
		Where("target.deleted_at IS NULL AND target.provider = ? AND target.external_workspace_id = ? AND target.external_channel_id = ? AND target.enabled = true",
			slackAgentProvider, workspaceID, channelID).
		Where("deployment.status = ? AND host.status = ?", models.AgentDeploymentActive, models.AgentHostActive).
		Count(&count).Error
	return count > 0, err
}

func (h *WorkflowHandler) StartHostedAgentWorker() {
	hostedWorkerOnce.Do(func() {
		for range hostedAgentWorkerCount {
			go h.hostedAgentWorkerLoop()
		}
	})
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
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		err = h.safeProcessHostedAgentDelivery(withSlackAgentDeliveryID(ctx, delivery.ID.String()), delivery)
		cancel()
		h.finishHostedAgentDelivery(delivery, err)
	}
}

func (h *WorkflowHandler) safeProcessHostedAgentDelivery(ctx context.Context, delivery *models.HostedAgentDelivery) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("hosted agent delivery panicked: %v", recovered)
			slog.ErrorContext(ctx, "hosted agent delivery panic recovered", "delivery_id", delivery.ID, "panic", recovered)
		}
	}()
	return h.processHostedAgentDelivery(ctx, delivery)
}

func (h *WorkflowHandler) claimHostedAgentDelivery() (*models.HostedAgentDelivery, error) {
	var delivery models.HostedAgentDelivery
	now := time.Now().UTC()
	// A turn can span several provider/tool rounds and a human-approved node can
	// itself be slow. Reclaim only after a wide crash window; thread advisory
	// locks provide the stronger concurrency guarantee while a worker is alive.
	stale := now.Add(-30 * time.Minute)
	err := h.db.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("(status = ? AND available_at <= ?) OR (status = ? AND claimed_at < ?)",
				models.HostedAgentDeliveryPending, now, models.HostedAgentDeliveryProcessing, stale).
			Where(`thread_key = '' OR NOT EXISTS (
				SELECT 1 FROM hosted_agent_deliveries AS earlier
				WHERE earlier.deleted_at IS NULL
				  AND earlier.thread_key = hosted_agent_deliveries.thread_key
				  AND earlier.id <> hosted_agent_deliveries.id
				  AND earlier.status IN (?, ?)
				  AND (earlier.created_at < hosted_agent_deliveries.created_at
				       OR (earlier.created_at = hosted_agent_deliveries.created_at AND earlier.id::text < hosted_agent_deliveries.id::text))
			)`, models.HostedAgentDeliveryPending, models.HostedAgentDeliveryProcessing).
			Order("available_at ASC").Limit(1).Find(&delivery)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
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
		if err := h.db.DB.Model(delivery).Updates(map[string]any{
			"status": models.HostedAgentDeliveryCompleted, "completed_at": now, "last_error": "",
		}).Error; err != nil {
			slog.Error("could not mark hosted agent delivery completed", "delivery_id", delivery.ID, "error", err)
		}
		return
	}
	slog.Warn("hosted agent delivery failed", "delivery_id", delivery.ID, "attempt", delivery.AttemptCount, "error", processErr)
	if delivery.AttemptCount >= 5 {
		if err := h.db.DB.Model(delivery).Updates(map[string]any{
			"status": models.HostedAgentDeliveryFailed, "completed_at": now,
			"last_error": truncate(processErr.Error(), 2000),
		}).Error; err != nil {
			slog.Error("could not mark hosted agent delivery failed", "delivery_id", delivery.ID, "error", err)
		}
		return
	}
	backoff := time.Duration(1<<min(delivery.AttemptCount, 6)) * time.Second
	if err := h.db.DB.Model(delivery).Updates(map[string]any{
		"status": models.HostedAgentDeliveryPending, "available_at": now.Add(backoff),
		"last_error": truncate(processErr.Error(), 2000),
	}).Error; err != nil {
		slog.Error("could not reschedule hosted agent delivery", "delivery_id", delivery.ID, "error", err)
	}
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
	return withHostedAdvisoryLock(ctx, h.db.DB, key, func(_ *gorm.DB) error {
		return run()
	})
}

func (h *WorkflowHandler) withHostedAuthorityLock(ctx context.Context, organizationID, userID string, run func(*gorm.DB) error) error {
	if organizationID == "" || userID == "" {
		return errors.New("hosted authority lock identity is empty")
	}
	return h.withHostedAuthorityLocks(ctx, organizationID, []string{userID}, run)
}

func (h *WorkflowHandler) withHostedAuthorityLocks(ctx context.Context, organizationID string, userIDs []string, run func(*gorm.DB) error) error {
	if organizationID == "" {
		return errors.New("hosted authority lock organization is empty")
	}
	keys := make([]string, 0, len(userIDs))
	for _, userID := range userIDs {
		if userID == "" {
			return errors.New("hosted authority lock user is empty")
		}
		keys = append(keys, "authority:"+organizationID+":"+userID)
	}
	return withHostedAdvisoryLocks(ctx, h.db.DB, keys, run)
}

func withHostedAdvisoryLocks(ctx context.Context, db *gorm.DB, requestedKeys []string, run func(*gorm.DB) error) error {
	unique := map[string]bool{}
	keys := make([]string, 0, len(requestedKeys))
	for _, key := range requestedKeys {
		if key == "" {
			return errors.New("hosted advisory lock key is empty")
		}
		if !unique[key] {
			unique[key] = true
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return db.WithContext(ctx).Connection(func(connection *gorm.DB) error {
		acquired := 0
		// Unlocks must still run after the request context is cancelled. Reusing a
		// cancelled context here can return the session to the pool with advisory
		// locks still held, permanently stalling later authority changes.
		unlockDB := connection.Session(&gorm.Session{NewDB: true}).WithContext(context.WithoutCancel(ctx))
		defer func() {
			for index := acquired - 1; index >= 0; index-- {
				if err := unlockDB.Exec("SELECT pg_advisory_unlock(hashtextextended(?, 0))", keys[index]).Error; err != nil {
					slog.ErrorContext(context.WithoutCancel(ctx), "could not release hosted authority lock", "key", keys[index], "error", err)
				}
			}
		}()
		for _, key := range keys {
			if err := connection.Exec("SELECT pg_advisory_lock(hashtextextended(?, 0))", key).Error; err != nil {
				return err
			}
			acquired++
		}
		return run(connection)
	})
}

// withHostedAdvisoryLock holds one connection-scoped PostgreSQL lock for the
// entire callback. Approval execution and member removal use the same authority
// key, so revocation cannot commit while an external mutation is still running.
func withHostedAdvisoryLock(ctx context.Context, db *gorm.DB, key string, run func(*gorm.DB) error) error {
	return withHostedAdvisoryLocks(ctx, db, []string{key}, run)
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
	if err := h.db.DB.Model(&models.OrgMember{}).
		Where("organization_id = ? AND user_id = ?", deployment.OrganizationID, deployment.DeployedByUserID).
		Count(&deployerMembership).Error; err != nil {
		return nil, err
	}
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

func verifyHostedAgentAuthorityOn(db *gorm.DB, resolved *resolvedSlackDeployment) (*models.AgentDeployment, error) {
	var deployment models.AgentDeployment
	if err := db.Session(&gorm.Session{NewDB: true}).
		Where("id = ? AND organization_id = ? AND deployed_by_user_id = ? AND version = ? AND status = ?",
			resolved.Deployment.ID, resolved.Deployment.OrganizationID, resolved.Deployment.DeployedByUserID,
			resolved.Deployment.Version, models.AgentDeploymentActive).First(&deployment).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errHostedAgentAuthorityEnded
		}
		return nil, err
	}
	var membership int64
	if err := db.Session(&gorm.Session{NewDB: true}).Model(&models.OrgMember{}).
		Where("organization_id = ? AND user_id = ?", deployment.OrganizationID, deployment.DeployedByUserID).
		Count(&membership).Error; err != nil {
		return nil, err
	}
	if membership != 1 {
		return nil, errHostedAgentAuthorityEnded
	}
	var hostCount int64
	if err := db.Session(&gorm.Session{NewDB: true}).Model(&models.AgentHostInstallation{}).
		Where("id = ? AND organization_id = ? AND external_workspace_id = ? AND status = ?",
			resolved.Host.ID, deployment.OrganizationID, resolved.Host.ExternalWorkspaceID, models.AgentHostActive).
		Count(&hostCount).Error; err != nil {
		return nil, err
	}
	var targetCount int64
	if err := db.Session(&gorm.Session{NewDB: true}).Model(&models.AgentDeploymentTarget{}).
		Where("id = ? AND deployment_id = ? AND organization_id = ? AND enabled = true",
			resolved.Target.ID, deployment.ID, deployment.OrganizationID).
		Count(&targetCount).Error; err != nil {
		return nil, err
	}
	if hostCount != 1 || targetCount != 1 {
		return nil, errHostedAgentAuthorityEnded
	}
	return &deployment, nil
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
		if err := h.db.DB.Model(&thread).Update("latest_external_message_id", payload.MessageID).Error; err != nil {
			return nil, nil, err
		}
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
	associated, err := h.associateHostedAgentDeliveryWithDeployment(delivery, resolved.Deployment.ID.String())
	if err != nil {
		return err
	}
	if !associated {
		// The provider retried an event after its channel was moved to another
		// deployment. Never rerun the old event under new delegated authority.
		return nil
	}
	if delivery.ResponseRecordedAt != nil {
		if delivery.ResponseDeploymentID != resolved.Deployment.ID.String() {
			return nil
		}
		return slackAgentPostText(ctx, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID,
			delivery.ResponseText, resolved.Deployment.Name)
	}

	var existingApproval models.HostedAgentApproval
	if err := h.db.DB.Where("source_delivery_id = ?", delivery.ID.String()).First(&existingApproval).Error; err == nil {
		if existingApproval.DeploymentID != resolved.Deployment.ID.String() ||
			existingApproval.OrganizationID != resolved.Deployment.OrganizationID {
			return errors.New("hosted approval delivery boundary mismatch")
		}
		if existingApproval.Status == models.HostedAgentApprovalPending {
			return slackAgentPostApproval(ctx, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID, &existingApproval, resolved.Deployment.Name)
		}
		// The mention already produced a durable approval and a later interaction
		// resolved it. Never ask the model to regenerate the source turn.
		return nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	// Provider retries must recover a durable approval before consuming another
	// rate-limit slot. Otherwise a transient failure posting the approval could
	// strand it permanently behind a later rate-limit response.
	if !auth.Allow(ctx, h.redis, "rl:hosted-agent-deployment:"+resolved.Deployment.ID.String(), 60, time.Minute) ||
		!auth.Allow(ctx, h.redis, "rl:hosted-agent-requester:"+resolved.Deployment.ID.String()+":"+payload.RequesterID, 12, time.Minute) {
		return h.slackAgentPostDeliveryText(ctx, delivery, &resolved.Deployment, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID,
			"This agent is receiving too many requests right now. Please try again in a minute.")
	}

	thread, session, err := h.loadOrCreateHostedThread(resolved, payload)
	if errors.Is(err, errHostedThreadStale) {
		return h.slackAgentPostDeliveryText(ctx, delivery, &resolved.Deployment, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID,
			"This thread belongs to an older agent deployment. Start a new top-level mention to use the current agent.")
	}
	if err != nil {
		return err
	}

	plan, err := h.bill.CheckBalance(resolved.Deployment.OrganizationID, resolved.Deployment.DeployedByUserID)
	if err != nil {
		message := "This organization's Fernary credits are exhausted, so the agent cannot run right now."
		if !errors.Is(err, billing.ErrOverCap) {
			message = "The agent could not check its Fernary credits. Please try again shortly."
		}
		if postErr := h.slackAgentPostDeliveryText(ctx, delivery, &resolved.Deployment, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID, message); postErr != nil {
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
	runtimeModel, err := prepareAgentRuntimeModel(resolved.Deployment.ModelID)
	if err != nil {
		return h.slackAgentPostDeliveryText(ctx, delivery, &resolved.Deployment, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID,
			"This agent's model is not configured on Fernary. Ask the deployment owner to update it.")
	}

	var approvals []*AgentApprovalActivity
	var streamedText strings.Builder
	sink := func(event AgentTurnEvent) {
		switch event.Type {
		case AgentTurnText:
			streamedText.WriteString(event.Text)
		case AgentTurnApproval:
			if event.Approval != nil {
				approvals = append(approvals, event.Approval)
			}
		}
	}
	requestApproval := func(ctx context.Context, call AgentAuthorizedCall, state map[string]string) (*AgentApprovalActivity, error) {
		return h.createHostedAgentApproval(ctx, delivery, resolved, thread, session, payload.RequesterID, call, state)
	}
	var result AgentTurnResult
	var turnErr error
	lockErr := h.withHostedAuthorityLock(ctx, resolved.Deployment.OrganizationID, resolved.Deployment.DeployedByUserID, func(connection *gorm.DB) error {
		liveDeployment, err := verifyHostedAgentAuthorityOn(connection, resolved)
		if err != nil {
			return err
		}
		// Capability policies are intentionally mutable. Read the authoritative
		// value only after acquiring the same authority lock used by permission
		// edits, so a turn can never execute with a stale policy after an edit has
		// committed.
		var policy AgentCapabilityPolicy
		if json.Unmarshal(liveDeployment.CapabilityPolicy, &policy) != nil {
			return errors.New("deployment policy is unreadable")
		}
		var analysis map[string]any
		_ = json.Unmarshal(liveDeployment.PermissionAnalysis, &analysis)
		goal, _ := analysis["goal"].(string)
		resolved.Deployment.CapabilityPolicy = liveDeployment.CapabilityPolicy
		resolved.Deployment.PermissionAnalysis = liveDeployment.PermissionAnalysis
		result, turnErr = h.RunAgentTurn(ctx, AgentTurnInput{
			Session: session, Workflow: ast, Policy: &policy,
			OwnerUserID: resolved.Deployment.DeployedByUserID, OrganizationID: resolved.Deployment.OrganizationID,
			Message: message, StoredMessage: requestText, HistoryLimit: 20, HistoryTextCap: 6000,
			Goal: goal, Model: runtimeModel, RequestApproval: requestApproval,
		}, sink)
		return nil
	})
	if errors.Is(lockErr, errHostedAgentAuthorityEnded) {
		current, resolveErr := h.resolveSlackDeployment(payload.WorkspaceID, payload.ChannelID)
		if resolveErr != nil || current.Deployment.ID != resolved.Deployment.ID {
			// Revocation, host disconnection, or a destination edit may have
			// removed this channel while the event waited for the authority lock.
			// Do not leak even an operational reply into a destination that is no
			// longer allowlisted.
			return nil
		}
		return h.slackAgentPostDeliveryText(ctx, delivery, &resolved.Deployment, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID,
			"This deployment can no longer act because its owner or destination is no longer active.")
	}
	if lockErr != nil {
		return lockErr
	}
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
		return h.slackAgentPostDeliveryText(ctx, delivery, &resolved.Deployment, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID,
			"The agent could not finish that request. Please try again or ask the deployment owner to check Fernary.")
	}
	answer := strings.TrimSpace(result.Text)
	if answer == "" {
		answer = strings.TrimSpace(streamedText.String())
	}
	if answer == "" {
		answer = "I couldn't produce an answer for that request."
	}
	return h.slackAgentPostDeliveryText(ctx, delivery, &resolved.Deployment, resolved.Host.BotToken, payload.ChannelID, payload.ThreadID,
		slackAgentSanitizeGeneratedText(answer))
}

func (h *WorkflowHandler) associateHostedAgentDeliveryWithDeployment(delivery *models.HostedAgentDelivery, deploymentID string) (bool, error) {
	if delivery.ResponseDeploymentID != "" {
		return delivery.ResponseDeploymentID == deploymentID, nil
	}
	result := h.db.DB.Model(&models.HostedAgentDelivery{}).
		Where("id = ? AND (response_deployment_id = '' OR response_deployment_id IS NULL)", delivery.ID).
		Update("response_deployment_id", deploymentID)
	if result.Error != nil {
		return false, fmt.Errorf("associate hosted delivery with deployment: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		delivery.ResponseDeploymentID = deploymentID
		return true, nil
	}
	var durable models.HostedAgentDelivery
	if err := h.db.DB.Select("response_deployment_id").First(&durable, "id = ?", delivery.ID).Error; err != nil {
		return false, fmt.Errorf("recover hosted delivery deployment: %w", err)
	}
	delivery.ResponseDeploymentID = durable.ResponseDeploymentID
	return delivery.ResponseDeploymentID == deploymentID, nil
}

// slackAgentPostDeliveryText stores the exact reply before contacting Slack.
// A provider timeout or a failure marking the queue row complete can then
// replay this text without rerunning the model, read tools, or billing.
func (h *WorkflowHandler) slackAgentPostDeliveryText(ctx context.Context, delivery *models.HostedAgentDelivery, deployment *models.AgentDeployment, token, channelID, threadID, message string) error {
	if deployment == nil {
		return errors.New("hosted agent reply deployment is required")
	}
	if delivery.ResponseRecordedAt == nil {
		recordedAt := time.Now().UTC()
		stored := truncate(message, 35000)
		result := h.db.DB.Model(&models.HostedAgentDelivery{}).
			Where("id = ? AND response_recorded_at IS NULL", delivery.ID).
			Updates(map[string]any{
				"response_deployment_id": deployment.ID.String(),
				"response_text":          stored, "response_recorded_at": recordedAt,
			})
		if result.Error != nil {
			return fmt.Errorf("record hosted agent reply: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			var durable models.HostedAgentDelivery
			if err := h.db.DB.Select("response_deployment_id", "response_text", "response_recorded_at").First(&durable, "id = ?", delivery.ID).Error; err != nil {
				return fmt.Errorf("recover hosted agent reply: %w", err)
			}
			delivery.ResponseDeploymentID = durable.ResponseDeploymentID
			delivery.ResponseText = durable.ResponseText
			delivery.ResponseRecordedAt = durable.ResponseRecordedAt
		} else {
			delivery.ResponseDeploymentID = deployment.ID.String()
			delivery.ResponseText = stored
			delivery.ResponseRecordedAt = &recordedAt
		}
	}
	if delivery.ResponseDeploymentID != deployment.ID.String() {
		return errors.New("hosted agent reply deployment changed before delivery")
	}
	return slackAgentPostText(ctx, token, channelID, threadID, delivery.ResponseText, deployment.Name)
}

func slackAgentSanitizeGeneratedText(message string) string {
	message = slackSpecialMentionPattern.ReplaceAllString(message, "@$1")
	message = slackSubteamMentionPattern.ReplaceAllString(message, "@user-group")
	return slackUserMentionPattern.ReplaceAllString(message, "@user-$1")
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

func (h *WorkflowHandler) createHostedAgentApproval(ctx context.Context, delivery *models.HostedAgentDelivery, resolved *resolvedSlackDeployment, thread *models.HostedAgentThread, session *models.ChatSession, requesterID string, call AgentAuthorizedCall, state map[string]string) (*AgentApprovalActivity, error) {
	effective, err := executor.ResolveSingleNodeDataForExecution(call.Node, call.Overrides, state)
	if err != nil {
		return nil, err
	}
	effectiveJSON, _ := json.Marshal(effective)
	hash := sha256.Sum256(effectiveJSON)
	details, err := agentApprovalDisplayDetails(effective)
	if err != nil {
		return nil, errors.New("could not render approval details")
	}
	credentialProvider, credentialConnectionID, credentialDescription, err := h.resolveHostedApprovalCredential(
		resolved.Deployment.OrganizationID, resolved.Deployment.DeployedByUserID, effective,
	)
	if err != nil {
		return nil, err
	}
	if credentialDescription != nil {
		details["credential"] = credentialDescription
	}
	detailJSON, _ := json.Marshal(details)
	if len(detailJSON) > 20<<10 {
		return nil, errors.New("the exact approval details are too large to display safely")
	}
	overrideJSON, _ := json.Marshal(call.Overrides)
	configHash := hex.EncodeToString(hash[:])
	approvalID := uuid.New()
	now := time.Now().UTC()
	reason := truncate(strings.TrimSpace(call.Reason), 2000)
	approval := models.HostedAgentApproval{
		BaseModel:      models.BaseModel{ID: approvalID},
		OrganizationID: resolved.Deployment.OrganizationID, DeploymentID: resolved.Deployment.ID.String(),
		DeploymentVersion: resolved.Deployment.Version, ThreadID: thread.ID.String(), ChatSessionID: session.ID.String(),
		RequesterExternalID: requesterID, SourceDeliveryID: delivery.ID.String(),
		NodeID: call.Node.ID, Operation: call.Operation.ID, Reason: reason,
		EffectiveOverrides: models.JSONB(overrideJSON), EffectiveConfigHash: configHash,
		ExecutionConfig: models.JSONB(effectiveJSON), ConfigRecordedAt: &now,
		ExecutionFingerprint: hostedApprovalExecutionFingerprint(effective),
		CredentialProvider:   credentialProvider, CredentialConnectionID: credentialConnectionID,
		DisplayDetails: models.JSONB(detailJSON), Status: models.HostedAgentApprovalPending,
		ExecutionKey: hostedApprovalExecutionKey(approvalID.String()),
		ExpiresAt:    now.Add(15 * time.Minute),
	}
	// The approval row is the durable pre-execution attempt record. Locking the
	// deployment serializes approval creation with claims and revocation, while
	// the unresolved query prevents a semantically identical non-idempotent call
	// from being approved again until the requester reconciles its outcome.
	if err := h.db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var live models.AgentDeployment
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ? AND version = ? AND status = ?",
				resolved.Deployment.ID, resolved.Deployment.OrganizationID,
				resolved.Deployment.Version, models.AgentDeploymentActive).
			First(&live).Error; err != nil {
			return err
		}
		var existing models.HostedAgentApproval
		if err := tx.Where("source_delivery_id = ?", delivery.ID.String()).First(&existing).Error; err == nil {
			approval = existing
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		unresolved, err := findEquivalentUnresolvedHostedApproval(tx, &approval)
		if err != nil {
			return err
		}
		if unresolved != nil {
			return &hostedApprovalOutcomeUnresolvedError{
				ApprovalID: unresolved.ID.String(), Operation: unresolved.Operation,
			}
		}
		return tx.Create(&approval).Error
	}); err != nil {
		return nil, err
	}
	return &AgentApprovalActivity{
		ApprovalID: approval.ID.String(), Node: call.Node.Data.Label, NodeID: call.Node.ID,
		Operation: call.Operation.Label, Effect: call.Operation.Effect, Reason: reason, Details: details,
	}, nil
}

func (h *WorkflowHandler) resolveHostedApprovalCredential(organizationID, userID string, effective executor.FlowNodeData) (string, string, map[string]any, error) {
	if effective.IntegrationOp == "" || effective.IntegrationToken != "" {
		return "", "", nil, nil
	}
	provider := string(effective.NodeType)
	var connection models.IntegrationConnection
	if err := h.db.DB.Select("id", "provider", "workspace_id", "workspace_name").
		Where("organization_id = ? AND user_id = ? AND provider = ?", organizationID, userID, provider).
		First(&connection).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", "", nil, fmt.Errorf("%s is not connected for the deployment owner", humanizeAgentOperation(provider))
		}
		return "", "", nil, err
	}
	description := map[string]any{"provider": provider}
	if connection.WorkspaceName != "" {
		description["workspace"] = connection.WorkspaceName
	} else if connection.WorkspaceID != "" {
		description["workspace"] = connection.WorkspaceID
	}
	return provider, connection.ID.String(), description, nil
}

func verifyHostedApprovalCredentialOn(db *gorm.DB, approval *models.HostedAgentApproval, deployment *models.AgentDeployment) error {
	if approval.CredentialProvider == "" && approval.CredentialConnectionID == "" {
		return nil
	}
	if approval.CredentialProvider == "" || approval.CredentialConnectionID == "" {
		return errHostedCredentialChanged
	}
	var count int64
	if err := db.Model(&models.IntegrationConnection{}).
		Where("id = ? AND organization_id = ? AND user_id = ? AND provider = ?",
			approval.CredentialConnectionID, deployment.OrganizationID, deployment.DeployedByUserID, approval.CredentialProvider).
		Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return errHostedCredentialChanged
	}
	return nil
}

func findEquivalentUnresolvedHostedApproval(db *gorm.DB, candidate *models.HostedAgentApproval) (*models.HostedAgentApproval, error) {
	var unresolved models.HostedAgentApproval
	query := db.Where(
		"id <> ? AND organization_id = ? AND operation = ? AND status IN ?",
		candidate.ID, candidate.OrganizationID, candidate.Operation,
		[]models.HostedAgentApprovalStatus{
			models.HostedAgentApprovalExecuting, models.HostedAgentApprovalOutcomeUnknown,
		},
	)
	if candidate.ExecutionFingerprint != "" {
		// Legacy rows predate the normalized fingerprint. Their exact pinned hash
		// remains a safe fallback for the same node, while current rows stay
		// blocked across deployment versions and cosmetic node-label changes.
		query = query.Where(
			"execution_fingerprint = ? OR (COALESCE(execution_fingerprint, '') = '' AND node_id = ? AND effective_config_hash = ?)",
			candidate.ExecutionFingerprint, candidate.NodeID, candidate.EffectiveConfigHash,
		)
	} else {
		query = query.Where("node_id = ? AND effective_config_hash = ?", candidate.NodeID, candidate.EffectiveConfigHash)
	}
	err := query.Order("created_at ASC").First(&unresolved).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &unresolved, nil
}

func hostedApprovalExecutionFingerprint(effective executor.FlowNodeData) string {
	// Labels are presentation only. Excluding them prevents a cosmetic rename or
	// a deployment version bump from bypassing an unresolved-action block.
	effective.Label = ""
	raw, _ := json.Marshal(effective)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
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
	if err := slackAgentAPIGet(ctx, token, "conversations.replies", map[string]any{
		"channel": channelID, "ts": threadID, "limit": 100,
	}, &response); err != nil {
		return "", err
	}
	messages := response.Messages
	if len(messages) > hostedAgentThreadMsgCap {
		messages = messages[len(messages)-hostedAgentThreadMsgCap:]
	}
	// Fill the bounded context newest-first, then reverse it for the model. A
	// long thread must lose its oldest messages, not the replies immediately
	// preceding the current request.
	reversed := make([]string, 0, len(messages))
	total := 0
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
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
		reversed = append(reversed, line)
		total += len(line)
	}
	lines := make([]string, len(reversed))
	for index := range reversed {
		lines[len(reversed)-1-index] = reversed[index]
	}
	return strings.Join(lines, "\n"), nil
}

// slackAgentAPIBase is a var so tests can drive these calls against a stub.
// Same seam the rest of the repo uses (asanaAPIURL, mondayAPIURL).
var slackAgentAPIBase = "https://slack.com/api/"

func slackAgentAPICall(ctx context.Context, token, method string, payload map[string]any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Slack API %s payload: %w", method, err)
	}
	return slackAgentAPIRequest(ctx, token, http.MethodPost, slackAgentAPIBase+method, strings.NewReader(string(body)), method, out)
}

// Slack documents its read-only Web API methods as GET endpoints. Sending
// their arguments as JSON can yield ok:false invalid_arguments even though the
// HTTP response is 200, because some workspaces ignore the POST body for those
// methods. Query encoding also keeps required arguments intact for channel
// validation and thread reads.
func slackAgentAPIGet(ctx context.Context, token, method string, payload map[string]any, out any) error {
	query := url.Values{}
	for key, value := range payload {
		var encoded string
		switch typed := value.(type) {
		case string:
			encoded = typed
		case bool:
			encoded = strconv.FormatBool(typed)
		case int:
			encoded = strconv.Itoa(typed)
		case int64:
			encoded = strconv.FormatInt(typed, 10)
		default:
			return fmt.Errorf("Slack API %s query parameter %q has unsupported type %T", method, key, value)
		}
		query.Set(key, encoded)
	}
	endpoint := slackAgentAPIBase + method
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	return slackAgentAPIRequest(ctx, token, http.MethodGet, endpoint, nil, method, out)
}

func slackAgentAPIRequest(ctx context.Context, token, requestMethod, endpoint string, body io.Reader, slackMethod string, out any) error {
	request, err := http.NewRequestWithContext(ctx, requestMethod, endpoint, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	response, err := slackAgentHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Slack API %s returned %d", slackMethod, response.StatusCode)
	}
	var status struct {
		OK               bool   `json:"ok"`
		Error            string `json:"error"`
		Needed           string `json:"needed"`
		Provided         string `json:"provided"`
		ResponseMetadata struct {
			Messages []string `json:"messages"`
		} `json:"response_metadata"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return fmt.Errorf("Slack API %s returned invalid JSON", slackMethod)
	}
	if !status.OK {
		details := make([]string, 0, len(status.ResponseMetadata.Messages)+2)
		for _, message := range status.ResponseMetadata.Messages {
			if message = strings.TrimSpace(message); message != "" {
				details = append(details, truncate(message, 300))
			}
		}
		if status.Needed != "" {
			details = append(details, "needed scopes: "+truncate(status.Needed, 200))
		}
		if status.Provided != "" {
			details = append(details, "provided scopes: "+truncate(status.Provided, 200))
		}
		reason := status.Error
		if reason == "" {
			reason = "unknown_error"
		}
		if len(details) > 0 {
			reason += " (" + strings.Join(details, "; ") + ")"
		}
		return fmt.Errorf("Slack API %s failed: %s", slackMethod, reason)
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
	if deliveryID := slackAgentDeliveryID(ctx); deliveryID != "" {
		payload["client_msg_id"] = deliveryID
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
				slackAgentEscape(humanizeAgentOperation(approval.Operation)), truncate(slackAgentEscape(approval.Reason), 1000), approval.RequesterExternalID)},
		},
	}
	detailRunes := []rune(string(detailJSON))
	for start := 0; start < len(detailRunes); start += 2400 {
		end := min(start+2400, len(detailRunes))
		blocks = append(blocks, map[string]any{
			"type": "section", "text": map[string]any{
				"type": "mrkdwn", "text": "```" + slackAgentEscape(string(detailRunes[start:end])) + "```",
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
		"text": slackAgentSanitizeGeneratedText(fmt.Sprintf(
			"Approval required for %s: %s", humanizeAgentOperation(approval.Operation), truncate(approval.Reason, 1000))),
		"blocks": blocks,
	}
	if len(agentName) > 0 && strings.TrimSpace(agentName[0]) != "" {
		payload["username"] = truncate(strings.TrimSpace(agentName[0]), 80)
	}
	if deliveryID := slackAgentDeliveryID(ctx); deliveryID != "" {
		payload["client_msg_id"] = deliveryID
	}
	return slackAgentAPICall(ctx, token, "chat.postMessage", payload, nil)
}

func slackAgentPostUnknownOutcome(ctx context.Context, token, channelID, threadID string, approval *models.HostedAgentApproval, agentName ...string) error {
	var details map[string]any
	if json.Unmarshal(approval.DisplayDetails, &details) != nil {
		details = map[string]any{"details": "The saved approval details could not be displayed."}
	}
	detailJSON, _ := json.MarshalIndent(details, "", "  ")
	executionKey := approval.ExecutionKey
	if executionKey == "" {
		executionKey = hostedApprovalExecutionKey(approval.ID.String())
	}
	blocks := []map[string]any{
		{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": fmt.Sprintf(
				"*Outcome needs confirmation*\n*Operation:* %s\n*Why it ran:* %s\n*Execution ID:* `%s`\n\nFernary cannot prove whether this external action completed, so equivalent actions are blocked. <@%s>, verify the target system and record what happened.",
				slackAgentEscape(humanizeAgentOperation(approval.Operation)), truncate(slackAgentEscape(approval.Reason), 600),
				slackAgentEscape(executionKey), approval.RequesterExternalID)},
		},
		{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn", "text": "*Exact approved details*\n```" + truncate(slackAgentEscape(string(detailJSON)), 2200) + "```",
			},
		},
		{
			"type": "actions",
			"elements": []map[string]any{
				{"type": "button", "action_id": "fernary_agent_outcome_completed", "text": map[string]string{"type": "plain_text", "text": "It completed"}, "style": "primary", "value": approval.ID.String()},
				{"type": "button", "action_id": "fernary_agent_outcome_not_run", "text": map[string]string{"type": "plain_text", "text": "It did not run"}, "value": approval.ID.String()},
			},
		},
	}
	payload := map[string]any{
		"channel": channelID, "thread_ts": threadID,
		"text":   "This action has an unknown outcome. Equivalent actions are blocked until its requester reconciles it.",
		"blocks": blocks,
	}
	if len(agentName) > 0 && strings.TrimSpace(agentName[0]) != "" {
		payload["username"] = truncate(strings.TrimSpace(agentName[0]), 80)
	}
	if deliveryID := slackAgentDeliveryID(ctx); deliveryID != "" {
		payload["client_msg_id"] = deliveryID
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
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
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
	if approval.OrganizationID != deployment.OrganizationID || approval.DeploymentID != deployment.ID.String() ||
		thread.DeploymentID != deployment.ID.String() || host.ExternalWorkspaceID != payload.WorkspaceID || thread.ExternalChannelID != payload.ChannelID ||
		(payload.ThreadID != "" && thread.ExternalThreadID != payload.ThreadID) ||
		host.OrganizationID != deployment.OrganizationID || thread.OrganizationID != deployment.OrganizationID {
		return errors.New("approval interaction does not match its Slack thread")
	}
	if payload.RequesterID != approval.RequesterExternalID {
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"Only the teammate who requested this action can approve, reject, or reconcile it.", deployment.Name)
	}
	if payload.Action == "reconcile_completed" || payload.Action == "reconcile_not_run" {
		return h.reconcileSlackAgentApprovalOutcome(ctx, &approval, &deployment, &host, &thread, payload)
	}
	switch approval.Status {
	case models.HostedAgentApprovalExecuted:
		// The tool result is durable before it is merged into the session or sent
		// to Slack. A delivery retry can therefore finish either interrupted step
		// without executing the external mutation again.
		return h.replaySlackAgentApprovalOutcome(ctx, &approval, &deployment, &host, &thread)
	case models.HostedAgentApprovalExecuting:
		// Interactions for one thread hold the advisory lock. Seeing executing here
		// means a prior worker stopped after the claim; it is not a concurrent live
		// execution. Recover a known successful result from the outcome cache before
		// treating a crash with no recorded result as genuinely indeterminate.
		return h.recoverExecutingSlackAgentApproval(ctx, &approval, &deployment, &host, &thread)
	case models.HostedAgentApprovalOutcomeUnknown:
		return slackAgentPostUnknownOutcome(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			&approval, deployment.Name)
	case models.HostedAgentApprovalPending:
		// Continue below.
	default:
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"That approval has already been resolved.", deployment.Name)
	}
	if time.Now().After(approval.ExpiresAt) {
		now := time.Now().UTC()
		result := h.db.DB.Model(&approval).Where("status = ?", models.HostedAgentApprovalPending).
			Updates(map[string]any{"status": models.HostedAgentApprovalExpired, "resolved_at": now})
		if result.Error != nil {
			return result.Error
		}
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
		if result.RowsAffected != 1 {
			return errors.New("approval was resolved concurrently")
		}
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"<@"+payload.RequesterID+"> rejected the action. Nothing was changed.", deployment.Name)
	}
	return h.executeSlackAgentApproval(ctx, &approval, &deployment, &host, &thread)
}

func (h *WorkflowHandler) reconcileSlackAgentApprovalOutcome(ctx context.Context, approval *models.HostedAgentApproval, deployment *models.AgentDeployment, host *models.AgentHostInstallation, thread *models.HostedAgentThread, payload hostedSlackInteractionPayload) error {
	completed := payload.Action == "reconcile_completed"
	reconciled, err := reconcileHostedAgentApprovalOn(h.db.DB.WithContext(ctx), approval, deployment, payload.RequesterID, completed)
	if err != nil {
		return err
	}
	if !reconciled {
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"That outcome has already been resolved.", deployment.Name)
	}
	message := "Recorded as not completed. A fresh equivalent request can now be reviewed and approved."
	if completed {
		message = "Recorded as completed. Fernary will not retry that execution; a future equivalent request will be treated as a new action."
	}
	return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
		"<@"+payload.RequesterID+"> "+message, deployment.Name)
}

// reconcileHostedAgentApprovalOn records the requester's verified outcome and
// appends that durable fact to the agent session. It takes the same deployment
// row lock used by approval creation and claiming, so no equivalent call can
// pass the unresolved check while reconciliation is in flight.
func reconcileHostedAgentApprovalOn(db *gorm.DB, approval *models.HostedAgentApproval, deployment *models.AgentDeployment, requesterID string, completed bool) (bool, error) {
	reconciled := false
	err := db.Transaction(func(tx *gorm.DB) error {
		var liveDeployment models.AgentDeployment
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", deployment.ID, deployment.OrganizationID).
			First(&liveDeployment).Error; err != nil {
			return err
		}
		var durable models.HostedAgentApproval
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND deployment_id = ? AND organization_id = ?", approval.ID, deployment.ID, deployment.OrganizationID).
			First(&durable).Error; err != nil {
			return err
		}
		approval.Status = durable.Status
		if durable.Status != models.HostedAgentApprovalOutcomeUnknown {
			return nil
		}
		if requesterID == "" || requesterID != durable.RequesterExternalID {
			return errors.New("only the original requester can reconcile this approval")
		}

		now := time.Now().UTC()
		executionKey := durable.ExecutionKey
		if executionKey == "" {
			executionKey = hostedApprovalExecutionKey(durable.ID.String())
		}
		status := models.HostedAgentApprovalReconciledVoid
		historyMessage := fmt.Sprintf(
			"The requester verified that %s did not complete externally. It is safe to request the action again.",
			humanizeAgentOperation(durable.Operation),
		)
		if completed {
			status = models.HostedAgentApprovalReconciledDone
			historyMessage = fmt.Sprintf(
				"The requester verified that %s completed externally, but Fernary could not recover its result. Execution ID: %s. Do not treat this as a failed action or retry it automatically.",
				humanizeAgentOperation(durable.Operation), executionKey,
			)
		}

		var session models.ChatSession
		sessionErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ?", durable.ChatSessionID).Error
		if sessionErr == nil {
			if session.OrganizationID != durable.OrganizationID || session.OrganizationID != deployment.OrganizationID ||
				session.UserID != deployment.DeployedByUserID || session.WorkflowID != deployment.WorkflowID {
				return errors.New("hosted approval session boundary mismatch")
			}
			var history []agentStoredMessage
			_ = json.Unmarshal(session.Messages, &history)
			history = append(history, agentStoredMessage{Role: "assistant", Content: historyMessage})
			historyJSON, _ := json.Marshal(boundedAgentHistory(history, 20, 6000))
			if err := tx.Model(&session).Update("messages", models.JSONB(historyJSON)).Error; err != nil {
				return err
			}
		} else if !errors.Is(sessionErr, gorm.ErrRecordNotFound) {
			return sessionErr
		}

		updates := map[string]any{
			"status": status, "outcome_reconciled_at": now, "outcome_reconciled_by": requesterID,
		}
		if durable.ExecutionKey == "" {
			updates["execution_key"] = executionKey
		}
		if sessionErr == nil {
			updates["session_recorded_at"] = now
		}
		result := tx.Model(&models.HostedAgentApproval{}).
			Where("id = ? AND status = ?", durable.ID, models.HostedAgentApprovalOutcomeUnknown).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("hosted approval outcome was concurrently reconciled")
		}
		approval.Status = status
		approval.OutcomeReconciledAt = &now
		approval.OutcomeReconciledBy = requesterID
		if sessionErr == nil {
			approval.SessionRecordedAt = &now
		}
		reconciled = true
		return nil
	})
	return reconciled, err
}

func (h *WorkflowHandler) executeSlackAgentApproval(ctx context.Context, approval *models.HostedAgentApproval, deployment *models.AgentDeployment, host *models.AgentHostInstallation, thread *models.HostedAgentThread) error {
	if approval.DeploymentVersion != deployment.Version || deployment.Status != models.AgentDeploymentActive {
		err := errors.New("deployment changed or is no longer active")
		if persistErr := h.failHostedApproval(approval, err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID, err.Error(), deployment.Name)
	}
	ast, node, err := hostedApprovalSnapshot(deployment, approval)
	if err != nil {
		if persistErr := h.failHostedApproval(approval, err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		return err
	}
	var policy AgentCapabilityPolicy
	var overrides map[string]any
	if json.Unmarshal(deployment.CapabilityPolicy, &policy) != nil || json.Unmarshal(approval.EffectiveOverrides, &overrides) != nil {
		err = errors.New("approved call is unreadable")
		if persistErr := h.failHostedApproval(approval, err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		return err
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
		if persistErr := h.failHostedApproval(approval, err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		return err
	}
	if approval.ConfigRecordedAt == nil {
		err = errors.New("approval predates pinned execution details; request the action again")
		if persistErr := h.failHostedApproval(approval, err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		return err
	}
	var effective executor.FlowNodeData
	if err := json.Unmarshal(approval.ExecutionConfig, &effective); err != nil {
		if persistErr := h.failHostedApproval(approval, err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		return errors.New("approved execution details are unreadable")
	}
	effectiveJSON, _ := json.Marshal(effective)
	hash := sha256.Sum256(effectiveJSON)
	if hex.EncodeToString(hash[:]) != approval.EffectiveConfigHash {
		err = errors.New("approved call hash does not match the pinned operation")
		if persistErr := h.failHostedApproval(approval, err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		return err
	}

	var session models.ChatSession
	if err := h.db.DB.First(&session, "id = ?", approval.ChatSessionID).Error; err != nil {
		return err
	}
	if approval.ChatSessionID != thread.ChatSessionID ||
		session.OrganizationID != approval.OrganizationID || session.OrganizationID != deployment.OrganizationID ||
		session.UserID != deployment.DeployedByUserID || session.WorkflowID != deployment.WorkflowID {
		err = errors.New("hosted approval session boundary mismatch")
		if persistErr := h.failHostedApproval(approval, err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
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

	var (
		claimed bool
		out     string
		toolErr error
	)
	fingerprint := approval.ExecutionFingerprint
	if fingerprint == "" {
		fingerprint = approval.NodeID + ":" + approval.EffectiveConfigHash
	}
	lockErr := withHostedAdvisoryLocks(ctx, h.db.DB, []string{
		"approval-execution:" + approval.OrganizationID + ":" + approval.Operation + ":" + fingerprint,
		"authority:" + deployment.OrganizationID + ":" + deployment.DeployedByUserID,
	}, func(connection *gorm.DB) error {
		if err := verifyHostedApprovalCredentialOn(connection, approval, deployment); err != nil {
			return err
		}
		var claimErr error
		claimed, claimErr = h.claimHostedAgentApprovalOn(connection, approval, deployment)
		if claimErr != nil || !claimed {
			return claimErr
		}
		keys := executor.APIKeys{
			Anthropic: os.Getenv("ANTHROPIC_API_KEY"), OpenAI: os.Getenv("OPENAI_API_KEY"),
			Brave: os.Getenv("BRAVE_API_KEY"), Jina: os.Getenv("JINA_API_KEY"),
		}
		executionNode := *node
		executionNode.Data = effective
		out, toolErr = executor.ExecuteSingleNode(ctx, executionNode, nil, map[string]string{}, ast.Edges, keys,
			approval.ExecutionKey, deployment.DeployedByUserID, deployment.OrganizationID, nil)
		if toolErr != nil {
			// Once execution has been dispatched, an error is not proof that the
			// external mutation did not happen (for example, the provider may have
			// committed it before our response timed out). Keep equivalent calls
			// blocked until the original requester reconciles the outcome.
			slog.ErrorContext(ctx, "hosted approval execution returned an indeterminate error",
				"approval_id", approval.ID, "execution_key", approval.ExecutionKey, "error", toolErr)
			return h.markHostedApprovalOutcomeUnknownOn(connection, approval,
				"the external action returned an error after execution started; it may have completed and must not be retried automatically")
		}
		if cacheErr := h.rememberHostedApprovalOutcome(ctx, approval.ID.String(), out); cacheErr != nil {
			// The in-process copy still covers a transient PostgreSQL error. Log
			// loss of the cross-replica copy, then attempt the authoritative write.
			slog.WarnContext(ctx, "could not cache successful hosted approval outcome",
				"approval_id", approval.ID, "error", cacheErr)
		}
		return h.recordHostedAgentApprovalExecutionOn(connection, approval, out)
	})
	var unresolvedErr *hostedApprovalOutcomeUnresolvedError
	if errors.As(lockErr, &unresolvedErr) {
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"This action is blocked because an equivalent action may already have completed. Its original requester must reconcile that outcome before this approval can run.", deployment.Name)
	}
	if errors.Is(lockErr, errHostedAgentAuthorityEnded) {
		if persistErr := h.failHostedApproval(approval, lockErr); persistErr != nil {
			return errors.Join(lockErr, persistErr)
		}
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"This deployment can no longer act because its owner is not a current organization member.", deployment.Name)
	}
	if errors.Is(lockErr, errHostedCredentialChanged) {
		if persistErr := h.failHostedApproval(approval, lockErr); persistErr != nil {
			return errors.Join(lockErr, persistErr)
		}
		return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			"The deployment owner's connected account changed after this approval was requested. Nothing was run; mention the agent again to review a fresh action.", deployment.Name)
	}
	if lockErr != nil {
		return lockErr
	}
	if !claimed {
		return nil
	}
	if toolErr != nil {
		return slackAgentPostUnknownOutcome(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
			approval, deployment.Name)
	}
	h.forgetHostedApprovalOutcome(ctx, approval.ID.String())
	if err := h.syncHostedAgentApprovalSession(approval, deployment, node.Data.Label); err != nil {
		return err
	}
	return postSlackAgentApprovalSuccess(ctx, host, thread, deployment, approval)
}

func hostedApprovalSnapshot(deployment *models.AgentDeployment, approval *models.HostedAgentApproval) (executor.WorkflowAST, *executor.WorkflowASTNode, error) {
	if approval.DeploymentVersion != deployment.Version {
		return executor.WorkflowAST{}, nil, errors.New("approval deployment version no longer matches its snapshot")
	}
	ast := executor.WorkflowAST{Name: deployment.SnapshotName}
	if json.Unmarshal(deployment.SnapshotNodes, &ast.Nodes) != nil || json.Unmarshal(deployment.SnapshotEdges, &ast.Edges) != nil {
		return executor.WorkflowAST{}, nil, errors.New("deployment snapshot is unreadable")
	}
	for i := range ast.Nodes {
		if ast.Nodes[i].ID == approval.NodeID {
			return ast, &ast.Nodes[i], nil
		}
	}
	return executor.WorkflowAST{}, nil, errors.New("approved node is absent from the deployment snapshot")
}

// claimHostedAgentApproval reloads the live deployment and membership in the
// same transaction that changes pending -> executing. Production execution
// calls the On variant while holding the deployer's authority advisory lock
// through the external call and outcome persistence; member removal takes the
// same lock before revoking authority.
func (h *WorkflowHandler) claimHostedAgentApproval(approval *models.HostedAgentApproval, deployment *models.AgentDeployment) (bool, error) {
	return h.claimHostedAgentApprovalOn(h.db.DB, approval, deployment)
}

func (h *WorkflowHandler) claimHostedAgentApprovalOn(db *gorm.DB, approval *models.HostedAgentApproval, deployment *models.AgentDeployment) (bool, error) {
	claimed := false
	now := time.Now().UTC()
	executionKey := approval.ExecutionKey
	if executionKey == "" {
		executionKey = hostedApprovalExecutionKey(approval.ID.String())
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		var live models.AgentDeployment
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ? AND deployed_by_user_id = ? AND version = ? AND status = ?",
				deployment.ID, deployment.OrganizationID, deployment.DeployedByUserID,
				approval.DeploymentVersion, models.AgentDeploymentActive).
			First(&live).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errHostedAgentAuthorityEnded
			}
			return err
		}
		var member models.OrgMember
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND user_id = ?", deployment.OrganizationID, deployment.DeployedByUserID).
			First(&member).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errHostedAgentAuthorityEnded
			}
			return err
		}
		unresolved, err := findEquivalentUnresolvedHostedApproval(tx, approval)
		if err != nil {
			return err
		}
		if unresolved != nil {
			return &hostedApprovalOutcomeUnresolvedError{
				ApprovalID: unresolved.ID.String(), Operation: unresolved.Operation,
			}
		}
		result := tx.Model(&models.HostedAgentApproval{}).
			Where("id = ? AND deployment_id = ? AND status = ?", approval.ID, deployment.ID, models.HostedAgentApprovalPending).
			Updates(map[string]any{
				"status": models.HostedAgentApprovalExecuting, "resolved_at": now,
				"execution_key": executionKey, "execution_started_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		claimed = result.RowsAffected == 1
		return nil
	})
	if claimed {
		approval.Status = models.HostedAgentApprovalExecuting
		approval.ResolvedAt = &now
		approval.ExecutionKey = executionKey
		approval.ExecutionStartedAt = &now
	}
	return claimed, err
}

// recordHostedAgentApprovalExecution commits the external outcome before any
// session or Slack write. Once this succeeds, every later step is retryable and
// must use the stored result instead of calling the tool again.
func (h *WorkflowHandler) recordHostedAgentApprovalExecution(approval *models.HostedAgentApproval, output string) error {
	return h.recordHostedAgentApprovalExecutionOn(h.db.DB, approval, output)
}

func (h *WorkflowHandler) recordHostedAgentApprovalExecutionOn(db *gorm.DB, approval *models.HostedAgentApproval, output string) error {
	now := time.Now().UTC()
	stored := truncate(output, agentResultCap)
	result := db.Model(&models.HostedAgentApproval{}).
		Where("id = ? AND status = ?", approval.ID, models.HostedAgentApprovalExecuting).
		Updates(map[string]any{
			"status":                       models.HostedAgentApprovalExecuted,
			"executed_at":                  now,
			"execution_result":             stored,
			"execution_result_recorded_at": now,
			"last_error":                   "",
		})
	if result.Error != nil {
		return fmt.Errorf("record successful hosted approval: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return errors.New("successful hosted approval could not be recorded from its executing state")
	}
	approval.Status = models.HostedAgentApprovalExecuted
	approval.ExecutedAt = &now
	approval.ExecutionResult = stored
	approval.ExecutionResultRecordedAt = &now
	return nil
}

func hostedApprovalExecutionKey(approvalID string) string {
	// Keep this UUID-shaped: it is also propagated as the execution run ID and
	// credit-ledger provenance uses a native PostgreSQL UUID column.
	return approvalID
}

func hostedApprovalOutcomeKey(approvalID string) string {
	return "hosted-agent:approval-outcome:" + approvalID
}

// rememberHostedApprovalOutcome keeps the known successful result outside
// PostgreSQL before attempting the authoritative approval update. Memory covers
// a transient write failure in this process; Redis lets another replica or a
// restarted worker finish persistence without replaying the external mutation.
func (h *WorkflowHandler) rememberHostedApprovalOutcome(ctx context.Context, approvalID, output string) error {
	outcome := hostedApprovalRecoverableOutcome{
		Output: truncate(output, agentResultCap), RecordedAt: time.Now().UTC(),
	}
	hostedApprovalOutcomeMemory.Store(approvalID, outcome)
	if h.redis == nil {
		return nil
	}
	raw, _ := json.Marshal(outcome)
	cacheCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return h.redis.Set(cacheCtx, hostedApprovalOutcomeKey(approvalID), raw, hostedApprovalOutcomeTTL).Err()
}

func (h *WorkflowHandler) recoverHostedApprovalOutcome(ctx context.Context, approvalID string) (hostedApprovalRecoverableOutcome, bool, error) {
	if cached, ok := hostedApprovalOutcomeMemory.Load(approvalID); ok {
		if outcome, valid := cached.(hostedApprovalRecoverableOutcome); valid {
			return outcome, true, nil
		}
		hostedApprovalOutcomeMemory.Delete(approvalID)
	}
	if h.redis == nil {
		return hostedApprovalRecoverableOutcome{}, false, nil
	}
	cacheCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	raw, err := h.redis.Get(cacheCtx, hostedApprovalOutcomeKey(approvalID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return hostedApprovalRecoverableOutcome{}, false, nil
	}
	if err != nil {
		return hostedApprovalRecoverableOutcome{}, false, err
	}
	var outcome hostedApprovalRecoverableOutcome
	if err := json.Unmarshal(raw, &outcome); err != nil {
		return hostedApprovalRecoverableOutcome{}, false, err
	}
	hostedApprovalOutcomeMemory.Store(approvalID, outcome)
	return outcome, true, nil
}

func (h *WorkflowHandler) forgetHostedApprovalOutcome(ctx context.Context, approvalID string) {
	hostedApprovalOutcomeMemory.Delete(approvalID)
	if h.redis == nil {
		return
	}
	cacheCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := h.redis.Del(cacheCtx, hostedApprovalOutcomeKey(approvalID)).Err(); err != nil {
		slog.WarnContext(ctx, "could not clear recovered hosted approval outcome",
			"approval_id", approvalID, "error", err)
	}
}

// syncHostedAgentApprovalSession merges an already-durable execution into the
// chat transcript exactly once. The session update and marker share a
// transaction, so an error rolls both back and the delivery retry can replay it.
func (h *WorkflowHandler) syncHostedAgentApprovalSession(approval *models.HostedAgentApproval, deployment *models.AgentDeployment, nodeLabel string) error {
	return h.db.DB.Transaction(func(tx *gorm.DB) error {
		var durable models.HostedAgentApproval
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&durable, "id = ?", approval.ID).Error; err != nil {
			return err
		}
		if durable.Status != models.HostedAgentApprovalExecuted {
			return fmt.Errorf("cannot record session outcome for approval in %s state", durable.Status)
		}
		if durable.SessionRecordedAt != nil {
			approval.SessionRecordedAt = durable.SessionRecordedAt
			return nil
		}

		recordedAt := time.Now().UTC()
		// Executed approvals from before execution_result was introduced already
		// updated the session in the same transaction as their status. Mark them as
		// reconciled without appending a duplicate legacy message.
		if durable.ExecutionResultRecordedAt == nil {
			if err := tx.Model(&durable).Update("session_recorded_at", recordedAt).Error; err != nil {
				return err
			}
			approval.SessionRecordedAt = &recordedAt
			return nil
		}

		var session models.ChatSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ?", durable.ChatSessionID).Error; err != nil {
			return err
		}
		if session.OrganizationID != durable.OrganizationID || session.OrganizationID != deployment.OrganizationID ||
			session.UserID != deployment.DeployedByUserID || session.WorkflowID != deployment.WorkflowID {
			return errors.New("hosted approval session boundary mismatch")
		}
		state := map[string]string{}
		_ = json.Unmarshal(session.State, &state)
		state[durable.NodeID] = truncate(durable.ExecutionResult, agentStateCap)
		stateJSON, _ := json.Marshal(state)
		var history []agentStoredMessage
		_ = json.Unmarshal(session.Messages, &history)
		history = append(history, agentStoredMessage{
			Role: "assistant",
			Content: fmt.Sprintf("The requester approved %s and it completed successfully. Result: %s",
				humanizeAgentOperation(durable.Operation), truncate(durable.ExecutionResult, agentResultCap)),
			ToolCalls: []agentToolCallRecord{{
				Node: nodeLabel, NodeID: durable.NodeID, Op: humanizeAgentOperation(durable.Operation), Status: "ok",
			}},
		})
		history = boundedAgentHistory(history, 20, 6000)
		historyJSON, _ := json.Marshal(history)
		if err := tx.Model(&session).Updates(map[string]any{
			"state": models.JSONB(stateJSON), "messages": models.JSONB(historyJSON),
		}).Error; err != nil {
			return err
		}
		result := tx.Model(&durable).Where("session_recorded_at IS NULL").
			Update("session_recorded_at", recordedAt)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("hosted approval session outcome was concurrently recorded")
		}
		approval.SessionRecordedAt = &recordedAt
		return nil
	})
}

func (h *WorkflowHandler) replaySlackAgentApprovalOutcome(ctx context.Context, approval *models.HostedAgentApproval, deployment *models.AgentDeployment, host *models.AgentHostInstallation, thread *models.HostedAgentThread) error {
	nodeLabel := approval.NodeID
	if approval.ExecutionResultRecordedAt != nil {
		_, node, err := hostedApprovalSnapshot(deployment, approval)
		if err != nil {
			return err
		}
		nodeLabel = node.Data.Label
	}
	if err := h.syncHostedAgentApprovalSession(approval, deployment, nodeLabel); err != nil {
		return err
	}
	return postSlackAgentApprovalSuccess(ctx, host, thread, deployment, approval)
}

func (h *WorkflowHandler) recoverExecutingSlackAgentApproval(ctx context.Context, approval *models.HostedAgentApproval, deployment *models.AgentDeployment, host *models.AgentHostInstallation, thread *models.HostedAgentThread) error {
	outcome, found, err := h.recoverHostedApprovalOutcome(ctx, approval.ID.String())
	if err != nil {
		// A cache outage is not evidence that the external outcome is unknown.
		// Leave the approval executing so a later delivery can recover it.
		return fmt.Errorf("recover successful hosted approval outcome: %w", err)
	}
	if !found {
		return h.markSlackAgentApprovalOutcomeUnknown(ctx, approval, deployment, host, thread)
	}
	if err := h.recordHostedAgentApprovalExecution(approval, outcome.Output); err != nil {
		return err
	}
	h.forgetHostedApprovalOutcome(ctx, approval.ID.String())
	return h.replaySlackAgentApprovalOutcome(ctx, approval, deployment, host, thread)
}

func (h *WorkflowHandler) markSlackAgentApprovalOutcomeUnknown(ctx context.Context, approval *models.HostedAgentApproval, deployment *models.AgentDeployment, host *models.AgentHostInstallation, thread *models.HostedAgentThread) error {
	message := "execution was interrupted after it started; the external action may have completed and must not be retried automatically"
	if err := h.markHostedApprovalOutcomeUnknownOn(h.db.DB, approval, message); err != nil {
		return err
	}
	return slackAgentPostUnknownOutcome(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID,
		approval, deployment.Name)
}

func (h *WorkflowHandler) markHostedApprovalOutcomeUnknownOn(db *gorm.DB, approval *models.HostedAgentApproval, message string) error {
	result := db.Model(&models.HostedAgentApproval{}).
		Where("id = ? AND status = ?", approval.ID, models.HostedAgentApprovalExecuting).
		Updates(map[string]any{
			"status": models.HostedAgentApprovalOutcomeUnknown, "last_error": message,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("indeterminate hosted approval could not be recorded from its executing state")
	}
	approval.Status = models.HostedAgentApprovalOutcomeUnknown
	approval.LastError = message
	return nil
}

func postSlackAgentApprovalSuccess(ctx context.Context, host *models.AgentHostInstallation, thread *models.HostedAgentThread, deployment *models.AgentDeployment, approval *models.HostedAgentApproval) error {
	message := fmt.Sprintf("Approved action completed: *%s*", slackAgentEscape(approval.Operation))
	if approval.ExecutionResultRecordedAt != nil {
		message += fmt.Sprintf("\n```%s```", slackAgentEscape(truncate(approval.ExecutionResult, 3000)))
	}
	return slackAgentPostText(ctx, host.BotToken, thread.ExternalChannelID, thread.ExternalThreadID, message, deployment.Name)
}

func (h *WorkflowHandler) failHostedApproval(approval *models.HostedAgentApproval, err error) error {
	return h.failHostedApprovalOn(h.db.DB, approval, err)
}

func (h *WorkflowHandler) failHostedApprovalOn(db *gorm.DB, approval *models.HostedAgentApproval, err error) error {
	now := time.Now().UTC()
	result := db.Model(&models.HostedAgentApproval{}).
		Where("id = ? AND status IN ?", approval.ID, []models.HostedAgentApprovalStatus{
			models.HostedAgentApprovalPending, models.HostedAgentApprovalExecuting,
		}).Updates(map[string]any{
		"status": models.HostedAgentApprovalFailed, "resolved_at": now,
		"last_error": truncate(err.Error(), 2000),
	})
	if result.Error != nil {
		return fmt.Errorf("record failed hosted approval: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return errors.New("failed hosted approval could not be recorded from its current state")
	}
	approval.Status = models.HostedAgentApprovalFailed
	approval.ResolvedAt = &now
	approval.LastError = truncate(err.Error(), 2000)
	return nil
}
