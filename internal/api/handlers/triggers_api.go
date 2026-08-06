package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/triggers"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Managing integration triggers.
//
// Creating one is not a local write: it registers a real webhook inside the
// user's GitHub repository (or Shopify store, or Slack workspace). That makes
// the create and delete paths the interesting ones — they have to leave the
// provider and our database agreed with each other even when one of them fails.

// GET /api/trigger-catalog — every provider and event we can subscribe to.
//
// Served from the adapter registry rather than a hand-written list, so the
// dropdown in the config panel cannot offer an event no adapter implements.
func (h *WorkflowHandler) TriggerCatalog(c *gin.Context) {
	type providerEntry struct {
		Provider string               `json:"provider"`
		Delivery string               `json:"delivery"`
		Events   []triggers.EventSpec `json:"events"`
	}
	out := []providerEntry{}
	for provider, events := range triggers.Catalog() {
		a := triggers.Get(provider)
		out = append(out, providerEntry{
			Provider: provider, Delivery: string(a.Delivery()), Events: events,
		})
	}
	c.JSON(http.StatusOK, gin.H{"providers": out})
}

// GET /api/workflows/:id/triggers
func (h *WorkflowHandler) ListTriggers(c *gin.Context) {
	wf, ok := h.loadOwnedWorkflow(c, c.Param("id"))
	if !ok {
		return
	}
	var list []models.IntegrationTrigger
	h.db.DB.Where("workflow_id = ?", wf.ID.String()).Order("created_at").Find(&list)
	c.JSON(http.StatusOK, gin.H{"triggers": list})
}

type createTriggerRequest struct {
	NodeID        string            `json:"node_id"`
	Provider      string            `json:"provider"`
	Event         string            `json:"event"`
	ResourceID    string            `json:"resource_id"`
	ResourceLabel string            `json:"resource_label"`
	Filters       map[string]string `json:"filters"`
}

// POST /api/workflows/:id/triggers
func (h *WorkflowHandler) CreateTrigger(c *gin.Context) {
	ctx := c.Request.Context()
	wf, ok := h.loadOwnedWorkflow(c, c.Param("id"))
	if !ok {
		return
	}
	var req createTriggerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	req.NodeID = strings.TrimSpace(req.NodeID)
	req.Provider = strings.TrimSpace(req.Provider)
	req.Event = strings.TrimSpace(req.Event)
	req.ResourceID = strings.TrimSpace(req.ResourceID)
	req.ResourceLabel = strings.TrimSpace(req.ResourceLabel)
	if req.Filters == nil {
		req.Filters = map[string]string{}
	}
	if err := validateIntegrationTriggerNode(wf, req.NodeID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Check the pair against the registry before anything else: a typo caught
	// here is a validation error, and a typo stored is a hook that registers
	// fine and then silently never matches an event.
	if !triggers.Supports(req.Provider, req.Event) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "unknown trigger: " + req.Provider + " / " + req.Event})
		return
	}
	adapter := triggers.Get(req.Provider)

	// A node is the idempotency key. Saving the same panel twice must not create
	// a second provider subscription or a second row. Local display-label edits
	// do not require re-registering the remote subscription.
	var existing models.IntegrationTrigger
	hasExisting := false
	err := h.db.DB.Where("workflow_id = ? AND node_id = ?", wf.ID.String(), req.NodeID).
		First(&existing).Error
	switch {
	case err == nil:
		hasExisting = true
	case errors.Is(err, gorm.ErrRecordNotFound):
	case err != nil:
		slog.ErrorContext(ctx, "could not look up integration trigger", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save the trigger"})
		return
	}
	if hasExisting && sameTriggerSpec(&existing, req) {
		if existing.ResourceLabel != req.ResourceLabel {
			if err := h.db.DB.Model(&existing).Update("resource_label", req.ResourceLabel).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save the trigger"})
				return
			}
			existing.ResourceLabel = req.ResourceLabel
		}
		c.JSON(http.StatusOK, existing)
		return
	}

	userID := currentUserID(c)
	token, workspace := FreshAccessTokenForOrg(h.db.DB, wf.OrganizationID, userID, req.Provider)
	if token == "" {
		// The honest failure. Registering needs the user's own credential, and
		// saying which account to connect beats a 500 from the provider.
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "connect your " + req.Provider + " account first",
			"connect": req.Provider,
		})
		return
	}

	filters, _ := json.Marshal(req.Filters)
	t := models.IntegrationTrigger{BaseModel: models.BaseModel{ID: uuid.New()}}
	if hasExisting {
		// Reuse both the database identity and callback URL. A changed panel is a
		// replacement of this node's subscription, not a second subscription.
		t = existing
	}
	applyTriggerSpec(&t, wf, userID, req, models.JSONB(filters), adapter)

	conn := triggers.Conn{AccessToken: token, WorkspaceID: workspace}
	if p := triggers.GetPusher(req.Provider); p != nil {
		reg, err := p.Register(ctx, conn, &t)
		if err != nil {
			slog.WarnContext(ctx, "trigger registration failed", "provider", req.Provider,
				"event", req.Event, "error", err.Error())
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		t.RemoteID, t.Secret, t.ExpiresAt = reg.RemoteID, reg.Secret, reg.ExpiresAt
		// The installation this trigger belongs to, resolved once here. Every
		// later delivery is matched against it.
		t.ScopeID = reg.ScopeID
	} else {
		// Poll delivery: no registration, but the cursor has to start at "now" or
		// the first poll would replay the entire history of the mailbox.
		next := time.Now().UTC()
		t.NextPollAt = &next
	}

	var saveErr error
	if hasExisting {
		saveErr = h.db.DB.Save(&t).Error
	} else {
		saveErr = h.db.DB.Create(&t).Error
	}
	if saveErr != nil {
		// Provider state was created first because a per-trigger callback needs a
		// stable id. If persistence loses the race or fails, compensate by
		// removing only the newly-created, distinct remote subscription.
		rollbackTriggerRegistration(ctx, triggers.GetPusher(req.Provider), conn, &t, existingOrNil(hasExisting, &existing))
		if !hasExisting {
			var winner models.IntegrationTrigger
			if err := h.db.DB.Where("workflow_id = ? AND node_id = ?", wf.ID.String(), req.NodeID).
				First(&winner).Error; err == nil && sameTriggerSpec(&winner, req) {
				c.JSON(http.StatusOK, winner)
				return
			}
		}
		slog.ErrorContext(ctx, "could not persist integration trigger", "error", saveErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save the trigger"})
		return
	}

	// The replacement is now authoritative. Retire the old remote subscription
	// only when Register returned a distinct one; app-level registrations have
	// no RemoteID and intentionally need no cleanup.
	if hasExisting && existing.RemoteID != "" &&
		(existing.Provider != t.Provider || existing.RemoteID != t.RemoteID) {
		h.unregisterTrigger(ctx, &existing)
	}

	action := "created"
	if hasExisting {
		action = "updated"
	}
	slog.InfoContext(ctx, "trigger "+action, "trigger_id", t.ID.String(),
		"provider", t.Provider, "event", t.Event, "workflow_id", t.WorkflowID)
	c.JSON(http.StatusOK, t)
}

func validateIntegrationTriggerNode(wf *models.Workflow, nodeID string) error {
	if nodeID == "" {
		return errors.New("node_id is required")
	}
	var nodes []executor.WorkflowASTNode
	if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
		return fmt.Errorf("workflow nodes are invalid: %w", err)
	}
	for _, node := range nodes {
		if node.ID != nodeID {
			continue
		}
		if node.Data.NodeType != executor.NodeTypeIntegrationTrigger {
			return errors.New("node_id must identify an integrationTrigger node")
		}
		return nil
	}
	return errors.New("node_id does not exist in this workflow")
}

func sameTriggerSpec(t *models.IntegrationTrigger, req createTriggerRequest) bool {
	if t.Provider != req.Provider || t.Event != req.Event || t.ResourceID != req.ResourceID {
		return false
	}
	var stored map[string]string
	if len(t.Filters) > 0 && string(t.Filters) != "null" {
		if err := json.Unmarshal(t.Filters, &stored); err != nil {
			return false
		}
	}
	if stored == nil {
		stored = map[string]string{}
	}
	if len(stored) != len(req.Filters) {
		return false
	}
	for key, value := range req.Filters {
		storedValue, exists := stored[key]
		if !exists || storedValue != value {
			return false
		}
	}
	return true
}

func applyTriggerSpec(t *models.IntegrationTrigger, wf *models.Workflow, userID string,
	req createTriggerRequest, filters models.JSONB, adapter triggers.Adapter) {
	t.OrganizationID = wf.OrganizationID
	t.UserID = userID
	t.WorkflowID = wf.ID.String()
	t.NodeID = req.NodeID
	t.Provider = req.Provider
	t.Event = req.Event
	t.ResourceID = req.ResourceID
	t.ResourceLabel = req.ResourceLabel
	t.Filters = filters
	t.Delivery = string(adapter.Delivery())
	t.Enabled = true
	if t.MaxRunsPerHour == 0 {
		t.MaxRunsPerHour = 60
	}
	if t.PollIntervalSeconds == 0 {
		t.PollIntervalSeconds = 60
	}

	// Provider-derived state belongs to the previous specification and must not
	// leak into a replacement while Register/Poll establishes the new state.
	t.ScopeID = ""
	t.RemoteID = ""
	t.Secret = ""
	t.ExpiresAt = nil
	t.Cursor = ""
	t.NextPollAt = nil
	t.LastPolledAt = nil
	t.LastEventAt = nil
	t.ConsecutiveFailures = 0
	t.LastError = ""
}

func existingOrNil(ok bool, t *models.IntegrationTrigger) *models.IntegrationTrigger {
	if !ok {
		return nil
	}
	return t
}

func rollbackTriggerRegistration(ctx context.Context, p triggers.Pusher, conn triggers.Conn,
	candidate, previous *models.IntegrationTrigger) {
	if p == nil || candidate.RemoteID == "" {
		return
	}
	if previous != nil && previous.Provider == candidate.Provider && previous.RemoteID == candidate.RemoteID {
		return
	}
	if err := p.Unregister(ctx, conn, candidate); err != nil {
		slog.WarnContext(ctx, "could not roll back trigger registration",
			"provider", candidate.Provider, "remote_id", candidate.RemoteID, "error", err.Error())
	}
}

// DELETE /api/triggers/:id
func (h *WorkflowHandler) DeleteTrigger(c *gin.Context) {
	ctx := c.Request.Context()
	var t models.IntegrationTrigger
	if err := h.orgScope(c).First(&t, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "trigger not found"})
		return
	}
	h.unregisterTrigger(ctx, &t)
	h.db.DB.Delete(&t)
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// unregisterTrigger removes the hook at the provider, best effort.
//
// A failure here is logged and swallowed on purpose: the user asked to delete
// the trigger, and refusing because GitHub is down would leave them with a
// trigger they cannot get rid of. The cost is an orphaned hook at the provider,
// which the inbound handler already tolerates — it acknowledges deliveries for
// triggers that no longer exist rather than erroring.
func (h *WorkflowHandler) unregisterTrigger(ctx context.Context, t *models.IntegrationTrigger) {
	p := triggers.GetPusher(t.Provider)
	if p == nil || t.RemoteID == "" {
		return
	}
	token, workspace := FreshAccessTokenForOrg(h.db.DB, t.OrganizationID, t.UserID, t.Provider)
	if token == "" {
		slog.WarnContext(ctx, "cannot unregister trigger: no credential",
			"trigger_id", t.ID.String(), "provider", t.Provider)
		return
	}
	if err := p.Unregister(ctx, triggers.Conn{AccessToken: token, WorkspaceID: workspace}, t); err != nil {
		slog.WarnContext(ctx, "could not remove the hook at the provider",
			"trigger_id", t.ID.String(), "provider", t.Provider, "error", err.Error())
	}
}
