package codingagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"workflow-ai/server/internal/database/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrUnavailable     = errors.New("coding agent execution is not configured")
	ErrEnvironmentBusy = errors.New("coding agent environment is busy")
)

var (
	repositorySegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	branchPattern            = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	domainPattern            = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
)

type ServiceConfig struct {
	WorkerCount     int
	PollInterval    time.Duration
	StaleAfter      time.Duration
	RecoveryGrace   time.Duration
	SandboxSnapshot string
	CodexCLIVersion string
	RepositoryToken func(context.Context, string, string, string) (string, error)
}

type Service struct {
	db          *gorm.DB
	store       *Store
	provider    SandboxProvider
	runtimes    map[string]Runtime
	config      ServiceConfig
	workerID    string
	wake        chan struct{}
	startOnce   sync.Once
	authMu      sync.Mutex
	authCancels map[string]context.CancelFunc
	jobMu       sync.Mutex
	jobCancels  map[string]context.CancelFunc
}

func NewService(db *gorm.DB, provider SandboxProvider, runtimes []Runtime, config ServiceConfig) *Service {
	if config.WorkerCount < 1 {
		config.WorkerCount = 2
	}
	if config.WorkerCount > 16 {
		config.WorkerCount = 16
	}
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
	}
	if config.StaleAfter < time.Minute {
		config.StaleAfter = 2 * time.Minute
	}
	minimumRecoveryGrace := 5 * config.StaleAfter
	if minimumRecoveryGrace < 15*time.Minute {
		minimumRecoveryGrace = 15 * time.Minute
	}
	if config.RecoveryGrace < minimumRecoveryGrace {
		config.RecoveryGrace = minimumRecoveryGrace
	}
	registered := make(map[string]Runtime, len(runtimes))
	for _, runtime := range runtimes {
		if runtime != nil {
			registered[runtime.Name()] = runtime
		}
	}
	return &Service{
		db: db, store: NewStore(db), provider: provider, runtimes: registered, config: config,
		workerID: uuid.NewString(), wake: make(chan struct{}, 1),
		authCancels: make(map[string]context.CancelFunc),
		jobCancels:  make(map[string]context.CancelFunc),
	}
}

func (s *Service) Available(runtime string) bool {
	if s == nil || s.provider == nil {
		return false
	}
	_, ok := s.runtimes[runtime]
	return ok
}

func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	staleBefore := time.Now().UTC().Add(-s.config.StaleAfter)
	runningBefore := time.Now().UTC().Add(-s.config.RecoveryGrace)
	if err := s.recoverCompletedJobs(ctx, staleBefore); err != nil {
		return fmt.Errorf("recover completed coding agent jobs: %w", err)
	}
	if err := s.store.ReconcileStale(ctx, staleBefore, runningBefore); err != nil {
		return fmt.Errorf("reconcile coding agent jobs: %w", err)
	}
	if err := s.reconcileAuthAttempts(ctx, staleBefore); err != nil {
		return fmt.Errorf("reconcile coding agent authentication: %w", err)
	}
	s.startOnce.Do(func() {
		for index := 0; index < s.config.WorkerCount; index++ {
			go s.worker(ctx, fmt.Sprintf("%s-%d", s.workerID, index+1))
		}
		go s.reconcileLoop(ctx)
	})
	return nil
}

// reconcileLoop recovers a completed runtime from its sandbox marker without
// invoking it again. A longer grace applies before an unresolved running job is
// made terminal so a transient Daytona outage cannot discard a valid marker.
func (s *Service) reconcileLoop(ctx context.Context) {
	interval := s.config.StaleAfter / 2
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			if err := s.reconcileAuthAttempts(ctx, now.Add(-s.config.RecoveryGrace)); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("failed to reconcile stale coding agent authentication", "error", err)
			}
			if err := s.recoverCompletedJobs(ctx, now.Add(-s.config.StaleAfter)); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("failed to recover completed coding agent jobs", "error", err)
			}
			claimedBefore := now.Add(-s.config.StaleAfter)
			runningBefore := now.Add(-s.config.RecoveryGrace)
			if err := s.store.ReconcileStale(ctx, claimedBefore, runningBefore); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("failed to reconcile stale coding agent jobs", "error", err)
			}
		}
	}
}

func (s *Service) Submit(ctx context.Context, req SubmitRequest) (*models.CodingAgentJob, bool, error) {
	if !s.Available(req.Runtime) {
		return nil, false, ErrUnavailable
	}
	job, created, err := s.store.Submit(ctx, req)
	if err == nil {
		s.signal()
	}
	return job, created, err
}

// SubmitAndWait is the workflow-node bridge. The durable job continues if the
// caller disconnects; a later API request can inspect it by ID without causing
// another execution because Submit is idempotent for workflowRun+node.
func (s *Service) SubmitAndWait(ctx context.Context, req SubmitRequest, emit func(StreamEvent)) (*models.CodingAgentJob, error) {
	job, _, err := s.Submit(ctx, req)
	if err != nil {
		return nil, err
	}
	lastSequence := 0
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		events, eventErr := s.store.ListEvents(ctx, req.OrganizationID, job.ID.String(), lastSequence)
		if eventErr != nil {
			return job, eventErr
		}
		for _, event := range events {
			lastSequence = event.Sequence
			if emit != nil {
				var payload map[string]any
				_ = json.Unmarshal(event.Payload, &payload)
				emit(StreamEvent{Type: event.Type, Message: event.Message, Payload: payload})
			}
		}
		job, err = s.store.GetJob(ctx, req.OrganizationID, job.ID.String())
		if err != nil {
			return nil, err
		}
		if job.Status.Terminal() {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return job, fmt.Errorf("coding agent job %s continues in the background: %w", job.ID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Service) GetJob(ctx context.Context, organizationID, jobID string) (*models.CodingAgentJob, error) {
	return s.store.GetJob(ctx, organizationID, jobID)
}

type JobDetails struct {
	Job       models.CodingAgentJob        `json:"job"`
	Artifacts []models.CodingAgentArtifact `json:"artifacts"`
}

func (s *Service) GetUserJob(ctx context.Context, organizationID, userID, jobID string) (*JobDetails, error) {
	var job models.CodingAgentJob
	if err := s.db.WithContext(ctx).Where(
		"id = ? AND organization_id = ? AND user_id = ?", jobID, organizationID, userID,
	).First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	var artifacts []models.CodingAgentArtifact
	if err := s.db.WithContext(ctx).Where(
		"organization_id = ? AND job_id = ?", organizationID, jobID,
	).Order("created_at ASC").Find(&artifacts).Error; err != nil {
		return nil, err
	}
	return &JobDetails{Job: job, Artifacts: artifacts}, nil
}

func (s *Service) ListUserJobs(ctx context.Context, organizationID, userID, workflowID, nodeID string, limit int) ([]models.CodingAgentJob, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	query := s.db.WithContext(ctx).Where("organization_id = ? AND user_id = ?", organizationID, userID)
	if workflowID != "" {
		query = query.Where("workflow_id = ?", workflowID)
	}
	if nodeID != "" {
		query = query.Where("node_id = ?", nodeID)
	}
	var jobs []models.CodingAgentJob
	err := query.Order("created_at DESC").Limit(limit).Find(&jobs).Error
	return jobs, err
}

func (s *Service) ListEvents(ctx context.Context, organizationID, jobID string, after int) ([]models.CodingAgentEvent, error) {
	return s.store.ListEvents(ctx, organizationID, jobID, after)
}

func (s *Service) Cancel(ctx context.Context, organizationID, userID, jobID string) (*models.CodingAgentJob, error) {
	job, err := s.store.RequestCancel(ctx, organizationID, userID, jobID)
	if err != nil {
		return job, err
	}
	s.cancelRunningJob(jobID)
	return job, nil
}

func (s *Service) ListUserEnvironments(ctx context.Context, organizationID, userID, workflowID, nodeID string) ([]models.CodingAgentEnvironment, error) {
	query := s.db.WithContext(ctx).Where("organization_id = ? AND user_id = ?", organizationID, userID)
	if workflowID != "" {
		query = query.Where("workflow_id = ?", workflowID)
	}
	if nodeID != "" {
		query = query.Where("node_id = ?", nodeID)
	}
	var environments []models.CodingAgentEnvironment
	err := query.Order("updated_at DESC").Limit(100).Find(&environments).Error
	return environments, err
}

// ResetEnvironment deletes a reusable provider sandbox and invalidates its
// continuity sessions. The workspace key is retired so the next workflow run
// provisions a clean environment rather than reviving stale state.
func (s *Service) ResetEnvironment(ctx context.Context, organizationID, userID, environmentID string) error {
	var environment models.CodingAgentEnvironment
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND user_id = ?", environmentID, organizationID, userID,
		).First(&environment).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrJobNotFound
			}
			return err
		}
		var activeJobs int64
		if err := tx.Model(&models.CodingAgentJob{}).Where(
			"environment_id = ? AND status IN ?", environment.ID, []models.CodingAgentJobStatus{
				models.CodingAgentJobClaimed, models.CodingAgentJobRunning,
			},
		).Count(&activeJobs).Error; err != nil {
			return err
		}
		if activeJobs > 0 || environment.CurrentJobID != "" || environment.Status == models.CodingAgentEnvironmentBusy {
			return ErrEnvironmentBusy
		}
		return tx.Model(&environment).Updates(map[string]any{
			"status": models.CodingAgentEnvironmentDeleting, "last_error": "",
		}).Error
	})
	if err != nil {
		return err
	}
	if environment.ExternalSandboxID != "" {
		cleanupCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		sandbox, getErr := s.provider.Get(cleanupCtx, environment.ExternalSandboxID)
		if getErr == nil {
			getErr = sandbox.Delete(cleanupCtx)
		}
		if errors.Is(getErr, ErrSandboxNotFound) {
			getErr = nil
		}
		if getErr != nil {
			_ = s.db.WithContext(ctx).Model(&environment).Updates(map[string]any{
				"status": models.CodingAgentEnvironmentError, "last_error": getErr.Error(),
			}).Error
			return fmt.Errorf("delete coding agent environment: %w", getErr)
		}
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		retiredKey := environment.WorkspaceKey + ":retired:" + environment.ID.String()
		if err := tx.Model(&models.CodingAgentEnvironment{}).Where(
			"id = ? AND organization_id = ? AND user_id = ? AND status = ?",
			environment.ID, organizationID, userID, models.CodingAgentEnvironmentDeleting,
		).Updates(map[string]any{
			"status": models.CodingAgentEnvironmentArchived, "workspace_key": retiredKey,
			"external_sandbox_id": "", "last_activity_at": now, "last_error": "",
		}).Error; err != nil {
			return err
		}
		return tx.Model(&models.CodingAgentSession{}).Where(
			"environment_id = ? AND organization_id = ? AND user_id = ?", environment.ID, organizationID, userID,
		).Updates(map[string]any{"status": models.CodingAgentSessionClosed, "last_activity_at": now}).Error
	})
}

func (s *Service) cancelRunningJob(jobID string) {
	s.jobMu.Lock()
	cancel := s.jobCancels[jobID]
	s.jobMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) worker(ctx context.Context, workerID string) {
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := s.drain(ctx, workerID); err != nil && !errors.Is(err, context.Canceled) {
			slog.ErrorContext(ctx, "coding agent worker failed", "worker_id", workerID, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *Service) drain(ctx context.Context, workerID string) error {
	for ctx.Err() == nil {
		job, err := s.store.ClaimNext(ctx, workerID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		s.processJob(ctx, workerID, job)
	}
	return ctx.Err()
}

func (s *Service) processJob(workerCtx context.Context, workerID string, job *models.CodingAgentJob) {
	policy, err := DecodePolicy(job)
	if err != nil {
		s.failClaimed(workerCtx, workerID, job, err)
		return
	}
	if err := validateRepository(policy.RepositoryProvider, policy.RepositoryID, policy.Repository, policy.Branch); err != nil {
		s.failClaimed(workerCtx, workerID, job, err)
		return
	}
	if err := s.verifyMember(workerCtx, job.OrganizationID, job.UserID); err != nil {
		s.failClaimed(workerCtx, workerID, job, err)
		return
	}

	environment, sandbox, err := s.acquireEnvironment(workerCtx, job, policy)
	if errors.Is(err, ErrEnvironmentBusy) {
		_ = s.store.RequeueBusy(workerCtx, job.ID.String(), workerID, 10*time.Second, err.Error())
		return
	}
	if err != nil {
		_ = s.store.Requeue(workerCtx, job.ID.String(), workerID, 10*time.Second, err.Error())
		return
	}
	released := false
	defer func() {
		if !released {
			s.releaseEnvironment(context.Background(), environment, job, policy, false, "worker exited before normal environment release")
		}
	}()
	releaseAuthority, err := acquireAuthorityLock(workerCtx, s.db, job.OrganizationID, job.UserID)
	if err != nil {
		s.releaseEnvironment(workerCtx, environment, job, policy, false, err.Error())
		released = true
		_ = s.store.Requeue(workerCtx, job.ID.String(), workerID, 10*time.Second, err.Error())
		return
	}
	defer releaseAuthority()
	if err := s.verifyMember(workerCtx, job.OrganizationID, job.UserID); err != nil {
		s.releaseEnvironment(workerCtx, environment, job, policy, false, err.Error())
		released = true
		s.failClaimed(workerCtx, workerID, job, err)
		return
	}

	workingDirectory, baselineSHA, err := s.prepareRepository(workerCtx, sandbox, job, policy)
	if err != nil {
		s.releaseEnvironment(workerCtx, environment, job, policy, false, err.Error())
		released = true
		_ = s.store.Requeue(workerCtx, job.ID.String(), workerID, 10*time.Second, err.Error())
		return
	}

	credential, err := s.loadCredential(workerCtx, job)
	if err != nil {
		s.releaseEnvironment(workerCtx, environment, job, policy, false, err.Error())
		released = true
		s.failClaimed(workerCtx, workerID, job, err)
		return
	}
	session, err := s.ensureSession(workerCtx, job, environment, policy)
	if err != nil {
		s.releaseEnvironment(workerCtx, environment, job, policy, false, err.Error())
		released = true
		s.failClaimed(workerCtx, workerID, job, err)
		return
	}
	if err := s.db.WithContext(workerCtx).Model(&models.CodingAgentJob{}).Where("id = ?", job.ID).
		Updates(map[string]any{
			"environment_id": environment.ID.String(), "session_id": session.ID.String(),
			"repository_base_sha": baselineSHA, "working_directory": workingDirectory,
		}).Error; err != nil {
		s.releaseEnvironment(workerCtx, environment, job, policy, false, err.Error())
		released = true
		s.failClaimed(workerCtx, workerID, job, err)
		return
	}
	job.EnvironmentID = environment.ID.String()
	job.SessionID = session.ID.String()
	job.RepositoryBaseSHA = baselineSHA
	job.WorkingDirectory = workingDirectory
	jobCtx, cancel := context.WithTimeout(workerCtx, time.Duration(policy.MaxDurationSeconds)*time.Second)
	s.jobMu.Lock()
	s.jobCancels[job.ID.String()] = cancel
	s.jobMu.Unlock()
	defer func() {
		cancel()
		s.jobMu.Lock()
		delete(s.jobCancels, job.ID.String())
		s.jobMu.Unlock()
	}()
	if err := s.store.MarkRunning(workerCtx, job.ID.String(), workerID, sandbox.ID()); err != nil {
		s.releaseEnvironment(workerCtx, environment, job, policy, false, err.Error())
		released = true
		return
	}
	cancelRequested := make(chan struct{}, 1)
	requested, err := s.store.Heartbeat(workerCtx, job.ID.String(), workerID)
	if err != nil || requested {
		cancel()
		s.releaseEnvironment(workerCtx, environment, job, policy, true, "")
		released = true
		if requested {
			_ = s.store.Fail(context.Background(), job.ID.String(), workerID, models.CodingAgentJobCancelled, errors.New("coding agent task cancelled before runtime execution"))
		}
		return
	}

	runtime := s.runtimes[job.Runtime]
	go s.heartbeat(jobCtx, cancel, job.ID.String(), workerID, cancelRequested)
	// The callback token lives exactly as long as the run. It is minted here
	// rather than at submit so a job queued for hours never has a usable
	// credential sitting in the database ahead of time, and revoked in the
	// defer so a sandbox that outlives its job cannot keep acting.
	toolEndpoint, toolToken := "", ""
	if grants := s.toolGrantCount(job); grants > 0 {
		endpoint, token, mintErr := s.mintToolToken(jobCtx, job)
		if mintErr != nil {
			cancel()
			s.releaseEnvironment(context.Background(), environment, job, policy, true, mintErr.Error())
			released = true
			_ = s.store.Fail(context.Background(), job.ID.String(), workerID, models.CodingAgentJobFailed,
				fmt.Errorf("workflow tools could not be configured: %w", mintErr))
			return
		}
		toolEndpoint, toolToken = endpoint, token
		defer s.revokeToolToken(job)
	}

	result, runErr := runtime.Run(jobCtx, sandbox, RuntimeRequest{
		JobID: job.ID.String(), SessionID: session.ID.String(), Task: job.Task, Model: policy.Model,
		WorkingDirectory: workingDirectory, BaselineSHA: baselineSHA,
		AuthBundle: []byte(credential.AuthBundle), ExternalThreadID: session.ExternalThreadID,
		AllowWrite: policy.AllowWorkspaceWrite, Timeout: time.Duration(policy.MaxDurationSeconds) * time.Second,
		ToolEndpoint: toolEndpoint, ToolToken: toolToken,
	}, s.jobEventSink(job, workerID))
	cancel()
	if len(result.RefreshedAuthBundle) > 0 {
		if err := s.persistCredentialRefresh(context.Background(), credential, result.RefreshedAuthBundle); err != nil {
			slog.Error("failed to persist refreshed Codex credential", "credential_id", credential.ID, "error", err)
		}
	}

	if runErr != nil {
		status := models.CodingAgentJobFailed
		select {
		case <-cancelRequested:
			status = models.CodingAgentJobCancelled
		default:
			if durable, loadErr := s.store.GetJob(context.Background(), job.OrganizationID, job.ID.String()); loadErr == nil && durable.CancelRequestedAt != nil {
				status = models.CodingAgentJobCancelled
			}
			if status != models.CodingAgentJobCancelled && errors.Is(jobCtx.Err(), context.DeadlineExceeded) {
				status = models.CodingAgentJobTimedOut
			}
		}
		s.releaseEnvironment(
			context.Background(), environment, job, policy,
			!errors.Is(runErr, ErrSandboxExecutionUncertain), runErr.Error(),
		)
		released = true
		_ = s.store.Fail(context.Background(), job.ID.String(), workerID, status, runErr)
		return
	}

	if err := s.finalizeRuntimeSuccess(context.Background(), job, workerID, session, environment, policy, result); err != nil {
		// Do not turn a successful agent run into a retryable failure. The job and
		// environment remain claimed, and startup reconciliation can recover the
		// completion marker from the sandbox without executing Codex again.
		slog.Error("failed to durably finalize coding agent result", "job_id", job.ID, "error", err)
		// Keep the durable environment lease intact until reconcileLoop commits
		// the marker. Releasing it here could let another task mutate the same
		// workspace before the completed result is recovered.
		released = true
		return
	}
	released = true
	s.removeRecoveryMarker(sandbox, job.ID.String())
	s.finishSuccessfulEnvironmentLifecycle(context.Background(), environment, policy)
}

func (s *Service) failClaimed(ctx context.Context, workerID string, job *models.CodingAgentJob, err error) {
	_ = s.store.Fail(ctx, job.ID.String(), workerID, models.CodingAgentJobFailed, err)
}

func (s *Service) heartbeat(ctx context.Context, cancel context.CancelFunc, jobID, workerID string, cancellation chan<- struct{}) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			requested, err := s.store.Heartbeat(context.Background(), jobID, workerID)
			if err != nil {
				cancel()
				return
			}
			if requested {
				select {
				case cancellation <- struct{}{}:
				default:
				}
				cancel()
				return
			}
		}
	}
}

func (s *Service) jobEventSink(job *models.CodingAgentJob, workerID string) func(StreamEvent) {
	var mu sync.Mutex
	count := 0
	return func(event StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		if count >= 500 {
			return
		}
		count++
		if err := s.store.AppendEvent(context.Background(), job.ID.String(), workerID, event.Type, event.Message, event.Payload); err != nil {
			slog.Warn("failed to persist coding agent event", "job_id", job.ID, "type", event.Type, "error", err)
		}
	}
}

func (s *Service) verifyMember(ctx context.Context, organizationID, userID string) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(&models.OrgMember{}).
		Where("organization_id = ? AND user_id = ?", organizationID, userID).Count(&count).Error; err != nil {
		return fmt.Errorf("verify coding agent authority: %w", err)
	}
	if count != 1 {
		return errors.New("coding agent authority was revoked because the user is no longer an organization member")
	}
	return nil
}

func (s *Service) loadCredential(ctx context.Context, job *models.CodingAgentJob) (*models.CodingAgentCredential, error) {
	var credential models.CodingAgentCredential
	if err := s.db.WithContext(ctx).Where(
		"id = ? AND organization_id = ? AND user_id = ? AND runtime = ? AND status = ?",
		job.CredentialID, job.OrganizationID, job.UserID, job.Runtime, models.CodingAgentCredentialConnected,
	).First(&credential).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrCredentialRequired
		}
		return nil, err
	}
	if !json.Valid([]byte(credential.AuthBundle)) {
		_ = s.db.WithContext(ctx).Model(&models.CodingAgentCredential{}).Where("id = ?", credential.ID).
			Updates(map[string]any{"status": models.CodingAgentCredentialError, "last_error": "stored Codex authentication could not be decrypted; reconnect Codex"}).Error
		return nil, ErrCredentialRequired
	}
	return &credential, nil
}

func (s *Service) persistCredentialRefresh(ctx context.Context, credential *models.CodingAgentCredential, bundle []byte) error {
	if len(bundle) == 0 || !json.Valid(bundle) {
		return errors.New("refreshed coding agent credential is invalid")
	}
	now := time.Now().UTC()
	credential.AuthBundle = string(bundle)
	credential.LastVerifiedAt = &now
	credential.LastError = ""
	credential.Status = models.CodingAgentCredentialConnected
	return s.db.WithContext(ctx).Save(credential).Error
}

func (s *Service) ensureSession(ctx context.Context, job *models.CodingAgentJob, environment *models.CodingAgentEnvironment, policy ExecutionPolicy) (*models.CodingAgentSession, error) {
	keySource := strings.TrimSpace(job.ConversationKey)
	if keySource == "" {
		// An omitted key means a fresh conversation. Continuity is an explicit
		// product choice; silently resuming every run of a persistent node leaks
		// old task context into unrelated workflow executions.
		keySource = job.ID.String()
	}
	if policy.WorkspaceMode == WorkspaceEphemeral {
		keySource += "\x00" + job.ID.String()
	}
	digest := sha256.Sum256([]byte(job.OrganizationID + "\x00" + job.UserID + "\x00" + environment.ID.String() + "\x00" + keySource))
	conversationKey := hex.EncodeToString(digest[:])
	var session models.CodingAgentSession
	err := s.db.WithContext(ctx).Where("conversation_key = ?", conversationKey).First(&session).Error
	if err == nil {
		return &session, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	now := time.Now().UTC()
	session = models.CodingAgentSession{
		OrganizationID: job.OrganizationID, UserID: job.UserID, EnvironmentID: environment.ID.String(),
		Runtime: job.Runtime, ConversationKey: conversationKey, Status: models.CodingAgentSessionActive,
		LastJobID: job.ID.String(), LastActivityAt: &now,
	}
	created := s.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "conversation_key"}}, DoNothing: true}).Create(&session)
	if created.Error != nil {
		return nil, created.Error
	}
	if created.RowsAffected == 0 {
		session = models.CodingAgentSession{}
		if err := s.db.WithContext(ctx).Where("conversation_key = ?", conversationKey).First(&session).Error; err != nil {
			return nil, err
		}
	}
	return &session, nil
}

func (s *Service) finalizeRuntimeSuccess(ctx context.Context, job *models.CodingAgentJob, workerID string, session *models.CodingAgentSession, environment *models.CodingAgentEnvironment, policy ExecutionPolicy, result RuntimeResult) error {
	persisted := result
	persisted.RefreshedAuthBundle = nil
	persisted.Artifacts = make([]Artifact, len(result.Artifacts))
	for index, artifact := range result.Artifacts {
		if len(artifact.Content) > maxInlineArtifactBytes {
			return errors.New("coding agent artifact exceeds the inline retention limit")
		}
		persisted.Artifacts[index] = artifact
		persisted.Artifacts[index].Content = ""
	}
	payload, err := jsonObject(persisted)
	if err != nil {
		return fmt.Errorf("encode coding agent result: %w", err)
	}
	if len(payload) > maxInlineArtifactBytes {
		return errors.New("coding agent result exceeds the retention limit")
	}

	finalize := func() error {
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var locked models.CodingAgentJob
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND claimed_by = ?", job.ID, workerID).First(&locked).Error; err != nil {
				return err
			}
			if locked.Status == models.CodingAgentJobSucceeded {
				return nil
			}
			if locked.Status != models.CodingAgentJobRunning {
				return fmt.Errorf("%w: %s to %s", ErrInvalidTransition, locked.Status, models.CodingAgentJobSucceeded)
			}
			now := time.Now().UTC()
			if err := tx.Model(&models.CodingAgentSession{}).Where(
				"id = ? AND organization_id = ? AND user_id = ?", session.ID, job.OrganizationID, job.UserID,
			).Updates(map[string]any{
				"external_thread_id": result.ExternalThreadID, "last_job_id": job.ID.String(),
				"last_activity_at": now, "status": models.CodingAgentSessionActive, "last_error": "",
			}).Error; err != nil {
				return err
			}
			for _, artifact := range result.Artifacts {
				row := models.CodingAgentArtifact{
					OrganizationID: job.OrganizationID, JobID: job.ID.String(), Kind: artifact.Kind,
					Path: artifact.Path, MediaType: artifact.MediaType, SizeBytes: artifact.SizeBytes,
					SHA256: artifact.SHA256, StorageKey: artifact.StorageKey, InlineContent: artifact.Content,
				}
				if err := tx.Create(&row).Error; err != nil {
					return err
				}
			}
			environmentStatus := models.CodingAgentEnvironmentReady
			if policy.WorkspaceMode == WorkspaceEphemeral {
				environmentStatus = models.CodingAgentEnvironmentDeleting
			}
			if err := tx.Model(&models.CodingAgentEnvironment{}).Where(
				"id = ? AND current_job_id = ?", environment.ID, job.ID.String(),
			).Updates(map[string]any{
				"status": environmentStatus, "current_job_id": "", "last_activity_at": now, "last_error": "",
			}).Error; err != nil {
				return err
			}
			if err := tx.Model(&locked).Updates(map[string]any{
				"status": models.CodingAgentJobSucceeded, "result": models.JSONB(payload),
				"summary": result.Summary, "completed_at": now, "heartbeat_at": now, "last_error": "",
				"tool_token_hash": "",
			}).Error; err != nil {
				return err
			}
			if err := cancelOpenToolCallsTx(tx, job.ID.String(), now, "coding agent job finished"); err != nil {
				return err
			}
			locked.Status = models.CodingAgentJobSucceeded
			if err := s.store.appendEventTx(tx, &locked, "completed", "Coding agent task completed", nil); err != nil {
				return err
			}
			return nil
		})
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if lastErr = finalize(); lastErr == nil {
			return nil
		}
		var durable models.CodingAgentJob
		if err := s.db.WithContext(ctx).Select("status").First(&durable, "id = ?", job.ID).Error; err == nil && durable.Status == models.CodingAgentJobSucceeded {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	return lastErr
}

const maxInlineArtifactBytes = 2 << 20

// recoverCompletedJobs closes the narrow crash window between Codex writing
// its completion marker in the sandbox and the database finalization commit.
// It never invokes the runtime, so recovery cannot repeat edits or commands.
func (s *Service) recoverCompletedJobs(ctx context.Context, staleBefore time.Time) error {
	var jobs []models.CodingAgentJob
	if err := s.db.WithContext(ctx).Where(
		"status = ? AND provider_execution_id <> '' AND session_id <> '' AND environment_id <> '' AND (heartbeat_at IS NULL OR heartbeat_at < ?)",
		models.CodingAgentJobRunning, staleBefore,
	).Find(&jobs).Error; err != nil {
		return err
	}
	for index := range jobs {
		job := &jobs[index]
		policy, err := DecodePolicy(job)
		if err != nil {
			continue
		}
		sandbox, err := s.provider.Get(ctx, job.ProviderExecutionID)
		if err != nil {
			continue
		}
		marker, err := sandbox.Download(ctx, RecoveryMarkerPath(job.ID.String()))
		if err != nil || len(marker) == 0 || len(marker) > maxInlineArtifactBytes*2 {
			continue
		}
		var result RuntimeResult
		if json.Unmarshal(marker, &result) != nil || strings.TrimSpace(result.Summary) == "" {
			continue
		}
		var session models.CodingAgentSession
		if err := s.db.WithContext(ctx).Where(
			"id = ? AND organization_id = ? AND user_id = ?", job.SessionID, job.OrganizationID, job.UserID,
		).First(&session).Error; err != nil {
			continue
		}
		var environment models.CodingAgentEnvironment
		if err := s.db.WithContext(ctx).Where(
			"id = ? AND organization_id = ? AND user_id = ?", job.EnvironmentID, job.OrganizationID, job.UserID,
		).First(&environment).Error; err != nil {
			continue
		}
		if err := s.finalizeRuntimeSuccess(ctx, job, job.ClaimedBy, &session, &environment, policy, result); err != nil {
			continue
		}
		s.removeRecoveryMarker(sandbox, job.ID.String())
		s.finishSuccessfulEnvironmentLifecycle(ctx, &environment, policy)
	}
	return nil
}

func (s *Service) removeRecoveryMarker(sandbox Sandbox, jobID string) {
	if sandbox == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = sandbox.Run(cleanupCtx, CommandSpec{
		Command: "rm -f -- " + quoteShell(RecoveryMarkerPath(jobID)), WorkingDir: "/tmp", Timeout: 30 * time.Second,
	}, nil)
}

func validateRepository(provider, repositoryID, repository, branch string) error {
	provider = normalizedRepositoryProvider(provider)
	repository = strings.TrimSpace(repository)
	repositoryID = strings.TrimSpace(repositoryID)
	parts := strings.Split(repository, "/")
	minimumParts, maximumParts := 2, 2
	if provider == RepositoryGitLab {
		maximumParts = 20
	} else if provider != RepositoryGitHub {
		return fmt.Errorf("unsupported coding agent repository provider %q", provider)
	}
	if len(repository) > 500 || len(parts) < minimumParts || len(parts) > maximumParts || strings.Contains(repository, "..") || strings.Contains(repository, "//") {
		return fmt.Errorf("coding agent repository is not a valid %s project path", providerDisplayName(provider))
	}
	for _, part := range parts {
		if !repositorySegmentPattern.MatchString(part) || strings.HasSuffix(part, ".") {
			return fmt.Errorf("coding agent repository is not a valid %s project path", providerDisplayName(provider))
		}
	}
	if provider == RepositoryGitHub {
		if repositoryID != "" && repositoryID != repository {
			return errors.New("coding agent GitHub repository ID does not match its repository path")
		}
	} else {
		projectID, err := strconv.ParseInt(repositoryID, 10, 64)
		if err != nil || projectID < 1 {
			return errors.New("coding agent GitLab repository must include its numeric project ID")
		}
	}
	if branch != "" && (!branchPattern.MatchString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/")) {
		return errors.New("coding agent branch is invalid")
	}
	return nil
}

func normalizedRepositoryProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return RepositoryGitHub
	}
	return provider
}

// ValidateAllowedDomains applies Daytona's domain-allow-list shape and limit.
// Provider adapters call it again as a defense against future non-workflow
// callers bypassing Store validation.
func ValidateAllowedDomains(domains []string) error {
	seen := make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		base := strings.TrimPrefix(domain, "*.")
		if len(domain) > 255 || !domainPattern.MatchString(base) {
			return fmt.Errorf("coding agent network domain %q is invalid", domain)
		}
		seen[domain] = struct{}{}
	}
	// Zero domains means unrestricted, which is the default. This used to
	// reject it, from back when the node always sent a list and "no domains"
	// could only be a mistake. The cap still applies: a list is a deny-by-default
	// policy and one long enough to be unreadable is not a policy.
	if len(seen) > 20 {
		return errors.New("coding agent allows at most 20 unique network domains")
	}
	return nil
}

func workspaceKey(job *models.CodingAgentJob, policy ExecutionPolicy, snapshot string) string {
	domains := append([]string(nil), policy.AllowedDomains...)
	for index := range domains {
		domains[index] = strings.ToLower(strings.TrimSpace(domains[index]))
	}
	sort.Strings(domains)
	parts := []string{
		job.OrganizationID, job.UserID, job.WorkflowID, job.NodeID, job.Runtime,
		normalizedRepositoryProvider(policy.RepositoryProvider), policy.RepositoryID, policy.Repository, policy.Branch,
		strings.TrimSpace(snapshot), strings.Join(domains, ","),
		strconv.FormatBool(policy.NetworkBlockAll), strconv.Itoa(policy.AutoStopMinutes), strconv.Itoa(policy.AutoDeleteMinutes),
	}
	if policy.WorkspaceMode == WorkspaceEphemeral {
		parts = append(parts, job.ID.String())
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])
}
