package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const authAttemptLifetime = 15 * time.Minute

var (
	deviceURLPattern   = regexp.MustCompile(`https://[^\s<>]+`)
	deviceCodePattern  = regexp.MustCompile(`\b[A-Z0-9]{4,8}-[A-Z0-9]{4,8}\b`)
	toolVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)
	ansiCSIPattern     = regexp.MustCompile("\\x1b\\[[0-?]*[ -/]*[@-~]")
	ansiOSCPattern     = regexp.MustCompile("\\x1b\\][^\\x07\\x1b]*(?:\\x07|\\x1b\\\\)")
)

func (s *Service) StartCodexConnection(ctx context.Context, organizationID, userID string) (*models.CodingAgentAuthAttempt, bool, error) {
	if !s.Available(RuntimeCodex) {
		return nil, false, ErrUnavailable
	}
	if err := s.verifyMember(ctx, organizationID, userID); err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	activeKey := organizationID + ":" + userID + ":" + RuntimeCodex
	var attempt models.CodingAgentAuthAttempt
	created := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("active_key = ?", activeKey).First(&attempt).Error
		if err == nil {
			if attempt.ExpiresAt.After(now) && !attempt.Status.Terminal() {
				return nil
			}
			completedAt := now
			if err := tx.Model(&attempt).Updates(map[string]any{
				"active_key": nil, "status": models.CodingAgentAuthExpired,
				"completed_at": completedAt, "last_error": "authentication attempt expired",
			}).Error; err != nil {
				return err
			}
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var recentAttempts int64
		if err := tx.Model(&models.CodingAgentAuthAttempt{}).Where(
			"organization_id = ? AND user_id = ? AND runtime = ? AND created_at > ?",
			organizationID, userID, RuntimeCodex, now.Add(-time.Hour),
		).Count(&recentAttempts).Error; err != nil {
			return err
		}
		if recentAttempts >= 10 {
			return fmt.Errorf("%w: wait before starting another Codex sign-in", ErrRateLimited)
		}
		attempt = models.CodingAgentAuthAttempt{
			ActiveKey:      &activeKey,
			OrganizationID: organizationID, UserID: userID, Runtime: RuntimeCodex,
			Provider: s.provider.Name(), Status: models.CodingAgentAuthProvisioning,
			ClaimedBy: s.workerID, HeartbeatAt: &now, ExpiresAt: now.Add(authAttemptLifetime),
		}
		if err := tx.Create(&attempt).Error; err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		// With no pre-existing row, two transactions cannot lock a gap under
		// READ COMMITTED. The nullable unique ActiveKey chooses one winner; load
		// it after the losing transaction rolls back.
		var winner models.CodingAgentAuthAttempt
		if loadErr := s.db.WithContext(ctx).Where("active_key = ?", activeKey).First(&winner).Error; loadErr == nil {
			return &winner, false, nil
		}
		return nil, false, err
	}
	if created {
		go s.runCodexConnection(attempt.ID.String())
	}
	return &attempt, created, nil
}

func (s *Service) GetAuthAttempt(ctx context.Context, organizationID, userID, attemptID string) (*models.CodingAgentAuthAttempt, error) {
	var attempt models.CodingAgentAuthAttempt
	if err := s.db.WithContext(ctx).Where(
		"id = ? AND organization_id = ? AND user_id = ?", attemptID, organizationID, userID,
	).First(&attempt).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	cleanedURL := sanitizeVerificationURL(attempt.VerificationURL)
	if cleanedURL != attempt.VerificationURL {
		attempt.VerificationURL = cleanedURL
		if cleanedURL != "" {
			_ = s.db.WithContext(ctx).Model(&models.CodingAgentAuthAttempt{}).
				Where("id = ?", attempt.ID).Update("verification_url", cleanedURL).Error
		}
	}
	return &attempt, nil
}

func (s *Service) CancelAuthAttempt(ctx context.Context, organizationID, userID, attemptID string) error {
	now := time.Now().UTC()
	result := s.db.WithContext(ctx).Model(&models.CodingAgentAuthAttempt{}).
		Where("id = ? AND organization_id = ? AND user_id = ? AND status IN ?", attemptID, organizationID, userID,
			[]models.CodingAgentAuthStatus{models.CodingAgentAuthProvisioning, models.CodingAgentAuthWaiting}).
		Updates(map[string]any{
			"active_key": nil, "status": models.CodingAgentAuthCancelled,
			"cancel_requested_at": now, "completed_at": now,
			"last_error": "authentication cancelled", "claimed_by": "", "heartbeat_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrJobNotFound
	}
	s.authMu.Lock()
	cancel := s.authCancels[attemptID]
	s.authMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (s *Service) GetCredential(ctx context.Context, organizationID, userID, runtime string) (*models.CodingAgentCredential, error) {
	var credential models.CodingAgentCredential
	if err := s.db.WithContext(ctx).Where(
		"organization_id = ? AND user_id = ? AND runtime = ?", organizationID, userID, runtime,
	).First(&credential).Error; err != nil {
		return nil, err
	}
	credential.AuthBundle = ""
	return &credential, nil
}

func (s *Service) DisconnectCredential(ctx context.Context, organizationID, userID, runtime string) error {
	releaseAuthority, err := acquireAuthorityLock(ctx, s.db, organizationID, userID)
	if err != nil {
		return err
	}
	defer releaseAuthority()

	now := time.Now().UTC()
	var runningJobIDs []string
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var credential models.CodingAgentCredential
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"organization_id = ? AND user_id = ? AND runtime = ?", organizationID, userID, runtime,
		).First(&credential).Error; err != nil {
			return err
		}
		credential.Status = models.CodingAgentCredentialRevoked
		credential.AuthBundle = ""
		credential.LastVerifiedAt = &now
		if err := tx.Save(&credential).Error; err != nil {
			return err
		}
		var jobs []models.CodingAgentJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"organization_id = ? AND user_id = ? AND runtime = ? AND status IN ?",
			organizationID, userID, runtime, []models.CodingAgentJobStatus{
				models.CodingAgentJobPending, models.CodingAgentJobClaimed, models.CodingAgentJobRunning,
			},
		).Find(&jobs).Error; err != nil {
			return err
		}
		for index := range jobs {
			if jobs[index].Status == models.CodingAgentJobPending {
				if err := tx.Model(&jobs[index]).Updates(map[string]any{
					"status": models.CodingAgentJobCancelled, "cancel_requested_at": now,
					"completed_at": now, "last_error": "Codex connection was disconnected",
				}).Error; err != nil {
					return err
				}
				jobs[index].Status = models.CodingAgentJobCancelled
				if err := s.store.appendEventTx(tx, &jobs[index], "cancelled", "Task cancelled because Codex was disconnected", nil); err != nil {
					return err
				}
				continue
			}
			if err := tx.Model(&jobs[index]).Update("cancel_requested_at", now).Error; err != nil {
				return err
			}
			runningJobIDs = append(runningJobIDs, jobs[index].ID.String())
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, jobID := range runningJobIDs {
		s.cancelRunningJob(jobID)
	}
	return nil
}

func (s *Service) runCodexConnection(attemptID string) {
	ctx, cancel := context.WithTimeout(context.Background(), authAttemptLifetime)
	s.authMu.Lock()
	s.authCancels[attemptID] = cancel
	s.authMu.Unlock()
	go s.monitorAuthCancellation(ctx, cancel, attemptID)
	defer func() {
		cancel()
		s.authMu.Lock()
		delete(s.authCancels, attemptID)
		s.authMu.Unlock()
	}()

	var attempt models.CodingAgentAuthAttempt
	if err := s.db.WithContext(ctx).First(&attempt, "id = ?", attemptID).Error; err != nil {
		return
	}
	if attempt.CancelRequestedAt != nil {
		s.finishAuthAttempt(attemptID, models.CodingAgentAuthCancelled, errors.New("authentication cancelled"))
		return
	}
	if err := s.verifyMember(ctx, attempt.OrganizationID, attempt.UserID); err != nil {
		s.finishAuthAttempt(attemptID, models.CodingAgentAuthFailed, err)
		return
	}

	sandbox, err := s.provider.Create(ctx, SandboxSpec{
		Name: "fernary-auth-" + attempt.ID.String()[:20], Snapshot: s.config.SandboxSnapshot,
		Labels:          map[string]string{"fernary": "coding-agent-auth", "fernary-organization": attempt.OrganizationID},
		AutoStopMinutes: 15, AutoDeleteMinutes: 15, Ephemeral: true,
		NetworkBlockAll: true,
		AllowedDomains:  []string{"auth.openai.com", "api.openai.com", "chatgpt.com", "openai.com", "registry.npmjs.org"},
	})
	if err != nil {
		s.finishAuthAttempt(attemptID, models.CodingAgentAuthFailed, fmt.Errorf("create authentication sandbox: %w", err))
		return
	}
	_ = s.db.Model(&models.CodingAgentAuthAttempt{}).Where("id = ?", attemptID).Update("external_sandbox_id", sandbox.ID()).Error
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := sandbox.Delete(cleanupCtx); err == nil {
			_ = s.db.Model(&models.CodingAgentAuthAttempt{}).Where("id = ?", attemptID).Update("external_sandbox_id", "").Error
		}
	}()

	secretDir := "/tmp/fernary-codex-auth"
	prepared, err := sandbox.Run(ctx, CommandSpec{
		Command: "mkdir -p -- " + quoteShell(secretDir), WorkingDir: "/tmp", Timeout: time.Minute,
	}, nil)
	if err != nil || prepared.ExitCode != 0 {
		s.finishAuthAttempt(attemptID, models.CodingAgentAuthFailed, fmt.Errorf("prepare authentication directory: %s", commandFailure(prepared, err)))
		return
	}
	if err := sandbox.Upload(ctx, secretDir+"/config.toml", []byte("cli_auth_credentials_store = \"file\"\n"), 0o600); err != nil {
		s.finishAuthAttempt(attemptID, models.CodingAgentAuthFailed, err)
		return
	}
	version := strings.TrimSpace(s.config.CodexCLIVersion)
	if version == "" {
		version = DefaultCodexCLIVersion
	}
	if !toolVersionPattern.MatchString(version) {
		s.finishAuthAttempt(attemptID, models.CodingAgentAuthFailed, errors.New("configured Codex CLI version is invalid"))
		return
	}
	install := "if ! command -v codex >/dev/null 2>&1 || ! codex --version | grep -Fqx -- " + quoteShell("codex-cli "+version) + "; then npm install -g " + quoteShell("@openai/codex@"+version) + "; fi"
	installed, err := sandbox.Run(ctx, CommandSpec{Command: install, WorkingDir: "/tmp", Timeout: 10 * time.Minute}, nil)
	if err != nil || installed.ExitCode != 0 {
		s.finishAuthAttempt(attemptID, models.CodingAgentAuthFailed, fmt.Errorf("install Codex CLI: %s", commandFailure(installed, err)))
		return
	}

	parser := &deviceLoginParser{}
	login, loginErr := sandbox.Run(ctx, CommandSpec{
		Command: "codex login --device-auth", WorkingDir: "/tmp",
		Environment: map[string]string{"CODEX_HOME": secretDir}, Timeout: authAttemptLifetime,
	}, func(event StreamEvent) {
		if event.Type != "stdout" && event.Type != "stderr" {
			return
		}
		verificationURL, userCode := parser.Feed(event.Message)
		if verificationURL != "" || userCode != "" {
			updates := map[string]any{"status": models.CodingAgentAuthWaiting}
			if verificationURL != "" {
				updates["verification_url"] = verificationURL
			}
			if userCode != "" {
				updates["user_code"] = userCode
			}
			_ = s.db.Model(&models.CodingAgentAuthAttempt{}).
				Where("id = ? AND status IN ?", attemptID, []models.CodingAgentAuthStatus{models.CodingAgentAuthProvisioning, models.CodingAgentAuthWaiting}).
				Updates(updates).Error
		}
	})
	if loginErr != nil || login.ExitCode != 0 {
		status := models.CodingAgentAuthFailed
		var current models.CodingAgentAuthAttempt
		_ = s.db.First(&current, "id = ?", attemptID).Error
		if current.CancelRequestedAt != nil {
			status = models.CodingAgentAuthCancelled
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = models.CodingAgentAuthExpired
		}
		s.finishAuthAttempt(attemptID, status, fmt.Errorf("Codex login did not complete: %s", commandFailure(login, loginErr)))
		return
	}
	authBundle, err := sandbox.Download(ctx, secretDir+"/auth.json")
	if err != nil || len(authBundle) == 0 || !json.Valid(authBundle) {
		s.finishAuthAttempt(attemptID, models.CodingAgentAuthFailed, errors.New("Codex login completed without a valid authentication cache"))
		return
	}
	if err := s.completeCodexConnection(ctx, &attempt, authBundle); err != nil {
		s.finishAuthAttempt(attemptID, models.CodingAgentAuthFailed, fmt.Errorf("store Codex credential: %w", err))
		return
	}
}

func (s *Service) monitorAuthCancellation(ctx context.Context, cancel context.CancelFunc, attemptID string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var attempt models.CodingAgentAuthAttempt
			now := time.Now().UTC()
			updated := s.db.WithContext(ctx).Model(&models.CodingAgentAuthAttempt{}).Where(
				"id = ? AND claimed_by = ? AND status IN ?", attemptID, s.workerID, []models.CodingAgentAuthStatus{
					models.CodingAgentAuthProvisioning, models.CodingAgentAuthWaiting,
				},
			).Update("heartbeat_at", now)
			if updated.Error != nil {
				continue
			}
			if updated.RowsAffected == 0 {
				cancel()
				return
			}
			if err := s.db.WithContext(ctx).Select("status", "cancel_requested_at").First(&attempt, "id = ?", attemptID).Error; err != nil {
				continue
			}
			if attempt.Status.Terminal() || attempt.CancelRequestedAt != nil {
				cancel()
				return
			}
		}
	}
}

// completeCodexConnection shares the same authority lock as jobs, credential
// disconnects, and member removal. The final membership check, encrypted
// credential upsert, and terminal attempt status therefore have one durable
// boundary across replicas.
func (s *Service) completeCodexConnection(ctx context.Context, attempt *models.CodingAgentAuthAttempt, authBundle []byte) error {
	releaseAuthority, err := acquireAuthorityLock(ctx, s.db, attempt.OrganizationID, attempt.UserID)
	if err != nil {
		return err
	}
	defer releaseAuthority()

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current models.CodingAgentAuthAttempt
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", attempt.ID).Error; err != nil {
			return err
		}
		if current.Status.Terminal() || current.CancelRequestedAt != nil {
			return errors.New("Codex authentication was cancelled before it completed")
		}
		now := time.Now().UTC()
		if !current.ExpiresAt.After(now) {
			return errors.New("Codex authentication expired before it completed")
		}
		var members int64
		if err := tx.Model(&models.OrgMember{}).Where(
			"organization_id = ? AND user_id = ?", attempt.OrganizationID, attempt.UserID,
		).Count(&members).Error; err != nil {
			return err
		}
		if members != 1 {
			return errors.New("coding agent authority was revoked because the user is no longer an organization member")
		}
		credential := models.CodingAgentCredential{
			OrganizationID: attempt.OrganizationID, UserID: attempt.UserID, Runtime: RuntimeCodex,
			Status: models.CodingAgentCredentialConnected, AccountLabel: "ChatGPT account",
			AuthBundle: string(authBundle), ConnectedAt: now, LastVerifiedAt: &now,
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "organization_id"}, {Name: "user_id"}, {Name: "runtime"}},
			DoUpdates: clause.AssignmentColumns([]string{"status", "account_label", "auth_bundle", "connected_at", "last_verified_at", "last_error", "updated_at"}),
		}).Create(&credential).Error; err != nil {
			return err
		}
		return tx.Model(&current).Where("claimed_by = ? AND status IN ?", s.workerID, []models.CodingAgentAuthStatus{
			models.CodingAgentAuthProvisioning, models.CodingAgentAuthWaiting,
		}).Updates(map[string]any{
			"active_key": nil, "status": models.CodingAgentAuthConnected,
			"completed_at": now, "last_error": "", "claimed_by": "", "heartbeat_at": now,
		}).Error
	})
}

func (s *Service) finishAuthAttempt(attemptID string, status models.CodingAgentAuthStatus, attemptErr error) {
	now := time.Now().UTC()
	updates := map[string]any{
		"active_key": nil, "status": status, "completed_at": now,
		"claimed_by": "", "heartbeat_at": now,
	}
	if attemptErr != nil {
		message := attemptErr.Error()
		if len(message) > 1000 {
			message = message[len(message)-1000:]
		}
		updates["last_error"] = message
	} else {
		updates["last_error"] = ""
	}
	_ = s.db.Model(&models.CodingAgentAuthAttempt{}).Where(
		"id = ? AND claimed_by = ? AND status IN ?", attemptID, s.workerID, []models.CodingAgentAuthStatus{
			models.CodingAgentAuthProvisioning, models.CodingAgentAuthWaiting,
		},
	).Updates(updates).Error
}

func (s *Service) reconcileAuthAttempts(ctx context.Context, staleBefore time.Time) error {
	var attempts []models.CodingAgentAuthAttempt
	if err := s.db.WithContext(ctx).Where("status IN ? AND (heartbeat_at IS NULL OR heartbeat_at < ?)", []models.CodingAgentAuthStatus{
		models.CodingAgentAuthProvisioning, models.CodingAgentAuthWaiting,
	}, staleBefore).Find(&attempts).Error; err != nil {
		return err
	}
	for _, attempt := range attempts {
		var sandboxID string
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var current models.CodingAgentAuthAttempt
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", attempt.ID).Error; err != nil {
				return err
			}
			if current.Status.Terminal() || (current.HeartbeatAt != nil && !current.HeartbeatAt.Before(staleBefore)) {
				return nil
			}
			now := time.Now().UTC()
			status := models.CodingAgentAuthFailed
			message := "authentication was interrupted by a server restart; start a new connection"
			if current.CancelRequestedAt != nil {
				status = models.CodingAgentAuthCancelled
				message = "authentication was cancelled"
			} else if now.After(current.ExpiresAt) {
				status = models.CodingAgentAuthExpired
				message = "authentication attempt expired"
			}
			sandboxID = current.ExternalSandboxID
			return tx.Model(&current).Updates(map[string]any{
				"active_key": nil, "status": status, "completed_at": now,
				"claimed_by": "", "heartbeat_at": now, "last_error": message,
			}).Error
		})
		if err != nil {
			return err
		}
		if sandboxID != "" && s.provider != nil {
			if sandbox, err := s.provider.Get(ctx, sandboxID); err == nil {
				_ = sandbox.Delete(ctx)
			}
		}
	}
	return nil
}

type deviceLoginParser struct {
	buffer string
}

func (p *deviceLoginParser) Feed(chunk string) (string, string) {
	p.buffer += chunk
	p.buffer = stripTerminalEscapes(p.buffer)
	if len(p.buffer) > 64<<10 {
		p.buffer = p.buffer[len(p.buffer)-(64<<10):]
	}
	verificationURL := ""
	for _, candidate := range deviceURLPattern.FindAllString(p.buffer, -1) {
		if candidate = sanitizeVerificationURL(candidate); candidate != "" {
			lower := strings.ToLower(candidate)
			if strings.Contains(lower, "device") || strings.Contains(lower, "activate") {
				verificationURL = candidate
			}
		}
	}
	userCode := ""
	upperBuffer := strings.ToUpper(p.buffer)
	// The Daytona execution stream can include the command itself, including
	// /tmp/fernary-codex-auth. That path happens to match the broad CODE-CODE
	// shape, so only inspect text after Codex's explicit device-code prompt.
	if promptIndex := strings.LastIndex(upperBuffer, "ONE-TIME CODE"); promptIndex >= 0 {
		matches := deviceCodePattern.FindAllString(upperBuffer[promptIndex+len("ONE-TIME CODE"):], -1)
		if len(matches) > 0 {
			userCode = matches[len(matches)-1]
		}
	}
	return verificationURL, userCode
}

func stripTerminalEscapes(value string) string {
	value = ansiOSCPattern.ReplaceAllString(value, "")
	return ansiCSIPattern.ReplaceAllString(value, "")
}

func sanitizeVerificationURL(value string) string {
	value = strings.TrimRight(strings.TrimSpace(stripTerminalEscapes(value)), ".,);]}")
	if index := strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }); index >= 0 {
		value = value[:index]
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || !trustedCodexAuthHost(parsed.Hostname()) {
		return ""
	}
	return parsed.String()
}

func trustedCodexAuthHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, domain := range []string{"openai.com", "chatgpt.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}
