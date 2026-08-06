package handlers

import (
	"context"
	"log/slog"
	"math/rand"
	"strconv"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/triggers"
)

// The two background sweeps that keep triggers alive.
//
// Both ride the scheduler's existing minute ticker rather than starting timers
// of their own, which also means they inherit its single-replica assumption: a
// second instance would poll every mailbox twice and race every renewal.
//
// A trigger that has quietly stopped working is the failure mode worth
// engineering against here. Both sweeps record why they failed on the row, and
// disable a trigger only after it has failed repeatedly — so the UI can say
// "reconnect GitHub" instead of showing something that looks healthy and never
// fires.

const (
	// pollBackoffCap keeps a broken trigger from hammering a provider while still
	// recovering on its own once the provider comes back.
	pollBackoffCap = time.Hour
	// disableAfterFailures is where "temporarily broken" becomes "needs a human".
	disableAfterFailures = 20
	// renewWindow is how far ahead of expiry a subscription is renewed. Generous
	// on purpose: renewing late means a window where events are silently lost,
	// and the sweep only runs once a minute.
	renewWindow = 24 * time.Hour
)

// pollDueTriggers asks poll-delivery providers what has happened.
func (h *WorkflowHandler) pollDueTriggers() {
	ctx := context.Background()
	now := time.Now().UTC()

	var due []models.IntegrationTrigger
	// Published only — polling a draft workflow would spend the user's provider
	// quota on a workflow that is not allowed to run anyway.
	h.db.DB.
		Joins("JOIN workflows ON workflows.id::text = integration_triggers.workflow_id").
		Where(`workflows.published = true AND workflows.deleted_at IS NULL
			AND integration_triggers.deleted_at IS NULL
			AND integration_triggers.delivery = ? AND integration_triggers.enabled = true
			AND integration_triggers.next_poll_at IS NOT NULL
			AND integration_triggers.next_poll_at <= ?`, models.DeliveryPoll, now).
		Find(&due)

	for i := range due {
		h.pollOne(ctx, &due[i])
	}
}

func (h *WorkflowHandler) pollOne(ctx context.Context, t *models.IntegrationTrigger) {
	poller := triggers.GetPoller(t.Provider)
	if poller == nil {
		return
	}
	token, workspace := FreshAccessTokenForOrg(h.db.DB, t.OrganizationID, t.UserID, t.Provider)
	if token == "" {
		h.failPoll(t, "the "+t.Provider+" connection is gone — reconnect it")
		return
	}

	events, cursor, err := poller.Poll(ctx, triggers.Conn{AccessToken: token, WorkspaceID: workspace}, t)
	if err != nil {
		h.failPoll(t, err.Error())
		return
	}

	for _, ev := range events {
		h.dispatch(ctx, t, ev)
	}

	// The cursor advances only now, after the events it produced have been
	// admitted. Advancing first would mean a crash here loses everything in the
	// window; this way the same window is re-read and the delivery dedupe throws
	// the repeats away.
	now := time.Now().UTC()
	next := now.Add(h.pollInterval(t))
	updates := map[string]any{
		"last_polled_at": now, "next_poll_at": next,
		"consecutive_failures": 0, "last_error": "",
	}
	if cursor != "" {
		updates["cursor"] = cursor
	}
	h.db.DB.Model(t).Updates(updates)
}

// pollInterval spreads load. Without the jitter every mailbox connected on the
// same day would hit the provider on the same second forever.
func (h *WorkflowHandler) pollInterval(t *models.IntegrationTrigger) time.Duration {
	base := time.Duration(t.PollIntervalSeconds) * time.Second
	if base < time.Minute {
		base = time.Minute
	}
	return base + time.Duration(rand.Intn(15))*time.Second
}

// failPoll backs a broken trigger off, and eventually gives up on it out loud.
func (h *WorkflowHandler) failPoll(t *models.IntegrationTrigger, reason string) {
	fails := t.ConsecutiveFailures + 1
	backoff := time.Duration(1<<min(fails, 6)) * time.Minute
	if backoff > pollBackoffCap {
		backoff = pollBackoffCap
	}
	next := time.Now().UTC().Add(backoff)
	updates := map[string]any{
		"consecutive_failures": fails,
		"last_error":           truncate(reason, 300),
		"next_poll_at":         next,
		"last_polled_at":       time.Now().UTC(),
	}
	if fails >= disableAfterFailures {
		updates["enabled"] = false
		updates["last_error"] = truncate("stopped after "+strconv.Itoa(fails)+" failures: "+reason, 300)
	}
	h.db.DB.Model(t).Updates(updates)
	slog.Warn("trigger poll failed", "trigger_id", t.ID.String(), "provider", t.Provider,
		"failures", fails, "reason", reason)
}

// renewExpiringSubscriptions extends push subscriptions before they lapse.
//
// Only some providers expire — GitHub hooks live forever, Microsoft Graph gives
// under three days, Airtable about a week. Adapters that return a nil time from
// Renew are saying "not applicable", and are left alone.
func (h *WorkflowHandler) renewExpiringSubscriptions() {
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(renewWindow)

	var due []models.IntegrationTrigger
	h.db.DB.Where(`delivery = ? AND enabled = true AND deleted_at IS NULL
		AND expires_at IS NOT NULL AND expires_at <= ?`, models.DeliveryPush, cutoff).
		Find(&due)

	for i := range due {
		t := &due[i]
		p := triggers.GetPusher(t.Provider)
		if p == nil {
			continue
		}
		token, workspace := FreshAccessTokenForOrg(h.db.DB, t.OrganizationID, t.UserID, t.Provider)
		if token == "" {
			h.noteTriggerError(t, "the "+t.Provider+" connection is gone — reconnect it")
			continue
		}
		expires, err := p.Renew(ctx, triggers.Conn{AccessToken: token, WorkspaceID: workspace}, t)
		if err != nil {
			h.noteTriggerError(t, "could not renew: "+err.Error())
			slog.Warn("trigger renewal failed", "trigger_id", t.ID.String(),
				"provider", t.Provider, "error", err.Error())
			continue
		}
		if expires == nil {
			continue
		}
		h.db.DB.Model(t).Updates(map[string]any{
			"expires_at": expires, "consecutive_failures": 0, "last_error": "",
		})
		slog.Info("trigger renewed", "trigger_id", t.ID.String(), "provider", t.Provider,
			"expires_at", expires)
	}
}
