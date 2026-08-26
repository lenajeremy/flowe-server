package handlers

import (
	"encoding/json"
	"testing"

	"workflow-ai/server/internal/executor"
)

// The path overlay reads this payload straight back, so its shape is a contract
// with the client, not an implementation detail.
func TestRunGraphSnapshotKeepsNodesAndEdges(t *testing.T) {
	nodes := []executor.WorkflowASTNode{
		{ID: "a", Data: executor.FlowNodeData{NodeType: executor.NodeTypeTextInput, Label: "Start"}},
		{ID: "b", Data: executor.FlowNodeData{NodeType: executor.NodeTypeTextOutput, Label: "End"}},
	}
	edges := []executor.WorkflowASTEdge{{ID: "e1", Source: "a", Target: "b"}}

	var got struct {
		Nodes []executor.WorkflowASTNode `json:"nodes"`
		Edges []executor.WorkflowASTEdge `json:"edges"`
	}
	if err := json.Unmarshal(runGraph(nodes, edges), &got); err != nil {
		t.Fatalf("snapshot does not parse back: %v", err)
	}
	if len(got.Nodes) != 2 || got.Nodes[0].ID != "a" || got.Nodes[1].Data.Label != "End" {
		t.Fatalf("nodes did not survive the snapshot: %#v", got.Nodes)
	}
	if len(got.Edges) != 1 || got.Edges[0].ID != "e1" || got.Edges[0].Target != "b" {
		t.Fatalf("edges did not survive the snapshot: %#v", got.Edges)
	}
}
