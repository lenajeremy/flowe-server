package handlers

import (
	"encoding/json"
	"testing"

	"workflow-ai/server/internal/executor"
)

func TestAgentPolicyJSONUsesArraysForEmptyCollections(t *testing.T) {
	t.Parallel()

	policy := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion, Nodes: []AgentNodeGrant{{
		NodeID: "github-1", AllowedOperations: []string{"list_issues"},
	}}}
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"version":1,"nodes":[{"nodeId":"github-1","allowedOperations":["list_issues"],"allowedOverrideFields":[]}]}`; got != want {
		t.Fatalf("policy JSON = %s, want %s", got, want)
	}

	raw, err = json.Marshal(AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"version":1,"nodes":[]}`; got != want {
		t.Fatalf("closed policy JSON = %s, want %s", got, want)
	}
}

func githubAgentNode() executor.WorkflowASTNode {
	return executor.WorkflowASTNode{
		ID: "github-1",
		Data: executor.FlowNodeData{
			NodeType:      executor.NodeTypeGithub,
			Label:         "Repository issues",
			IntegrationOp: "list_issues",
			GithubRepo:    "fernary/example",
		},
	}
}

func TestDefaultAgentPolicyIsReadOnlyAndExcludesSearch(t *testing.T) {
	t.Parallel()
	policy := defaultSafeAgentPolicy(executor.WorkflowAST{Nodes: []executor.WorkflowASTNode{githubAgentNode()}})
	grant, ok := agentPolicyGrant(policy, "github-1")
	if !ok {
		t.Fatal("read-capable GitHub node was omitted")
	}
	for _, forbidden := range []string{"create_issue", "delete_issue", "search_issues"} {
		if stringSliceContains(grant.AllowedOperations, forbidden) {
			t.Errorf("safe default allowed %q: %#v", forbidden, grant.AllowedOperations)
		}
	}
	if !stringSliceContains(grant.AllowedOperations, "list_issues") {
		t.Errorf("safe default omitted list_issues: %#v", grant.AllowedOperations)
	}
	if len(grant.AllowedOverrideFields) != 0 {
		t.Fatalf("safe default unpinned fields: %#v", grant.AllowedOverrideFields)
	}
}

func TestAgentOperationClassificationFailsConservatively(t *testing.T) {
	t.Parallel()
	if got := classifyAgentOperation("find_replace"); got != AgentEffectWrite {
		t.Fatalf("find_replace = %q, want write", got)
	}
	if got := classifyAgentOperation("verify_license"); got != AgentEffectWrite {
		t.Fatalf("verify_license = %q, want write because it may increment use", got)
	}
	if got := classifyAgentOperation("unknown_future_operation"); got != AgentEffectWrite {
		t.Fatalf("unknown operation = %q, want conservative write", got)
	}
	if got := classifyAgentOperation("list_and_delete_contacts"); got != AgentEffectDestructive {
		t.Fatalf("read-prefixed mutation = %q, want destructive", got)
	}
	for _, operation := range []string{"get_or_create_contact", "list_and_update_issues", "search_and_send_email", "list_and_adjust_inventory"} {
		if got := classifyAgentOperation(operation); got == AgentEffectRead {
			t.Errorf("composite mutation %q classified as read", operation)
		}
	}
	if got := classifyAgentOperation("run_sql_read_only"); got != AgentEffectRead {
		t.Fatalf("explicit read-only SQL = %q, want read", got)
	}
	if got := classifyAgentOperation("run_sql"); got != AgentEffectDestructive {
		t.Fatalf("arbitrary SQL = %q, want destructive", got)
	}
	if !sensitiveAgentReadOperation("get_auth_config") || !sensitiveAgentReadOperation("list_env_vars") ||
		!sensitiveAgentReadOperation("get_api_key") || !sensitiveAgentReadOperation("list_deploy_keys") {
		t.Fatal("credential-adjacent reads were not marked sensitive")
	}
}

func TestHostedCapabilitiesFailClosedWithoutOperationMetadata(t *testing.T) {
	t.Parallel()
	node := executor.WorkflowASTNode{ID: "future-1", Data: executor.FlowNodeData{
		NodeType: executor.NodeType("futureIntegration"), Label: "Future integration", IntegrationOp: "list_and_update",
	}}
	if _, ok := agentNodeCapability(node); ok {
		t.Fatal("integration without catalog operation metadata was exposed")
	}
	for _, safeType := range []executor.NodeType{executor.NodeTypeTextInput, executor.NodeTypeLLM} {
		capability, ok := agentNodeCapability(executor.WorkflowASTNode{ID: string(safeType), Data: executor.FlowNodeData{NodeType: safeType}})
		if !ok || len(capability.Operations) != 1 || capability.Operations[0].ID != "run" || capability.Operations[0].Effect != AgentEffectRead {
			t.Fatalf("safe local node %q capability = %#v, ok=%v", safeType, capability, ok)
		}
	}
}

func TestHostedCapabilityCatalogOmitsCredentialReturningOperations(t *testing.T) {
	t.Parallel()
	node := executor.WorkflowASTNode{ID: "supabase-1", Data: executor.FlowNodeData{
		NodeType: executor.NodeTypeSupabase, Label: "Database", IntegrationOp: "list_api_keys",
	}}
	capability, ok := agentNodeCapability(node)
	if !ok {
		t.Fatal("Supabase node unexpectedly has no safe operations")
	}
	for _, operation := range capability.Operations {
		if operation.ID == "list_api_keys" || operation.ID == "list_secrets" || operation.ID == "get_function_body" {
			t.Errorf("credential-returning operation exposed to hosted model: %q", operation.ID)
		}
	}
}

func TestHostedCapabilitiesExcludeBlockingApprovalAndImageNodes(t *testing.T) {
	t.Parallel()
	for _, nodeType := range []executor.NodeType{executor.NodeTypeHumanApproval, executor.NodeTypeImageInput} {
		if _, ok := agentNodeCapability(executor.WorkflowASTNode{
			ID: "not-hosted", Data: executor.FlowNodeData{NodeType: nodeType, Label: "Not hosted"},
		}); ok {
			t.Errorf("hosted capability exposed %q", nodeType)
		}
	}
}

func TestHostedCapabilitiesRejectEmbeddedIntegrationCredentials(t *testing.T) {
	t.Parallel()
	node := githubAgentNode()
	node.Data.IntegrationToken = "legacy-plaintext-token"
	if _, ok := agentNodeCapability(node); ok {
		t.Fatal("hosted capability exposed a node with an embedded credential")
	}
}

func TestNormalizeAgentPolicyDropsUnknownAndSecretCapabilities(t *testing.T) {
	t.Parallel()
	ast := executor.WorkflowAST{Nodes: []executor.WorkflowASTNode{githubAgentNode()}}
	policy, warnings := normalizeAgentCapabilityPolicy(ast, AgentCapabilityPolicy{
		Nodes: []AgentNodeGrant{{
			NodeID:                "github-1",
			AllowedOperations:     []string{"list_issues", "invent_operation", "list_issues"},
			AllowedOverrideFields: []string{"githubRepo", "integrationToken", "githubRepo", "madeUp"},
		}},
	})
	grant, ok := agentPolicyGrant(policy, "github-1")
	if !ok {
		t.Fatal("valid node grant was dropped")
	}
	if len(grant.AllowedOperations) != 1 || grant.AllowedOperations[0] != "list_issues" {
		t.Fatalf("normalized operations = %#v", grant.AllowedOperations)
	}
	if len(grant.AllowedOverrideFields) != 1 || grant.AllowedOverrideFields[0] != "githubRepo" {
		t.Fatalf("normalized fields = %#v", grant.AllowedOverrideFields)
	}
	if len(warnings) < 3 {
		t.Fatalf("warnings = %#v, want invalid entries reported", warnings)
	}
}

func TestAuthorizeAgentToolCallEnforcesOperationAndPinnedFields(t *testing.T) {
	t.Parallel()
	node := githubAgentNode()
	policy := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion, Nodes: []AgentNodeGrant{{
		NodeID: "github-1", AllowedOperations: []string{"list_issues"}, AllowedOverrideFields: []string{"githubLimit"},
	}}}

	call, err := authorizeAgentToolCall(policy, node, map[string]any{
		"integrationOp": "list_issues",
		"githubLimit":   float64(10),
	})
	if err != nil {
		t.Fatalf("valid read call rejected: %v", err)
	}
	if call.Operation.Effect != AgentEffectRead || call.Operation.ID != "list_issues" {
		t.Fatalf("authorized operation = %+v", call.Operation)
	}
	if _, err := authorizeAgentToolCall(policy, node, map[string]any{
		"integrationOp": "create_issue", "reason": "requested by teammate",
	}); err == nil {
		t.Fatal("disallowed operation was authorized")
	}
	if _, err := authorizeAgentToolCall(policy, node, map[string]any{
		"integrationOp": "list_issues", "githubRepo": "someone/else",
	}); err == nil {
		t.Fatal("pinned repository was overridable")
	}
}

func TestPolicyNarrowsModelToolSchemaIndependentlyOfExecutionGuard(t *testing.T) {
	t.Parallel()
	ast := executor.WorkflowAST{Nodes: []executor.WorkflowASTNode{githubAgentNode()}}
	policy := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion, Nodes: []AgentNodeGrant{{
		NodeID: "github-1", AllowedOperations: []string{"list_issues"}, AllowedOverrideFields: []string{"githubLimit"},
	}}}
	tools := buildAgentToolsWithPolicy(ast, &policy)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	schema := tools[0].Schema["input_schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	for _, field := range []string{"integrationOp", "githubLimit"} {
		if _, ok := properties[field]; !ok {
			t.Errorf("reduced schema omitted allowed field %q", field)
		}
	}
	for _, field := range []string{"githubRepo", "githubTitle", "integrationToken"} {
		if _, ok := properties[field]; ok {
			t.Errorf("reduced schema exposed pinned field %q", field)
		}
	}
	operation := properties["integrationOp"].(map[string]any)
	enum := operation["enum"].([]string)
	if len(enum) != 1 || enum[0] != "list_issues" {
		t.Fatalf("operation enum = %#v", enum)
	}
}

func TestWriteAuthorizationRequiresModelReason(t *testing.T) {
	t.Parallel()
	node := githubAgentNode()
	policy := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion, Nodes: []AgentNodeGrant{{
		NodeID: "github-1", AllowedOperations: []string{"create_issue"}, AllowedOverrideFields: []string{"githubTitle"},
	}}}
	if _, err := authorizeAgentToolCall(policy, node, map[string]any{
		"integrationOp": "create_issue", "githubTitle": "Fix the build",
	}); err == nil {
		t.Fatal("write without a reason was authorized")
	}
	call, err := authorizeAgentToolCall(policy, node, map[string]any{
		"integrationOp": "create_issue", "githubTitle": "Fix the build", "reason": "The teammate asked for a tracking issue",
	})
	if err != nil {
		t.Fatalf("write with a reason rejected: %v", err)
	}
	if call.Reason == "" {
		t.Fatal("authorized write lost its approval reason")
	}
	if _, leaked := call.Overrides["reason"]; leaked {
		t.Fatal("approval reason would leak into FlowNodeData overrides")
	}
}

func TestWriteOnlyToolSchemaRequiresApprovalReason(t *testing.T) {
	t.Parallel()
	ast := executor.WorkflowAST{Nodes: []executor.WorkflowASTNode{githubAgentNode()}}
	policy := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion, Nodes: []AgentNodeGrant{{
		NodeID: "github-1", AllowedOperations: []string{"create_issue"}, AllowedOverrideFields: []string{"githubTitle"},
	}}}
	tools := buildAgentToolsWithPolicy(ast, &policy)
	schema := tools[0].Schema["input_schema"].(map[string]any)
	required := schema["required"].([]string)
	for _, field := range []string{"integrationOp", "reason"} {
		if !stringSliceContains(required, field) {
			t.Errorf("write-only schema does not require %q: %#v", field, required)
		}
	}
}
