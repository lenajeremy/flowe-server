package handlers

import (
	"encoding/json"
	"net/http"
	"sync"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Data-store proposals pause the AI builder mid-turn. create_data_store
// registers a pending proposal, streams it to the chat UI, and blocks the tool
// loop until the user accepts or rejects — so the model cannot keep building
// against a store that does not exist yet. On accept the store is created here
// (the user's click is the consent) and the id is handed back as the tool
// result, letting the model wire it up in the same turn.
//
// Like the executor's humanApproval channels, this is in-process state: the
// resolve request must land on the instance holding the paused stream (single
// replica today).

type storeProposalSpec struct {
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Scope  string          `json:"scope"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Reason string          `json:"reason,omitempty"`
}

type proposalOutcome struct {
	Action  string // approved | rejected
	StoreID string
	Note    string
}

type pendingProposal struct {
	spec       storeProposalSpec
	userID     string
	orgID      string
	workflowID string
	ch         chan proposalOutcome
}

var (
	pendingProposals   = map[string]*pendingProposal{}
	pendingProposalsMu sync.Mutex
)

func registerProposal(userID, orgID, workflowID string, spec storeProposalSpec) (string, chan proposalOutcome) {
	id := uuid.NewString()
	ch := make(chan proposalOutcome, 1)
	pendingProposalsMu.Lock()
	pendingProposals[id] = &pendingProposal{spec: spec, userID: userID, orgID: orgID,
		workflowID: workflowID, ch: ch}
	pendingProposalsMu.Unlock()
	return id, ch
}

func clearProposal(id string) {
	pendingProposalsMu.Lock()
	delete(pendingProposals, id)
	pendingProposalsMu.Unlock()
}

// POST /api/ai/data-store-proposals/:id/resolve — {action: accept|reject, note?}
// Accepting creates the store and releases the paused builder turn.
func (h *WorkflowHandler) ResolveDataStoreProposal(c *gin.Context) {
	id := c.Param("id")
	var body struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.Action != "accept" && body.Action != "reject" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be accept or reject"})
		return
	}

	pendingProposalsMu.Lock()
	p, ok := pendingProposals[id]
	pendingProposalsMu.Unlock()
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "this proposal is no longer pending"})
		return
	}
	if p.userID != auth.UserID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "this proposal is no longer pending"})
		return
	}

	if body.Action == "reject" {
		p.ch <- proposalOutcome{Action: "rejected", Note: body.Note}
		clearProposal(id)
		c.JSON(http.StatusOK, gin.H{"status": "rejected"})
		return
	}

	// Accept — create the store from the proposed spec, with the same
	// validation the REST create path uses.
	spec := p.spec
	if !validKinds[spec.Kind] || !validScopes[spec.Scope] || spec.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the proposed store is not valid"})
		return
	}
	if err := executor.ValidateSchema(spec.Schema); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	workflowID := p.workflowID
	if spec.Scope == "account" {
		workflowID = ""
	} else if workflowID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "save the workflow first — run and workflow stores belong to a saved workflow"})
		return
	}

	store := models.DataStore{
		UserID:         p.userID,
		OrganizationID: p.orgID,
		WorkflowID:     workflowID,
		Name:           spec.Name,
		Kind:           spec.Kind,
		Scope:          spec.Scope,
		Schema:         models.JSONB(spec.Schema),
	}
	if err := h.db.DB.Create(&store).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "a store with that name already exists in this scope"})
		return
	}

	p.ch <- proposalOutcome{Action: "approved", StoreID: store.ID.String()}
	clearProposal(id)
	c.JSON(http.StatusOK, store)
}
