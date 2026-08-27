package executor

import (
	"context"
	"encoding/json"
	"testing"

	"workflow-ai/server/internal/codingagent"
	"workflow-ai/server/internal/database/models"
)

func TestRunKeepsDisconnectedAgentToolsInSnapshotWithoutExecutingThem(t *testing.T) {
	previous := CodingAgentRun
	t.Cleanup(func() { CodingAgentRun = previous })

	var captured codingagent.SubmitRequest
	CodingAgentRun = func(_ context.Context, req codingagent.SubmitRequest, _ func(codingagent.StreamEvent)) (string, string, []byte, string, string, error) {
		captured = req
		return "job", string(models.CodingAgentJobSucceeded), []byte(`{}`), "done", "", nil
	}
	grant := codingagent.ToolGrant{NodeType: string(NodeTypeGitlab), NodeIDs: []string{"gitlab-mr"}, AllowedOperations: []string{"create_merge_request"}}
	agent := WorkflowASTNode{ID: "agent", Data: FlowNodeData{
		NodeType: NodeTypeCodingAgent, CodingAgentRuntime: codingagent.RuntimeCodex,
		CodingAgentTask: "open the merge request", CodingAgentToolGrants: []codingagent.ToolGrant{grant},
	}}
	output := WorkflowASTNode{ID: "output", Data: FlowNodeData{NodeType: NodeTypeTextOutput}}
	gitlab := WorkflowASTNode{ID: "gitlab-mr", Data: FlowNodeData{NodeType: NodeTypeGitlab, IntegrationOp: "create_merge_request"}}
	full := WorkflowAST{Version: "1.0", Name: "full", Nodes: []WorkflowASTNode{agent, output, gitlab}, Edges: []WorkflowASTEdge{{Source: "agent", Target: "output"}}}
	execution := WorkflowAST{Version: "1.0", Name: "execution", Nodes: []WorkflowASTNode{agent, output}, Edges: []WorkflowASTEdge{{Source: "agent", Target: "output"}}}

	RunWorkflow(context.Background(), execution, APIKeys{}, "run", "owner", "org", func(ExecutionEvent) {}, RunOptions{ToolWorkflow: &full})
	var frozen WorkflowAST
	if err := json.Unmarshal(captured.ToolWorkflow, &frozen); err != nil {
		t.Fatal(err)
	}
	if len(frozen.Nodes) != 3 || frozen.Nodes[2].ID != "gitlab-mr" {
		t.Fatalf("frozen tool snapshot = %#v", frozen.Nodes)
	}
}

func TestNormalRunDoesNotExecuteDisconnectedAgentToolNodes(t *testing.T) {
	workflow := WorkflowAST{Nodes: []WorkflowASTNode{
		{ID: "agent", Data: FlowNodeData{NodeType: NodeTypeCodingAgent, CodingAgentToolGrants: []codingagent.ToolGrant{{NodeType: "gitlab", NodeIDs: []string{"gitlab-write"}}}}},
		{ID: "gitlab-write", Data: FlowNodeData{NodeType: NodeTypeGitlab, IntegrationOp: "create_merge_request"}},
	}}
	filtered := workflowWithoutCodingAgentTools(workflow, "")
	if len(filtered.Nodes) != 1 || filtered.Nodes[0].ID != "agent" {
		t.Fatalf("tool-only node remained executable: %#v", filtered.Nodes)
	}
	if tested := workflowWithoutCodingAgentTools(workflow, "gitlab-write"); len(tested.Nodes) != 2 {
		t.Fatal("single-node test removed its explicit target")
	}
}
