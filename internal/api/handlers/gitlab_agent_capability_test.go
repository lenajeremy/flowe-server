package handlers

import (
	"testing"

	"workflow-ai/server/internal/executor"
)

func TestGitLabMergeRequestOperationsAreAgentCapabilities(t *testing.T) {
	capability, ok := agentNodeCapability(executor.WorkflowASTNode{
		ID: "gitlab",
		Data: executor.FlowNodeData{
			NodeType:        executor.NodeTypeGitlab,
			Label:           "GitLab",
			GitlabProjectId: "85184925",
		},
	})
	if !ok {
		t.Fatal("GitLab node exposes no agent capability")
	}

	effects := map[string]AgentEffect{}
	for _, operation := range capability.Operations {
		effects[operation.ID] = operation.Effect
	}
	for _, operation := range []string{"create_branch", "commit_file", "create_merge_request"} {
		effect, present := effects[operation]
		if !present {
			t.Fatalf("%s is not offered as an agent operation", operation)
		}
		if effect == AgentEffectRead {
			t.Fatalf("%s is classified read and would bypass approval", operation)
		}
	}

	fields := map[string]bool{}
	for _, field := range capability.OverridableFields {
		fields[field] = true
	}
	for _, field := range []string{
		"gitlabSourceBranch", "gitlabTargetBranch", "gitlabRef", "gitlabPath",
		"gitlabContent", "gitlabCommitMessage", "gitlabTitle", "gitlabDescription",
	} {
		if !fields[field] {
			t.Fatalf("%s is not overridable", field)
		}
	}
}
