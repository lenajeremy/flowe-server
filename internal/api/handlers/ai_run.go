package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"workflow-ai/server/config"
	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/hub"

	"github.com/gin-gonic/gin"
)

// The AI builder can execute the workflow it just built and read back what each
// node did, so it can debug instead of guessing. Guardrails: irreversible
// integration ops are refused, the run is capped in time, and a turn can only
// run a couple of times so a confused model can't loop on live side effects.

const (
	aiRunTimeout      = 90 * time.Second
	aiRunOutputCap    = 600 // per node, in the tool result
	aiRunsPerTurn     = 2
	aiRunMaxNodeCount = 40
)

// irreversibleOps are integration operations a test run must never fire on its
// own — money movement and destruction. Sends (email/message/issue) are allowed
// because otherwise there is nothing worth testing.
var irreversibleOps = []string{
	"delete", "remove", "archive", "trash", "cancel", "refund", "merge", "close",
	"revoke", "clear", "purge", "unsubscribe",
}

func destructiveNode(n executor.WorkflowASTNode) string {
	op := strings.ToLower(n.Data.IntegrationOp)
	if op == "" {
		return ""
	}
	for _, bad := range irreversibleOps {
		if strings.Contains(op, bad) {
			return fmt.Sprintf("%s.%s", n.Data.NodeType, n.Data.IntegrationOp)
		}
	}
	return ""
}

// astFromRequest reads the builder's working copy of the canvas — which
// execChatTool keeps current as create_workflow/update_workflow are called.
func astFromRequest(req *aiGenerateRequest) (executor.WorkflowAST, error) {
	var ast executor.WorkflowAST
	nodesJSON, err := json.Marshal(req.CurrentNodes)
	if err != nil {
		return ast, err
	}
	edgesJSON, err := json.Marshal(req.CurrentEdges)
	if err != nil {
		return ast, err
	}
	if err := json.Unmarshal(nodesJSON, &ast.Nodes); err != nil {
		return ast, err
	}
	if err := json.Unmarshal(edgesJSON, &ast.Edges); err != nil {
		return ast, err
	}
	ast.Version = "1.0"
	ast.Name = "AI test run"
	return ast, nil
}

type aiRunNodeResult struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Type   string `json:"type"`
	Status string `json:"status"` // ok | error | skipped
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// runWorkflowForAI executes the current canvas and returns a per-node report.
func (h *WorkflowHandler) runWorkflowForAI(c *gin.Context, req *aiGenerateRequest) string {
	uid := auth.UserID(c)

	if req.aiRunCount >= aiRunsPerTurn {
		return fmt.Sprintf(`{"error":"you have already test-run this workflow %d times in this turn — stop running it and either fix the problem from the results you have or tell the user what is wrong"}`, req.aiRunCount)
	}
	req.aiRunCount++

	// Same per-user cap the manual Run button obeys.
	if !auth.Allow(c.Request.Context(), h.redis, "rl:run:"+uid, 60, time.Minute) {
		return `{"error":"too many runs in the last minute — tell the user to try again shortly"}`
	}

	ast, err := astFromRequest(req)
	if err != nil {
		return `{"error":"could not read the current workflow"}`
	}
	if len(ast.Nodes) == 0 {
		return `{"error":"there is nothing on the canvas to run yet"}`
	}
	if len(ast.Nodes) > aiRunMaxNodeCount {
		return fmt.Sprintf(`{"error":"this workflow has %d nodes, too many to test-run automatically — ask the user to run it"}`, len(ast.Nodes))
	}

	// Refuse to fire anything irreversible without a human.
	var blocked []string
	for _, n := range ast.Nodes {
		if d := destructiveNode(n); d != "" {
			blocked = append(blocked, d)
		}
	}
	if len(blocked) > 0 {
		out, _ := json.Marshal(map[string]any{
			"error":   "refused to run: this workflow performs irreversible operations",
			"ops":     blocked,
			"message": "Do not try again. Explain to the user what these steps would do and let them run it themselves with the Run button.",
		})
		return string(out)
	}

	// Persist the run like any other so it shows up in history, and publish it so
	// the open canvas attaches and the user sees exactly what the AI ran.
	run := &models.WorkflowRun{
		UserID:       uid,
		WorkflowID:   req.WorkflowID,
		WorkflowName: ast.Name,
		Status:       models.RunStatusRunning,
	}
	if err := h.db.DB.Create(run).Error; err != nil {
		slog.ErrorContext(c.Request.Context(), "failed to persist AI test run", "error", err)
	}
	runID := run.ID.String()
	if req.WorkflowID != "" {
		hub.Workflow.Publish(req.WorkflowID, runID)
	}

	keys := executor.APIKeys{
		Anthropic: config.GetEnv("ANTHROPIC_API_KEY"),
		OpenAI:    config.GetEnv("OPENAI_API_KEY"),
		Brave:     config.GetEnv("BRAVE_API_KEY"),
		Jina:      config.GetEnv("JINA_API_KEY"),
	}

	// Bounded: an approval node with no timeout would otherwise wait forever.
	ctx, cancel := context.WithTimeout(executor.WithTrigger(context.WithoutCancel(c.Request.Context()), "ai_test"), aiRunTimeout)
	defer cancel()

	var events []executor.ExecutionEvent
	finalStatus := models.RunStatusCompleted
	var runErr string

	executor.RunWorkflow(ctx, ast, keys, runID, uid, func(ev executor.ExecutionEvent) {
		hub.Global.Publish(runID, ev)
		events = append(events, ev)
		if ev.Type == executor.EventWorkflowError {
			finalStatus = models.RunStatusError
			runErr = ev.Message
		}
	})

	eventsJSON, _ := json.Marshal(events)
	updates := map[string]any{"status": finalStatus, "events": models.JSONB(eventsJSON)}
	if runErr != "" {
		updates["error_message"] = runErr
	}
	h.db.DB.Model(run).Updates(updates)
	hub.Global.ClearBuffer(runID)

	// Fold the event stream into one row per node, newest state winning.
	order := []string{}
	byNode := map[string]*aiRunNodeResult{}
	ensure := func(ev executor.ExecutionEvent) *aiRunNodeResult {
		id := ""
		if ev.NodeID != nil {
			id = *ev.NodeID
		}
		r, ok := byNode[id]
		if !ok {
			r = &aiRunNodeResult{ID: id, Status: "skipped"}
			if ev.NodeLabel != nil {
				r.Label = *ev.NodeLabel
			}
			if ev.NodeType != nil {
				r.Type = string(*ev.NodeType)
			}
			byNode[id] = r
			order = append(order, id)
		}
		return r
	}
	for _, ev := range events {
		if ev.NodeID == nil {
			continue
		}
		r := ensure(ev)
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
	results := make([]aiRunNodeResult, 0, len(order))
	for _, id := range order {
		results = append(results, *byNode[id])
	}

	slog.InfoContext(c.Request.Context(), "AI test run finished",
		"run_id", runID, "status", finalStatus, "nodes", len(results))

	payload := map[string]any{
		"status":   string(finalStatus),
		"run_id":   runID,
		"nodes":    results,
		"finished": true,
	}
	if runErr != "" {
		payload["error"] = runErr
	}
	if finalStatus == models.RunStatusCompleted {
		payload["message"] = "Every node ran. If an output is not what the user asked for, fix the node config and you may re-run once."
	} else {
		payload["message"] = "The run failed. Use the failing node's error to fix its configuration, then re-run to confirm. Do not guess."
	}
	out, _ := json.Marshal(payload)
	return string(out)
}

func truncateForAI(s string) string {
	if len(s) <= aiRunOutputCap {
		return s
	}
	return s[:aiRunOutputCap] + "… (truncated)"
}
