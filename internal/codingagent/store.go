package codingagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrCredentialRequired = errors.New("connect the selected coding agent before running this node")
	ErrJobNotFound        = errors.New("coding agent job not found")
	ErrJobTerminal        = errors.New("coding agent job is already complete")
	ErrInvalidTransition  = errors.New("invalid coding agent job state transition")
	ErrInvalidRequest     = errors.New("invalid coding agent request")
	ErrRateLimited        = errors.New("coding agent request rate limit exceeded")
)

type Store struct {
	db  *gorm.DB
	now func() time.Time
}

func NewStore(db *gorm.DB) *Store {
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) Submit(ctx context.Context, req SubmitRequest) (*models.CodingAgentJob, bool, error) {
	if err := validateSubmitRequest(req); err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	input, err := jsonObject(req.Input)
	if err != nil {
		return nil, false, fmt.Errorf("encode coding agent input: %w", err)
	}
	policy, err := jsonObject(req.Policy)
	if err != nil {
		return nil, false, fmt.Errorf("encode coding agent policy: %w", err)
	}
	key := idempotencyKey(req)
	now := s.now()
	job := models.CodingAgentJob{
		OrganizationID:    req.OrganizationID,
		UserID:            req.UserID,
		WorkflowID:        req.WorkflowID,
		WorkflowRunID:     req.WorkflowRunID,
		NodeID:            req.NodeID,
		ConversationKey:   storedConversationKey(req),
		IdempotencyKey:    key,
		Runtime:           req.Runtime,
		Task:              strings.TrimSpace(req.Task),
		Input:             models.JSONB(input),
		ExecutionPolicy:   models.JSONB(policy),
		Status:            models.CodingAgentJobPending,
		MaxAttempts:       3,
		AvailableAt:       now,
		Result:            models.JSONB("{}"),
		ToolNodeIDs:       models.JSONB(toolNodeIDsJSON(req.ToolNodeIDs)),
		NextEventSequence: 1,
	}

	created := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "coding-agent-queue:"+req.OrganizationID).Error; err != nil {
				return fmt.Errorf("lock coding agent queue capacity: %w", err)
			}
		}
		var existing models.CodingAgentJob
		if err := tx.Where("idempotency_key = ?", key).First(&existing).Error; err == nil {
			if existing.OrganizationID != req.OrganizationID || existing.UserID != req.UserID {
				return ErrJobNotFound
			}
			job = existing
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var credential models.CodingAgentCredential
		if err := tx.Where(
			"organization_id = ? AND user_id = ? AND runtime = ? AND status = ?",
			req.OrganizationID, req.UserID, req.Runtime, models.CodingAgentCredentialConnected,
		).First(&credential).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrCredentialRequired
			}
			return fmt.Errorf("find coding agent credential: %w", err)
		}
		job.CredentialID = credential.ID.String()
		activeStatuses := []models.CodingAgentJobStatus{
			models.CodingAgentJobPending, models.CodingAgentJobClaimed, models.CodingAgentJobRunning,
		}
		var organizationJobs, userJobs int64
		if err := tx.Model(&models.CodingAgentJob{}).Where(
			"organization_id = ? AND status IN ?", req.OrganizationID, activeStatuses,
		).Count(&organizationJobs).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.CodingAgentJob{}).Where(
			"organization_id = ? AND user_id = ? AND status IN ?", req.OrganizationID, req.UserID, activeStatuses,
		).Count(&userJobs).Error; err != nil {
			return err
		}
		if organizationJobs >= 20 || userJobs >= 5 {
			return fmt.Errorf("%w: too many coding agent jobs are already queued or running", ErrRateLimited)
		}

		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "idempotency_key"}}, DoNothing: true}).Create(&job)
		if result.Error != nil {
			return fmt.Errorf("create coding agent job: %w", result.Error)
		}
		created = result.RowsAffected == 1
		if !created {
			// Create assigns a fresh primary key before PostgreSQL/SQLite reports
			// the conflict. Reset the struct so GORM does not silently add that
			// unused ID to the idempotency-key lookup.
			job = models.CodingAgentJob{}
			if err := tx.Where("idempotency_key = ?", key).First(&job).Error; err != nil {
				return fmt.Errorf("load idempotent coding agent job: %w", err)
			}
			if job.OrganizationID != req.OrganizationID || job.UserID != req.UserID {
				return ErrJobNotFound
			}
			return nil
		}
		return s.appendEventTx(tx, &job, "queued", "Coding agent task queued", map[string]any{
			"runtime": req.Runtime,
		})
	})
	if err != nil {
		return nil, false, err
	}
	return &job, created, nil
}

func validateSubmitRequest(req SubmitRequest) error {
	for field, value := range map[string]string{
		"organization": req.OrganizationID,
		"user":         req.UserID,
		"workflow":     req.WorkflowID,
		"workflow run": req.WorkflowRunID,
		"node":         req.NodeID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", field)
		}
	}
	if req.Runtime != RuntimeCodex {
		return fmt.Errorf("unsupported coding agent runtime %q", req.Runtime)
	}
	if strings.TrimSpace(req.Task) == "" {
		return errors.New("coding agent task is required")
	}
	if len(req.Task) > 100_000 {
		return errors.New("coding agent task is too large")
	}
	if req.Policy.WorkspaceMode != WorkspacePersistent && req.Policy.WorkspaceMode != WorkspaceEphemeral {
		return errors.New("workspace mode must be persistent or ephemeral")
	}
	if req.Policy.MaxDurationSeconds < 30 || req.Policy.MaxDurationSeconds > 7200 {
		return errors.New("coding agent duration must be between 30 seconds and 2 hours")
	}
	if err := validateRepository(req.Policy.RepositoryProvider, req.Policy.RepositoryID, req.Policy.Repository, req.Policy.Branch); err != nil {
		return err
	}
	if req.Policy.AutoStopMinutes < 1 || req.Policy.AutoStopMinutes > 24*60 {
		return errors.New("coding agent auto-stop must be between 1 minute and 24 hours")
	}
	if req.Policy.AutoDeleteMinutes < req.Policy.AutoStopMinutes || req.Policy.AutoDeleteMinutes > 30*24*60 {
		return errors.New("coding agent auto-delete must be after auto-stop and no more than 30 days")
	}
	// No check that egress is blocked. This used to require it, but the flag was
	// never load-bearing: the node always sent a domain list too, and the
	// provider returns early on a non-empty list, so the effective policy was
	// always the allowlist and the flag was a rubber stamp. Open access is now a
	// deliberate default — a coding agent that cannot reach a package registry
	// fails at the work it exists for — so requiring the flag only rejected the
	// policy the caller meant.
	if err := ValidateAllowedDomains(req.Policy.AllowedDomains); err != nil {
		return err
	}
	return nil
}

func storedConversationKey(req SubmitRequest) string {
	value := strings.TrimSpace(req.ConversationKey)
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(req.OrganizationID + "\x00" + req.UserID + "\x00" + value))
	return hex.EncodeToString(digest[:])
}

func idempotencyKey(req SubmitRequest) string {
	// Workflow runs execute a DAG node at most once. Deliberately exclude task
	// text so a client retry with a mutated payload cannot replace a queued job.
	h := sha256.Sum256([]byte(strings.Join([]string{
		req.OrganizationID, req.WorkflowRunID, req.NodeID, req.Runtime,
	}, "\x00")))
	return "caj_" + hex.EncodeToString(h[:])
}

func (s *Store) GetJob(ctx context.Context, organizationID, jobID string) (*models.CodingAgentJob, error) {
	var job models.CodingAgentJob
	if err := s.db.WithContext(ctx).Where("organization_id = ? AND id = ?", organizationID, jobID).First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	return &job, nil
}

func (s *Store) ListEvents(ctx context.Context, organizationID, jobID string, after int) ([]models.CodingAgentEvent, error) {
	var events []models.CodingAgentEvent
	err := s.db.WithContext(ctx).
		Where("organization_id = ? AND job_id = ? AND sequence > ?", organizationID, jobID, after).
		Order("sequence ASC").Find(&events).Error
	return events, err
}

func (s *Store) ClaimNext(ctx context.Context, workerID string) (*models.CodingAgentJob, error) {
	var claimed models.CodingAgentJob
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job models.CodingAgentJob
		result := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND available_at <= ? AND attempt_count < max_attempts", models.CodingAgentJobPending, s.now()).
			Order("available_at ASC, created_at ASC").Limit(1).Find(&job)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		now := s.now()
		res := tx.Model(&models.CodingAgentJob{}).
			Where("id = ? AND status = ?", job.ID, models.CodingAgentJobPending).
			Updates(map[string]any{
				"status":        models.CodingAgentJobClaimed,
				"claimed_at":    now,
				"heartbeat_at":  now,
				"claimed_by":    workerID,
				"attempt_count": gorm.Expr("attempt_count + 1"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		if err := tx.First(&job, "id = ?", job.ID).Error; err != nil {
			return err
		}
		if err := s.appendEventTx(tx, &job, "claimed", "Coding agent worker claimed the task", nil); err != nil {
			return err
		}
		claimed = job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &claimed, nil
}

func (s *Store) MarkRunning(ctx context.Context, jobID, workerID, providerExecutionID string) error {
	return s.transition(ctx, jobID, workerID, models.CodingAgentJobRunning, map[string]any{
		"started_at": s.now(), "heartbeat_at": s.now(), "provider_execution_id": providerExecutionID,
	}, "started", "Coding agent started", nil)
}

func (s *Store) Complete(ctx context.Context, jobID, workerID string, result any, summary string) error {
	payload, err := jsonObject(result)
	if err != nil {
		return err
	}
	return s.transition(ctx, jobID, workerID, models.CodingAgentJobSucceeded, map[string]any{
		"result": models.JSONB(payload), "summary": summary, "completed_at": s.now(), "heartbeat_at": s.now(), "last_error": "",
	}, "completed", "Coding agent task completed", nil)
}

func (s *Store) Fail(ctx context.Context, jobID, workerID string, status models.CodingAgentJobStatus, jobErr error) error {
	if status != models.CodingAgentJobFailed && status != models.CodingAgentJobTimedOut && status != models.CodingAgentJobCancelled {
		return ErrInvalidTransition
	}
	message := "Coding agent task failed"
	lastError := ""
	if jobErr != nil {
		lastError = jobErr.Error()
	}
	if status == models.CodingAgentJobTimedOut {
		message = "Coding agent task timed out"
	}
	if status == models.CodingAgentJobCancelled {
		message = "Coding agent task cancelled"
	}
	return s.transition(ctx, jobID, workerID, status, map[string]any{
		"completed_at": s.now(), "heartbeat_at": s.now(), "last_error": lastError,
	}, string(status), message, nil)
}

func (s *Store) Heartbeat(ctx context.Context, jobID, workerID string) (bool, error) {
	var job models.CodingAgentJob
	err := s.db.WithContext(ctx).Select("status", "cancel_requested_at").
		Where("id = ? AND claimed_by = ?", jobID, workerID).First(&job).Error
	if err != nil {
		return false, err
	}
	if job.Status.Terminal() {
		return false, ErrJobTerminal
	}
	if err := s.db.WithContext(ctx).Model(&models.CodingAgentJob{}).
		Where("id = ? AND claimed_by = ?", jobID, workerID).
		Update("heartbeat_at", s.now()).Error; err != nil {
		return false, err
	}
	return job.CancelRequestedAt != nil, nil
}

// Requeue returns a claimed job to the queue only before runtime execution has
// started. It is used for a busy reusable environment and transient sandbox
// provisioning failures; running jobs are never automatically replayed.
func (s *Store) Requeue(ctx context.Context, jobID, workerID string, delay time.Duration, reason string) error {
	if delay < time.Second {
		delay = time.Second
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job models.CodingAgentJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND claimed_by = ?", jobID, workerID).First(&job).Error; err != nil {
			return err
		}
		if job.Status != models.CodingAgentJobClaimed || job.StartedAt != nil {
			return fmt.Errorf("%w: only an unstarted claimed job can be requeued", ErrInvalidTransition)
		}
		if job.AttemptCount >= job.MaxAttempts {
			now := s.now()
			if err := tx.Model(&job).Updates(map[string]any{
				"status": models.CodingAgentJobFailed, "completed_at": now,
				"last_error": reason, "claimed_by": "",
			}).Error; err != nil {
				return err
			}
			job.Status = models.CodingAgentJobFailed
			return s.appendEventTx(tx, &job, "failed", "Coding agent task exhausted its safe startup retries", nil)
		}
		availableAt := s.now().Add(delay)
		if err := tx.Model(&job).Updates(map[string]any{
			"status": models.CodingAgentJobPending, "available_at": availableAt,
			"claimed_at": nil, "heartbeat_at": nil, "claimed_by": "", "last_error": reason,
		}).Error; err != nil {
			return err
		}
		job.Status = models.CodingAgentJobPending
		return s.appendEventTx(tx, &job, "delayed", "Coding agent task will retry before execution", map[string]any{
			"availableAt": availableAt,
		})
	})
}

// ReconcileStale prevents a process restart from replaying a task after Codex
// may already have edited files. Unstarted claims are safe to requeue; running
// jobs become terminal with an explicit indeterminate/partial-work warning.
// Separate cutoffs let completion-marker recovery run for longer than a claim.
func (s *Store) ReconcileStale(ctx context.Context, claimedBefore, runningBefore time.Time) error {
	var jobs []models.CodingAgentJob
	if err := s.db.WithContext(ctx).
		Where(
			"(status = ? AND (heartbeat_at IS NULL OR heartbeat_at < ?)) OR (status = ? AND (heartbeat_at IS NULL OR heartbeat_at < ?))",
			models.CodingAgentJobClaimed, claimedBefore, models.CodingAgentJobRunning, runningBefore,
		).Find(&jobs).Error; err != nil {
		return err
	}
	for i := range jobs {
		jobID := jobs[i].ID.String()
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var job models.CodingAgentJob
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&job, "id = ?", jobID).Error; err != nil {
				return err
			}
			if job.Status != models.CodingAgentJobClaimed && job.Status != models.CodingAgentJobRunning {
				return nil
			}
			cutoff := claimedBefore
			if job.Status == models.CodingAgentJobRunning {
				cutoff = runningBefore
			}
			// The worker may have refreshed the heartbeat after the candidate list
			// was loaded but before this row lock was acquired.
			if job.HeartbeatAt != nil && !job.HeartbeatAt.Before(cutoff) {
				return nil
			}
			now := s.now()
			if job.Status == models.CodingAgentJobClaimed && job.StartedAt == nil && job.ProviderExecutionID == "" {
				if err := tx.Model(&job).Updates(map[string]any{
					"status": models.CodingAgentJobPending, "available_at": now,
					"claimed_at": nil, "heartbeat_at": nil, "claimed_by": "",
				}).Error; err != nil {
					return err
				}
				job.Status = models.CodingAgentJobPending
				return s.appendEventTx(tx, &job, "recovered", "Recovered an unstarted task after worker interruption", nil)
			}
			warning := "the worker stopped after execution began; the sandbox may contain partial changes and this task will not be replayed automatically"
			if err := tx.Model(&job).Updates(map[string]any{
				"status": models.CodingAgentJobFailed, "completed_at": now,
				"last_error": warning, "claimed_by": "",
			}).Error; err != nil {
				return err
			}
			job.Status = models.CodingAgentJobFailed
			if job.EnvironmentID != "" {
				if err := tx.Model(&models.CodingAgentEnvironment{}).
					Where("id = ? AND current_job_id = ?", job.EnvironmentID, job.ID.String()).
					Updates(map[string]any{"current_job_id": "", "status": models.CodingAgentEnvironmentError, "last_error": warning}).Error; err != nil {
					return err
				}
			}
			return s.appendEventTx(tx, &job, "interrupted", "Coding agent worker was interrupted after execution began", nil)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RequestCancel(ctx context.Context, organizationID, userID, jobID string) (*models.CodingAgentJob, error) {
	var job models.CodingAgentJob
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND user_id = ? AND id = ?", organizationID, userID, jobID).
			First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrJobNotFound
			}
			return err
		}
		if job.Status.Terminal() {
			return ErrJobTerminal
		}
		now := s.now()
		if job.Status == models.CodingAgentJobPending {
			if err := tx.Model(&job).Updates(map[string]any{
				"status": models.CodingAgentJobCancelled, "cancel_requested_at": now, "completed_at": now,
			}).Error; err != nil {
				return err
			}
			job.Status = models.CodingAgentJobCancelled
			job.CancelRequestedAt = &now
			job.CompletedAt = &now
			return s.appendEventTx(tx, &job, "cancelled", "Coding agent task cancelled before execution", nil)
		}
		if job.CancelRequestedAt == nil {
			if err := tx.Model(&job).Update("cancel_requested_at", now).Error; err != nil {
				return err
			}
			job.CancelRequestedAt = &now
			return s.appendEventTx(tx, &job, "cancel_requested", "Cancellation requested", nil)
		}
		return nil
	})
	return &job, err
}

func (s *Store) AppendEvent(ctx context.Context, jobID, workerID, eventType, message string, payload map[string]any) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job models.CodingAgentJob
		q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", jobID)
		if workerID != "" {
			q = q.Where("claimed_by = ?", workerID)
		}
		if err := q.First(&job).Error; err != nil {
			return err
		}
		return s.appendEventTx(tx, &job, eventType, message, payload)
	})
}

func (s *Store) transition(ctx context.Context, jobID, workerID string, target models.CodingAgentJobStatus, updates map[string]any, eventType, message string, payload map[string]any) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job models.CodingAgentJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND claimed_by = ?", jobID, workerID).First(&job).Error; err != nil {
			return err
		}
		if !validTransition(job.Status, target) {
			if job.Status.Terminal() {
				return ErrJobTerminal
			}
			return fmt.Errorf("%w: %s to %s", ErrInvalidTransition, job.Status, target)
		}
		updates["status"] = target
		if err := tx.Model(&job).Updates(updates).Error; err != nil {
			return err
		}
		job.Status = target
		return s.appendEventTx(tx, &job, eventType, message, payload)
	})
}

func validTransition(from, to models.CodingAgentJobStatus) bool {
	switch from {
	case models.CodingAgentJobPending:
		return to == models.CodingAgentJobClaimed || to == models.CodingAgentJobCancelled
	case models.CodingAgentJobClaimed:
		return to == models.CodingAgentJobRunning || to == models.CodingAgentJobFailed || to == models.CodingAgentJobCancelled || to == models.CodingAgentJobTimedOut
	case models.CodingAgentJobRunning:
		return to == models.CodingAgentJobSucceeded || to == models.CodingAgentJobFailed || to == models.CodingAgentJobCancelled || to == models.CodingAgentJobTimedOut
	default:
		return false
	}
}

func (s *Store) appendEventTx(tx *gorm.DB, job *models.CodingAgentJob, eventType, message string, payload map[string]any) error {
	encoded, err := jsonObject(payload)
	if err != nil {
		return fmt.Errorf("encode coding agent event: %w", err)
	}
	sequence := job.NextEventSequence
	if sequence < 1 {
		sequence = 1
	}
	event := models.CodingAgentEvent{
		OrganizationID: job.OrganizationID,
		JobID:          job.ID.String(),
		Sequence:       sequence,
		Type:           eventType,
		Message:        message,
		Payload:        models.JSONB(encoded),
	}
	if err := tx.Create(&event).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.CodingAgentJob{}).Where("id = ?", job.ID).
		Update("next_event_sequence", sequence+1).Error; err != nil {
		return err
	}
	job.NextEventSequence = sequence + 1
	return nil
}

func DecodePolicy(job *models.CodingAgentJob) (ExecutionPolicy, error) {
	var policy ExecutionPolicy
	if err := json.Unmarshal(job.ExecutionPolicy, &policy); err != nil {
		return policy, fmt.Errorf("decode coding agent execution policy: %w", err)
	}
	return policy, nil
}

// toolNodeIDsJSON renders the grant for storage, always as an array. A null
// here would read as "no grant recorded" rather than "granted nothing", and
// those must not be confusable for a deny-by-default check.
func toolNodeIDsJSON(ids []string) []byte {
	if len(ids) == 0 {
		return []byte("[]")
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return []byte("[]")
	}
	return raw
}
