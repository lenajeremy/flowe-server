package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Service) acquireEnvironment(ctx context.Context, job *models.CodingAgentJob, policy ExecutionPolicy) (*models.CodingAgentEnvironment, Sandbox, error) {
	key := workspaceKey(job, policy, s.config.SandboxSnapshot)
	encodedPolicy, _ := json.Marshal(policy)
	var environment models.CodingAgentEnvironment
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("workspace_key = ?", key).First(&environment).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			environment = models.CodingAgentEnvironment{
				OrganizationID: job.OrganizationID, UserID: job.UserID, WorkflowID: job.WorkflowID,
				NodeID: job.NodeID, WorkspaceKey: key, Provider: s.provider.Name(), Snapshot: s.config.SandboxSnapshot,
				Status: models.CodingAgentEnvironmentProvisioning, RepositoryProvider: normalizedRepositoryProvider(policy.RepositoryProvider),
				RepositoryID: policy.RepositoryID, Repository: policy.Repository, Branch: policy.Branch,
				CurrentJobID: job.ID.String(), AutoStopMinutes: policy.AutoStopMinutes,
				AutoDeleteMinutes: policy.AutoDeleteMinutes, Configuration: models.JSONB(encodedPolicy),
			}
			if err := tx.Create(&environment).Error; err != nil {
				return err
			}
			return nil
		}
		if err != nil {
			return err
		}
		if environment.OrganizationID != job.OrganizationID || environment.UserID != job.UserID {
			return ErrEnvironmentBusy
		}
		if environment.CurrentJobID != "" && environment.CurrentJobID != job.ID.String() {
			return ErrEnvironmentBusy
		}
		if environment.Status == models.CodingAgentEnvironmentDeleting || environment.Status == models.CodingAgentEnvironmentArchived {
			return errors.New("coding agent environment is no longer available")
		}
		if environment.Status == models.CodingAgentEnvironmentError && strings.Contains(environment.LastError, ErrSandboxExecutionUncertain.Error()) {
			return errors.New("coding agent environment must be reset because its previous process may still be running")
		}
		status := models.CodingAgentEnvironmentBusy
		if environment.ExternalSandboxID == "" {
			status = models.CodingAgentEnvironmentProvisioning
		}
		return tx.Model(&environment).Updates(map[string]any{
			"current_job_id": job.ID.String(), "status": status, "last_error": "",
		}).Error
	})
	if err != nil {
		return nil, nil, err
	}

	name := "fernary-" + key[:20]
	var sandbox Sandbox
	provision := func() (Sandbox, bool, error) {
		recovered, getErr := s.provider.Get(ctx, name)
		if getErr == nil {
			return recovered, false, nil
		}
		if !errors.Is(getErr, ErrSandboxNotFound) {
			return nil, false, getErr
		}
		createdSandbox, createErr := s.provider.Create(ctx, SandboxSpec{
			Name: name, Snapshot: s.config.SandboxSnapshot,
			Labels: map[string]string{
				"fernary": "coding-agent", "fernary-workspace": key[:32],
				"fernary-organization": job.OrganizationID,
			},
			AutoStopMinutes: policy.AutoStopMinutes, AutoDeleteMinutes: policy.AutoDeleteMinutes,
			NetworkBlockAll: policy.NetworkBlockAll, AllowedDomains: policy.AllowedDomains,
			Ephemeral: policy.WorkspaceMode == WorkspaceEphemeral,
		})
		return createdSandbox, createErr == nil, createErr
	}
	if environment.ExternalSandboxID == "" {
		// A deterministic name lets a retry recover a sandbox created immediately
		// before a worker crash instead of leaking a duplicate environment.
		var sandboxCreated bool
		sandbox, sandboxCreated, err = provision()
		if err != nil {
			_ = s.markEnvironmentError(ctx, &environment, job.ID.String(), err)
			return &environment, nil, fmt.Errorf("provision coding agent environment: %w", err)
		}
		if err := s.db.WithContext(ctx).Model(&models.CodingAgentEnvironment{}).
			Where("id = ? AND current_job_id = ?", environment.ID, job.ID.String()).
			Updates(map[string]any{
				"external_sandbox_id": sandbox.ID(), "status": models.CodingAgentEnvironmentBusy,
				"last_activity_at": time.Now().UTC(), "last_error": "",
			}).Error; err != nil {
			if sandboxCreated {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				_ = sandbox.Delete(cleanupCtx)
			}
			return &environment, nil, fmt.Errorf("record coding agent environment: %w", err)
		}
		environment.ExternalSandboxID = sandbox.ID()
		environment.Status = models.CodingAgentEnvironmentBusy
		return &environment, sandbox, nil
	}

	sandbox, err = s.provider.Get(ctx, environment.ExternalSandboxID)
	if errors.Is(err, ErrSandboxNotFound) {
		// Daytona may have auto-deleted a stopped reusable sandbox. Recreate the
		// deterministic workspace instead of permanently poisoning its DB row.
		if clearErr := s.db.WithContext(ctx).Model(&models.CodingAgentEnvironment{}).
			Where("id = ? AND current_job_id = ?", environment.ID, job.ID.String()).
			Updates(map[string]any{"external_sandbox_id": "", "status": models.CodingAgentEnvironmentProvisioning}).Error; clearErr != nil {
			return &environment, nil, clearErr
		}
		var sandboxCreated bool
		sandbox, sandboxCreated, err = provision()
		if err == nil {
			recorded := s.db.WithContext(ctx).Model(&models.CodingAgentEnvironment{}).
				Where("id = ? AND current_job_id = ?", environment.ID, job.ID.String()).
				Updates(map[string]any{
					"external_sandbox_id": sandbox.ID(), "status": models.CodingAgentEnvironmentBusy,
					"last_activity_at": time.Now().UTC(), "last_error": "",
				})
			if recorded.Error != nil {
				if sandboxCreated {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					_ = sandbox.Delete(cleanupCtx)
				}
				return &environment, nil, recorded.Error
			}
			environment.ExternalSandboxID = sandbox.ID()
			environment.Status = models.CodingAgentEnvironmentBusy
			return &environment, sandbox, nil
		}
	}
	if err != nil {
		_ = s.markEnvironmentError(ctx, &environment, job.ID.String(), err)
		return &environment, nil, fmt.Errorf("load coding agent environment: %w", err)
	}
	if err := sandbox.Start(ctx); err != nil {
		_ = s.markEnvironmentError(ctx, &environment, job.ID.String(), err)
		return &environment, nil, fmt.Errorf("start coding agent environment: %w", err)
	}
	_ = s.db.WithContext(ctx).Model(&models.CodingAgentEnvironment{}).Where("id = ?", environment.ID).
		Updates(map[string]any{"status": models.CodingAgentEnvironmentBusy, "last_activity_at": time.Now().UTC(), "last_error": ""}).Error
	return &environment, sandbox, nil
}

func (s *Service) markEnvironmentError(ctx context.Context, environment *models.CodingAgentEnvironment, jobID string, environmentErr error) error {
	return s.db.WithContext(ctx).Model(&models.CodingAgentEnvironment{}).
		Where("id = ? AND current_job_id = ?", environment.ID, jobID).
		Updates(map[string]any{
			"status": models.CodingAgentEnvironmentError, "current_job_id": "", "last_error": environmentErr.Error(),
		}).Error
}

func (s *Service) releaseEnvironment(ctx context.Context, environment *models.CodingAgentEnvironment, job *models.CodingAgentJob, policy ExecutionPolicy, healthy bool, lastError string) {
	if environment == nil {
		return
	}
	releaseCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	status := models.CodingAgentEnvironmentReady
	if !healthy {
		status = models.CodingAgentEnvironmentError
	}
	if policy.WorkspaceMode == WorkspaceEphemeral {
		status = models.CodingAgentEnvironmentDeleting
		_ = s.db.WithContext(releaseCtx).Model(&models.CodingAgentEnvironment{}).
			Where("id = ? AND current_job_id = ?", environment.ID, job.ID.String()).
			Updates(map[string]any{"status": status, "last_error": lastError}).Error
		if environment.ExternalSandboxID == "" {
			status = models.CodingAgentEnvironmentArchived
		} else if sandbox, err := s.provider.Get(releaseCtx, environment.ExternalSandboxID); err != nil {
			if errors.Is(err, ErrSandboxNotFound) {
				status = models.CodingAgentEnvironmentArchived
			} else {
				lastError = err.Error()
				status = models.CodingAgentEnvironmentError
			}
		} else if err := sandbox.Delete(releaseCtx); err != nil {
			if errors.Is(err, ErrSandboxNotFound) {
				status = models.CodingAgentEnvironmentArchived
			} else {
				lastError = err.Error()
				status = models.CodingAgentEnvironmentError
			}
		} else {
			status = models.CodingAgentEnvironmentArchived
		}
	}
	updates := map[string]any{
		"status": status, "current_job_id": "", "last_activity_at": time.Now().UTC(), "last_error": lastError,
	}
	if policy.WorkspaceMode == WorkspaceEphemeral && status == models.CodingAgentEnvironmentArchived {
		updates["external_sandbox_id"] = ""
	}
	_ = s.db.WithContext(releaseCtx).Model(&models.CodingAgentEnvironment{}).
		Where("id = ? AND current_job_id = ?", environment.ID, job.ID.String()).
		Updates(updates).Error
}

// finishSuccessfulEnvironmentLifecycle runs only after the result and lease
// release are durable. Provider cleanup failures never erase a successful job;
// the environment is marked for an operator/user reset instead.
func (s *Service) finishSuccessfulEnvironmentLifecycle(ctx context.Context, environment *models.CodingAgentEnvironment, policy ExecutionPolicy) {
	if environment == nil || policy.WorkspaceMode != WorkspaceEphemeral || environment.ExternalSandboxID == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	status := models.CodingAgentEnvironmentArchived
	lastError := ""
	sandbox, err := s.provider.Get(cleanupCtx, environment.ExternalSandboxID)
	if err == nil {
		err = sandbox.Delete(cleanupCtx)
	}
	if errors.Is(err, ErrSandboxNotFound) {
		err = nil
	}
	updates := map[string]any{"status": status, "external_sandbox_id": "", "last_error": lastError}
	if err != nil {
		updates["status"] = models.CodingAgentEnvironmentError
		updates["last_error"] = err.Error()
		// Preserve the provider ID so reset/reconciliation can retry cleanup.
		// Clearing it here would leak an unreachable Daytona sandbox.
		delete(updates, "external_sandbox_id")
	}
	_ = s.db.WithContext(cleanupCtx).Model(&models.CodingAgentEnvironment{}).
		Where("id = ? AND current_job_id = ''", environment.ID).Updates(updates).Error
}

func (s *Service) prepareRepository(ctx context.Context, sandbox Sandbox, job *models.CodingAgentJob, policy ExecutionPolicy) error {
	provider, repositoryURL, gitUsername, err := repositoryCloneSettings(policy)
	if err != nil {
		return err
	}
	token := ""
	if s.config.RepositoryToken == nil {
		return errors.New("repository credential lookup is not configured")
	}
	token, err = s.config.RepositoryToken(ctx, job.OrganizationID, job.UserID, provider)
	if err != nil {
		return fmt.Errorf("load %s credential: %w", providerDisplayName(provider), err)
	}
	if token == "" {
		return fmt.Errorf("connect %s before running this coding agent", providerDisplayName(provider))
	}

	authPrefix, cleanup, err := prepareGitAuthentication(ctx, sandbox, token, gitUsername)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := sandbox.Run(ctx, CommandSpec{Command: "mkdir -p -- /workspace", WorkingDir: "/", Timeout: time.Minute}, nil); err != nil {
		return fmt.Errorf("prepare repository workspace: %w", err)
	}
	present, err := sandbox.Run(ctx, CommandSpec{Command: "test -d /workspace/repo/.git", WorkingDir: "/", Timeout: time.Minute}, nil)
	if err != nil {
		return fmt.Errorf("inspect repository workspace: %w", err)
	}
	if present.ExitCode == 0 {
		remote, err := sandbox.Run(ctx, CommandSpec{Command: "git remote get-url origin", WorkingDir: "/workspace/repo", Timeout: time.Minute}, nil)
		if err != nil || remote.ExitCode != 0 {
			return errors.New("persistent coding-agent workspace has no readable Git origin")
		}
		got := strings.TrimSuffix(strings.TrimSpace(remote.Stdout), "/")
		if got != repositoryURL && got != strings.TrimSuffix(repositoryURL, ".git") {
			return errors.New("persistent coding-agent workspace origin no longer matches its configured repository")
		}
		fetch, err := sandbox.Run(ctx, CommandSpec{Command: authPrefix + "git fetch --prune --no-tags origin", WorkingDir: "/workspace/repo", Timeout: 5 * time.Minute}, nil)
		if err != nil || fetch.ExitCode != 0 {
			return fmt.Errorf("refresh coding-agent repository: %s", commandFailure(fetch, err))
		}
		return nil
	}
	if present.ExitCode != 1 {
		return fmt.Errorf("inspect repository workspace: unexpected exit code %d", present.ExitCode)
	}
	branch := ""
	if policy.Branch != "" {
		branch = " --branch " + quoteShell(policy.Branch)
	}
	clone, err := sandbox.Run(ctx, CommandSpec{
		Command:    authPrefix + "git clone --no-tags" + branch + " -- " + quoteShell(repositoryURL) + " /workspace/repo",
		WorkingDir: "/workspace", Timeout: 10 * time.Minute,
	}, nil)
	if err != nil || clone.ExitCode != 0 {
		return fmt.Errorf("clone coding-agent repository: %s", commandFailure(clone, err))
	}
	return nil
}

func prepareGitAuthentication(ctx context.Context, sandbox Sandbox, token, username string) (string, func(), error) {
	if token == "" {
		return "GIT_TERMINAL_PROMPT=0 ", func() {}, nil
	}
	directory := "/tmp/fernary-git-" + uuid.NewString()
	tokenPath := directory + "/token"
	askpassPath := directory + "/askpass.sh"
	created, err := sandbox.Run(ctx, CommandSpec{Command: "mkdir -p -- " + quoteShell(directory), WorkingDir: "/tmp", Timeout: time.Minute}, nil)
	if err != nil || created.ExitCode != 0 {
		return "", func() {}, fmt.Errorf("prepare Git credential directory: %s", commandFailure(created, err))
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := "rm -f -- " + quoteShell(tokenPath) + " " + quoteShell(askpassPath) + " && rmdir -- " + quoteShell(directory)
		_, _ = sandbox.Run(cleanupCtx, CommandSpec{Command: command, WorkingDir: "/tmp", Timeout: 30 * time.Second}, nil)
	}
	if err := sandbox.Upload(ctx, tokenPath, []byte(token), 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("upload temporary Git credential: %w", err)
	}
	script := "#!/bin/sh\ncase \"$1\" in\n*Username*) printf '%s\\n' " + quoteShell(username) + " ;;\n*Password*) cat -- " + quoteShell(tokenPath) + " ;;\n*) exit 1 ;;\nesac\n"
	if err := sandbox.Upload(ctx, askpassPath, []byte(script), 0o700); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("upload Git credential helper: %w", err)
	}
	prefix := "GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=" + quoteShell(askpassPath) + " "
	return prefix, cleanup, nil
}

func repositoryCloneSettings(policy ExecutionPolicy) (provider, repositoryURL, username string, err error) {
	provider = normalizedRepositoryProvider(policy.RepositoryProvider)
	switch provider {
	case RepositoryGitHub:
		return provider, "https://github.com/" + policy.Repository + ".git", "x-access-token", nil
	case RepositoryGitLab:
		return provider, "https://gitlab.com/" + policy.Repository + ".git", "oauth2", nil
	default:
		return "", "", "", fmt.Errorf("unsupported coding agent repository provider %q", provider)
	}
}

func providerDisplayName(provider string) string {
	if provider == RepositoryGitLab {
		return "GitLab"
	}
	return "GitHub"
}

func commandFailure(result CommandResult, err error) string {
	if err != nil {
		return err.Error()
	}
	message := strings.TrimSpace(result.Stderr)
	if message == "" {
		message = strings.TrimSpace(result.Stdout)
	}
	if len(message) > 1000 {
		message = message[len(message)-1000:]
	}
	if message == "" {
		message = fmt.Sprintf("command exited with code %d", result.ExitCode)
	}
	return message
}

func quoteShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
