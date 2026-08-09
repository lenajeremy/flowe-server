package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func hostedAgentReconciliationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open reconciliation database: %v", err)
	}
	if err := db.AutoMigrate(
		&models.OrgMember{}, &models.AgentDeployment{}, &models.HostedAgentDelivery{}, &models.HostedAgentApproval{}, &models.ChatSession{},
		&models.IntegrationConnection{},
	); err != nil {
		t.Fatalf("migrate reconciliation database: %v", err)
	}
	return db
}

func TestHostedApprovalPinsConnectedAccountIdentity(t *testing.T) {
	db := hostedAgentReconciliationDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()
	connection := models.IntegrationConnection{
		OrganizationID: orgID, UserID: userID, Provider: "github", AccessToken: "secret",
		WorkspaceID: "acme", WorkspaceName: "Acme GitHub",
	}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	provider, connectionID, details, err := handler.resolveHostedApprovalCredential(orgID, userID, executor.FlowNodeData{
		NodeType: executor.NodeTypeGithub, IntegrationOp: "create_issue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider != "github" || connectionID != connection.ID.String() || details["workspace"] != "Acme GitHub" {
		t.Fatalf("pinned credential = (%q, %q, %#v)", provider, connectionID, details)
	}
	deployment := safetyDeployment(orgID, userID)
	approval := models.HostedAgentApproval{CredentialProvider: provider, CredentialConnectionID: connectionID}
	if err := verifyHostedApprovalCredentialOn(db, &approval, &deployment); err != nil {
		t.Fatalf("unchanged credential rejected: %v", err)
	}
	if err := db.Unscoped().Delete(&connection).Error; err != nil {
		t.Fatal(err)
	}
	replacement := connection
	replacement.BaseModel = models.BaseModel{}
	replacement.AccessToken = "replacement"
	if err := db.Create(&replacement).Error; err != nil {
		t.Fatal(err)
	}
	if err := verifyHostedApprovalCredentialOn(db, &approval, &deployment); !errors.Is(err, errHostedCredentialChanged) {
		t.Fatalf("reconnected credential verification = %v, want changed", err)
	}
}

func TestHostedDeliveryReplyIsDurableBeforeSlackPost(t *testing.T) {
	db := hostedAgentReconciliationDB(t)
	deployment := safetyDeployment(uuid.NewString(), uuid.NewString())
	if err := db.Create(&deployment).Error; err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	delivery := models.HostedAgentDelivery{
		Provider: slackAgentProvider, ExternalDeliveryID: uuid.NewString(), ThreadKey: "T:C:100.1",
		EventKind: "mention", Payload: models.JSONB(`{}`), Status: models.HostedAgentDeliveryProcessing,
		AvailableAt: time.Now().UTC(),
	}
	if err := db.Create(&delivery).Error; err != nil {
		t.Fatalf("create delivery: %v", err)
	}

	oldClient := slackAgentHTTPClient
	t.Cleanup(func() { slackAgentHTTPClient = oldClient })
	var posted []string
	slackAgentHTTPClient = &http.Client{Transport: slackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode Slack payload: %v", err)
		}
		posted = append(posted, payload["text"].(string))
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: http.Header{}}, nil
	})}

	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	if err := handler.slackAgentPostDeliveryText(context.Background(), &delivery, &deployment, "xoxb-test", "C123", "100.1", "first answer"); err != nil {
		t.Fatalf("first post: %v", err)
	}
	if err := handler.slackAgentPostDeliveryText(context.Background(), &delivery, &deployment, "xoxb-test", "C123", "100.1", "different retry answer"); err != nil {
		t.Fatalf("retry post: %v", err)
	}
	if len(posted) != 2 || posted[0] != "first answer" || posted[1] != "first answer" {
		t.Fatalf("posted replies = %#v, want durable first answer twice", posted)
	}
	var stored models.HostedAgentDelivery
	if err := db.First(&stored, "id = ?", delivery.ID).Error; err != nil {
		t.Fatalf("reload delivery: %v", err)
	}
	if stored.ResponseRecordedAt == nil || stored.ResponseDeploymentID != deployment.ID.String() || stored.ResponseText != "first answer" {
		t.Fatalf("durable reply record = %#v", stored)
	}
}

func TestDispatchedToolErrorBecomesUnknownOutcome(t *testing.T) {
	db := hostedAgentReconciliationDB(t)
	approval := models.HostedAgentApproval{
		OrganizationID: uuid.NewString(), DeploymentID: uuid.NewString(), DeploymentVersion: 1,
		ThreadID: uuid.NewString(), ChatSessionID: uuid.NewString(), RequesterExternalID: "U123",
		SourceDeliveryID: uuid.NewString(), NodeID: "node-1", Operation: "create_issue", Reason: "requested",
		EffectiveOverrides: models.JSONB(`{}`), EffectiveConfigHash: "hash", DisplayDetails: models.JSONB(`{}`),
		Status: models.HostedAgentApprovalExecuting, ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := db.Create(&approval).Error; err != nil {
		t.Fatal(err)
	}
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	message := "provider timed out after execution started"
	if err := handler.markHostedApprovalOutcomeUnknownOn(db, &approval, message); err != nil {
		t.Fatal(err)
	}
	var stored models.HostedAgentApproval
	if err := db.First(&stored, "id = ?", approval.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.HostedAgentApprovalOutcomeUnknown || stored.LastError != message {
		t.Fatalf("indeterminate tool result was not blocked: %#v", stored)
	}
	candidate := stored
	candidate.BaseModel.ID = uuid.New()
	if unresolved, err := findEquivalentUnresolvedHostedApproval(db, &candidate); err != nil || unresolved == nil {
		t.Fatalf("equivalent retry was not blocked: approval=%#v err=%v", unresolved, err)
	}
}

func TestEquivalentApprovalBlockedUntilUnknownOutcomeIsReconciled(t *testing.T) {
	db := hostedAgentReconciliationDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()
	if err := db.Create(&models.OrgMember{
		OrganizationID: orgID, UserID: userID, Role: models.RoleMember,
	}).Error; err != nil {
		t.Fatalf("create membership: %v", err)
	}
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

	startedAt := time.Now().UTC()
	unknown := models.HostedAgentApproval{
		OrganizationID: orgID, DeploymentID: deployment.ID.String(), DeploymentVersion: deployment.Version,
		ThreadID: uuid.NewString(), ChatSessionID: session.ID.String(), RequesterExternalID: "U123",
		SourceDeliveryID: uuid.NewString(), NodeID: "node-1", Operation: "create_issue", Reason: "requested",
		EffectiveOverrides: models.JSONB(`{"title":"same issue"}`), EffectiveConfigHash: "same-call-hash",
		ExecutionFingerprint: "same-execution-fingerprint",
		DisplayDetails:       models.JSONB(`{"title":"same issue"}`), Status: models.HostedAgentApprovalOutcomeUnknown,
		ExecutionKey: uuid.NewString(), ExecutionStartedAt: &startedAt, ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := db.Create(&unknown).Error; err != nil {
		t.Fatalf("create unresolved approval: %v", err)
	}
	if err := db.Model(&deployment).Update("version", 2).Error; err != nil {
		t.Fatalf("publish next deployment version: %v", err)
	}
	deployment.Version = 2
	candidate := models.HostedAgentApproval{
		OrganizationID: orgID, DeploymentID: deployment.ID.String(), DeploymentVersion: deployment.Version,
		ThreadID: uuid.NewString(), ChatSessionID: session.ID.String(), RequesterExternalID: "U456",
		SourceDeliveryID: uuid.NewString(), NodeID: unknown.NodeID, Operation: unknown.Operation, Reason: "requested again",
		EffectiveOverrides: unknown.EffectiveOverrides, EffectiveConfigHash: unknown.EffectiveConfigHash,
		ExecutionFingerprint: unknown.ExecutionFingerprint,
		DisplayDetails:       unknown.DisplayDetails, Status: models.HostedAgentApprovalPending, ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := db.Create(&candidate).Error; err != nil {
		t.Fatalf("create candidate approval: %v", err)
	}
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	claimed, err := handler.claimHostedAgentApproval(&candidate, &deployment)
	var unresolvedErr *hostedApprovalOutcomeUnresolvedError
	if claimed || !errors.As(err, &unresolvedErr) {
		t.Fatalf("claim with unresolved equivalent = (%v, %v), want blocked", claimed, err)
	}

	reconciled, err := reconcileHostedAgentApprovalOn(db, &unknown, &deployment, "U456", true)
	if err == nil || reconciled {
		t.Fatalf("different requester reconciled approval = (%v, %v), want rejection", reconciled, err)
	}
	reconciled, err = reconcileHostedAgentApprovalOn(db, &unknown, &deployment, "U123", true)
	if err != nil || !reconciled {
		t.Fatalf("reconcile unknown approval = (%v, %v), want success", reconciled, err)
	}
	var storedUnknown models.HostedAgentApproval
	if err := db.First(&storedUnknown, "id = ?", unknown.ID).Error; err != nil {
		t.Fatalf("reload reconciled approval: %v", err)
	}
	if storedUnknown.Status != models.HostedAgentApprovalReconciledDone || storedUnknown.OutcomeReconciledAt == nil ||
		storedUnknown.OutcomeReconciledBy != "U123" || storedUnknown.SessionRecordedAt == nil {
		t.Fatalf("reconciled approval was not durably recorded: %#v", storedUnknown)
	}
	if err := db.First(&session, "id = ?", session.ID).Error; err != nil {
		t.Fatalf("reload reconciled session: %v", err)
	}
	var history []agentStoredMessage
	if err := json.Unmarshal(session.Messages, &history); err != nil || len(history) != 1 ||
		!strings.Contains(history[0].Content, "completed externally") {
		t.Fatalf("session did not retain the reconciled outcome: history=%#v err=%v", history, err)
	}

	claimed, err = handler.claimHostedAgentApproval(&candidate, &deployment)
	if err != nil || !claimed {
		t.Fatalf("claim after reconciliation = (%v, %v), want success", claimed, err)
	}
}

func TestFreshEquivalentApprovalIsRejectedWhileOutcomeIsUnknown(t *testing.T) {
	db := hostedAgentReconciliationDB(t)
	orgID, userID := uuid.NewString(), uuid.NewString()
	deployment := safetyDeployment(orgID, userID)
	if err := db.Create(&deployment).Error; err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	resolved := &resolvedSlackDeployment{Deployment: deployment}
	thread := &models.HostedAgentThread{BaseModel: models.BaseModel{ID: uuid.New()}}
	session := &models.ChatSession{BaseModel: models.BaseModel{ID: uuid.New()}}
	call := AgentAuthorizedCall{
		Node: executor.WorkflowASTNode{
			ID: "email-node",
			Data: executor.FlowNodeData{
				NodeType: executor.NodeTypeEmailSend, Label: "Send update",
				EmailTo: "team@example.com", EmailSubject: "Update", EmailBody: "{{lookup.output.body}}",
			},
		},
		Operation: AgentOperationCapability{ID: "send_email", Label: "Send email", Effect: AgentEffectWrite},
		Overrides: map[string]any{}, Reason: "send the requested update",
	}
	firstDelivery := &models.HostedAgentDelivery{BaseModel: models.BaseModel{ID: uuid.New()}}
	activity, err := handler.createHostedAgentApproval(
		context.Background(), firstDelivery, resolved, thread, session, "U123", call,
		map[string]string{"lookup": `{"body":"Done"}`, "unrelated": `{"secret":"must-not-be-copied"}`},
	)
	if err != nil {
		t.Fatalf("create first approval: %v", err)
	}
	var firstApproval models.HostedAgentApproval
	if err := db.First(&firstApproval, "id = ?", activity.ApprovalID).Error; err != nil {
		t.Fatalf("load first approval: %v", err)
	}
	if firstApproval.ExecutionKey != firstApproval.ID.String() {
		t.Fatalf("durable execution key = %q, want approval UUID %q", firstApproval.ExecutionKey, firstApproval.ID)
	}
	if firstApproval.ExecutionFingerprint == "" {
		t.Fatal("approval did not persist its normalized execution fingerprint")
	}
	if firstApproval.ConfigRecordedAt == nil || !strings.Contains(string(firstApproval.ExecutionConfig), `"emailBody":"Done"`) {
		t.Fatalf("approval did not pin its resolved execution config: %s", firstApproval.ExecutionConfig)
	}
	if strings.Contains(string(firstApproval.ExecutionConfig), "must-not-be-copied") {
		t.Fatalf("approval copied unrelated session state: %s", firstApproval.ExecutionConfig)
	}
	if err := db.Model(&firstApproval).Update("status", models.HostedAgentApprovalOutcomeUnknown).Error; err != nil {
		t.Fatalf("mark first outcome unknown: %v", err)
	}
	nextDeployment := safetyDeployment(orgID, userID)
	nextDeployment.WorkflowID = deployment.WorkflowID
	nextDeployment.Version = 2
	if err := db.Create(&nextDeployment).Error; err != nil {
		t.Fatalf("publish replacement deployment: %v", err)
	}
	resolved.Deployment = nextDeployment
	call.Node.Data.Label = "Renamed update sender"

	secondDelivery := &models.HostedAgentDelivery{BaseModel: models.BaseModel{ID: uuid.New()}}
	_, err = handler.createHostedAgentApproval(
		context.Background(), secondDelivery, resolved, thread, session, "U456", call,
		map[string]string{"lookup": `{"body":"Done"}`},
	)
	var unresolvedErr *hostedApprovalOutcomeUnresolvedError
	if !errors.As(err, &unresolvedErr) {
		t.Fatalf("fresh equivalent approval error = %v, want unresolved outcome", err)
	}
	var approvalCount int64
	if err := db.Model(&models.HostedAgentApproval{}).Count(&approvalCount).Error; err != nil {
		t.Fatalf("count approvals: %v", err)
	}
	if approvalCount != 1 {
		t.Fatalf("stored %d approvals, want only the unresolved attempt across deployments", approvalCount)
	}
}
