package handlers

import (
	"testing"

	"workflow-ai/server/internal/executor"
)

// The agent tool catalog is derived from the AI builder's operation docstring,
// so an operation the executor implements but the catalog omits is invisible to
// a hosted agent. This pins the two that let an agent open a pull request.
func TestGithubCommitOperationsAreAgentCapabilities(t *testing.T) {
	capability, ok := agentNodeCapability(executor.WorkflowASTNode{
		ID: "gh",
		Data: executor.FlowNodeData{
			NodeType:   executor.NodeTypeGithub,
			Label:      "GitHub",
			GithubRepo: "acme/widget",
		},
	})
	if !ok {
		t.Fatal("github node exposes no agent capability")
	}

	effects := map[string]AgentEffect{}
	for _, operation := range capability.Operations {
		effects[operation.ID] = operation.Effect
	}

	for _, operation := range []string{"create_branch", "commit_files", "create_pull_request"} {
		effect, present := effects[operation]
		if !present {
			t.Fatalf("%s is not offered as an agent operation", operation)
		}
		// Writing to someone's repository must never be classified read: read
		// operations bypass the approval gate entirely.
		if effect == AgentEffectRead {
			t.Fatalf("%s is classified read — it would skip approval", operation)
		}
	}

	// The change set has to be settable per call, or an agent could only ever
	// commit whatever the node was configured with at design time.
	var overridable bool
	for _, field := range capability.OverridableFields {
		if field == "githubFiles" {
			overridable = true
		}
	}
	if !overridable {
		t.Fatal("githubFiles is not overridable, so an agent cannot supply its own files")
	}
}
