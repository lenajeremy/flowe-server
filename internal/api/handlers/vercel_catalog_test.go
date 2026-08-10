package handlers

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"workflow-ai/server/internal/executor"
)

// The Vercel node's catalog entry is not documentation — three separate systems
// are generated from it:
//
//   - the AI builder's field list, which is what the model is allowed to set;
//   - agentWorkflowCapabilities, which parses the integrationOp string with a
//     regex to decide what a hosted Slack agent may call;
//   - OverridableFields, which is intersected with FlowNodeData by REFLECTION on
//     json tags.
//
// So a field documented under a name the struct does not have is not a compile
// error and not a runtime error. It silently drops out of the overridable set,
// and a typo'd op name silently becomes an operation the agent can be granted
// but the executor rejects. These tests close both directions.

// vercelCatalog returns the entry, failing loudly rather than nil-panicking.
func vercelCatalog(t *testing.T) map[string]any {
	t.Helper()
	entry := catalogEntry("vercel")
	if entry == nil {
		t.Fatal("no catalog entry for vercel — the node is invisible to the AI builder")
	}
	return entry
}

func vercelFieldDocs(t *testing.T) map[string]any {
	t.Helper()
	docs, ok := vercelCatalog(t)["dataFields"].(map[string]any)
	if !ok {
		t.Fatal("vercel catalog entry has no dataFields map")
	}
	return docs
}

func TestVercelDocumentedFieldsAllExistOnFlowNodeData(t *testing.T) {
	for field := range vercelFieldDocs(t) {
		if field == "integrationOp" {
			continue
		}
		if _, ok := flowDataFieldType(field); !ok {
			t.Errorf("catalog documents %q but FlowNodeData has no such json tag — "+
				"the AI will emit it and the executor will ignore it", field)
		}
	}
}

// The other direction: a field added to the struct but never documented is a
// capability nobody can reach, including the builder.
func TestEveryVercelStructFieldIsDocumented(t *testing.T) {
	documented := vercelFieldDocs(t)
	structType := reflect.TypeOf(executor.FlowNodeData{})
	for i := 0; i < structType.NumField(); i++ {
		tag := strings.Split(structType.Field(i).Tag.Get("json"), ",")[0]
		if !strings.HasPrefix(tag, "vercel") {
			continue
		}
		if _, ok := documented[tag]; !ok {
			t.Errorf("FlowNodeData has %q but the catalog never documents it, so the "+
				"builder cannot set it", tag)
		}
	}
}

// vercelCatalogOperations parses the advertised operations exactly the way
// agentWorkflowCapabilities does, so this test and the runtime agree by
// construction rather than by a second hand-maintained list.
func vercelCatalogOperations(t *testing.T) []string {
	t.Helper()
	doc, ok := vercelFieldDocs(t)["integrationOp"].(string)
	if !ok {
		t.Fatal("vercel catalog has no integrationOp documentation, so the node can " +
			"never be offered to a hosted agent")
	}
	var ops []string
	seen := map[string]bool{}
	for _, match := range quotedOperation.FindAllStringSubmatch(doc, -1) {
		if op := match[1]; op != "" && !seen[op] {
			seen[op] = true
			ops = append(ops, op)
		}
	}
	return ops
}

// vercelImplementedOperations reads the case labels out of the executor.
//
// runVercel is unexported in another package, so it cannot be called from here,
// and calling it through ExecuteNode would need a network. Comparing the catalog
// against the switch in the source is what actually catches the drift: a typo in
// either list is otherwise a runtime "unsupported Vercel operation" on a
// customer's workflow, or a working operation nobody can reach.
func vercelImplementedOperations(t *testing.T) map[string]bool {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("..", "..", "executor", "vercel.go"))
	if err != nil {
		t.Fatalf("could not read the executor source: %v", err)
	}
	ops := map[string]bool{}
	for _, match := range regexp.MustCompile(`(?m)^\tcase "([a-z_]+)":`).FindAllStringSubmatch(string(source), -1) {
		ops[match[1]] = true
	}
	if len(ops) == 0 {
		t.Fatal("parsed zero case labels out of executor/vercel.go — the switch was " +
			"reindented or restructured, so this test is no longer checking anything")
	}
	return ops
}

// Every operation the catalog advertises must be one runVercel actually handles.
// The executor's default branch returns "unsupported Vercel operation", so a typo
// here ships as a runtime failure on someone's workflow.
func TestEveryAdvertisedVercelOperationIsImplemented(t *testing.T) {
	advertised := vercelCatalogOperations(t)
	if len(advertised) < 20 {
		t.Fatalf("only parsed %d operations out of the catalog; the regex or the "+
			"quoting changed", len(advertised))
	}
	implemented := vercelImplementedOperations(t)
	for _, op := range advertised {
		if !implemented[op] {
			t.Errorf("catalog advertises %q but runVercel has no case for it — the "+
				"builder will emit it and the run will fail", op)
		}
	}
}

// And the reverse, which is the quieter failure: an operation that works but is
// documented nowhere is unreachable by the builder and by hosted agents.
func TestEveryImplementedVercelOperationIsAdvertised(t *testing.T) {
	advertised := map[string]bool{}
	for _, op := range vercelCatalogOperations(t) {
		advertised[op] = true
	}
	for op := range vercelImplementedOperations(t) {
		if !advertised[op] {
			t.Errorf("runVercel implements %q but the catalog never mentions it, so "+
				"nothing can ask for it", op)
		}
	}
}

func TestVercelSecretsAreNotAgentToolsOrOverridableFields(t *testing.T) {
	// The decrypted-value read must never become a hosted model tool: its response
	// body IS the secret.
	if !sensitiveAgentReadOperation("get_env_var_value") {
		t.Error("get_env_var_value is not treated as a sensitive read, so a Slack " +
			"agent could be granted it and the secret would enter model context")
	}
	// And the value a workflow sets must not be echoed back to a model.
	if !agentFieldContainsSecret("vercelEnvValue") {
		t.Error("vercelEnvValue is not marked secret, so a pinned environment " +
			"variable value would be shown to the model")
	}
	// Sanity: the ordinary fields are NOT swept up by that rule, or the node
	// becomes unconfigurable by the agent.
	for _, ordinary := range []string{"vercelProjectId", "vercelTeamId", "vercelTarget"} {
		if agentFieldContainsSecret(ordinary) {
			t.Errorf("%s is wrongly marked secret", ordinary)
		}
	}
}

// The whole point of the per-operation grading is that production-changing and
// irreversible calls cannot run without a human. Assert the grade rather than
// trusting that the word list happens to cover Vercel's verbs.
func TestVercelDestructiveOperationsRequireApproval(t *testing.T) {
	for _, op := range []string{
		"delete_deployment", "delete_env_var", "remove_project_domain",
		"cancel_deployment", "rollback_deployment",
	} {
		if got := classifyAgentOperation(op); got != AgentEffectDestructive {
			t.Errorf("%s graded %q, want destructive", op, got)
		}
	}
	// Writes: not destructive, but still never automatic.
	for _, op := range []string{
		"redeploy", "create_env_var", "update_env_var", "update_project",
		"add_project_domain", "promote_deployment", "assign_alias",
	} {
		if got := classifyAgentOperation(op); got == AgentEffectRead {
			t.Errorf("%s graded as a READ, so a hosted agent would run it with no "+
				"approval", op)
		}
	}
	// Reads: must be reads, or the useful half of the node needs approval for
	// every question and the agent is not worth deploying.
	for _, op := range []string{
		"list_deployments", "get_deployment", "get_deployment_events",
		"get_runtime_logs", "list_projects", "get_project", "list_env_vars",
		"list_domains", "get_domain", "list_project_domains", "list_teams",
		"get_current_user", "list_deployment_aliases",
	} {
		if got := classifyAgentOperation(op); got != AgentEffectRead {
			t.Errorf("%s graded %q, want read", op, got)
		}
	}
}

// A hosted agent must actually be offered this node, with its reads available
// without approval. This exercises the real capability builder rather than the
// classifier alone.
func TestVercelNodeIsOfferedToHostedAgentsWithReadsUngated(t *testing.T) {
	ast := executor.WorkflowAST{Nodes: []executor.WorkflowASTNode{{
		ID:   "n1",
		Type: executor.NodeTypeVercel,
		Data: executor.FlowNodeData{
			NodeType:      executor.NodeTypeVercel,
			Label:         "Vercel",
			IntegrationOp: "list_deployments",
		},
	}}}
	capabilities := agentWorkflowCapabilities(ast)
	if len(capabilities) != 1 {
		t.Fatalf("the Vercel node produced %d capabilities, want 1 — an integration "+
			"without parseable operations is dropped entirely", len(capabilities))
	}
	capability := capabilities[0]
	if capability.OperationField != "integrationOp" {
		t.Errorf("OperationField = %q", capability.OperationField)
	}
	var sawRead, sawSecretRead bool
	for _, operation := range capability.Operations {
		if operation.ID == "list_deployments" && operation.Effect == AgentEffectRead {
			sawRead = true
		}
		if operation.ID == "get_env_var_value" {
			sawSecretRead = true
		}
	}
	if !sawRead {
		t.Error("list_deployments is not offered as a read")
	}
	if sawSecretRead {
		t.Error("get_env_var_value leaked into the hosted agent's operation list")
	}

	// The default policy is what an owner gets before touching anything, so the
	// reads must land in it and nothing else may.
	policy := defaultSafeAgentPolicy(ast)
	if len(policy.Nodes) != 1 {
		t.Fatalf("default policy granted %d nodes, want 1", len(policy.Nodes))
	}
	for _, granted := range policy.Nodes[0].AllowedOperations {
		if classifyAgentOperation(granted) != AgentEffectRead {
			t.Errorf("default policy grants %q, which is not a read", granted)
		}
	}
}

// Team scoping is the single most common Vercel failure, so the catalog has to
// say so — the model cannot infer it, and the resulting 404 reads as a bad id.
func TestVercelCatalogWarnsAboutTeamScoping(t *testing.T) {
	auth, _ := vercelCatalog(t)["auth"].(string)
	if !strings.Contains(auth, "vercelTeamId") {
		t.Error("the auth note never mentions vercelTeamId, so the builder will omit " +
			"it and every team project will 404")
	}
	teamDoc, _ := vercelFieldDocs(t)["vercelTeamId"].(string)
	if !strings.Contains(strings.ToLower(teamDoc), "404") {
		t.Error("vercelTeamId's description does not explain the 404 it prevents")
	}
}
