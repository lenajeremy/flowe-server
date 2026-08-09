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
	"workflow-ai/server/internal/tenancy"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var agentAliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)
var slackChannelIDPattern = regexp.MustCompile(`^[CG][A-Z0-9]{1,31}$`)
var slackAgentHostRequiredScopes = []string{
	"app_mentions:read", "chat:write", "chat:write.customize",
	"channels:read", "channels:history", "groups:read", "groups:history",
}
var slackAgentHostRequestedScopes = append(append([]string{}, slackAgentHostRequiredScopes...), "channels:join")

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

type agentHostSlackChannel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsMember  bool   `json:"is_member"`
	IsPrivate bool   `json:"is_private"`
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
	Deployment   agentDeploymentView         `json:"deployment"`
	Targets      []agentDeploymentTargetView `json:"targets"`
	Review       AgentPermissionSummary      `json:"review"`
	Capabilities []AgentNodeCapability       `json:"capabilities,omitempty"`
	Workflow     agentDeploymentWorkflowView `json:"workflow"`
	Deployer     agentDeploymentDeployerView `json:"deployer"`
	Host         *agentDeploymentHostView    `json:"host,omitempty"`
	Health       agentDeploymentHealthView   `json:"health"`
	CanManage    bool                        `json:"can_manage"`
}

// agentDeploymentView deliberately excludes the stored node and edge snapshot.
// Organization members need to understand and operate deployments, but returning
// every saved node configuration would make the inventory both enormous and an
// unnecessary source of integration settings. The detail endpoint exposes the
// derived capability catalog instead.
type agentDeploymentView struct {
	ID               string                       `json:"id"`
	WorkflowID       string                       `json:"workflow_id"`
	Name             string                       `json:"name"`
	Alias            string                       `json:"alias"`
	Provider         string                       `json:"provider"`
	ModelID          string                       `json:"model_id"`
	Version          int                          `json:"version"`
	Status           models.AgentDeploymentStatus `json:"status"`
	SnapshotName     string                       `json:"snapshot_name"`
	SnapshotHash     string                       `json:"snapshot_hash"`
	SourceUpdatedAt  time.Time                    `json:"source_updated_at"`
	CapabilityPolicy AgentCapabilityPolicy        `json:"capability_policy"`
	CreatedAt        time.Time                    `json:"created_at"`
	UpdatedAt        time.Time                    `json:"updated_at"`
}

type agentDeploymentTargetView struct {
	ID                  string `json:"id"`
	ExternalChannelID   string `json:"external_channel_id"`
	ExternalChannelName string `json:"external_channel_name"`
	Enabled             bool   `json:"enabled"`
}

type agentDeploymentWorkflowView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type agentDeploymentDeployerView struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Email     string `json:"email,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

type agentDeploymentHostView struct {
	ID                    string                 `json:"id"`
	Provider              string                 `json:"provider"`
	ExternalWorkspaceID   string                 `json:"external_workspace_id"`
	ExternalWorkspaceName string                 `json:"external_workspace_name"`
	Status                models.AgentHostStatus `json:"status"`
	LastError             string                 `json:"last_error,omitempty"`
}

type agentDeploymentHealthView struct {
	Status             string                            `json:"status"`
	Message            string                            `json:"message"`
	LastActivityAt     *time.Time                        `json:"last_activity_at,omitempty"`
	LastDeliveryStatus *models.HostedAgentDeliveryStatus `json:"last_delivery_status,omitempty"`
	LastError          string                            `json:"last_error,omitempty"`
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
	if len(request.Channels) == 0 || len(request.Channels) > 20 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "select between 1 and 20 allowed channels"})
		return
	}
	if installation.Provider == "slack" {
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
	modelID := resolveChatModel(request.ModelID).ID
	deployment := models.AgentDeployment{
		OrganizationID: currentOrgID(c), WorkflowID: workflow.ID.String(),
		DeployedByUserID: auth.UserID(c), HostInstallationID: installation.ID.String(),
		Provider: installation.Provider, Name: request.Name, Alias: request.Alias,
		ModelID: modelID, Version: 1, Status: models.AgentDeploymentActive,
		SnapshotName: workflow.Name, SnapshotNodes: append(models.JSONB(nil), workflow.Nodes...),
		SnapshotEdges:   append(models.JSONB(nil), workflow.Edges...),
		SnapshotHash:    snapshotHash,
		SourceUpdatedAt: workflow.UpdatedAt, CapabilityPolicy: models.JSONB(policyJSON),
		PermissionAnalysis: models.JSONB(analysisJSON),
	}

	var targets []models.AgentDeploymentTarget
	err = h.db.DB.Transaction(func(tx *gorm.DB) error {
		// Serialize version allocation for this workflow. Concurrent deploys must
		// not both observe the same MAX(version).
		var lockedWorkflow models.Workflow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", workflow.ID, currentOrgID(c)).First(&lockedWorkflow).Error; err != nil {
			return err
		}
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

	responses, err := h.buildAgentDeploymentResponses(c, []models.AgentDeployment{deployment}, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "deployment was created but its summary could not be loaded"})
		return
	}
	c.JSON(http.StatusCreated, responses[0])
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
	responses, err := h.buildAgentDeploymentResponses(c, deployments, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load deployment details"})
		return
	}
	c.JSON(http.StatusOK, responses)
}

// ListAllAgentDeployments is the organization-wide operational inventory. It
// includes revoked history so the UI can keep it behind an explicit History
// filter without losing who deployed what and where it used to run.
func (h *WorkflowHandler) ListAllAgentDeployments(c *gin.Context) {
	var deployments []models.AgentDeployment
	if err := h.db.DB.Where("organization_id = ?", currentOrgID(c)).
		Order("created_at DESC").Find(&deployments).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list agent deployments"})
		return
	}
	responses, err := h.buildAgentDeploymentResponses(c, deployments, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load deployment details"})
		return
	}
	c.JSON(http.StatusOK, responses)
}

func (h *WorkflowHandler) GetAgentDeployment(c *gin.Context) {
	deployment, ok := h.loadAgentDeployment(c)
	if !ok {
		return
	}
	responses, err := h.buildAgentDeploymentResponses(c, []models.AgentDeployment{*deployment}, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load deployment details"})
		return
	}
	c.JSON(http.StatusOK, responses[0])
}

func (h *WorkflowHandler) buildAgentDeploymentResponses(c *gin.Context, deployments []models.AgentDeployment, includeCapabilities bool) ([]agentDeploymentResponse, error) {
	responses := make([]agentDeploymentResponse, 0, len(deployments))
	if len(deployments) == 0 {
		return responses, nil
	}
	organizationID := currentOrgID(c)
	deploymentIDs := make([]string, 0, len(deployments))
	hostIDs := make([]string, 0, len(deployments))
	workflowIDs := make([]string, 0, len(deployments))
	deployerIDs := make([]string, 0, len(deployments))
	for i := range deployments {
		deploymentIDs = append(deploymentIDs, deployments[i].ID.String())
		hostIDs = append(hostIDs, deployments[i].HostInstallationID)
		workflowIDs = append(workflowIDs, deployments[i].WorkflowID)
		deployerIDs = append(deployerIDs, deployments[i].DeployedByUserID)
	}

	// Revocation soft-deletes targets to release the channel uniqueness guard.
	// Unscoped is intentional here: historical destinations are operational
	// metadata and remain tenant-scoped by organization_id.
	var targets []models.AgentDeploymentTarget
	if err := h.db.DB.Unscoped().Where("organization_id = ? AND deployment_id IN ?", organizationID, deploymentIDs).
		Order("created_at ASC").Find(&targets).Error; err != nil {
		return nil, err
	}
	targetsByDeployment := map[string][]models.AgentDeploymentTarget{}
	for _, target := range targets {
		targetsByDeployment[target.DeploymentID] = append(targetsByDeployment[target.DeploymentID], target)
	}

	var hosts []models.AgentHostInstallation
	if err := h.db.DB.Where("organization_id = ? AND id IN ?", organizationID, hostIDs).Find(&hosts).Error; err != nil {
		return nil, err
	}
	hostByID := map[string]models.AgentHostInstallation{}
	for _, host := range hosts {
		hostByID[host.ID.String()] = host
	}

	var workflows []models.Workflow
	if err := h.db.DB.Select("id", "name").Where("organization_id = ? AND id IN ?", organizationID, workflowIDs).
		Find(&workflows).Error; err != nil {
		return nil, err
	}
	workflowByID := map[string]models.Workflow{}
	for _, workflow := range workflows {
		workflowByID[workflow.ID.String()] = workflow
	}

	var deployers []models.User
	if err := h.db.DB.Select("id", "name", "email", "avatar_url").Where("id IN ?", deployerIDs).
		Find(&deployers).Error; err != nil {
		return nil, err
	}
	deployerByID := map[string]models.User{}
	for _, deployer := range deployers {
		deployerByID[deployer.ID.String()] = deployer
	}

	var memberships []models.OrgMember
	if err := h.db.DB.Where("organization_id = ? AND user_id IN ?", organizationID, deployerIDs).
		Find(&memberships).Error; err != nil {
		return nil, err
	}
	membershipByUserID := map[string]models.OrgMember{}
	for _, membership := range memberships {
		membershipByUserID[membership.UserID] = membership
	}

	var deliveries []models.HostedAgentDelivery
	deliveries, err := latestHostedAgentDeliveries(h.db.DB, deploymentIDs)
	if err != nil {
		return nil, err
	}
	latestDeliveryByDeployment := map[string]models.HostedAgentDelivery{}
	for _, delivery := range deliveries {
		latestDeliveryByDeployment[delivery.ResponseDeploymentID] = delivery
	}

	actorID := auth.UserID(c)
	actorIsAdmin := tenancy.CanManageMembers(h.db.DB, organizationID, actorID)
	for i := range deployments {
		deployment := &deployments[i]
		policy := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion, Nodes: []AgentNodeGrant{}}
		var ast executor.WorkflowAST
		ast.Name = deployment.SnapshotName
		decodeError := ""
		if err := json.Unmarshal(deployment.CapabilityPolicy, &policy); err != nil {
			decodeError = "Stored permission policy is unreadable"
		}
		if err := json.Unmarshal(deployment.SnapshotNodes, &ast.Nodes); err != nil {
			decodeError = "Stored workflow snapshot is unreadable"
		}
		if err := json.Unmarshal(deployment.SnapshotEdges, &ast.Edges); err != nil {
			decodeError = "Stored workflow snapshot is unreadable"
		}
		var analysis map[string]any
		_ = json.Unmarshal(deployment.PermissionAnalysis, &analysis)
		goal, _ := analysis["goal"].(string)
		warnings := stringValues(analysis["warnings"])

		deploymentTargets := targetsByDeployment[deployment.ID.String()]
		targetViews := make([]agentDeploymentTargetView, 0, len(deploymentTargets))
		for _, target := range deploymentTargets {
			if deployment.Status != models.AgentDeploymentRevoked && target.DeletedAt.Valid {
				continue
			}
			targetViews = append(targetViews, agentDeploymentTargetView{
				ID: target.ID.String(), ExternalChannelID: target.ExternalChannelID,
				ExternalChannelName: target.ExternalChannelName, Enabled: target.Enabled,
			})
		}

		workflowView := agentDeploymentWorkflowView{ID: deployment.WorkflowID, Name: deployment.SnapshotName}
		if workflow, exists := workflowByID[deployment.WorkflowID]; exists {
			workflowView.Name = workflow.Name
		}
		deployerView := agentDeploymentDeployerView{ID: deployment.DeployedByUserID}
		if deployer, exists := deployerByID[deployment.DeployedByUserID]; exists {
			deployerView.Name = deployer.Name
			deployerView.Email = deployer.Email
			deployerView.AvatarURL = deployer.AvatarURL
		}
		var hostView *agentDeploymentHostView
		host, hostExists := hostByID[deployment.HostInstallationID]
		if hostExists {
			hostView = &agentDeploymentHostView{
				ID: host.ID.String(), Provider: host.Provider, ExternalWorkspaceID: host.ExternalWorkspaceID,
				ExternalWorkspaceName: host.ExternalWorkspaceName, Status: host.Status, LastError: host.LastError,
			}
		}
		latestDelivery, hasDelivery := latestDeliveryByDeployment[deployment.ID.String()]
		_, deployerIsMember := membershipByUserID[deployment.DeployedByUserID]
		health := agentDeploymentHealth(*deployment, hostView, deploymentTargets, deployerIsMember, latestDelivery, hasDelivery, decodeError)
		response := agentDeploymentResponse{
			Deployment: agentDeploymentView{
				ID: deployment.ID.String(), WorkflowID: deployment.WorkflowID, Name: deployment.Name,
				Alias: deployment.Alias, Provider: deployment.Provider, ModelID: deployment.ModelID,
				Version: deployment.Version, Status: deployment.Status, SnapshotName: deployment.SnapshotName,
				SnapshotHash: deployment.SnapshotHash, SourceUpdatedAt: deployment.SourceUpdatedAt,
				CapabilityPolicy: policy, CreatedAt: deployment.CreatedAt, UpdatedAt: deployment.UpdatedAt,
			},
			Targets: targetViews, Review: summarizeAgentPolicy(ast, policy, goal, warnings),
			Workflow: workflowView, Deployer: deployerView, Host: hostView, Health: health,
			CanManage: actorID == deployment.DeployedByUserID || actorIsAdmin,
		}
		if includeCapabilities && decodeError == "" {
			response.Capabilities = agentWorkflowCapabilities(ast)
		}
		responses = append(responses, response)
	}
	return responses, nil
}

func latestHostedAgentDeliveries(db *gorm.DB, deploymentIDs []string) ([]models.HostedAgentDelivery, error) {
	deliveries := make([]models.HostedAgentDelivery, 0, len(deploymentIDs))
	if len(deploymentIDs) == 0 {
		return deliveries, nil
	}
	if err := db.Model(&models.HostedAgentDelivery{}).
		Select("DISTINCT ON (response_deployment_id) id, response_deployment_id, status, last_error, created_at, updated_at, completed_at").
		Where("response_deployment_id IN ?", deploymentIDs).
		Order("response_deployment_id, created_at DESC, id DESC").Find(&deliveries).Error; err != nil {
		return nil, err
	}
	return deliveries, nil
}

func agentDeploymentHealth(deployment models.AgentDeployment, host *agentDeploymentHostView, targets []models.AgentDeploymentTarget, deployerIsMember bool, latest models.HostedAgentDelivery, hasDelivery bool, decodeError string) agentDeploymentHealthView {
	health := agentDeploymentHealthView{Status: "healthy", Message: "Ready for mentions"}
	if hasDelivery {
		activityAt := latest.CreatedAt
		health.LastActivityAt = &activityAt
		status := latest.Status
		health.LastDeliveryStatus = &status
		health.LastError = latest.LastError
	}
	switch deployment.Status {
	case models.AgentDeploymentPaused:
		health.Status, health.Message = "paused", "Paused and not accepting mentions"
		return health
	case models.AgentDeploymentRevoked:
		health.Status, health.Message = "revoked", "Revoked permanently"
		return health
	case models.AgentDeploymentActive:
		// Continue with live dependency checks below.
	default:
		health.Status, health.Message = "needs_attention", "Deployment is not active"
		return health
	}
	if decodeError != "" {
		health.Status, health.Message, health.LastError = "needs_attention", decodeError, decodeError
		return health
	}
	if !deployerIsMember {
		health.Status, health.Message = "needs_attention", "Deployment owner is no longer an organization member"
		return health
	}
	if host == nil || host.Status != models.AgentHostActive {
		health.Status, health.Message = "needs_attention", "Slack needs to be reconnected"
		if host != nil && host.LastError != "" {
			health.LastError = host.LastError
		}
		return health
	}
	enabledTargets := 0
	for _, target := range targets {
		if target.Enabled && !target.DeletedAt.Valid {
			enabledTargets++
		}
	}
	if enabledTargets == 0 {
		health.Status, health.Message = "needs_attention", "No allowed channel is enabled"
		return health
	}
	if hasDelivery && latest.Status == models.HostedAgentDeliveryFailed {
		health.Status, health.Message = "needs_attention", "The most recent Slack request failed"
	}
	return health
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
		Name               *string                        `json:"name"`
		Status             *string                        `json:"status"`
		Policy             *AgentCapabilityPolicy         `json:"policy"`
		HostInstallationID *string                        `json:"hostInstallationId"`
		Channels           *[]agentDeploymentChannelInput `json:"channels"`
		ExpectedUpdatedAt  *time.Time                     `json:"expectedUpdatedAt"`
	}
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	actorID := auth.UserID(c)
	if !h.canManageAgentDeployment(h.db.DB, deployment, actorID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the deployment owner or an organization admin can manage this agent"})
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
		updates["status"] = status
	}
	if (request.HostInstallationID == nil) != (request.Channels == nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hostInstallationId and channels must be changed together"})
		return
	}
	var destinationInstallation *models.AgentHostInstallation
	var destinationChannels []agentDeploymentChannelInput
	if request.HostInstallationID != nil && request.Channels != nil {
		if request.ExpectedUpdatedAt == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "expectedUpdatedAt is required when changing the Slack destination"})
			return
		}
		hostID := strings.TrimSpace(*request.HostInstallationID)
		if hostID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "select an active Slack workspace"})
			return
		}
		var installation models.AgentHostInstallation
		if err := h.db.DB.Where("id = ? AND organization_id = ? AND status = ?", hostID, deployment.OrganizationID, models.AgentHostActive).
			First(&installation).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "active host installation not found"})
			return
		}
		if installation.Provider != slackAgentProvider || !slackAgentHostScopesReady(installation.Scopes) {
			c.JSON(http.StatusConflict, gin.H{"error": "Slack must be reconnected with hosted-agent mention permissions before changing this destination"})
			return
		}
		if len(*request.Channels) == 0 || len(*request.Channels) > 20 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "select between 1 and 20 allowed channels"})
			return
		}
		validated, err := validateSlackDeploymentChannels(c.Request.Context(), installation.BotToken, *request.Channels)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "could not validate allowed Slack channels", "detail": err.Error()})
			return
		}
		destinationInstallation = &installation
		destinationChannels = validated
		updates["host_installation_id"] = installation.ID.String()
		updates["provider"] = installation.Provider
	}
	var policyJSON, analysisJSON []byte
	if request.Policy != nil {
		if request.ExpectedUpdatedAt == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "expectedUpdatedAt is required when changing permissions"})
			return
		}
		ast := executor.WorkflowAST{Name: deployment.SnapshotName}
		if json.Unmarshal(deployment.SnapshotNodes, &ast.Nodes) != nil || json.Unmarshal(deployment.SnapshotEdges, &ast.Edges) != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "stored deployment snapshot is unreadable; deploy a new snapshot"})
			return
		}
		normalized, warnings := normalizeAgentCapabilityPolicy(ast, *request.Policy)
		if len(normalized.Nodes) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "an agent must retain at least one valid operation", "warnings": warnings,
			})
			return
		}
		warnings = uniqueStrings(append(warnings, agentPolicyRiskWarnings(ast, normalized)...))
		policyJSON, _ = json.Marshal(normalized)
		var analysis map[string]any
		_ = json.Unmarshal(deployment.PermissionAnalysis, &analysis)
		if analysis == nil {
			analysis = map[string]any{}
		}
		analysis["source"] = "manual"
		analysis["warnings"] = warnings
		analysis["lastEditedBy"] = actorID
		analysis["lastEditedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
		analysisJSON, _ = json.Marshal(analysis)
		updates["capability_policy"] = models.JSONB(policyJSON)
		updates["permission_analysis"] = models.JSONB(analysisJSON)
	}
	if len(updates) > 0 {
		err := h.withHostedAuthorityLocks(c.Request.Context(), deployment.OrganizationID,
			[]string{deployment.DeployedByUserID, actorID}, func(connection *gorm.DB) error {
				return connection.Transaction(func(tx *gorm.DB) error {
					var live models.AgentDeployment
					if err := tx.Session(&gorm.Session{NewDB: true}).Clauses(clause.Locking{Strength: "UPDATE"}).
						Where("id = ? AND organization_id = ? AND status <> ?", deployment.ID, deployment.OrganizationID, models.AgentDeploymentRevoked).
						First(&live).Error; err != nil {
						if errors.Is(err, gorm.ErrRecordNotFound) {
							return errHostedAgentAuthorityEnded
						}
						return err
					}
					if !h.canManageAgentDeployment(tx.Session(&gorm.Session{NewDB: true}), &live, actorID) {
						return errAgentDeploymentForbidden
					}
					if (request.Policy != nil || destinationInstallation != nil) && !live.UpdatedAt.Equal(request.ExpectedUpdatedAt.UTC()) {
						return errAgentDeploymentChanged
					}
					if destinationInstallation != nil {
						var hostCount int64
						if err := tx.Session(&gorm.Session{NewDB: true}).Model(&models.AgentHostInstallation{}).
							Where("id = ? AND organization_id = ? AND status = ?", destinationInstallation.ID, live.OrganizationID, models.AgentHostActive).
							Count(&hostCount).Error; err != nil {
							return err
						}
						if hostCount != 1 {
							return errHostedAgentAuthorityEnded
						}
					}
					resultingStatus := live.Status
					if request.Status != nil {
						resultingStatus = models.AgentDeploymentStatus(*request.Status)
					}
					if resultingStatus == models.AgentDeploymentActive && (request.Status != nil || destinationInstallation != nil) {
						activationDeployment := live
						if destinationInstallation != nil {
							activationDeployment.HostInstallationID = destinationInstallation.ID.String()
						}
						if err := verifyAgentDeploymentCanActivate(tx.Session(&gorm.Session{NewDB: true}), &activationDeployment, destinationInstallation != nil); err != nil {
							return err
						}
					}
					expiryReason := ""
					if request.Policy != nil && destinationInstallation != nil {
						expiryReason = "Deployment permissions or Slack destination changed before approval"
					} else if request.Policy != nil {
						expiryReason = "Deployment permissions changed before approval"
					} else if destinationInstallation != nil {
						expiryReason = "Deployment Slack destination changed before approval"
					}
					if err := updateAgentDeploymentAndExpireApprovals(tx, &live, updates, expiryReason); err != nil {
						return err
					}
					if destinationInstallation != nil {
						return replaceAgentDeploymentTargets(tx, &live, destinationInstallation, destinationChannels)
					}
					return nil
				})
			})
		if errors.Is(err, errAgentDeploymentForbidden) {
			c.JSON(http.StatusForbidden, gin.H{"error": "only the deployment owner or an organization admin can manage this agent"})
			return
		}
		if errors.Is(err, errHostedAgentAuthorityEnded) {
			c.JSON(http.StatusConflict, gin.H{"error": "this deployment can no longer be changed or activated; deploy a new snapshot if its authority was revoked"})
			return
		}
		if errors.Is(err, errAgentDeploymentChanged) {
			c.JSON(http.StatusConflict, gin.H{"error": "this agent changed since you opened it; refresh before saving permissions"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update deployment"})
			return
		}
	}
	h.GetAgentDeployment(c)
}

var errAgentDeploymentForbidden = errors.New("agent deployment management is forbidden")
var errAgentDeploymentChanged = errors.New("agent deployment changed concurrently")

func (h *WorkflowHandler) canManageAgentDeployment(db *gorm.DB, deployment *models.AgentDeployment, actorID string) bool {
	var membership models.OrgMember
	if err := db.Where("organization_id = ? AND user_id = ?", deployment.OrganizationID, actorID).
		First(&membership).Error; err != nil {
		return false
	}
	return actorID == deployment.DeployedByUserID || membership.Role == models.RoleOwner || membership.Role == models.RoleAdmin
}

func verifyAgentDeploymentCanActivate(db *gorm.DB, deployment *models.AgentDeployment, destinationWillBeReplaced bool) error {
	var membershipCount, hostCount, targetCount int64
	if err := db.Model(&models.OrgMember{}).
		Where("organization_id = ? AND user_id = ?", deployment.OrganizationID, deployment.DeployedByUserID).
		Count(&membershipCount).Error; err != nil {
		return err
	}
	if err := db.Model(&models.AgentHostInstallation{}).
		Where("id = ? AND organization_id = ? AND status = ?", deployment.HostInstallationID, deployment.OrganizationID, models.AgentHostActive).
		Count(&hostCount).Error; err != nil {
		return err
	}
	if destinationWillBeReplaced {
		targetCount = 1
	} else {
		if err := db.Model(&models.AgentDeploymentTarget{}).
			Where("deployment_id = ? AND organization_id = ? AND enabled = true", deployment.ID, deployment.OrganizationID).
			Count(&targetCount).Error; err != nil {
			return err
		}
	}
	if membershipCount != 1 || hostCount != 1 || targetCount == 0 {
		return errHostedAgentAuthorityEnded
	}
	return nil
}

// updateAgentDeploymentOnLockedConnection starts a fresh GORM statement while
// retaining the connection that owns the hosted-authority advisory lock. The
// queries immediately above this write use agent_deployments too; reusing their
// statement state can make PostgreSQL generate an invalid
// `UPDATE agent_deployments ... FROM agent_deployments` query.
func updateAgentDeploymentOnLockedConnection(db *gorm.DB, deployment *models.AgentDeployment, updates map[string]any) error {
	result := db.Session(&gorm.Session{NewDB: true}).
		Model(&models.AgentDeployment{}).
		Where("id = ? AND organization_id = ? AND status <> ?",
			deployment.ID, deployment.OrganizationID, models.AgentDeploymentRevoked).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errHostedAgentAuthorityEnded
	}
	return nil
}

func updateAgentDeploymentAndExpireApprovals(db *gorm.DB, deployment *models.AgentDeployment, updates map[string]any, approvalExpiryReason string) error {
	if err := updateAgentDeploymentOnLockedConnection(db, deployment, updates); err != nil {
		return err
	}
	if approvalExpiryReason == "" {
		return nil
	}
	now := time.Now().UTC()
	return db.Session(&gorm.Session{NewDB: true}).Model(&models.HostedAgentApproval{}).
		Where("deployment_id = ? AND organization_id = ? AND status = ?", deployment.ID, deployment.OrganizationID, models.HostedAgentApprovalPending).
		Updates(map[string]any{
			"status": models.HostedAgentApprovalExpired, "resolved_at": now,
			"last_error": approvalExpiryReason,
		}).Error
}

func replaceAgentDeploymentTargets(db *gorm.DB, deployment *models.AgentDeployment, installation *models.AgentHostInstallation, channels []agentDeploymentChannelInput) error {
	if installation == nil || installation.OrganizationID != deployment.OrganizationID || len(channels) == 0 {
		return errHostedAgentAuthorityEnded
	}
	if err := db.Session(&gorm.Session{NewDB: true}).Model(&models.AgentDeploymentTarget{}).
		Where("deployment_id = ? AND organization_id = ?", deployment.ID, deployment.OrganizationID).
		Update("enabled", false).Error; err != nil {
		return err
	}
	if err := db.Session(&gorm.Session{NewDB: true}).
		Where("deployment_id = ? AND organization_id = ?", deployment.ID, deployment.OrganizationID).
		Delete(&models.AgentDeploymentTarget{}).Error; err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, channel := range channels {
		channel.ID = strings.TrimSpace(channel.ID)
		if channel.ID == "" || seen[channel.ID] {
			return errors.New("Slack channel IDs must be non-empty and unique")
		}
		seen[channel.ID] = true
		target := models.AgentDeploymentTarget{
			OrganizationID: deployment.OrganizationID, DeploymentID: deployment.ID.String(),
			Provider: installation.Provider, ExternalWorkspaceID: installation.ExternalWorkspaceID,
			ExternalChannelID: channel.ID, ExternalChannelName: truncate(strings.TrimSpace(channel.Name), 120),
			Enabled: true,
		}
		if err := db.Session(&gorm.Session{NewDB: true}).Create(&target).Error; err != nil {
			return err
		}
	}
	return nil
}

func (h *WorkflowHandler) DeleteAgentDeployment(c *gin.Context) {
	deployment, ok := h.loadAgentDeployment(c)
	if !ok {
		return
	}
	actorID := auth.UserID(c)
	if !h.canManageAgentDeployment(h.db.DB, deployment, actorID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the deployment owner or an organization admin can manage this agent"})
		return
	}
	err := h.withHostedAuthorityLocks(c.Request.Context(), deployment.OrganizationID,
		[]string{deployment.DeployedByUserID, actorID}, func(connection *gorm.DB) error {
			return connection.Transaction(func(tx *gorm.DB) error {
				var live models.AgentDeployment
				if err := tx.Session(&gorm.Session{NewDB: true}).Clauses(clause.Locking{Strength: "UPDATE"}).
					Where("id = ? AND organization_id = ?", deployment.ID, deployment.OrganizationID).First(&live).Error; err != nil {
					return err
				}
				if !h.canManageAgentDeployment(tx.Session(&gorm.Session{NewDB: true}), &live, actorID) {
					return errAgentDeploymentForbidden
				}
				if live.Status == models.AgentDeploymentRevoked {
					return nil
				}
				if err := tx.Session(&gorm.Session{NewDB: true}).Model(&models.AgentDeployment{}).
					Where("id = ? AND organization_id = ?", live.ID, live.OrganizationID).
					Update("status", models.AgentDeploymentRevoked).Error; err != nil {
					return err
				}
				if err := tx.Session(&gorm.Session{NewDB: true}).Model(&models.AgentDeploymentTarget{}).
					Where("deployment_id = ? AND organization_id = ?", live.ID, live.OrganizationID).Update("enabled", false).Error; err != nil {
					return err
				}
				now := time.Now().UTC()
				if err := tx.Session(&gorm.Session{NewDB: true}).Model(&models.HostedAgentApproval{}).
					Where("deployment_id = ? AND organization_id = ? AND status = ?", live.ID, live.OrganizationID, models.HostedAgentApprovalPending).
					Updates(map[string]any{
						"status": models.HostedAgentApprovalExpired, "resolved_at": now,
						"last_error": "Deployment was revoked before approval",
					}).Error; err != nil {
					return err
				}
				// Soft-delete targets to release the live channel uniqueness constraint.
				// The global inventory deliberately reads them unscoped for history.
				return tx.Session(&gorm.Session{NewDB: true}).Where("deployment_id = ? AND organization_id = ?", live.ID, live.OrganizationID).
					Delete(&models.AgentDeploymentTarget{}).Error
			})
		})
	if errors.Is(err, errAgentDeploymentForbidden) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the deployment owner or an organization admin can manage this agent"})
		return
	}
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
	if !tenancy.CanManageMembers(h.db.DB, currentOrgID(c), auth.UserID(c)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only organization owners and admins can connect the shared Slack host"})
		return
	}
	// The agent-host OAuth route is intentionally static. Inject the provider
	// expected by the shared integration OAuth implementation.
	c.Params = append(c.Params, gin.Param{Key: "provider", Value: "slack"})
	c.Set("agent-host-connect", true)
	h.ConnectIntegration(c)
}

// DeleteAgentHost revokes only the org-level chat transport. It deliberately
// does not delete or revoke any member's Slack action credential.
func (h *WorkflowHandler) DeleteAgentHost(c *gin.Context) {
	if !tenancy.CanManageMembers(h.db.DB, currentOrgID(c), auth.UserID(c)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only organization owners and admins can disconnect the shared Slack host"})
		return
	}
	var installation models.AgentHostInstallation
	if err := h.db.DB.Where("id = ? AND organization_id = ?", c.Param("id"), currentOrgID(c)).
		First(&installation).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "agent host installation not found"})
		return
	}
	var deployerIDs []string
	if err := h.db.DB.Model(&models.AgentDeployment{}).
		Where("host_installation_id = ? AND organization_id = ? AND status <> ?",
			installation.ID, installation.OrganizationID, models.AgentDeploymentRevoked).
		Distinct("deployed_by_user_id").Pluck("deployed_by_user_id", &deployerIDs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve deployments using this host"})
		return
	}
	err := h.withHostedAuthorityLocks(c.Request.Context(), installation.OrganizationID, deployerIDs, func(connection *gorm.DB) error {
		return connection.Transaction(func(tx *gorm.DB) error {
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
	var channels []agentHostSlackChannel
	cursor := ""
	for page := 0; page < 10; page++ {
		var response struct {
			Channels []agentHostSlackChannel `json:"channels"`
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
		if err := slackAgentAPIGet(c.Request.Context(), installation.BotToken, "conversations.list", payload, &response); err != nil {
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

// JoinAgentDeploymentSlackChannel lets a deployment manager add the shared bot
// to a public Slack channel before allowlisting it. Private channels cannot be
// self-joined by Slack apps and must still be opened in Slack and explicitly
// invited by a channel member.
func (h *WorkflowHandler) JoinAgentDeploymentSlackChannel(c *gin.Context) {
	deployment, ok := h.loadAgentDeployment(c)
	if !ok {
		return
	}
	actorID := auth.UserID(c)
	if !h.canManageAgentDeployment(h.db.DB, deployment, actorID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the deployment owner or an organization admin can manage this agent"})
		return
	}
	if deployment.Status == models.AgentDeploymentRevoked {
		c.JSON(http.StatusConflict, gin.H{"error": "a revoked deployment cannot change Slack membership"})
		return
	}
	if !auth.Allow(c.Request.Context(), h.redis,
		"rl:agent-host-channel-join:"+deployment.OrganizationID+":"+actorID, 20, time.Minute) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many Slack channel join requests; try again in a minute"})
		return
	}
	var request struct {
		HostInstallationID string `json:"hostInstallationId" binding:"required"`
	}
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hostInstallationId is required"})
		return
	}
	channelID := strings.TrimSpace(c.Param("channelId"))
	if !slackChannelIDPattern.MatchString(channelID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Slack channel ID"})
		return
	}
	var installation models.AgentHostInstallation
	if err := h.db.DB.Where("id = ? AND organization_id = ? AND provider = ? AND status = ?",
		strings.TrimSpace(request.HostInstallationID), deployment.OrganizationID, slackAgentProvider, models.AgentHostActive).
		First(&installation).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "active Slack workspace not found"})
		return
	}
	if !oauthScopeContains(installation.Scopes, "channels:join") {
		c.JSON(http.StatusConflict, gin.H{
			"error": "reconnect Slack to add Fernary to public channels automatically",
			"code":  "slack_reconnect_required",
		})
		return
	}
	channel, err := joinSlackAgentPublicChannel(c.Request.Context(), installation.BotToken, channelID)
	if errors.Is(err, errSlackAgentPrivateChannel) {
		c.JSON(http.StatusConflict, gin.H{"error": "private Slack channels must invite Fernary from inside Slack"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not add Fernary to the Slack channel", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, channel)
}

var errSlackAgentPrivateChannel = errors.New("Slack apps cannot self-join private channels")

func joinSlackAgentPublicChannel(ctx context.Context, token, channelID string) (agentHostSlackChannel, error) {
	var info struct {
		Channel agentHostSlackChannel `json:"channel"`
	}
	if err := slackAgentAPIGet(ctx, token, "conversations.info", map[string]any{"channel": channelID}, &info); err != nil {
		return agentHostSlackChannel{}, fmt.Errorf("inspect Slack channel: %w", err)
	}
	if info.Channel.ID != channelID {
		return agentHostSlackChannel{}, errors.New("Slack returned a different channel")
	}
	if info.Channel.IsPrivate {
		return agentHostSlackChannel{}, errSlackAgentPrivateChannel
	}
	if info.Channel.IsMember {
		return info.Channel, nil
	}
	var joined struct {
		Channel agentHostSlackChannel `json:"channel"`
	}
	if err := slackAgentAPICall(ctx, token, "conversations.join", map[string]any{"channel": channelID}, &joined); err != nil {
		return agentHostSlackChannel{}, fmt.Errorf("join Slack channel: %w", err)
	}
	if joined.Channel.ID != channelID {
		return agentHostSlackChannel{}, errors.New("Slack joined a different channel")
	}
	joined.Channel.IsMember = true
	return joined.Channel, nil
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
		if err := slackAgentAPIGet(ctx, token, "conversations.info", map[string]any{"channel": channelID}, &response); err != nil {
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
	for _, required := range slackAgentHostRequiredScopes {
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
