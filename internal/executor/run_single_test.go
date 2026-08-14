package executor

import (
	"context"
	"strings"
	"testing"
)

func TestRunWorkflowOnlyNodeUsesCachedUpstreamOutput(t *testing.T) {
	source := WorkflowASTNode{ID: "source", Data: FlowNodeData{
		NodeType: NodeTypeTextInput,
		Label:    "Source",
	}}
	target := WorkflowASTNode{ID: "target", Data: FlowNodeData{
		NodeType: NodeTypeTextOutput,
		Label:    "Result",
	}}
	workflow := WorkflowAST{
		Name:  "Test",
		Nodes: []WorkflowASTNode{source, target},
		Edges: []WorkflowASTEdge{{ID: "edge", Source: source.ID, Target: target.ID}},
	}

	var events []ExecutionEvent
	RunWorkflow(context.Background(), workflow, APIKeys{}, "run", "owner", "org", func(event ExecutionEvent) {
		events = append(events, event)
	}, RunOptions{
		OnlyNodeID:     target.ID,
		InitialOutputs: map[string]string{source.ID: "cached result"},
	})

	started := 0
	foundOutput := false
	for _, event := range events {
		if event.Type == EventNodeStarted {
			started++
			if event.NodeID == nil || *event.NodeID != target.ID {
				t.Fatalf("started unexpected node: %#v", event.NodeID)
			}
		}
		if event.Type == EventNodeOutput && event.Output != nil && *event.Output == "cached result" {
			foundOutput = true
		}
	}
	if started != 1 {
		t.Fatalf("started %d nodes, want exactly the target", started)
	}
	if !foundOutput {
		t.Fatal("target did not receive the cached upstream output")
	}
}

func TestRunWorkflowOnlyNodeBlocksWithoutRequiredOutput(t *testing.T) {
	source := WorkflowASTNode{ID: "source", Data: FlowNodeData{NodeType: NodeTypeTextInput, Label: "Source"}}
	target := WorkflowASTNode{ID: "target", Data: FlowNodeData{NodeType: NodeTypeTextOutput, Label: "Result"}}
	workflow := WorkflowAST{
		Name:  "Test",
		Nodes: []WorkflowASTNode{source, target},
		Edges: []WorkflowASTEdge{{ID: "edge", Source: source.ID, Target: target.ID}},
	}

	var events []ExecutionEvent
	RunWorkflow(context.Background(), workflow, APIKeys{}, "run", "owner", "org", func(event ExecutionEvent) {
		events = append(events, event)
	}, RunOptions{OnlyNodeID: target.ID})

	for _, event := range events {
		if event.Type == EventNodeStarted {
			t.Fatal("node started even though its required upstream output was unavailable")
		}
		if event.Type == EventWorkflowError && strings.Contains(event.Message, "Run this graph first") {
			return
		}
	}
	t.Fatal("missing upstream output did not produce the expected workflow error")
}

func TestRequiredOutputNodeIDsIncludesTemplates(t *testing.T) {
	prompt := "Summarize {{source.output.body}}"
	node := WorkflowASTNode{ID: "target", Data: FlowNodeData{
		NodeType:   NodeTypeLLM,
		Label:      "Summarize",
		UserPrompt: &prompt,
	}}
	required := requiredOutputNodeIDs(node, nil)
	if len(required) != 1 || required[0] != "source" {
		t.Fatalf("required outputs = %v, want [source]", required)
	}
}
