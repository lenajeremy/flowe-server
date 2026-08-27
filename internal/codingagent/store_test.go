package codingagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"workflow-ai/server/internal/database/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testStore(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.CodingAgentCredential{},
		&models.CodingAgentJob{},
		&models.CodingAgentToolCall{},
		&models.CodingAgentEvent{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	store := NewStore(db)
	store.now = func() time.Time { return now }
	return store, db
}

func connectedCredential(t *testing.T, db *gorm.DB) {
	t.Helper()
	credential := models.CodingAgentCredential{
		OrganizationID: "00000000-0000-0000-0000-000000000001",
		UserID:         "00000000-0000-0000-0000-000000000002",
		Runtime:        RuntimeCodex,
		Status:         models.CodingAgentCredentialConnected,
		AuthBundle:     `{"tokens":{"access_token":"secret"}}`,
		ConnectedAt:    time.Now().UTC(),
	}
	if err := db.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
}

func validRequest() SubmitRequest {
	return SubmitRequest{
		OrganizationID: "00000000-0000-0000-0000-000000000001",
		UserID:         "00000000-0000-0000-0000-000000000002",
		WorkflowID:     "00000000-0000-0000-0000-000000000003",
		WorkflowRunID:  "00000000-0000-0000-0000-000000000004",
		NodeID:         "coding-agent-1",
		Runtime:        RuntimeCodex,
		Task:           "Inspect the repository and fix the failing test.",
		Input:          map[string]any{"issue": 42},
		Policy: ExecutionPolicy{
			WorkspaceMode:       WorkspacePersistent,
			RepositoryProvider:  RepositoryGitHub,
			RepositoryID:        "acme/widgets",
			Repository:          "acme/widgets",
			MaxDurationSeconds:  900,
			AutoStopMinutes:     15,
			AutoDeleteMinutes:   10080,
			NetworkBlockAll:     true,
			AllowedDomains:      []string{"api.openai.com", "github.com"},
			AllowWorkspaceWrite: true,
		},
	}
}

func TestSubmitIsIdempotentAndPinsPolicy(t *testing.T) {
	store, db := testStore(t)
	connectedCredential(t, db)

	first, created, err := store.Submit(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first submit was not created")
	}

	mutated := validRequest()
	mutated.Task = "A different task must not replace the queued job."
	mutated.Policy.RepositoryID = "attacker/other"
	mutated.Policy.Repository = "attacker/other"
	second, created, err := store.Submit(context.Background(), mutated)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("idempotent retry created a second job")
	}
	if second.ID != first.ID {
		t.Fatalf("retry returned job %s, want %s", second.ID, first.ID)
	}
	if second.Task != first.Task {
		t.Fatalf("retry changed task to %q", second.Task)
	}
	policy, err := DecodePolicy(second)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Repository != "acme/widgets" {
		t.Fatalf("retry changed pinned repository to %q", policy.Repository)
	}

	events, err := store.ListEvents(context.Background(), first.OrganizationID, first.ID.String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "queued" || events[0].Sequence != 1 {
		t.Fatalf("unexpected initial events: %#v", events)
	}
}

func TestSubmitRejectsToolGrantWithoutUsableFrozenGraph(t *testing.T) {
	req := validRequest()
	req.ToolGrants = []ToolGrant{{NodeID: "github-1", AllowedOperations: []string{"get_issue"}}}
	req.ToolWorkflow = []byte(`{}`)
	if err := validateSubmitRequest(req); err == nil || !strings.Contains(err.Error(), "valid graph snapshot") {
		t.Fatalf("validateSubmitRequest error = %v, want frozen graph rejection", err)
	}

	req.ToolWorkflow = []byte(`{"nodes":[{"id":"github-1"}],"edges":[]}`)
	if err := validateSubmitRequest(req); err != nil {
		t.Fatalf("valid frozen graph rejected: %v", err)
	}
}

func TestJobLifecycleProducesOrderedDurableEvents(t *testing.T) {
	store, db := testStore(t)
	connectedCredential(t, db)
	job, _, err := store.Submit(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}

	claimed, err := store.ClaimNext(context.Background(), "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != job.ID || claimed.Status != models.CodingAgentJobClaimed {
		t.Fatalf("unexpected claimed job: %#v", claimed)
	}
	if err := store.MarkRunning(context.Background(), job.ID.String(), "worker-1", "daytona-command-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(context.Background(), job.ID.String(), "worker-1", "stdout", "checking tests", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(context.Background(), job.ID.String(), "worker-1", map[string]any{"changedFiles": 2}, "Fixed the tests"); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetJob(context.Background(), job.OrganizationID, job.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.CodingAgentJobSucceeded || got.Summary != "Fixed the tests" || got.CompletedAt == nil {
		t.Fatalf("unexpected completed job: %#v", got)
	}
	events, err := store.ListEvents(context.Background(), job.OrganizationID, job.ID.String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"queued", "claimed", "started", "stdout", "completed"}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %#v", len(events), len(want), events)
	}
	for i, event := range events {
		if event.Sequence != i+1 || event.Type != want[i] {
			t.Fatalf("event %d = (%d, %q), want (%d, %q)", i, event.Sequence, event.Type, i+1, want[i])
		}
	}
	if err := store.Complete(context.Background(), job.ID.String(), "worker-1", nil, "again"); !errors.Is(err, ErrJobTerminal) {
		t.Fatalf("second completion error = %v, want ErrJobTerminal", err)
	}
}

func TestBusyWorkspaceRequeueDoesNotConsumeAttempts(t *testing.T) {
	store, db := testStore(t)
	connectedCredential(t, db)
	job, _, err := store.Submit(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		claimed, claimErr := store.ClaimNext(context.Background(), "worker-1")
		if claimErr != nil {
			t.Fatalf("claim %d: %v", index, claimErr)
		}
		if requeueErr := store.RequeueBusy(context.Background(), claimed.ID.String(), "worker-1", time.Second, "workspace busy"); requeueErr != nil {
			t.Fatalf("busy requeue %d: %v", index, requeueErr)
		}
		if err := db.Model(&models.CodingAgentJob{}).Where("id = ?", claimed.ID).Update("available_at", store.now()).Error; err != nil {
			t.Fatal(err)
		}
	}
	stored, err := store.GetJob(context.Background(), job.OrganizationID, job.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.CodingAgentJobPending || stored.AttemptCount != 0 {
		t.Fatalf("busy workspace consumed retries: status=%s attempts=%d", stored.Status, stored.AttemptCount)
	}
}

func TestCancellationRevokesToolTokenAndOpenCallsAtomically(t *testing.T) {
	store, db := testStore(t)
	connectedCredential(t, db)
	job, _, err := store.Submit(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNext(context.Background(), "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunning(context.Background(), job.ID.String(), "worker-1", "sandbox"); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.CodingAgentJob{}).Where("id = ?", job.ID).Update("tool_token_hash", "usable").Error; err != nil {
		t.Fatal(err)
	}
	call := models.CodingAgentToolCall{
		OrganizationID: job.OrganizationID, UserID: job.UserID, JobID: job.ID.String(),
		RequestKey: "request", Fingerprint: "fingerprint", NodeID: "github", ToolName: "github",
		Operation: "create_branch", Effect: "write", Status: models.CodingAgentToolCallPendingApproval,
		RequestedAt: store.now(), Arguments: models.JSONB(`{}`), EffectiveConfig: models.JSONB(`{}`), Result: models.JSONB(`{}`),
	}
	if err := db.Create(&call).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestCancel(context.Background(), job.OrganizationID, job.UserID, job.ID.String()); err != nil {
		t.Fatal(err)
	}
	var storedJob models.CodingAgentJob
	var storedCall models.CodingAgentToolCall
	if err := db.First(&storedJob, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&storedCall, "id = ?", call.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedJob.ToolTokenHash != "" || storedCall.Status != models.CodingAgentToolCallCancelled {
		t.Fatalf("cancellation left delegated authority: token=%q call=%s", storedJob.ToolTokenHash, storedCall.Status)
	}
}

func TestPendingCancellationPreventsClaim(t *testing.T) {
	store, db := testStore(t)
	connectedCredential(t, db)
	job, _, err := store.Submit(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}

	cancelled, err := store.RequestCancel(context.Background(), job.OrganizationID, job.UserID, job.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != models.CodingAgentJobCancelled || cancelled.CompletedAt == nil {
		t.Fatalf("unexpected cancelled job: %#v", cancelled)
	}
	if _, err := store.ClaimNext(context.Background(), "worker-1"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("claim error = %v, want record not found", err)
	}
}

func TestSubmitRequiresConnectedCredential(t *testing.T) {
	store, _ := testStore(t)
	if _, _, err := store.Submit(context.Background(), validRequest()); !errors.Is(err, ErrCredentialRequired) {
		t.Fatalf("submit error = %v, want ErrCredentialRequired", err)
	}
}

func TestSubmitHashesConversationKeyBeforePersistence(t *testing.T) {
	store, db := testStore(t)
	connectedCredential(t, db)
	request := validRequest()
	request.ConversationKey = "customer-secret-ticket-42"
	job, _, err := store.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if job.ConversationKey == request.ConversationKey || strings.Contains(job.ConversationKey, "ticket") {
		t.Fatalf("raw conversation key was persisted: %q", job.ConversationKey)
	}
	if len(job.ConversationKey) != 64 {
		t.Fatalf("conversation key hash length = %d", len(job.ConversationKey))
	}
}

func TestSubmitEnforcesPerUserActiveJobLimitButAllowsIdempotentRead(t *testing.T) {
	store, db := testStore(t)
	connectedCredential(t, db)
	var first *models.CodingAgentJob
	for index := 0; index < 5; index++ {
		request := validRequest()
		request.WorkflowRunID = fmt.Sprintf("00000000-0000-0000-0000-%012d", index+10)
		job, _, err := store.Submit(context.Background(), request)
		if err != nil {
			t.Fatalf("submit %d: %v", index, err)
		}
		if index == 0 {
			first = job
		}
	}
	blocked := validRequest()
	blocked.WorkflowRunID = "00000000-0000-0000-0000-000000000099"
	if _, _, err := store.Submit(context.Background(), blocked); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("sixth active job error = %v, want ErrRateLimited", err)
	}
	retry := validRequest()
	retry.WorkflowRunID = "00000000-0000-0000-0000-000000000010"
	job, created, err := store.Submit(context.Background(), retry)
	if err != nil || created || job.ID != first.ID {
		t.Fatalf("idempotent read at capacity = (%v, %v, %v), want existing %s", job, created, err, first.ID)
	}
}

func TestValidateAllowedDomainsMatchesDaytonaLimits(t *testing.T) {
	request := validRequest()
	request.Policy.AllowedDomains = []string{"openai.com", "*.openai.com", "github.com"}
	if err := validateSubmitRequest(request); err != nil {
		t.Fatalf("valid wildcard domains rejected: %v", err)
	}
	request.Policy.AllowedDomains = []string{"https://openai.com/path"}
	if err := validateSubmitRequest(request); err == nil {
		t.Fatal("URL was accepted where Daytona requires a domain")
	}
	request.Policy.AllowedDomains = make([]string, 21)
	for index := range request.Policy.AllowedDomains {
		request.Policy.AllowedDomains[index] = fmt.Sprintf("host-%d.example.com", index)
	}
	if err := validateSubmitRequest(request); err == nil {
		t.Fatal("more than Daytona's 20-domain limit was accepted")
	}
}

func TestValidateRepositorySupportsConnectedGitHubAndGitLabProjects(t *testing.T) {
	request := validRequest()
	if err := validateSubmitRequest(request); err != nil {
		t.Fatalf("valid GitHub repository rejected: %v", err)
	}

	request.Policy.RepositoryProvider = RepositoryGitLab
	request.Policy.RepositoryID = "48291"
	request.Policy.Repository = "acme/platform/widgets"
	request.Policy.AllowedDomains = []string{"api.openai.com", "gitlab.com"}
	if err := validateSubmitRequest(request); err != nil {
		t.Fatalf("valid GitLab repository rejected: %v", err)
	}

	request.Policy.RepositoryID = "acme/platform/widgets"
	if err := validateSubmitRequest(request); err == nil || !strings.Contains(err.Error(), "numeric project ID") {
		t.Fatalf("non-numeric GitLab project ID error = %v", err)
	}

	request.Policy.RepositoryProvider = "unknown"
	if err := validateSubmitRequest(request); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported provider error = %v", err)
	}
}

func TestRepositoryCloneSettingsUseProviderSpecificHostsAndUsernames(t *testing.T) {
	tests := []struct {
		policy   ExecutionPolicy
		provider string
		url      string
		username string
	}{
		{
			policy:   ExecutionPolicy{RepositoryProvider: RepositoryGitHub, Repository: "acme/widgets"},
			provider: RepositoryGitHub, url: "https://github.com/acme/widgets.git", username: "x-access-token",
		},
		{
			policy:   ExecutionPolicy{RepositoryProvider: RepositoryGitLab, Repository: "acme/platform/widgets"},
			provider: RepositoryGitLab, url: "https://gitlab.com/acme/platform/widgets.git", username: "oauth2",
		},
	}
	for _, test := range tests {
		provider, url, username, err := repositoryCloneSettings(test.policy)
		if err != nil {
			t.Fatal(err)
		}
		if provider != test.provider || url != test.url || username != test.username {
			t.Fatalf("clone settings = (%q, %q, %q), want (%q, %q, %q)",
				provider, url, username, test.provider, test.url, test.username)
		}
	}
}

func TestReconcileStaleUsesSeparateClaimAndRuntimeGrace(t *testing.T) {
	store, db := testStore(t)
	connectedCredential(t, db)
	now := store.now()

	create := func(runID string) *models.CodingAgentJob {
		t.Helper()
		request := validRequest()
		request.WorkflowRunID = runID
		job, _, err := store.Submit(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	oldClaim := create("00000000-0000-0000-0000-000000000101")
	recentRun := create("00000000-0000-0000-0000-000000000102")
	oldRun := create("00000000-0000-0000-0000-000000000103")

	claimHeartbeat := now.Add(-3 * time.Minute)
	recentHeartbeat := now.Add(-5 * time.Minute)
	oldHeartbeat := now.Add(-20 * time.Minute)
	startedAt := now.Add(-30 * time.Minute)
	if err := db.Model(&models.CodingAgentJob{}).Where("id = ?", oldClaim.ID).Updates(map[string]any{
		"status": models.CodingAgentJobClaimed, "claimed_by": "dead-worker", "heartbeat_at": claimHeartbeat,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		job       *models.CodingAgentJob
		heartbeat time.Time
	}{{recentRun, recentHeartbeat}, {oldRun, oldHeartbeat}} {
		if err := db.Model(&models.CodingAgentJob{}).Where("id = ?", item.job.ID).Updates(map[string]any{
			"status": models.CodingAgentJobRunning, "claimed_by": "dead-worker", "heartbeat_at": item.heartbeat,
			"started_at": startedAt, "provider_execution_id": "daytona-session",
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	if err := store.ReconcileStale(context.Background(), now.Add(-2*time.Minute), now.Add(-15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertStatus := func(job *models.CodingAgentJob, want models.CodingAgentJobStatus) {
		t.Helper()
		got, err := store.GetJob(context.Background(), job.OrganizationID, job.ID.String())
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want {
			t.Fatalf("job %s status = %s, want %s", job.ID, got.Status, want)
		}
	}
	assertStatus(oldClaim, models.CodingAgentJobPending)
	assertStatus(recentRun, models.CodingAgentJobRunning)
	assertStatus(oldRun, models.CodingAgentJobFailed)
}
