package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// run drives a workflow to completion and returns everything it emitted.
func runAll(t *testing.T, wf WorkflowAST, runID string) []ExecutionEvent {
	t.Helper()
	var events []ExecutionEvent
	RunWorkflow(context.Background(), wf, APIKeys{}, runID, "owner", "org",
		func(ev ExecutionEvent) { events = append(events, ev) })
	return events
}

func eventsOfType(events []ExecutionEvent, t ExecutionEventType) []ExecutionEvent {
	var out []ExecutionEvent
	for _, ev := range events {
		if ev.Type == t {
			out = append(out, ev)
		}
	}
	return out
}

func TestLoopEmitsPerIterationEvents(t *testing.T) {
	items := `["alpha","beta","gamma"]`
	source := WorkflowASTNode{ID: "src", Data: FlowNodeData{
		NodeType: NodeTypeTextInput, Label: "Items", DefaultValue: &items,
	}}
	loop := WorkflowASTNode{ID: "loop", Data: FlowNodeData{
		NodeType: NodeTypeLoop, Label: "For each",
	}}
	body := WorkflowASTNode{ID: "body", Data: FlowNodeData{
		NodeType: NodeTypeTextOutput, Label: "Handle item",
	}}
	wf := WorkflowAST{Name: "Loop", Nodes: []WorkflowASTNode{source, loop, body}, Edges: []WorkflowASTEdge{
		{ID: "e1", Source: "src", Target: "loop"},
		{ID: "e2", Source: "loop", Target: "body"},
	}}

	events := runAll(t, wf, "run-loop")

	started := eventsOfType(events, EventIterationStarted)
	done := eventsOfType(events, EventIterationCompleted)
	if len(started) != 3 || len(done) != 3 {
		t.Fatalf("iteration events = %d started / %d completed, want 3 each", len(started), len(done))
	}
	for i, ev := range started {
		if ev.Iteration == nil {
			t.Fatalf("iteration_started %d carries no reference", i)
		}
		if ev.Iteration.Index != i || ev.Iteration.Total != 3 {
			t.Fatalf("iteration %d = index %d of %d, want index %d of 3", i, ev.Iteration.Index, ev.Iteration.Total, i)
		}
		if ev.Iteration.LoopNodeID != "loop" {
			t.Fatalf("iteration %d attributed to %q, want the loop node", i, ev.Iteration.LoopNodeID)
		}
	}
	if got := started[1].Iteration.ItemPreview; got != "beta" {
		t.Fatalf("second item preview = %q, want the item itself", got)
	}
	for _, ev := range done {
		if ev.Status != "ok" {
			t.Fatalf("iteration status = %q, want ok", ev.Status)
		}
	}

	// Body events must be attributable to their pass without parsing the label.
	var bodyOutputs int
	for _, ev := range events {
		if ev.Type != EventNodeOutput || ev.NodeID == nil || *ev.NodeID != "body" {
			continue
		}
		bodyOutputs++
		if ev.Iteration == nil {
			t.Fatal("body node output carries no iteration reference")
		}
		if strings.Contains(ev.Message, "[") {
			t.Fatalf("body message %q still carries the old [n/total] prefix", ev.Message)
		}
	}
	if bodyOutputs != 3 {
		t.Fatalf("body ran %d times, want once per item", bodyOutputs)
	}
}

func TestApprovalGatingRecordsPathAndSkips(t *testing.T) {
	seed := "go"
	source := WorkflowASTNode{ID: "src", Data: FlowNodeData{
		NodeType: NodeTypeTextInput, Label: "Seed", DefaultValue: &seed,
	}}
	gate := WorkflowASTNode{ID: "gate", Data: FlowNodeData{
		NodeType: NodeTypeHumanApproval, Label: "Review", ApprovalTimeout: 5,
	}}
	yes := WorkflowASTNode{ID: "yes", Data: FlowNodeData{NodeType: NodeTypeTextOutput, Label: "Approved path"}}
	no := WorkflowASTNode{ID: "no", Data: FlowNodeData{NodeType: NodeTypeTextOutput, Label: "Rejected path"}}

	approved, rejected := "approved", "rejected"
	wf := WorkflowAST{Name: "Gate", Nodes: []WorkflowASTNode{source, gate, yes, no}, Edges: []WorkflowASTEdge{
		{ID: "e1", Source: "src", Target: "gate"},
		{ID: "e-yes", Source: "gate", Target: "yes", SourceHandle: &approved},
		{ID: "e-no", Source: "gate", Target: "no", SourceHandle: &rejected},
	}}

	// The node registers its channel only once it starts, so keep offering the
	// approval until it is accepted rather than racing a single attempt.
	go func() {
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if ResolveApproval("run-gate:gate", true) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	events := runAll(t, wf, "run-gate")

	var tookYes bool
	for _, ev := range eventsOfType(events, EventEdgeTaken) {
		if ev.EdgeID == "e-yes" {
			tookYes = true
			if ev.SourceHandle != "approved" {
				t.Fatalf("approved edge recorded handle %q", ev.SourceHandle)
			}
		}
		if ev.EdgeID == "e-no" {
			t.Fatal("recorded the rejected edge as taken")
		}
	}
	if !tookYes {
		t.Fatal("no edge_taken recorded for the approved path")
	}

	skips := eventsOfType(events, EventNodeSkipped)
	if len(skips) != 1 {
		t.Fatalf("skipped %d nodes, want only the rejected path: %#v", len(skips), skips)
	}
	if *skips[0].NodeID != "no" || skips[0].SkipReason != SkipBranchNotTaken {
		t.Fatalf("skip = node %q reason %q, want node \"no\" not-taken", *skips[0].NodeID, skips[0].SkipReason)
	}
}

func TestSkipsAfterAFailureReportNotReached(t *testing.T) {
	seed := "go"
	source := WorkflowASTNode{ID: "src", Data: FlowNodeData{
		NodeType: NodeTypeTextInput, Label: "Seed", DefaultValue: &seed,
	}}
	// A branch with no condition fails before it can call anything out.
	broken := WorkflowASTNode{ID: "broken", Data: FlowNodeData{
		NodeType: NodeTypeBranch, Label: "Decide",
	}}
	after := WorkflowASTNode{ID: "after", Data: FlowNodeData{NodeType: NodeTypeTextOutput, Label: "Downstream"}}

	wf := WorkflowAST{Name: "Fail", Nodes: []WorkflowASTNode{source, broken, after}, Edges: []WorkflowASTEdge{
		{ID: "e1", Source: "src", Target: "broken"},
		{ID: "e2", Source: "broken", Target: "after"},
	}}

	events := runAll(t, wf, "run-fail")

	skips := eventsOfType(events, EventNodeSkipped)
	var found bool
	for _, ev := range skips {
		if *ev.NodeID == "after" {
			found = true
			if ev.SkipReason != SkipNotReached {
				t.Fatalf("downstream of a failure reported %q, want not-reached", ev.SkipReason)
			}
		}
	}
	if !found {
		t.Fatalf("no skip recorded for the node after the failure: %#v", skips)
	}
}

func TestEventOutputIsCappedAndMarked(t *testing.T) {
	big := strings.Repeat("x", maxEventOutput+500)
	source := WorkflowASTNode{ID: "src", Data: FlowNodeData{
		NodeType: NodeTypeTextInput, Label: "Big", DefaultValue: &big,
	}}
	wf := WorkflowAST{Name: "Big", Nodes: []WorkflowASTNode{source}}

	var marked bool
	for _, ev := range runAll(t, wf, "run-big") {
		if ev.Type != EventNodeOutput || ev.Output == nil {
			continue
		}
		marked = true
		if !ev.OutputTruncated {
			t.Fatal("oversized output was clipped without being marked truncated")
		}
		if len(*ev.Output) > maxEventOutput+3 {
			t.Fatalf("output kept %d bytes, want the cap of %d", len(*ev.Output), maxEventOutput)
		}
	}
	if !marked {
		t.Fatal("no node output event was emitted")
	}
}
