package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/hub"

	"github.com/google/uuid"
)

// Waking a workflow, once.
//
// A schedule firing, a webhook arriving, and an integration trigger matching are
// the same act: load the workflow, ask billing for permission, record a run,
// execute, persist what happened. That sequence lived in two places already and
// a third was about to be written — and the copies had drifted (only one of them
// recorded the refusal on the run row). Three copies of the credit-admission
// path is how one of them silently stops charging.
//
// Split in two on purpose: admission has to finish before an HTTP handler can
// answer, while execution takes as long as the workflow takes. The webhook
// endpoint needs a run id in its response and a 402 when credit has run out;
// the scheduler needs neither. One shared body, two call shapes.

// pendingRun is an admitted, recorded run that has not started yet.
type pendingRun struct {
	run      models.WorkflowRun
	workflow models.Workflow
	ast      executor.WorkflowAST
	res      *billing.Reservation
	trigger  string
	entry    string
}

// RunID is the id assigned before execution, so a caller can hand it back to
// whoever asked before the work is done.
func (p *pendingRun) RunID() string { return p.run.ID.String() }

// admitRun loads a workflow, asks billing, and records the run row.
//
// A refusal is still written to the database with its reason: a scheduled agent
// that stops because the allowance ran out has to be visibly stopped, not
// indistinguishable from one that was never triggered.
func (h *WorkflowHandler) admitRun(ctx context.Context, workflowID, trigger string,
	inject func(nodes []executor.WorkflowASTNode) string) (*pendingRun, error) {

	var workflow models.Workflow
	if err := h.db.DB.First(&workflow, "id = ?", workflowID).Error; err != nil {
		return nil, fmt.Errorf("workflow not found")
	}

	var nodes []executor.WorkflowASTNode
	var edges []executor.WorkflowASTEdge
	// Which trigger woke this run, so its own graph executes rather than
	// whichever graph on the canvas happens to be largest.
	entryNodeID := ""
	json.Unmarshal(workflow.Nodes, &nodes)
	json.Unmarshal(workflow.Edges, &edges)
	if inject != nil {
		entryNodeID = inject(nodes)
	}

	runID := uuid.New()
	res, admitErr := h.bill.AdmitRun(workflow.OrganizationID, workflow.UserID, runID.String())
	run := models.WorkflowRun{
		BaseModel:      models.BaseModel{ID: runID},
		UserID:         workflow.UserID,
		OrganizationID: workflow.OrganizationID,
		WorkflowID:     workflowID,
		WorkflowName:   workflow.Name,
		Status:         models.RunStatusRunning,
		Graph:          runGraph(nodes, edges),
	}
	if admitErr != nil {
		run.Status = models.RunStatusError
		run.ErrorMessage = admitErr.Error()
		h.db.DB.Create(&run)
		slog.WarnContext(ctx, "run refused", "workflow_id", workflowID,
			"trigger", trigger, "reason", admitErr.Error(), "run_id", run.ID.String())
		return nil, admitErr
	}
	h.db.DB.Create(&run)

	return &pendingRun{
		run:      run,
		workflow: workflow,
		ast:      executor.WorkflowAST{Version: "1.0", Name: workflow.Name, Nodes: nodes, Edges: edges},
		res:      res,
		trigger:  trigger,
		entry:    entryNodeID,
	}, nil
}

// executeRun runs an admitted workflow to completion and stores the result.
// Blocking — callers that owe someone an HTTP response run it in a goroutine.
func (h *WorkflowHandler) executeRun(ctx context.Context, p *pendingRun) models.RunStatus {
	defer h.bill.Finish(p.res)

	runID := p.RunID()
	keys := executor.KeysFromEnv()

	// Tell any open canvas for this workflow that a run started, so it can attach
	// its event stream before the first node finishes.
	hub.Workflow.Publish(p.workflow.ID.String(), runID)

	var events []executor.ExecutionEvent
	start := time.Now()
	finalStatus := models.RunStatusCompleted
	runCtx := p.res.Context(executor.WithWorkflowID(ctx, p.workflow.ID.String()), runID)
	executor.RunWorkflow(executor.WithTrigger(runCtx, p.trigger), p.ast, keys, runID,
		p.workflow.UserID, p.workflow.OrganizationID, func(ev executor.ExecutionEvent) {
			ev.Timestamp = time.Since(start).Milliseconds()
			events = append(events, ev)
			hub.Global.Publish(runID, ev)
			if ev.Type == executor.EventWorkflowError {
				finalStatus = models.RunStatusError
			}
		}, executor.RunOptions{EntryNodeID: p.entry})

	eventsJSON, _ := json.Marshal(events)
	h.db.DB.Model(&p.run).Updates(map[string]interface{}{
		"status": finalStatus,
		"events": models.JSONB(eventsJSON),
	})
	hub.Global.ClearBuffer(runID)
	slog.InfoContext(ctx, "run finished", "run_id", runID, "trigger", p.trigger,
		"status", finalStatus, "event_count", len(events))
	return finalStatus
}

// injectInto returns an inject function that hands a payload to trigger nodes of
// one type. Matching on node id when there is one keeps a canvas with several
// triggers from waking all of them with the same event.
func injectInto(nodeType executor.NodeType, nodeID, payload string) func([]executor.WorkflowASTNode) string {
	return func(nodes []executor.WorkflowASTNode) string {
		entry := ""
		for i := range nodes {
			// Both fields carry the node type and the two existing trigger paths
			// disagree about which one they read, so match on either — a mismatch
			// here would inject nothing and look like a payload that went missing.
			if nodes[i].Data.NodeType != nodeType && nodes[i].Type != nodeType {
				continue
			}
			if nodeID != "" && nodes[i].ID != nodeID {
				continue
			}
			nodes[i].Data.DefaultValue = &payload
		}
		if entry == "" {
			// The first matching trigger is the one whose graph runs. A canvas
			// with several triggers of one type is ambiguous by construction;
			// picking the first keeps it deterministic rather than arbitrary.
			for i := range nodes {
				if (nodes[i].Data.NodeType == nodeType || nodes[i].Type == nodeType) &&
					(nodeID == "" || nodes[i].ID == nodeID) {
					entry = nodes[i].ID
					break
				}
			}
		}
		return entry
	}
}

// runGraph captures the nodes and edges a run is about to execute, so its path
// can be drawn later against the shape that actually ran rather than whatever
// the workflow has since been edited into. Taken after any trigger injection,
// which is the point: the payload a trigger wrote into a node is part of what
// ran. Marshalling cannot fail for values that were just unmarshalled, and a
// snapshot is not worth failing a run over, so an error yields no snapshot.
func runGraph(nodes []executor.WorkflowASTNode, edges []executor.WorkflowASTEdge) models.JSONB {
	raw, err := json.Marshal(map[string]any{"nodes": nodes, "edges": edges})
	if err != nil {
		return nil
	}
	return models.JSONB(raw)
}
