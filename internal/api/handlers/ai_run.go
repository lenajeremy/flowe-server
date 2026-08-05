package handlers

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"

	"github.com/gin-gonic/gin"
)

// The AI builder debugs from run history rather than by executing anything: it
// lists recent runs and reads the per-node events of whichever one failed. That
// keeps the builder side-effect free and, more usefully, shows it real failures
// from scheduled runs it could never have reproduced on demand.

const (
	aiRunOutputCap = 600 // per-node output in a tool result
	aiRunListLimit = 15
	aiRunNodeLimit = 60 // nodes reported per run (loops can emit many)
)

type aiRunSummary struct {
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at"`
	Error     string `json:"error,omitempty"`
	FailedAt  string `json:"failed_at,omitempty"`
	Nodes     int    `json:"nodes_touched"`
}

// listRunsForAI backs the list_runs tool: the workflow's recent runs, newest
// first, each tagged with the node that failed so the model knows what to open.
func (h *WorkflowHandler) listRunsForAI(c *gin.Context, workflowID string) string {
	q := h.orgScope(c).Model(&models.WorkflowRun{})
	scope := "this workflow"
	if workflowID != "" {
		q = q.Where("workflow_id = ?", workflowID)
	} else {
		// Unsaved canvas — fall back to the account's recent runs so the model
		// still has something to learn from.
		scope = "your account (this workflow has not been saved yet)"
	}

	var runs []models.WorkflowRun
	if err := q.Order("created_at DESC").Limit(aiRunListLimit).Find(&runs).Error; err != nil {
		return `{"error":"could not read run history"}`
	}
	if len(runs) == 0 {
		return `{"runs":[],"message":"No runs recorded yet. Ask the user to hit Run (or wait for the schedule to fire), then check again — do not guess at causes."}`
	}

	out := make([]aiRunSummary, 0, len(runs))
	for _, r := range runs {
		s := aiRunSummary{
			RunID:     r.ID.String(),
			Status:    string(r.Status),
			StartedAt: r.CreatedAt.UTC().Format(time.RFC3339),
			Error:     r.ErrorMessage,
		}
		var events []executor.ExecutionEvent
		if len(r.Events) > 0 && json.Unmarshal(r.Events, &events) == nil {
			seen := map[string]bool{}
			for _, ev := range events {
				if ev.NodeID != nil && !seen[*ev.NodeID] {
					seen[*ev.NodeID] = true
				}
				if ev.Type == executor.EventNodeError && ev.NodeLabel != nil && s.FailedAt == "" {
					s.FailedAt = *ev.NodeLabel
				}
			}
			s.Nodes = len(seen)
		}
		out = append(out, s)
	}

	b, _ := json.Marshal(map[string]any{
		"runs":    out,
		"scope":   scope,
		"message": "Call get_run_logs with the run_id of the run you want to understand — prefer the most recent failing one.",
	})
	return string(b)
}

type aiRunNodeResult struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Type   string `json:"type"`
	Status string `json:"status"` // ok | error | running
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// runLogsForAI backs the get_run_logs tool: one row per node for a single run,
// with the output it produced or the error that stopped it.
func (h *WorkflowHandler) runLogsForAI(c *gin.Context, input any) string {
	m, _ := input.(map[string]any)
	runID, _ := m["run_id"].(string)
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return `{"error":"run_id is required — get one from list_runs"}`
	}

	var run models.WorkflowRun
	if err := h.orgScope(c).First(&run, "id = ?", runID).Error; err != nil {
		return `{"error":"run not found"}`
	}

	var events []executor.ExecutionEvent
	if len(run.Events) > 0 {
		_ = json.Unmarshal(run.Events, &events)
	}
	if len(events) == 0 {
		return fmt.Sprintf(`{"run_id":%q,"status":%q,"nodes":[],"message":"This run recorded no node events%s"}`,
			runID, run.Status,
			map[bool]string{true: " — it is still running, so check again shortly.", false: "."}[run.Status == models.RunStatusRunning])
	}

	order := []string{}
	byNode := map[string]*aiRunNodeResult{}
	for _, ev := range events {
		if ev.NodeID == nil {
			continue
		}
		id := *ev.NodeID
		r, ok := byNode[id]
		if !ok {
			r = &aiRunNodeResult{ID: id, Status: "running"}
			if ev.NodeLabel != nil {
				r.Label = *ev.NodeLabel
			}
			if ev.NodeType != nil {
				r.Type = string(*ev.NodeType)
			}
			byNode[id] = r
			order = append(order, id)
		}
		switch ev.Type {
		case executor.EventNodeOutput:
			if ev.Output != nil {
				r.Output = truncateForAI(*ev.Output)
			}
		case executor.EventNodeCompleted:
			if r.Status != "error" {
				r.Status = "ok"
			}
		case executor.EventNodeError:
			r.Status = "error"
			r.Error = ev.Message
		}
	}

	if len(order) > aiRunNodeLimit {
		order = order[:aiRunNodeLimit]
	}
	results := make([]aiRunNodeResult, 0, len(order))
	for _, id := range order {
		results = append(results, *byNode[id])
	}

	payload := map[string]any{
		"run_id":     runID,
		"status":     string(run.Status),
		"started_at": run.CreatedAt.UTC().Format(time.RFC3339),
		"nodes":      results,
	}
	if run.ErrorMessage != "" {
		payload["error"] = run.ErrorMessage
	}
	if run.Status == models.RunStatusError {
		payload["message"] = "Fix the node whose status is error, using its exact error text. If the error names a missing value the user must supply (a recipient, a resource id), say which field they need to fill instead of inventing one."
	} else {
		payload["message"] = "This run succeeded. If the result still isn't what the user wanted, compare each node's output against their intent and adjust the node that produced the wrong value."
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

func truncateForAI(s string) string {
	if len(s) <= aiRunOutputCap {
		return s
	}
	return s[:aiRunOutputCap] + "… (truncated)"
}
