package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/triggers"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Inbound provider events.
//
// Two routes because providers disagree about who owns the URL:
//
//	POST /api/hooks/:provider/:triggerID   one hook per subscription
//	POST /api/hooks/:provider              one URL for the whole app (GitHub, Slack)
//
// The order of operations is the security model, and it does not bend:
// read the raw body → answer any handshake → verify the signature → dedupe →
// acknowledge → only then run anything. A workflow run is a side effect in
// someone's account; it happens after we are certain the sender is who they
// claim, and never inline with the response.
//
// Acknowledging fast is not politeness. Slack requires a 200 within three
// seconds and disables an endpoint whose success rate stays under 5%, so a
// handler that waited for a workflow would take the whole integration down
// under load. The same instinct as the Stripe fix: an event we cannot place is
// acknowledged, not 500'd, because a 5xx buys retries for days and then a
// disabled endpoint.

// maxHookBody caps what we will read from a stranger. Provider payloads are
// kilobytes; anything at this size is not a payload.
const maxHookBody = 2 << 20 // 2 MiB

// ReceiveProviderHook handles both hook routes.
func (h *WorkflowHandler) ReceiveProviderHook(c *gin.Context) {
	ctx := c.Request.Context()
	provider := c.Param("provider")
	triggerID := c.Param("triggerID")

	adapter := triggers.GetPusher(provider)
	if adapter == nil {
		// Not a provider we speak. Genuinely not found — unlike an event we
		// cannot place, there is no endpoint here to protect from retries.
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxHookBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "could not read body"})
		return
	}

	// Handshakes come before everything: a provider verifying a URL has no
	// trigger to check against and no signature we could match.
	if status, resp, handled := adapter.Handshake(c.Request, body); handled {
		slog.InfoContext(ctx, "hook handshake", "provider", provider)
		c.Data(status, "text/plain; charset=utf-8", resp)
		return
	}

	var trig *models.IntegrationTrigger
	if triggerID != "" {
		var t models.IntegrationTrigger
		if err := h.db.DB.First(&t, "id = ?", triggerID).Error; err != nil {
			// The hook outlived the trigger — someone deleted it here but the hook
			// still exists at the provider. Acknowledge so the provider stops
			// retrying, and say so in the log; the repair is to unregister it.
			slog.WarnContext(ctx, "hook for a trigger that no longer exists",
				"provider", provider, "trigger_id", triggerID)
			c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "unknown trigger"})
			return
		}
		trig = &t
	}

	if err := adapter.Verify(c.Request, body, trig); err != nil {
		// The one case that is genuinely the sender's problem. Never run anything.
		slog.WarnContext(ctx, "hook signature rejected", "provider", provider,
			"trigger_id", triggerID, "reason", err.Error())
		c.JSON(http.StatusUnauthorized, gin.H{"error": "signature verification failed"})
		return
	}

	events, err := adapter.Parse(c.Request, body)
	if err != nil {
		slog.WarnContext(ctx, "hook payload unreadable", "provider", provider, "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "unreadable payload"})
		return
	}
	if len(events) == 0 {
		// The common case, not a failure: a repo hook hears every action on every
		// PR and this one was not the action anybody asked for.
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
		return
	}

	// Detach from the request: the response is about to be sent and the runs
	// outlive it. Span context carries over so the runs stay on this trace.
	bgCtx := trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))

	fired := 0
	for _, ev := range events {
		if ev.Lifecycle != nil {
			h.applyTriggerLifecycle(provider, ev)
			continue
		}
		matches := []models.IntegrationTrigger{}
		if trig != nil {
			matches = append(matches, *trig)
		} else {
			matches = h.triggersFor(provider, ev)
		}
		for i := range matches {
			if h.dispatch(bgCtx, &matches[i], ev) {
				fired++
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "events": len(events), "runs": fired})
}

const (
	installationRemovedError   = "GitHub App installation was removed — install Fernary again, then recreate this trigger"
	installationSuspendedError = "GitHub App installation is suspended — restore it on GitHub to resume this trigger"
	repositoryRemovedError     = "Fernary no longer has access to this repository — add it to the GitHub App installation to resume"
	authorizationRevokedError  = "GitHub authorization was revoked — reconnect GitHub to resume this trigger"
)

// applyTriggerLifecycle keeps the durable trigger state in step with an
// app-level provider. GitHub sends these control-plane events through the same
// signed webhook as pull requests and pushes, but they are never workflow
// inputs. Updates are deliberately idempotent because providers may redeliver.
func (h *WorkflowHandler) applyTriggerLifecycle(provider string, ev triggers.Event) {
	lifecycle := ev.Lifecycle
	if lifecycle == nil {
		return
	}
	base := h.db.DB.Model(&models.IntegrationTrigger{}).
		Where("provider = ? AND deleted_at IS NULL", provider)
	if ev.ScopeID != "" {
		base = base.Where("scope_id = ?", ev.ScopeID)
	}

	switch lifecycle.Action {
	case triggers.LifecycleScopeRemoved:
		base.Updates(map[string]any{"enabled": false, "last_error": installationRemovedError})

	case triggers.LifecycleScopeSuspended:
		base.Updates(map[string]any{"enabled": false, "last_error": installationSuspendedError})

	case triggers.LifecycleScopeRestored:
		// Only undo the state this lifecycle handler set. A trigger disabled for a
		// different reason must not be resurrected by an unrelated GitHub event.
		base.Where("last_error = ?", installationSuspendedError).
			Updates(map[string]any{"enabled": true, "last_error": "", "consecutive_failures": 0})

	case triggers.LifecycleResourcesRemoved:
		resources := normalizedResourceIDs(lifecycle.ResourceIDs)
		if len(resources) == 0 {
			return
		}
		base.Where("LOWER(resource_id) IN ?", resources).
			Updates(map[string]any{"enabled": false, "last_error": repositoryRemovedError})

	case triggers.LifecycleResourcesAdded:
		resources := normalizedResourceIDs(lifecycle.ResourceIDs)
		if len(resources) == 0 {
			return
		}
		base.Where("LOWER(resource_id) IN ? AND last_error = ?", resources, repositoryRemovedError).
			Updates(map[string]any{"enabled": true, "last_error": "", "consecutive_failures": 0})

	case triggers.LifecycleAuthorizationRevoked:
		h.revokeProviderAuthorization(provider, lifecycle.AccountID, lifecycle.AccountName)
	}
}

func normalizedResourceIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// revokeProviderAuthorization removes every stored grant for the provider
// account GitHub named. Authorization is per GitHub user, so the revocation
// applies across our organizations, not only to the installation that happened
// to emit the event.
func (h *WorkflowHandler) revokeProviderAuthorization(provider, accountID, accountName string) {
	accountID = strings.TrimSpace(accountID)
	accountName = strings.TrimSpace(accountName)
	if accountID == "" && accountName == "" {
		return
	}
	var connections []models.IntegrationConnection
	query := h.db.DB.Where("provider = ?", provider)
	switch {
	case accountID != "" && accountName != "":
		query = query.Where("workspace_id = ? OR LOWER(workspace_name) = LOWER(?)", accountID, accountName)
	case accountID != "":
		query = query.Where("workspace_id = ?", accountID)
	default:
		query = query.Where("LOWER(workspace_name) = LOWER(?)", accountName)
	}
	if err := query.Find(&connections).Error; err != nil {
		slog.Warn("could not resolve revoked provider authorization", "provider", provider, "error", err)
		return
	}
	for i := range connections {
		h.db.DB.Model(&models.IntegrationTrigger{}).
			Where("provider = ? AND organization_id = ? AND user_id = ? AND deleted_at IS NULL",
				provider, connections[i].OrganizationID, connections[i].UserID).
			Updates(map[string]any{"enabled": false, "last_error": authorizationRevokedError})
	}
	if len(connections) > 0 {
		h.db.DB.Unscoped().Delete(&connections)
	}
}

// triggersFor finds the triggers an app-level delivery belongs to.
//
// Slack posts every workspace's events to one URL, so the payload's own
// identifiers are the only routing information there is: match the provider,
// the event type, and the workspace the connection was made against.
func (h *WorkflowHandler) triggersFor(provider string, ev triggers.Event) []models.IntegrationTrigger {
	var out []models.IntegrationTrigger
	q := h.db.DB.
		Joins("JOIN integration_connections ic ON ic.user_id = integration_triggers.user_id"+
			" AND ic.provider = integration_triggers.provider AND ic.deleted_at IS NULL").
		Where("integration_triggers.provider = ? AND integration_triggers.event = ?"+
			" AND integration_triggers.enabled = true AND integration_triggers.deleted_at IS NULL",
			provider, ev.Type)

	// The installation is the strong key. Two accounts can each own a repository
	// called "acme/widgets", and one app-level webhook hears both — matching on
	// the name alone would let one account's pull request start the other's
	// workflow. When the delivery names its installation, the trigger must have
	// been set up against that same installation, full stop.
	if ev.ScopeID != "" {
		q = q.Where("integration_triggers.scope_id = ?", ev.ScopeID)
	}
	if ev.ResourceID != "" {
		// Either the trigger is scoped to this exact resource, or it is
		// account-wide and the connection's workspace is the one that spoke.
		q = q.Where("integration_triggers.resource_id = ? OR (integration_triggers.resource_id = '' AND ic.workspace_id = ?)",
			ev.ResourceID, ev.ResourceID)
	}
	q.Find(&out)
	return out
}

// dispatch decides whether one event should wake one trigger, and starts the
// run if so. Returns whether a run was started.
func (h *WorkflowHandler) dispatch(ctx context.Context, t *models.IntegrationTrigger, ev triggers.Event) bool {
	log := func(reason string) {
		slog.InfoContext(ctx, "trigger event not run", "trigger_id", t.ID.String(),
			"provider", t.Provider, "event", ev.Type, "reason", reason)
	}

	if t.Event != ev.Type || !t.Enabled {
		return false
	}
	// Checked here as well as in the routing query, because a delivery addressed
	// to a specific trigger id skips that query entirely. Both doors need the
	// same lock: an event from another installation must never wake this trigger,
	// however it arrived.
	if ev.ScopeID != "" && t.ScopeID != "" && ev.ScopeID != t.ScopeID {
		log("different installation")
		return false
	}
	// Filters run before admission — the point of filtering at the trigger is
	// that an event nobody wanted costs nothing.
	if !triggers.Matches(t, ev) {
		log("filtered")
		return false
	}

	// Published only. Same rule as schedules: a workflow still being edited must
	// not start acting on real events.
	var wf models.Workflow
	if err := h.db.DB.First(&wf, "id = ? AND deleted_at IS NULL", t.WorkflowID).Error; err != nil {
		log("workflow missing")
		return false
	}
	if !wf.Published {
		log("workflow not published")
		return false
	}

	// Claim the delivery before running. The unique index is what makes a
	// provider's retry harmless; ON CONFLICT DO NOTHING rather than
	// check-then-insert because two concurrent redeliveries would both pass the
	// check, and because a failed insert inside a transaction poisons it.
	if !h.claimDelivery(t.Provider, ev.Key+"@"+t.ID.String(), t.ID.String()) {
		log("duplicate delivery")
		return false
	}

	// Spend guard. A busy account can emit events far faster than anyone wants
	// workflow runs; without a ceiling the first chatty repo empties the org's
	// allowance and every other workflow stops with it.
	if over, count := h.overTriggerRate(t); over {
		h.noteTriggerError(t, "paused: more than "+strconv.Itoa(t.MaxRunsPerHour)+" events in the last hour")
		slog.WarnContext(ctx, "trigger over its hourly cap", "trigger_id", t.ID.String(),
			"count", count, "cap", t.MaxRunsPerHour)
		return false
	}

	payload, _ := json.Marshal(map[string]any{
		"provider": t.Provider, "event": ev.Type, "resource": ev.ResourceID,
		"occurred_at": ev.OccurredAt.UTC().Format(time.RFC3339), "data": ev.Data,
	})
	p, err := h.admitRun(ctx, t.WorkflowID, t.Provider,
		injectInto(executor.NodeTypeIntegrationTrigger, t.NodeID, string(payload)))
	if err != nil {
		h.noteTriggerError(t, err.Error())
		return false
	}

	now := time.Now().UTC()
	h.db.DB.Model(t).Updates(map[string]any{
		"last_event_at": now, "consecutive_failures": 0, "last_error": "",
	})
	slog.InfoContext(ctx, "trigger fired", "trigger_id", t.ID.String(), "provider", t.Provider,
		"event", ev.Type, "workflow_id", t.WorkflowID, "run_id", p.RunID())

	go h.executeRun(ctx, p)
	return true
}

// claimDelivery records that this delivery has been handled, and reports
// whether we are the first to do so.
func (h *WorkflowHandler) claimDelivery(provider, key, triggerID string) bool {
	row := models.WebhookDelivery{Provider: provider, Key: key, TriggerID: triggerID}
	res := h.db.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if res.Error != nil {
		// Never drop an event because bookkeeping failed — a missed run is worse
		// than a duplicate one, and duplicates are already rare.
		slog.Warn("could not record webhook delivery", "provider", provider, "error", res.Error)
		return true
	}
	return res.RowsAffected == 1
}

// overTriggerRate reports whether this trigger has already fired its hourly
// allowance. Counted from the delivery log, which only holds events that got
// as far as claiming a run.
func (h *WorkflowHandler) overTriggerRate(t *models.IntegrationTrigger) (bool, int64) {
	if t.MaxRunsPerHour <= 0 {
		return false, 0
	}
	var count int64
	h.db.DB.Model(&models.WebhookDelivery{}).
		Where("trigger_id = ? AND created_at > ?", t.ID.String(), time.Now().Add(-time.Hour)).
		Count(&count)
	return count > int64(t.MaxRunsPerHour), count
}

// noteTriggerError records why a trigger did not run, so the UI can say what
// happened instead of leaving a trigger that looks fine and does nothing.
func (h *WorkflowHandler) noteTriggerError(t *models.IntegrationTrigger, msg string) {
	h.db.DB.Model(t).Updates(map[string]any{
		"last_error":           truncate(msg, 300),
		"consecutive_failures": gorm.Expr("consecutive_failures + 1"),
	})
}
