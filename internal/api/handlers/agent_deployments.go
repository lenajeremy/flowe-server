package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var agentAliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)
var slackChannelIDPattern = regexp.MustCompile(`^[CG][A-Z0-9]+$`)

type AgentPermissionSummary struct {
	Goal                    string   `json:"goal,omitempty"`
	CanRead                 []string `json:"canRead"`
	WritesRequiringApproval []string `json:"writesRequiringApproval"`
	FixedSettings           []string `json:"fixedSettings"`
	Warnings                []string `json:"warnings"`
}

type AgentPermissionAnalysis struct {
	AnalysisID string                 `json:"analysisId"`
	Goal       string                 `json:"goal"`
	Summary    string                 `json:"summary"`
	Policy     AgentCapabilityPolicy  `json:"policy"`
	Review     AgentPermissionSummary `json:"review"`
	Warnings   []string               `json:"warnings"`
	Source     string                 `json:"source"`
}

type agentDeploymentChannelInput struct {
	ID   string `json:"id" binding:"required"`
	Name string `json:"name"`
}

type createAgentDeploymentRequest struct {
	Name               string                        `json:"name" binding:"required"`
	Alias              string                        `json:"alias" binding:"required"`
	HostInstallationID string                        `json:"hostInstallationId" binding:"required"`
	AnalysisID         string                        `json:"analysisId" binding:"required"`
	ModelID            string                        `json:"modelId"`
	Policy             AgentCapabilityPolicy         `json:"policy"`
	Channels           []agentDeploymentChannelInput `json:"channels"`
}

type agentDeploymentResponse struct {
	Deployment models.AgentDeployment         `json:"deployment"`
	Targets    []models.AgentDeploymentTarget `json:"targets"`
	Review     AgentPermissionSummary         `json:"review"`
}

func workflowASTFromModel(workflow *models.Workflow) (executor.WorkflowAST, error) {
	ast := executor.WorkflowAST{Name: workflow.Name}
	if err := json.Unmarshal(workflow.Nodes, &ast.Nodes); err != nil {
		return executor.WorkflowAST{}, fmt.Errorf("workflow nodes unreadable: %w", err)
	}
	if err := json.Unmarshal(workflow.Edges, &ast.Edges); err != nil {
		return executor.WorkflowAST{}, fmt.Errorf("workflow edges unreadable: %w", err)
	}
	return ast, nil
}

func agentSnapshotHash(name string, nodes, edges []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(name))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(nodes)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(edges)
	return hex.EncodeToString(hash.Sum(nil))
}

func summarizeAgentPolicy(ast executor.WorkflowAST, policy AgentCapabilityPolicy, goal string, warnings []string) AgentPermissionSummary {
	byNode := map[string]executor.WorkflowASTNode{}
	for _, node := range ast.Nodes {
		byNode[node.ID] = node
	}
	summary := AgentPermissionSummary{
		Goal:                    strings.TrimSpace(goal),
		CanRead:                 []string{},
		WritesRequiringApproval: []string{},
		FixedSettings:           []string{},
		Warnings:                append([]string{}, warnings...),
	}
	for _, grant := range policy.Nodes {
		node, exists := byNode[grant.NodeID]
		if !exists {
			continue
		}
		capability, exists := agentNodeCapability(node)
		if !exists {
			continue
		}
		operations := map[string]AgentOperationCapability{}
		for _, operation := range capability.Operations {
			operations[operation.ID] = operation
		}
		for _, operationID := range grant.AllowedOperations {
			operation, exists := operations[operationID]
			if !exists {
				continue
			}
			line := fmt.Sprintf("%s can %s", node.Data.Label, strings.ToLower(operation.Label))
			if operation.Effect == AgentEffectRead {
				summary.CanRead = append(summary.CanRead, line)
			} else {
				summary.WritesRequiringApproval = append(summary.WritesRequiringApproval, line+" after the requesting teammate approves it")
			}
		}
		if len(grant.AllowedOverrideFields) == 0 {
			summary.FixedSettings = append(summary.FixedSettings,
				fmt.Sprintf("%s uses its deployed settings exactly", node.Data.Label))
		} else {
			fieldLabels := make([]string, 0, len(grant.AllowedOverrideFields))
			for _, field := range grant.AllowedOverrideFields {
				fieldLabels = append(fieldLabels, humanizeAgentField(field))
			}
			summary.FixedSettings = append(summary.FixedSettings,
				fmt.Sprintf("%s stays fixed except: %s", node.Data.Label, strings.Join(fieldLabels, ", ")))
		}
	}
	sort.Strings(summary.CanRead)
	sort.Strings(summary.WritesRequiringApproval)
	sort.Strings(summary.FixedSettings)
	return summary
}

func humanizeAgentField(field string) string {
	var words strings.Builder
	for i, r := range field {
		if i > 0 && r >= 'A' && r <= 'Z' {
			words.WriteByte(' ')
		}
		words.WriteRune(r)
	}
	label := words.String()
	if label == "" {
		return field
	}
	return strings.ToUpper(label[:1]) + label[1:]
}

// AgentDeploymentCapabilities exposes the deterministic catalog and its
// closed, read-only fallback without spending model credits.
func (h *WorkflowHandler) AgentDeploymentCapabilities(c *gin.Context) {
	workflow, ok := h.loadOwnedWorkflow(c, c.Param("id"))
	if !ok {
		return
	}
	ast, err := workflowASTFromModel(workflow)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	policy := defaultSafeAgentPolicy(ast)
	c.JSON(http.StatusOK, gin.H{
		"capabilities":  agentWorkflowCapabilities(ast),
		"defaultPolicy": policy,
		"review":        summarizeAgentPolicy(ast, policy, "", nil),
	})
}

// CreateAgentDeployment snapshots a workflow and revalidates every capability
// selected by the analyzer/owner before storing it.
func (h *WorkflowHandler) CreateAgentDeployment(c *gin.Context) {
	workflow, ok := h.loadOwnedWorkflow(c, c.Param("id"))
	if !ok {
		return
	}
	var request createAgentDeploymentRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid deployment request"})
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.Alias = strings.ToLower(strings.TrimSpace(request.Alias))
	if request.Name == "" || len(request.Name) > 80 || !agentAliasPattern.MatchString(request.Alias) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must be at most 80 characters and alias must be 2–32 lowercase letters, numbers, hyphens or underscores"})
		return
	}

	var installation models.AgentHostInstallation
	if err := h.db.DB.Where("id = ? AND organization_id = ? AND status = ?",
		request.HostInstallationID, currentOrgID(c), models.AgentHostActive).First(&installation).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "active host installation not found"})
		return
	}
	if installation.Provider == "slack" && !slackAgentHostScopesReady(installation.Scopes) {
		c.JSON(http.StatusConflict, gin.H{"error": "Slack must be reconnected with hosted-agent mention permissions before deployment"})
		return
	}
	if len(request.Channels) > 20 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a deployment can allow at most 20 channels"})
		return
	}
	if installation.Provider == "slack" && len(request.Channels) > 0 {
		channels, err := validateSlackDeploymentChannels(c.Request.Context(), installation.BotToken, request.Channels)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "could not validate allowed Slack channels", "detail": err.Error()})
			return
		}
		request.Channels = channels
	}
	ast, err := workflowASTFromModel(workflow)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	policy, warnings := normalizeAgentCapabilityPolicy(ast, request.Policy)
	if len(policy.Nodes) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "deployment must expose at least one valid read or approval-gated operation", "warnings": warnings})
		return
	}
	snapshotHash := agentSnapshotHash(workflow.Name, workflow.Nodes, workflow.Edges)
	var analysisRecord models.AgentPermissionAnalysisRecord
	if err := h.db.DB.Where("id = ? AND organization_id = ? AND workflow_id = ? AND requested_by_user_id = ? AND snapshot_hash = ? AND expires_at > ?",
		request.AnalysisID, currentOrgID(c), workflow.ID.String(), auth.UserID(c), snapshotHash, time.Now().UTC()).
		First(&analysisRecord).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "permission analysis is missing, expired, or belongs to an older workflow snapshot; analyze again"})
		return
	}
	var analysisWarnings []string
	_ = json.Unmarshal(analysisRecord.Warnings, &analysisWarnings)
	analysisWarnings = append(analysisWarnings, warnings...)
	analysisWarnings = append(analysisWarnings, agentPolicyRiskWarnings(ast, policy)...)
	analysisWarnings = uniqueStrings(analysisWarnings)

	policyJSON, _ := json.Marshal(policy)
	analysisJSON, _ := json.Marshal(map[string]any{
		"analysisId": analysisRecord.ID.String(), "goal": analysisRecord.Goal,
		"summary": analysisRecord.Summary, "warnings": analysisWarnings, "source": "ai",
	})
	status := models.AgentDeploymentDraft
	if len(request.Channels) > 0 {
		status = models.AgentDeploymentActive
	}
	modelID := resolveChatModel(request.ModelID).ID
	deployment := models.AgentDeployment{
		OrganizationID: currentOrgID(c), WorkflowID: workflow.ID.String(),
		DeployedByUserID: auth.UserID(c), HostInstallationID: installation.ID.String(),
		Provider: installation.Provider, Name: request.Name, Alias: request.Alias,
		ModelID: modelID, Version: 1, Status: status,
		SnapshotName: workflow.Name, SnapshotNodes: append(models.JSONB(nil), workflow.Nodes...),
		SnapshotEdges:   append(models.JSONB(nil), workflow.Edges...),
		SnapshotHash:    snapshotHash,
		SourceUpdatedAt: workflow.UpdatedAt, CapabilityPolicy: models.JSONB(policyJSON),
		PermissionAnalysis: models.JSONB(analysisJSON),
	}

	var targets []models.AgentDeploymentTarget
	err = h.db.DB.Transaction(func(tx *gorm.DB) error {
		var latest int
		if err := tx.Model(&models.AgentDeployment{}).
			Where("workflow_id = ? AND alias = ?", workflow.ID.String(), request.Alias).
			Select("COALESCE(MAX(version), 0)").Scan(&latest).Error; err != nil {
			return err
		}
		deployment.Version = latest + 1
		if err := tx.Create(&deployment).Error; err != nil {
			return err
		}
		seenChannels := map[string]bool{}
		for _, channel := range request.Channels {
			channel.ID = strings.TrimSpace(channel.ID)
			if channel.ID == "" || seenChannels[channel.ID] {
				return fmt.Errorf("channel IDs must be non-empty and unique")
			}
			seenChannels[channel.ID] = true
			target := models.AgentDeploymentTarget{
				OrganizationID: currentOrgID(c), DeploymentID: deployment.ID.String(),
				Provider: installation.Provider, ExternalWorkspaceID: installation.ExternalWorkspaceID,
				ExternalChannelID: channel.ID, ExternalChannelName: truncate(strings.TrimSpace(channel.Name), 120),
				Enabled: true,
			}
			if err := tx.Create(&target).Error; err != nil {
				return err
			}
			targets = append(targets, target)
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "a selected channel may already have an agent deployment", "detail": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, agentDeploymentResponse{
		Deployment: deployment, Targets: targets,
		Review: summarizeAgentPolicy(ast, policy, analysisRecord.Goal, analysisWarnings),
	})
}

func (h *WorkflowHandler) ListAgentDeployments(c *gin.Context) {
	if _, ok := h.loadOwnedWorkflow(c, c.Param("id")); !ok {
		return
	}
	var deployments []models.AgentDeployment
	if err := h.db.DB.Where("organization_id = ? AND workflow_id = ?", currentOrgID(c), c.Param("id")).
		Order("created_at DESC").Find(&deployments).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list deployments"})
		return
	}
	responses := make([]agentDeploymentResponse, 0, len(deployments))
	for _, deployment := range deployments {
		var targets []models.AgentDeploymentTarget
		h.db.DB.Where("deployment_id = ?", deployment.ID.String()).Find(&targets)
		var policy AgentCapabilityPolicy
		_ = json.Unmarshal(deployment.CapabilityPolicy, &policy)
		ast := executor.WorkflowAST{Name: deployment.SnapshotName}
		_ = json.Unmarshal(deployment.SnapshotNodes, &ast.Nodes)
		_ = json.Unmarshal(deployment.SnapshotEdges, &ast.Edges)
		var analysis map[string]any
		_ = json.Unmarshal(deployment.PermissionAnalysis, &analysis)
		goal, _ := analysis["goal"].(string)
		warnings := stringValues(analysis["warnings"])
		responses = append(responses, agentDeploymentResponse{
			Deployment: deployment, Targets: targets, Review: summarizeAgentPolicy(ast, policy, goal, warnings),
		})
	}
	c.JSON(http.StatusOK, responses)
}

func (h *WorkflowHandler) GetAgentDeployment(c *gin.Context) {
	deployment, ok := h.loadAgentDeployment(c)
	if !ok {
		return
	}
	var targets []models.AgentDeploymentTarget
	h.db.DB.Where("deployment_id = ?", deployment.ID.String()).Find(&targets)
	var policy AgentCapabilityPolicy
	_ = json.Unmarshal(deployment.CapabilityPolicy, &policy)
	ast := executor.WorkflowAST{Name: deployment.SnapshotName}
	_ = json.Unmarshal(deployment.SnapshotNodes, &ast.Nodes)
	_ = json.Unmarshal(deployment.SnapshotEdges, &ast.Edges)
	var analysis map[string]any
	_ = json.Unmarshal(deployment.PermissionAnalysis, &analysis)
	goal, _ := analysis["goal"].(string)
	warnings := stringValues(analysis["warnings"])
	c.JSON(http.StatusOK, agentDeploymentResponse{
		Deployment: *deployment, Targets: targets, Review: summarizeAgentPolicy(ast, policy, goal, warnings),
	})
}

func (h *WorkflowHandler) loadAgentDeployment(c *gin.Context) (*models.AgentDeployment, bool) {
	var deployment models.AgentDeployment
	if err := h.db.DB.Where("id = ? AND organization_id = ?", c.Param("id"), currentOrgID(c)).First(&deployment).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "agent deployment not found"})
		return nil, false
	}
	return &deployment, true
}

func (h *WorkflowHandler) PatchAgentDeployment(c *gin.Context) {
	deployment, ok := h.loadAgentDeployment(c)
	if !ok {
		return
	}
	if deployment.Status == models.AgentDeploymentRevoked {
		c.JSON(http.StatusConflict, gin.H{"error": "a revoked deployment cannot be reactivated; deploy a new snapshot"})
		return
	}
	var request struct {
		Name   *string `json:"name"`
		Status *string `json:"status"`
	}
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	updates := map[string]any{}
	if request.Name != nil {
		name := strings.TrimSpace(*request.Name)
		if name == "" || len(name) > 80 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name must be 1–80 characters"})
			return
		}
		updates["name"] = name
	}
	if request.Status != nil {
		status := models.AgentDeploymentStatus(*request.Status)
		if status != models.AgentDeploymentActive && status != models.AgentDeploymentPaused {
			c.JSON(http.StatusBadRequest, gin.H{"error": "status must be active or paused"})
			return
		}
		if status == models.AgentDeploymentActive {
			var deployerMembership int64
			h.db.DB.Model(&models.OrgMember{}).
				Where("organization_id = ? AND user_id = ?", deployment.OrganizationID, deployment.DeployedByUserID).
				Count(&deployerMembership)
			if deployerMembership == 0 {
				c.JSON(http.StatusConflict, gin.H{"error": "the deploying user is no longer an organization member; deploy a new snapshot under a current member"})
				return
			}
			var host models.AgentHostInstallation
			if err := h.db.DB.Where("id = ? AND organization_id = ? AND status = ?",
				deployment.HostInstallationID, deployment.OrganizationID, models.AgentHostActive).First(&host).Error; err != nil {
				c.JSON(http.StatusConflict, gin.H{"error": "reconnect the team-chat host before activating this deployment"})
				return
			}
			var enabledTargets int64
			h.db.DB.Model(&models.AgentDeploymentTarget{}).
				Where("deployment_id = ? AND enabled = true", deployment.ID.String()).Count(&enabledTargets)
			if enabledTargets == 0 {
				c.JSON(http.StatusConflict, gin.H{"error": "add an allowed channel before activating this deployment"})
				return
			}
		}
		updates["status"] = status
	}
	if len(updates) > 0 {
		if err := h.db.DB.Model(deployment).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update deployment"})
			return
		}
	}
	h.GetAgentDeployment(c)
}

func (h *WorkflowHandler) DeleteAgentDeployment(c *gin.Context) {
	deployment, ok := h.loadAgentDeployment(c)
	if !ok {
		return
	}
	err := h.db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(deployment).Update("status", models.AgentDeploymentRevoked).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.AgentDeploymentTarget{}).
			Where("deployment_id = ?", deployment.ID.String()).Update("enabled", false).Error; err != nil {
			return err
		}
		return tx.Where("deployment_id = ?", deployment.ID.String()).Delete(&models.AgentDeploymentTarget{}).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke deployment"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *WorkflowHandler) ListAgentHosts(c *gin.Context) {
	var installations []models.AgentHostInstallation
	if err := h.db.DB.Where("organization_id = ?", currentOrgID(c)).Order("created_at DESC").Find(&installations).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list host installations"})
		return
	}
	c.JSON(http.StatusOK, installations)
}

func (h *WorkflowHandler) ConnectAgentHost(c *gin.Context) {
	// The agent-host OAuth route is intentionally static. Inject the provider
	// expected by the shared integration OAuth implementation.
	c.Params = append(c.Params, gin.Param{Key: "provider", Value: "slack"})
	c.Set("agent-host-connect", true)
	h.ConnectIntegration(c)
}

// DeleteAgentHost revokes only the org-level chat transport. It deliberately
// does not delete or revoke any member's Slack action credential.
func (h *WorkflowHandler) DeleteAgentHost(c *gin.Context) {
	var installation models.AgentHostInstallation
	if err := h.db.DB.Where("id = ? AND organization_id = ?", c.Param("id"), currentOrgID(c)).
		First(&installation).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "agent host installation not found"})
		return
	}
	err := h.db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&installation).Updates(map[string]any{
			"status": models.AgentHostRevoked, "bot_token": "", "bot_user_id": "",
			"last_error": "Team-chat host disconnected by an organization member",
		}).Error; err != nil {
			return err
		}
		return tx.Model(&models.AgentDeployment{}).
			Where("host_installation_id = ? AND status = ?", installation.ID.String(), models.AgentDeploymentActive).
			Update("status", models.AgentDeploymentPaused).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to disconnect agent host"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *WorkflowHandler) ListAgentHostChannels(c *gin.Context) {
	var installation models.AgentHostInstallation
	if err := h.db.DB.Where("id = ? AND organization_id = ? AND status = ?",
		c.Param("id"), currentOrgID(c), models.AgentHostActive).First(&installation).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "active host installation not found"})
		return
	}
	if installation.Provider != "slack" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "this host does not expose Slack channels"})
		return
	}
	// Membership is transport metadata, not a persistent deployment permission.
	type slackChannel struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		IsMember  bool   `json:"is_member"`
		IsPrivate bool   `json:"is_private"`
	}
	var channels []slackChannel
	cursor := ""
	for page := 0; page < 10; page++ {
		var response struct {
			Channels []slackChannel `json:"channels"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		payload := map[string]any{
			"limit": 200, "exclude_archived": true, "types": "public_channel,private_channel",
		}
		if cursor != "" {
			payload["cursor"] = cursor
		}
		if err := slackAgentAPICall(c.Request.Context(), installation.BotToken, "conversations.list", payload, &response); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "could not list Slack channels", "detail": err.Error()})
			return
		}
		channels = append(channels, response.Channels...)
		cursor = strings.TrimSpace(response.Metadata.NextCursor)
		if cursor == "" {
			break
		}
	}
	c.JSON(http.StatusOK, channels)
}

func validateSlackDeploymentChannels(ctx context.Context, token string, channels []agentDeploymentChannelInput) ([]agentDeploymentChannelInput, error) {
	validated := make([]agentDeploymentChannelInput, 0, len(channels))
	seen := map[string]bool{}
	for _, requested := range channels {
		channelID := strings.TrimSpace(requested.ID)
		if !slackChannelIDPattern.MatchString(channelID) || seen[channelID] {
			return nil, fmt.Errorf("channel IDs must be unique Slack channel IDs")
		}
		seen[channelID] = true
		var response struct {
			Channel struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				IsMember bool   `json:"is_member"`
			} `json:"channel"`
		}
		if err := slackAgentAPICall(ctx, token, "conversations.info", map[string]any{"channel": channelID}, &response); err != nil {
			return nil, fmt.Errorf("%s: %w", channelID, err)
		}
		if response.Channel.ID != channelID {
			return nil, fmt.Errorf("Slack returned the wrong channel for %s", channelID)
		}
		if !response.Channel.IsMember {
			return nil, fmt.Errorf("invite Fernary to #%s before deploying there", response.Channel.Name)
		}
		validated = append(validated, agentDeploymentChannelInput{ID: channelID, Name: response.Channel.Name})
	}
	return validated, nil
}

func (h *WorkflowHandler) syncSlackAgentHost(organizationID, userID string, connection *models.IntegrationConnection) error {
	if connection == nil || connection.Provider != "slack" || connection.WorkspaceID == "" || connection.AccessToken == "" {
		return errors.New("complete Slack installation details are required")
	}
	var existing models.AgentHostInstallation
	err := h.db.DB.Unscoped().Where("provider = ? AND external_workspace_id = ?", "slack", connection.WorkspaceID).First(&existing).Error
	if err == nil && existing.OrganizationID != organizationID {
		return errors.New("this Slack workspace is already connected to another Fernary organization")
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	now := time.Now().UTC()
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return h.db.DB.Create(&models.AgentHostInstallation{
			OrganizationID: organizationID, InstalledByUserID: userID, Provider: "slack",
			ExternalWorkspaceID: connection.WorkspaceID, ExternalWorkspaceName: connection.WorkspaceName,
			BotUserID: connection.BotUserID, BotToken: connection.AccessToken,
			Scopes: connection.Scope, Status: models.AgentHostActive,
			LastVerifiedAt: &now,
		}).Error
	}
	existing.OrganizationID = organizationID
	existing.InstalledByUserID = userID
	existing.ExternalWorkspaceName = connection.WorkspaceName
	existing.BotUserID = connection.BotUserID
	existing.BotToken = connection.AccessToken
	existing.Scopes = connection.Scope
	existing.Status = models.AgentHostActive
	existing.LastVerifiedAt = &now
	existing.LastError = ""
	existing.DeletedAt = gorm.DeletedAt{}
	return h.db.DB.Unscoped().Save(&existing).Error
}

func oauthScopeContains(scopeList, want string) bool {
	for _, scope := range strings.FieldsFunc(scopeList, func(r rune) bool { return r == ',' || r == ' ' }) {
		if scope == want {
			return true
		}
	}
	return false
}

func slackAgentHostScopesReady(scopeList string) bool {
	for _, required := range []string{
		"app_mentions:read", "chat:write", "chat:write.customize",
		"channels:read", "channels:history", "groups:read", "groups:history",
	} {
		if !oauthScopeContains(scopeList, required) {
			return false
		}
	}
	return true
}

func stringValues(value any) []string {
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok && text != "" {
			out = append(out, text)
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
