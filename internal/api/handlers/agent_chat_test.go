package handlers

import (
	"testing"

	"workflow-ai/server/internal/executor"
)

func TestAgentSkipNodeExcludesTriggerNodes(t *testing.T) {
	t.Parallel()

	for _, nodeType := range []executor.NodeType{
		executor.NodeTypeWebhookTrigger,
		executor.NodeTypeScheduledTrigger,
		executor.NodeTypeIntegrationTrigger,
	} {
		if !agentSkipNode(nodeType) {
			t.Errorf("agentSkipNode(%q) = false, want true", nodeType)
		}
	}

	if agentSkipNode(executor.NodeTypeGithub) {
		t.Error("agentSkipNode(github) = true, want false for an executable integration node")
	}
}
