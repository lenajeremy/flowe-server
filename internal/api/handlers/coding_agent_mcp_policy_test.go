package handlers

import (
	"encoding/json"
	"testing"

	"workflow-ai/server/internal/codingagent"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
)

func codingAgentPolicyTestAST() executor.WorkflowAST {
	return executor.WorkflowAST{Version: "1.0", Name: "Frozen", Nodes: []executor.WorkflowASTNode{
		{ID: "github", Data: executor.FlowNodeData{
			NodeType: executor.NodeTypeGithub, Label: "Ship change", IntegrationOp: "list_pull_requests",
			GithubRepo: "acme/widget", GithubBranch: "fernary/fix",
		}},
	}}
}

func TestLegacyCodingAgentNodeGrantNarrowsToPinnedReads(t *testing.T) {
	policy := safeLegacyCodingAgentPolicy(codingAgentPolicyTestAST(), []string{"github"})
	grant, ok := agentPolicyGrant(policy, codingAgentPolicyTestAST().Nodes[0])
	if !ok {
		t.Fatal("legacy GitHub node did not retain safe read access")
	}
	if len(grant.AllowedOverrideFields) != 0 {
		t.Fatalf("legacy grant unexpectedly made fields editable: %v", grant.AllowedOverrideFields)
	}
	for _, operation := range grant.AllowedOperations {
		if classifyAgentOperation(operation) != AgentEffectRead {
			t.Fatalf("legacy grant retained non-read operation %q", operation)
		}
	}
	for _, forbidden := range []string{"create_branch", "commit_files", "create_pull_request", "merge_pull_request"} {
		if stringSliceContains(grant.AllowedOperations, forbidden) {
			t.Fatalf("legacy grant broadened to %q", forbidden)
		}
	}
}

func TestCodingAgentToolsUseFrozenGraphAndExactPolicy(t *testing.T) {
	ast := codingAgentPolicyTestAST()
	workflowJSON, _ := json.Marshal(ast)
	policyJSON, _ := json.Marshal(codingagent.ToolPolicy{Version: 1, Nodes: []codingagent.ToolGrant{{
		NodeID: "github", AllowedOperations: []string{"create_branch"}, AllowedOverrideFields: []string{"githubBranch"},
	}}})
	job := &models.CodingAgentJob{ToolWorkflow: models.JSONB(workflowJSON), ToolPolicy: models.JSONB(policyJSON)}

	tools, frozen, policy, err := (&WorkflowHandler{}).toolsForJob(job)
	if err != nil {
		t.Fatalf("resolve frozen tools: %v", err)
	}
	if frozen.Name != "Frozen" || len(tools) != 1 {
		t.Fatalf("unexpected frozen tool set: name=%q tools=%d", frozen.Name, len(tools))
	}
	grant, ok := agentPolicyGrant(policy, ast.Nodes[0])
	if !ok || len(grant.AllowedOperations) != 1 || grant.AllowedOperations[0] != "create_branch" {
		t.Fatalf("policy drifted from exact operation grant: %#v", grant)
	}
	if len(grant.AllowedOverrideFields) != 1 || grant.AllowedOverrideFields[0] != "githubBranch" {
		t.Fatalf("policy drifted from exact field grant: %#v", grant)
	}
}

func TestCodingAgentToolsUseIntegrationPolicyAndExactResources(t *testing.T) {
	ast := executor.WorkflowAST{Version: "1.0", Name: "Frozen", Nodes: []executor.WorkflowASTNode{
		githubAgentNode(), githubPullRequestAgentNode(),
	}}
	workflowJSON, _ := json.Marshal(ast)
	policyJSON, _ := json.Marshal(codingagent.ToolPolicy{Version: 2, Integrations: []codingagent.ToolGrant{{
		NodeType: string(executor.NodeTypeGithub), NodeIDs: []string{"github-2"},
		AllowedOperations: []string{"create_pull_request"}, AllowedOverrideFields: []string{"githubTitle"},
	}}})
	job := &models.CodingAgentJob{ToolWorkflow: models.JSONB(workflowJSON), ToolPolicy: models.JSONB(policyJSON)}

	tools, _, policy, err := (&WorkflowHandler{}).toolsForJob(job)
	if err != nil {
		t.Fatalf("resolve frozen tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Node.ID != "github-2" {
		t.Fatalf("resource allowlist escaped: %#v", tools)
	}
	if _, ok := agentPolicyGrant(policy, githubAgentNode()); ok {
		t.Fatal("integration policy authorized an unselected GitHub resource")
	}
	grant, ok := agentPolicyGrant(policy, githubPullRequestAgentNode())
	if !ok || len(grant.AllowedOperations) != 1 || grant.AllowedOperations[0] != "create_pull_request" {
		t.Fatalf("selected integration grant changed: %#v", grant)
	}
}

func TestCodingAgentToolsRejectLegacyJobWithoutFrozenGraph(t *testing.T) {
	job := &models.CodingAgentJob{ToolNodeIDs: models.JSONB(`["github"]`)}
	if _, _, _, err := (&WorkflowHandler{}).toolsForJob(job); err == nil {
		t.Fatal("legacy job without a frozen graph retained callback authority")
	}
}
