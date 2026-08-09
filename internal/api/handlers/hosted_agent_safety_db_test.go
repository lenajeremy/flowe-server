package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
		&models.AgentDeployment{},
		&models.AgentDeploymentTarget{},
		&models.HostedAgentApproval{},
		&models.ChatSession{},
	); err != nil {
		t.Fatalf("migrate hosted agent safety models: %v", err)
	}
	return db
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
