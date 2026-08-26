package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
)

// These tests exercise transaction and row-lock guarantees against Postgres.
//
// TEST_DATABASE_URL="host=localhost user=postgres password=postgres dbname=workflow_ai port=5434 sslmode=disable"
func hostedAgentSafetyDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run hosted agent safety tests")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(
		&models.OrgMember{},
		&models.AgentHostInstallation{},
		&models.AgentDeployment{},
		&models.AgentDeploymentTarget{},
		&models.HostedAgentDelivery{},
		&models.HostedAgentApproval{},
		&models.ChatSession{},
		&models.CodingAgentCredential{},
		&models.CodingAgentAuthAttempt{},
		&models.CodingAgentJob{},
		&models.CodingAgentEvent{},
	); err != nil {
		t.Fatalf("migrate hosted agent safety models: %v", err)
	}
	return db
}

func TestHostedDeliveryClaimsPreserveThreadOrder(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	threadKey := "T" + uuid.NewString() + ":C123:100.1"
	createdAt := time.Now().UTC().Add(-time.Minute)
	deliveries := []models.HostedAgentDelivery{
		{
			BaseModel: models.BaseModel{CreatedAt: createdAt}, Provider: slackAgentProvider,
			ExternalDeliveryID: uuid.NewString(), ExternalWorkspaceID: "T123", ThreadKey: threadKey,
			EventKind: "mention", Payload: models.JSONB(`{}`), Status: models.HostedAgentDeliveryPending,
			AvailableAt: createdAt,
		},
		{
			BaseModel: models.BaseModel{CreatedAt: createdAt.Add(time.Millisecond)}, Provider: slackAgentProvider,
			ExternalDeliveryID: uuid.NewString(), ExternalWorkspaceID: "T123", ThreadKey: threadKey,
			EventKind: "mention", Payload: models.JSONB(`{}`), Status: models.HostedAgentDeliveryPending,
			AvailableAt: createdAt,
		},
	}
	if err := db.Create(&deliveries).Error; err != nil {
		t.Fatalf("create ordered deliveries: %v", err)
	}
	t.Cleanup(func() { db.Unscoped().Where("thread_key = ?", threadKey).Delete(&models.HostedAgentDelivery{}) })

	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	first, err := handler.claimHostedAgentDelivery()
	if err != nil || first == nil || first.ID != deliveries[0].ID {
		t.Fatalf("first claim = (%v, %v), want %s", first, err, deliveries[0].ID)
	}
	blocked, err := handler.claimHostedAgentDelivery()
	if err != nil || blocked != nil {
		t.Fatalf("later same-thread delivery claimed early = (%v, %v)", blocked, err)
	}
	handler.finishHostedAgentDelivery(first, nil)
	second, err := handler.claimHostedAgentDelivery()
	if err != nil || second == nil || second.ID != deliveries[1].ID {
		t.Fatalf("second claim = (%v, %v), want %s", second, err, deliveries[1].ID)
	}
	handler.finishHostedAgentDelivery(second, nil)
}

func safetyDeployment(orgID, userID string) models.AgentDeployment {
	return models.AgentDeployment{
		OrganizationID: orgID, WorkflowID: uuid.NewString(), DeployedByUserID: userID,
		HostInstallationID: uuid.NewString(), Provider: slackAgentProvider,
		Name: "Safety agent", Alias: "safety-agent", Version: 1, Status: models.AgentDeploymentActive,
		SnapshotName: "Safety workflow", SnapshotNodes: models.JSONB(`[]`), SnapshotEdges: models.JSONB(`[]`),
		SnapshotHash: uuid.NewString(), CapabilityPolicy: models.JSONB(`{"version":1,"nodes":[]}`),
		PermissionAnalysis: models.JSONB(`{}`),
	}
}

func cleanupHostedAgentSafety(t *testing.T, db *gorm.DB, orgID string) {
	t.Helper()
	t.Cleanup(func() {
		db.Unscoped().Where("organization_id = ?", orgID).Delete(&models.HostedAgentApproval{})
		db.Unscoped().Where("organization_id = ?", orgID).Delete(&models.AgentDeploymentTarget{})
		db.Unscoped().Where("organization_id = ?", orgID).Delete(&models.AgentDeployment{})
		db.Unscoped().Where("organization_id = ?", orgID).Delete(&models.ChatSession{})
		db.Unscoped().Where("organization_id = ?", orgID).Delete(&models.AgentHostInstallation{})
		db.Unscoped().Where("organization_id = ?", orgID).Delete(&models.CodingAgentJob{})
		db.Unscoped().Where("organization_id = ?", orgID).Delete(&models.CodingAgentEvent{})
		db.Unscoped().Where("organization_id = ?", orgID).Delete(&models.CodingAgentAuthAttempt{})
		db.Unscoped().Where("organization_id = ?", orgID).Delete(&models.CodingAgentCredential{})
		db.Unscoped().Where("organization_id = ?", orgID).Delete(&models.OrgMember{})
	})
}

func TestMemberRemovalAtomicallyRevokesHostedAgentAuthority(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()
	cleanupHostedAgentSafety(t, db, orgID)
	if err := db.Create(&models.OrgMember{
		OrganizationID: orgID, UserID: userID, Role: models.RoleMember,
	}).Error; err != nil {
		t.Fatalf("create membership: %v", err)
	}
	deployment := safetyDeployment(orgID, userID)
	if err := db.Create(&deployment).Error; err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	target := models.AgentDeploymentTarget{
		OrganizationID: orgID, DeploymentID: deployment.ID.String(), Provider: slackAgentProvider,
		ExternalWorkspaceID: "T" + uuid.NewString(), ExternalChannelID: "C" + uuid.NewString(), Enabled: true,
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatalf("create deployment target: %v", err)
	}

	if err := removeMemberAndRevokeAgentAuthority(db, orgID, userID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	var memberCount int64
	db.Model(&models.OrgMember{}).Where("organization_id = ? AND user_id = ?", orgID, userID).Count(&memberCount)
	if memberCount != 0 {
		t.Fatalf("membership remains after removal: %d", memberCount)
	}
	if err := db.First(&deployment, "id = ?", deployment.ID).Error; err != nil {
		t.Fatalf("reload deployment: %v", err)
	}
	if deployment.Status != models.AgentDeploymentRevoked {
		t.Fatalf("deployment status = %q, want revoked", deployment.Status)
	}
	if err := db.First(&target, "id = ?", target.ID).Error; err != nil {
		t.Fatalf("reload target: %v", err)
	}
	if target.Enabled {
		t.Fatal("revoked deployment target remains enabled")
	}
}

func TestAgentDeploymentStatusUpdateUsesCleanLockedStatement(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()
	cleanupHostedAgentSafety(t, db, orgID)
	deployment := safetyDeployment(orgID, userID)
	if err := db.Create(&deployment).Error; err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	err := handler.withHostedAuthorityLock(context.Background(), orgID, userID, func(connection *gorm.DB) error {
		var live models.AgentDeployment
		if err := connection.Where("id = ? AND organization_id = ? AND status <> ?",
			deployment.ID, orgID, models.AgentDeploymentRevoked).First(&live).Error; err != nil {
			return err
		}
		return updateAgentDeploymentOnLockedConnection(connection, &live, map[string]any{
			"status": models.AgentDeploymentPaused,
		})
	})
	if err != nil {
		t.Fatalf("update deployment under authority lock: %v", err)
	}
	var stored models.AgentDeployment
	if err := db.First(&stored, "id = ?", deployment.ID).Error; err != nil {
		t.Fatalf("reload deployment: %v", err)
	}
	if stored.Status != models.AgentDeploymentPaused {
		t.Fatalf("deployment status = %q, want paused", stored.Status)
	}
}

func TestHostedAuthorityVerificationUsesCleanLockedStatements(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()
	cleanupHostedAgentSafety(t, db, orgID)
	if err := db.Create(&models.OrgMember{
		OrganizationID: orgID, UserID: userID, Role: models.RoleMember,
	}).Error; err != nil {
		t.Fatalf("create membership: %v", err)
	}
	host := models.AgentHostInstallation{
		OrganizationID: orgID, InstalledByUserID: userID, Provider: slackAgentProvider,
		ExternalWorkspaceID:   "T" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10],
		ExternalWorkspaceName: "Safety workspace", BotUserID: "U123", BotToken: "xoxb-test",
		Status: models.AgentHostActive,
	}
	if err := db.Create(&host).Error; err != nil {
		t.Fatalf("create host: %v", err)
	}
	deployment := safetyDeployment(orgID, userID)
	deployment.HostInstallationID = host.ID.String()
	if err := db.Create(&deployment).Error; err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	target := models.AgentDeploymentTarget{
		OrganizationID: orgID, DeploymentID: deployment.ID.String(), Provider: slackAgentProvider,
		ExternalWorkspaceID: host.ExternalWorkspaceID, ExternalChannelID: "C" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10],
		ExternalChannelName: "safety", Enabled: true,
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatalf("create target: %v", err)
	}
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	resolved := &resolvedSlackDeployment{Deployment: deployment, Target: target, Host: host}
	updatedPolicy := models.JSONB(`{"version":1,"nodes":[{"nodeId":"live-node","allowedOperations":["list"],"allowedOverrideFields":[]}]}`)
	if err := db.Model(&models.AgentDeployment{}).Where("id = ?", deployment.ID).
		Update("capability_policy", updatedPolicy).Error; err != nil {
		t.Fatalf("update live deployment policy: %v", err)
	}
	var verified *models.AgentDeployment
	if err := handler.withHostedAuthorityLock(context.Background(), orgID, userID, func(connection *gorm.DB) error {
		var err error
		verified, err = verifyHostedAgentAuthorityOn(connection, resolved)
		return err
	}); err != nil {
		t.Fatalf("verify hosted authority under advisory lock: %v", err)
	}
	if verified == nil {
		t.Fatal("authority verification returned no live deployment")
	}
	assertJSONEqual(t, verified.CapabilityPolicy, updatedPolicy)
}

func TestAgentDeploymentManagementAuthorization(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	orgID := uuid.NewString()
	deployerID, adminID, memberID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cleanupHostedAgentSafety(t, db, orgID)
	members := []models.OrgMember{
		{OrganizationID: orgID, UserID: deployerID, Role: models.RoleMember},
		{OrganizationID: orgID, UserID: adminID, Role: models.RoleAdmin},
		{OrganizationID: orgID, UserID: memberID, Role: models.RoleMember},
	}
	if err := db.Create(&members).Error; err != nil {
		t.Fatalf("create memberships: %v", err)
	}
	deployment := safetyDeployment(orgID, deployerID)
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	if !handler.canManageAgentDeployment(db, &deployment, deployerID) {
		t.Fatal("deployment owner cannot manage their agent")
	}
	if !handler.canManageAgentDeployment(db, &deployment, adminID) {
		t.Fatal("organization admin cannot manage an agent")
	}
	if handler.canManageAgentDeployment(db, &deployment, memberID) {
		t.Fatal("plain organization member can manage another member's agent")
	}
	if handler.canManageAgentDeployment(db, &deployment, uuid.NewString()) {
		t.Fatal("non-member can manage an agent")
	}
}

func TestAgentPolicyUpdateExpiresPendingApprovals(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()
	cleanupHostedAgentSafety(t, db, orgID)
	deployment := safetyDeployment(orgID, userID)
	if err := db.Create(&deployment).Error; err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	approval := models.HostedAgentApproval{
		OrganizationID: orgID, DeploymentID: deployment.ID.String(), DeploymentVersion: deployment.Version,
		ThreadID: uuid.NewString(), ChatSessionID: uuid.NewString(), RequesterExternalID: "U123",
		SourceDeliveryID: uuid.NewString(), NodeID: "node-1", Operation: "create_issue", Reason: "requested",
		EffectiveOverrides: models.JSONB(`{}`), EffectiveConfigHash: "hash", DisplayDetails: models.JSONB(`{}`),
		Status: models.HostedAgentApprovalPending, ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := db.Create(&approval).Error; err != nil {
		t.Fatalf("create approval: %v", err)
	}
	newPolicy := models.JSONB(`{"version":1,"nodes":[{"nodeId":"node-1","allowedOperations":["list"],"allowedOverrideFields":[]}]}`)
	if err := db.Transaction(func(tx *gorm.DB) error {
		return updateAgentDeploymentAndExpireApprovals(tx, &deployment, map[string]any{
			"capability_policy": newPolicy,
		}, "Deployment permissions changed before approval")
	}); err != nil {
		t.Fatalf("update policy: %v", err)
	}
	var storedDeployment models.AgentDeployment
	if err := db.First(&storedDeployment, "id = ?", deployment.ID).Error; err != nil {
		t.Fatalf("reload deployment: %v", err)
	}
	assertJSONEqual(t, storedDeployment.CapabilityPolicy, newPolicy)
	var storedApproval models.HostedAgentApproval
	if err := db.First(&storedApproval, "id = ?", approval.ID).Error; err != nil {
		t.Fatalf("reload approval: %v", err)
	}
	if storedApproval.Status != models.HostedAgentApprovalExpired || storedApproval.ResolvedAt == nil {
		t.Fatalf("approval after policy edit = (%q, %v), want expired with resolved time", storedApproval.Status, storedApproval.ResolvedAt)
	}
}

func TestAgentDestinationUpdateAtomicallyReplacesTargetsAndExpiresApprovals(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()
	cleanupHostedAgentSafety(t, db, orgID)
	oldHost := models.AgentHostInstallation{
		OrganizationID: orgID, InstalledByUserID: userID, Provider: slackAgentProvider,
		ExternalWorkspaceID:   "T" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10],
		ExternalWorkspaceName: "Old workspace", BotToken: "xoxb-old", Status: models.AgentHostActive,
	}
	newHost := models.AgentHostInstallation{
		OrganizationID: orgID, InstalledByUserID: userID, Provider: slackAgentProvider,
		ExternalWorkspaceID:   "T" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10],
		ExternalWorkspaceName: "New workspace", BotToken: "xoxb-new", Status: models.AgentHostActive,
	}
	if err := db.Create(&oldHost).Error; err != nil {
		t.Fatalf("create old host: %v", err)
	}
	if err := db.Create(&newHost).Error; err != nil {
		t.Fatalf("create new host: %v", err)
	}
	deployment := safetyDeployment(orgID, userID)
	deployment.HostInstallationID = oldHost.ID.String()
	if err := db.Create(&deployment).Error; err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	oldTarget := models.AgentDeploymentTarget{
		OrganizationID: orgID, DeploymentID: deployment.ID.String(), Provider: slackAgentProvider,
		ExternalWorkspaceID: oldHost.ExternalWorkspaceID, ExternalChannelID: "COLD123", ExternalChannelName: "old", Enabled: true,
	}
	if err := db.Create(&oldTarget).Error; err != nil {
		t.Fatalf("create old target: %v", err)
	}
	approval := models.HostedAgentApproval{
		OrganizationID: orgID, DeploymentID: deployment.ID.String(), DeploymentVersion: deployment.Version,
		ThreadID: uuid.NewString(), ChatSessionID: uuid.NewString(), RequesterExternalID: "U123",
		SourceDeliveryID: uuid.NewString(), NodeID: "node-1", Operation: "create_issue", Reason: "requested",
		EffectiveOverrides: models.JSONB(`{}`), EffectiveConfigHash: "hash", DisplayDetails: models.JSONB(`{}`),
		Status: models.HostedAgentApprovalPending, ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := db.Create(&approval).Error; err != nil {
		t.Fatalf("create approval: %v", err)
	}
	channels := []agentDeploymentChannelInput{{ID: "CNEW123", Name: "new"}}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := updateAgentDeploymentAndExpireApprovals(tx, &deployment, map[string]any{
			"host_installation_id": newHost.ID.String(), "provider": newHost.Provider,
		}, "Deployment Slack destination changed before approval"); err != nil {
			return err
		}
		return replaceAgentDeploymentTargets(tx, &deployment, &newHost, channels)
	}); err != nil {
		t.Fatalf("replace destination: %v", err)
	}
	var storedDeployment models.AgentDeployment
	if err := db.First(&storedDeployment, "id = ?", deployment.ID).Error; err != nil {
		t.Fatalf("reload deployment: %v", err)
	}
	if storedDeployment.HostInstallationID != newHost.ID.String() {
		t.Fatalf("deployment host = %q, want %q", storedDeployment.HostInstallationID, newHost.ID)
	}
	var liveTargets []models.AgentDeploymentTarget
	if err := db.Where("deployment_id = ?", deployment.ID).Find(&liveTargets).Error; err != nil {
		t.Fatalf("load live targets: %v", err)
	}
	if len(liveTargets) != 1 || liveTargets[0].ExternalWorkspaceID != newHost.ExternalWorkspaceID || liveTargets[0].ExternalChannelID != "CNEW123" {
		t.Fatalf("live targets = %#v, want only new destination", liveTargets)
	}
	var storedApproval models.HostedAgentApproval
	if err := db.First(&storedApproval, "id = ?", approval.ID).Error; err != nil {
		t.Fatalf("reload approval: %v", err)
	}
	if storedApproval.Status != models.HostedAgentApprovalExpired {
		t.Fatalf("approval status = %q, want expired", storedApproval.Status)
	}
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode actual JSON %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode expected JSON %s: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
}

func TestHostedDeliveryKeepsItsFirstResolvedDeployment(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	delivery := models.HostedAgentDelivery{
		Provider: slackAgentProvider, ExternalDeliveryID: uuid.NewString(), ExternalWorkspaceID: "T123",
		EventKind: "mention", Payload: models.JSONB(`{}`), Status: models.HostedAgentDeliveryProcessing,
		AvailableAt: time.Now().UTC(),
	}
	if err := db.Create(&delivery).Error; err != nil {
		t.Fatalf("create delivery: %v", err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&models.HostedAgentDelivery{}, "id = ?", delivery.ID) })
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	firstID, secondID := uuid.NewString(), uuid.NewString()
	associated, err := handler.associateHostedAgentDeliveryWithDeployment(&delivery, firstID)
	if err != nil || !associated {
		t.Fatalf("first association = (%v, %v), want success", associated, err)
	}
	associated, err = handler.associateHostedAgentDeliveryWithDeployment(&delivery, secondID)
	if err != nil || associated {
		t.Fatalf("replacement association = (%v, %v), want safely rejected", associated, err)
	}
	var stored models.HostedAgentDelivery
	if err := db.First(&stored, "id = ?", delivery.ID).Error; err != nil {
		t.Fatalf("reload delivery: %v", err)
	}
	if stored.ResponseDeploymentID != firstID {
		t.Fatalf("delivery deployment = %q, want first deployment %q", stored.ResponseDeploymentID, firstID)
	}
}

func TestLatestHostedAgentDeliveriesReturnsOneNewestRowPerDeployment(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	firstDeploymentID, secondDeploymentID := uuid.NewString(), uuid.NewString()
	base := time.Now().UTC().Add(-time.Minute)
	deliveries := []models.HostedAgentDelivery{
		{BaseModel: models.BaseModel{CreatedAt: base}, Provider: slackAgentProvider, ExternalDeliveryID: uuid.NewString(), EventKind: "mention", Payload: models.JSONB(`{}`), Status: models.HostedAgentDeliveryFailed, AvailableAt: base, ResponseDeploymentID: firstDeploymentID, LastError: "old failure"},
		{BaseModel: models.BaseModel{CreatedAt: base.Add(time.Second)}, Provider: slackAgentProvider, ExternalDeliveryID: uuid.NewString(), EventKind: "mention", Payload: models.JSONB(`{}`), Status: models.HostedAgentDeliveryCompleted, AvailableAt: base, ResponseDeploymentID: firstDeploymentID},
		{BaseModel: models.BaseModel{CreatedAt: base.Add(2 * time.Second)}, Provider: slackAgentProvider, ExternalDeliveryID: uuid.NewString(), EventKind: "mention", Payload: models.JSONB(`{}`), Status: models.HostedAgentDeliveryProcessing, AvailableAt: base, ResponseDeploymentID: secondDeploymentID},
	}
	if err := db.Create(&deliveries).Error; err != nil {
		t.Fatalf("create delivery history: %v", err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("response_deployment_id IN ?", []string{firstDeploymentID, secondDeploymentID}).Delete(&models.HostedAgentDelivery{})
	})
	latest, err := latestHostedAgentDeliveries(db, []string{firstDeploymentID, secondDeploymentID})
	if err != nil {
		t.Fatalf("load latest deliveries: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest deliveries = %d, want 2", len(latest))
	}
	byDeployment := map[string]models.HostedAgentDelivery{}
	for _, delivery := range latest {
		byDeployment[delivery.ResponseDeploymentID] = delivery
	}
	if byDeployment[firstDeploymentID].Status != models.HostedAgentDeliveryCompleted {
		t.Fatalf("first deployment latest status = %q, want completed", byDeployment[firstDeploymentID].Status)
	}
	if byDeployment[secondDeploymentID].Status != models.HostedAgentDeliveryProcessing {
		t.Fatalf("second deployment latest status = %q, want processing", byDeployment[secondDeploymentID].Status)
	}
}

func TestHostedApprovalClaimRechecksDeployerMembership(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()
	cleanupHostedAgentSafety(t, db, orgID)
	deployment := safetyDeployment(orgID, userID)
	if err := db.Create(&deployment).Error; err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	approval := models.HostedAgentApproval{
		OrganizationID: orgID, DeploymentID: deployment.ID.String(), DeploymentVersion: deployment.Version,
		ThreadID: uuid.NewString(), ChatSessionID: uuid.NewString(), RequesterExternalID: "U123",
		SourceDeliveryID: uuid.NewString(), NodeID: "node-1", Operation: "create_issue", Reason: "requested",
		EffectiveOverrides: models.JSONB(`{}`), EffectiveConfigHash: "hash", DisplayDetails: models.JSONB(`{}`),
		Status: models.HostedAgentApprovalPending, ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := db.Create(&approval).Error; err != nil {
		t.Fatalf("create approval: %v", err)
	}
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	claimed, err := handler.claimHostedAgentApproval(&approval, &deployment)
	if claimed || !errors.Is(err, errHostedAgentAuthorityEnded) {
		t.Fatalf("claim without membership = (%v, %v), want authority ended", claimed, err)
	}
	var stored models.HostedAgentApproval
	if err := db.First(&stored, "id = ?", approval.ID).Error; err != nil {
		t.Fatalf("reload denied approval: %v", err)
	}
	if stored.Status != models.HostedAgentApprovalPending {
		t.Fatalf("denied approval status = %q, want pending", stored.Status)
	}

	if err := db.Create(&models.OrgMember{
		OrganizationID: orgID, UserID: userID, Role: models.RoleMember,
	}).Error; err != nil {
		t.Fatalf("create membership: %v", err)
	}
	claimed, err = handler.claimHostedAgentApproval(&approval, &deployment)
	if err != nil || !claimed {
		t.Fatalf("claim with membership = (%v, %v), want success", claimed, err)
	}
}

func TestHostedAuthorityLockBlocksRemovalUntilExecutionFinishes(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()
	cleanupHostedAgentSafety(t, db, orgID)
	if err := db.Create(&models.OrgMember{
		OrganizationID: orgID, UserID: userID, Role: models.RoleMember,
	}).Error; err != nil {
		t.Fatalf("create membership: %v", err)
	}
	deployment := safetyDeployment(orgID, userID)
	if err := db.Create(&deployment).Error; err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	approval := models.HostedAgentApproval{
		OrganizationID: orgID, DeploymentID: deployment.ID.String(), DeploymentVersion: deployment.Version,
		ThreadID: uuid.NewString(), ChatSessionID: uuid.NewString(), RequesterExternalID: "U123",
		SourceDeliveryID: uuid.NewString(), NodeID: "node-1", Operation: "create_issue", Reason: "requested",
		EffectiveOverrides: models.JSONB(`{}`), EffectiveConfigHash: "hash", DisplayDetails: models.JSONB(`{}`),
		Status: models.HostedAgentApprovalPending, ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := db.Create(&approval).Error; err != nil {
		t.Fatalf("create approval: %v", err)
	}
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	claimed := make(chan struct{})
	releaseExecution := make(chan struct{})
	executionDone := make(chan error, 1)
	go func() {
		executionDone <- handler.withHostedAuthorityLock(context.Background(), orgID, userID, func(connection *gorm.DB) error {
			ok, err := handler.claimHostedAgentApprovalOn(connection, &approval, &deployment)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("approval was not claimed")
			}
			close(claimed)
			<-releaseExecution
			return nil
		})
	}()
	select {
	case <-claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("approval did not acquire authority lock")
	}

	removalDone := make(chan error, 1)
	go func() {
		removalDone <- handler.withHostedAuthorityLock(context.Background(), orgID, userID, func(connection *gorm.DB) error {
			return removeMemberAndRevokeAgentAuthority(connection, orgID, userID)
		})
	}()
	select {
	case err := <-removalDone:
		t.Fatalf("member removal completed during execution: %v", err)
	case <-time.After(150 * time.Millisecond):
		// Expected: removal is waiting on the authority lock.
	}
	close(releaseExecution)
	if err := <-executionDone; err != nil {
		t.Fatalf("approval execution lock: %v", err)
	}
	select {
	case err := <-removalDone:
		if err != nil {
			t.Fatalf("remove member after execution: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("member removal did not resume after execution")
	}
	var memberCount int64
	db.Model(&models.OrgMember{}).Where("organization_id = ? AND user_id = ?", orgID, userID).Count(&memberCount)
	if memberCount != 0 {
		t.Fatalf("membership remains after serialized removal: %d", memberCount)
	}
}

func TestExecutedHostedApprovalSessionMergeIsIdempotent(t *testing.T) {
	db := hostedAgentSafetyDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()
	cleanupHostedAgentSafety(t, db, orgID)
	deployment := safetyDeployment(orgID, userID)
	if err := db.Create(&deployment).Error; err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	session := models.ChatSession{
		UserID: userID, OrganizationID: orgID, WorkflowID: deployment.WorkflowID,
		Messages: models.JSONB(`[]`), State: models.JSONB(`{}`),
	}
	if err := db.Create(&session).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Now().UTC()
	approval := models.HostedAgentApproval{
		OrganizationID: orgID, DeploymentID: deployment.ID.String(), DeploymentVersion: deployment.Version,
		ThreadID: uuid.NewString(), ChatSessionID: session.ID.String(), RequesterExternalID: "U123",
		SourceDeliveryID: uuid.NewString(), NodeID: "node-1", Operation: "create_issue", Reason: "requested",
		EffectiveOverrides: models.JSONB(`{}`), EffectiveConfigHash: "hash", DisplayDetails: models.JSONB(`{}`),
		Status: models.HostedAgentApprovalExecuted, ExpiresAt: now.Add(time.Minute), ExecutedAt: &now,
		ExecutionResult: `{"id":"ISSUE-1"}`, ExecutionResultRecordedAt: &now,
	}
	if err := db.Create(&approval).Error; err != nil {
		t.Fatalf("create approval: %v", err)
	}
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	for attempt := 0; attempt < 2; attempt++ {
		if err := handler.syncHostedAgentApprovalSession(&approval, &deployment, "Create issue"); err != nil {
			t.Fatalf("sync attempt %d: %v", attempt+1, err)
		}
	}
	if err := db.First(&session, "id = ?", session.ID).Error; err != nil {
		t.Fatalf("reload session: %v", err)
	}
	var history []agentStoredMessage
	if err := json.Unmarshal(session.Messages, &history); err != nil {
		t.Fatalf("decode session history: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("session history contains %d outcome messages, want 1", len(history))
	}
	var state map[string]string
	if err := json.Unmarshal(session.State, &state); err != nil {
		t.Fatalf("decode session state: %v", err)
	}
	if state[approval.NodeID] != approval.ExecutionResult {
		t.Fatalf("session state result = %q, want %q", state[approval.NodeID], approval.ExecutionResult)
	}
	var stored models.HostedAgentApproval
	if err := db.First(&stored, "id = ?", approval.ID).Error; err != nil {
		t.Fatalf("reload approval: %v", err)
	}
	if stored.SessionRecordedAt == nil {
		t.Fatal("approval session merge was not durably marked")
	}
}
