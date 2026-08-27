package codingagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"workflow-ai/server/internal/database/models"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func serviceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.OrgMember{}, &models.CodingAgentCredential{},
		&models.CodingAgentAuthAttempt{}, &models.CodingAgentEnvironment{},
		&models.CodingAgentSession{}, &models.CodingAgentJob{}, &models.CodingAgentToolCall{},
		&models.CodingAgentEvent{}, &models.CodingAgentArtifact{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCompleteCodexConnectionCannotReviveCancelledAuthority(t *testing.T) {
	db := serviceTestDB(t)
	service := &Service{db: db, store: NewStore(db)}
	orgID, userID := uuid.NewString(), uuid.NewString()
	if err := db.Create(&models.OrgMember{OrganizationID: orgID, UserID: userID, Role: models.RoleMember}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	attempt := &models.CodingAgentAuthAttempt{
		OrganizationID: orgID, UserID: userID, Runtime: RuntimeCodex, Provider: ProviderDaytona,
		Status: models.CodingAgentAuthCancelled, ExpiresAt: now.Add(time.Minute), CompletedAt: &now,
	}
	if err := db.Create(attempt).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.completeCodexConnection(context.Background(), attempt, []byte(`{"tokens":{"access_token":"secret"}}`)); err == nil {
		t.Fatal("cancelled authentication was completed")
	}
	var credentials int64
	if err := db.Model(&models.CodingAgentCredential{}).Count(&credentials).Error; err != nil {
		t.Fatal(err)
	}
	if credentials != 0 {
		t.Fatalf("cancelled authentication stored %d credentials", credentials)
	}
}

func TestCancelAuthAttemptIsImmediatelyTerminalAndReleasesActiveKey(t *testing.T) {
	db := serviceTestDB(t)
	service := &Service{db: db, store: NewStore(db), authCancels: make(map[string]context.CancelFunc)}
	orgID, userID := uuid.NewString(), uuid.NewString()
	activeKey := orgID + ":" + userID + ":" + RuntimeCodex
	attempt := &models.CodingAgentAuthAttempt{
		ActiveKey: &activeKey, OrganizationID: orgID, UserID: userID, Runtime: RuntimeCodex,
		Provider: ProviderDaytona, Status: models.CodingAgentAuthWaiting, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	if err := db.Create(attempt).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.CancelAuthAttempt(context.Background(), orgID, userID, attempt.ID.String()); err != nil {
		t.Fatal(err)
	}
	var stored models.CodingAgentAuthAttempt
	if err := db.First(&stored, "id = ?", attempt.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.CodingAgentAuthCancelled || stored.ActiveKey != nil || stored.CompletedAt == nil || stored.CancelRequestedAt == nil {
		t.Fatalf("attempt was not cancelled atomically: %#v", stored)
	}
	replacement := *attempt
	replacement.ID = uuid.Nil
	replacement.Status = models.CodingAgentAuthProvisioning
	replacement.CompletedAt = nil
	replacement.CancelRequestedAt = nil
	if err := db.Create(&replacement).Error; err != nil {
		t.Fatalf("cancelled attempt still blocked a replacement: %v", err)
	}
}

func TestCompleteCodexConnectionCommitsCredentialAndAttemptTogether(t *testing.T) {
	db := serviceTestDB(t)
	service := &Service{db: db, store: NewStore(db)}
	orgID, userID := uuid.NewString(), uuid.NewString()
	if err := db.Create(&models.OrgMember{OrganizationID: orgID, UserID: userID, Role: models.RoleMember}).Error; err != nil {
		t.Fatal(err)
	}
	activeKey := orgID + ":" + userID + ":" + RuntimeCodex
	attempt := &models.CodingAgentAuthAttempt{
		ActiveKey: &activeKey, OrganizationID: orgID, UserID: userID, Runtime: RuntimeCodex,
		Provider: ProviderDaytona, Status: models.CodingAgentAuthWaiting, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	if err := db.Create(attempt).Error; err != nil {
		t.Fatal(err)
	}
	authBundle := []byte(`{"tokens":{"access_token":"secret"}}`)
	if err := service.completeCodexConnection(context.Background(), attempt, authBundle); err != nil {
		t.Fatal(err)
	}
	var storedAttempt models.CodingAgentAuthAttempt
	if err := db.First(&storedAttempt, "id = ?", attempt.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedAttempt.Status != models.CodingAgentAuthConnected || storedAttempt.ActiveKey != nil || storedAttempt.CompletedAt == nil {
		t.Fatalf("authentication attempt was not completed: %#v", storedAttempt)
	}
	var credential models.CodingAgentCredential
	if err := db.Where("organization_id = ? AND user_id = ? AND runtime = ?", orgID, userID, RuntimeCodex).First(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if credential.Status != models.CodingAgentCredentialConnected || credential.AuthBundle != string(authBundle) {
		t.Fatalf("credential was not connected: %#v", credential)
	}
}

func runningJobFixture(t *testing.T, db *gorm.DB) (*models.CodingAgentJob, *models.CodingAgentSession, *models.CodingAgentEnvironment) {
	t.Helper()
	orgID, userID := uuid.NewString(), uuid.NewString()
	job := &models.CodingAgentJob{
		OrganizationID: orgID, UserID: userID, WorkflowID: uuid.NewString(), WorkflowRunID: uuid.NewString(),
		NodeID: "coding-node", IdempotencyKey: "fixture-" + uuid.NewString(), Runtime: RuntimeCodex,
		Task: "fix it", Input: models.JSONB(`{}`), ExecutionPolicy: models.JSONB(`{}`),
		Status: models.CodingAgentJobRunning, ClaimedBy: "worker-1", AvailableAt: time.Now().UTC(),
		Result: models.JSONB(`{}`), NextEventSequence: 1,
	}
	if err := db.Create(job).Error; err != nil {
		t.Fatal(err)
	}
	environment := &models.CodingAgentEnvironment{
		OrganizationID: orgID, UserID: userID, WorkflowID: job.WorkflowID, NodeID: job.NodeID,
		WorkspaceKey: uuid.NewString(), Provider: ProviderDaytona, Status: models.CodingAgentEnvironmentBusy,
		CurrentJobID: job.ID.String(), AutoStopMinutes: 15, AutoDeleteMinutes: 60, Configuration: models.JSONB(`{}`),
	}
	if err := db.Create(environment).Error; err != nil {
		t.Fatal(err)
	}
	session := &models.CodingAgentSession{
		OrganizationID: orgID, UserID: userID, EnvironmentID: environment.ID.String(), Runtime: RuntimeCodex,
		ConversationKey: uuid.NewString(), Status: models.CodingAgentSessionActive,
	}
	if err := db.Create(session).Error; err != nil {
		t.Fatal(err)
	}
	return job, session, environment
}

func TestFinalizeRuntimeSuccessCommitsOutcomeAndLeaseAtomically(t *testing.T) {
	db := serviceTestDB(t)
	service := &Service{db: db, store: NewStore(db)}
	job, session, environment := runningJobFixture(t, db)
	result := RuntimeResult{
		ExternalThreadID: "thread-123", Summary: "Fixed the failing test",
		Output:    map[string]any{"summary": "Fixed the failing test"},
		Artifacts: []Artifact{{Kind: "patch", Path: "changes.patch", MediaType: "text/x-diff", Content: "secret patch body", SizeBytes: 17}},
	}
	policy := ExecutionPolicy{WorkspaceMode: WorkspacePersistent}
	if err := service.finalizeRuntimeSuccess(context.Background(), job, "worker-1", session, environment, policy, result); err != nil {
		t.Fatal(err)
	}
	var storedJob models.CodingAgentJob
	if err := db.First(&storedJob, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedJob.Status != models.CodingAgentJobSucceeded || storedJob.Summary != result.Summary || storedJob.CompletedAt == nil {
		t.Fatalf("job was not finalized: %#v", storedJob)
	}
	if strings.Contains(string(storedJob.Result), "secret patch body") {
		t.Fatal("large artifact content was duplicated into the job result")
	}
	var storedEnvironment models.CodingAgentEnvironment
	if err := db.First(&storedEnvironment, "id = ?", environment.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedEnvironment.Status != models.CodingAgentEnvironmentReady || storedEnvironment.CurrentJobID != "" {
		t.Fatalf("environment lease was not released atomically: %#v", storedEnvironment)
	}
	var storedSession models.CodingAgentSession
	if err := db.First(&storedSession, "id = ?", session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedSession.ExternalThreadID != "thread-123" || storedSession.LastJobID != job.ID.String() {
		t.Fatalf("session continuity was not updated: %#v", storedSession)
	}
	var artifacts []models.CodingAgentArtifact
	if err := db.Where("job_id = ?", job.ID).Find(&artifacts).Error; err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].InlineContent != "secret patch body" {
		t.Fatalf("artifact was not retained: %#v", artifacts)
	}
	var events []models.CodingAgentEvent
	if err := db.Where("job_id = ?", job.ID).Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "completed" {
		t.Fatalf("completion event was not committed: %#v", events)
	}
}

func TestFinalizeRuntimeSuccessRejectsOversizedArtifactWithoutPartialWrites(t *testing.T) {
	db := serviceTestDB(t)
	service := &Service{db: db, store: NewStore(db)}
	job, session, environment := runningJobFixture(t, db)
	result := RuntimeResult{Summary: "done", Artifacts: []Artifact{{Kind: "patch", Content: strings.Repeat("x", maxInlineArtifactBytes+1)}}}
	if err := service.finalizeRuntimeSuccess(context.Background(), job, "worker-1", session, environment, ExecutionPolicy{WorkspaceMode: WorkspacePersistent}, result); err == nil {
		t.Fatal("oversized artifact was accepted")
	}
	var storedJob models.CodingAgentJob
	_ = db.First(&storedJob, "id = ?", job.ID).Error
	if storedJob.Status != models.CodingAgentJobRunning {
		t.Fatalf("job changed despite rejected finalization: %s", storedJob.Status)
	}
	var artifacts int64
	db.Model(&models.CodingAgentArtifact{}).Where("job_id = ?", job.ID).Count(&artifacts)
	if artifacts != 0 {
		t.Fatalf("partial artifacts were stored: %d", artifacts)
	}
}

func TestAuthAttemptActiveKeyAllowsOneLiveAttemptAndManyTerminalRows(t *testing.T) {
	db := serviceTestDB(t)
	key := uuid.NewString()
	active := models.CodingAgentAuthAttempt{
		ActiveKey: &key, OrganizationID: uuid.NewString(), UserID: uuid.NewString(), Runtime: RuntimeCodex,
		Provider: ProviderDaytona, Status: models.CodingAgentAuthWaiting, ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := db.Create(&active).Error; err != nil {
		t.Fatal(err)
	}
	duplicate := active
	duplicate.ID = uuid.Nil
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("duplicate active authentication attempt was accepted")
	}
	for index := 0; index < 2; index++ {
		terminal := active
		terminal.ID = uuid.Nil
		terminal.ActiveKey = nil
		terminal.Status = models.CodingAgentAuthCancelled
		if err := db.Create(&terminal).Error; err != nil {
			t.Fatalf("terminal attempt %d rejected: %v", index, err)
		}
	}
}

func TestDeviceLoginParserStripsTerminalFormattingFromVerificationURL(t *testing.T) {
	parser := &deviceLoginParser{}
	verificationURL, userCode := parser.Feed("Open https://auth.openai.com/codex/device\x1b[0m\r\nEnter this one-time code\r\nABCD-EFGH\x1b[0m")
	if verificationURL != "https://auth.openai.com/codex/device" {
		t.Fatalf("verification URL = %q", verificationURL)
	}
	if userCode != "ABCD-EFGH" {
		t.Fatalf("user code = %q", userCode)
	}
}

func TestDeviceLoginParserDoesNotMistakeCommandPathForUserCode(t *testing.T) {
	parser := &deviceLoginParser{}
	_, userCode := parser.Feed("env CODEX_HOME=/tmp/fernary-codex-auth codex login --device-auth")
	if userCode != "" {
		t.Fatalf("command path was accepted as user code %q", userCode)
	}
	_, userCode = parser.Feed("\r\nEnter this one-time code (expires in 15 minutes)\r\nDLIX-DU2NG\r\n")
	if userCode != "DLIX-DU2NG" {
		t.Fatalf("real device code = %q", userCode)
	}
}

func TestDeviceLoginParserRejectsLookalikeAuthHost(t *testing.T) {
	parser := &deviceLoginParser{}
	verificationURL, _ := parser.Feed("Open https://auth.openai.com.attacker.example/codex/device")
	if verificationURL != "" {
		t.Fatalf("accepted untrusted verification URL %q", verificationURL)
	}
}
