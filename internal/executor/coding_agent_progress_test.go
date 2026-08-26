package executor

import (
	"context"
	"testing"
	"time"

	"workflow-ai/server/internal/codingagent"
)

func TestCodingAgentProgressIsNotHumanApproval(t *testing.T) {
	startedAt := time.Now().Add(-time.Second)
	ctx := context.WithValue(context.Background(), workflowStartedAtCtxKey{}, startedAt)
	node := WorkflowASTNode{ID: "codex-1", Data: FlowNodeData{NodeType: NodeTypeCodingAgent, Label: "Fix tests"}}

	event, ok := codingAgentProgressExecutionEvent(ctx, node, "run-1", codingagent.StreamEvent{
		Type: "command_started", Message: "Codex started a command",
		Payload: map[string]any{"kind": "command", "command": "go test ./..."},
	})
	if !ok {
		t.Fatal("coding agent progress event was discarded")
	}
	if event.Type != EventNodeProgress {
		t.Fatalf("event type = %q, want %q", event.Type, EventNodeProgress)
	}
	if event.Type == EventNodeWaiting {
		t.Fatal("coding agent progress entered the human approval flow")
	}
	if event.Payload["activityType"] != "command_started" || event.Payload["command"] != "go test ./..." {
		t.Fatalf("structured command payload was lost: %#v", event.Payload)
	}
	if event.Timestamp < 900 || event.Timestamp > 2000 {
		t.Fatalf("timestamp = %d, want run-relative milliseconds", event.Timestamp)
	}
}

func TestCodingAgentProgressKeepsPayloadOnlyEvents(t *testing.T) {
	node := WorkflowASTNode{ID: "codex-1", Data: FlowNodeData{NodeType: NodeTypeCodingAgent, Label: "Fix tests"}}
	event, ok := codingAgentProgressExecutionEvent(context.Background(), node, "run-1", codingagent.StreamEvent{
		Type: "execution_started", Payload: map[string]any{"executionId": "exec-1"},
	})
	if !ok || event.Payload["executionId"] != "exec-1" {
		t.Fatalf("payload-only event was lost: ok=%v payload=%#v", ok, event.Payload)
	}
}
