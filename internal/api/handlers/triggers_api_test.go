package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/triggers"
)

func workflowWithNodes(t *testing.T, nodes ...executor.WorkflowASTNode) *models.Workflow {
	t.Helper()
	b, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	return &models.Workflow{Nodes: models.JSONB(b)}
}

func TestValidateIntegrationTriggerNode(t *testing.T) {
	t.Parallel()
	wf := workflowWithNodes(t,
		executor.WorkflowASTNode{ID: "app-trigger", Data: executor.FlowNodeData{NodeType: executor.NodeTypeIntegrationTrigger}},
		executor.WorkflowASTNode{ID: "github-action", Data: executor.FlowNodeData{NodeType: executor.NodeTypeGithub}},
	)

	tests := []struct {
		name    string
		nodeID  string
		wantErr string
	}{
		{name: "valid trigger", nodeID: "app-trigger"},
		{name: "missing id", nodeID: "", wantErr: "node_id is required"},
		{name: "unknown node", nodeID: "not-on-canvas", wantErr: "does not exist"},
		{name: "wrong type", nodeID: "github-action", wantErr: "integrationTrigger node"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateIntegrationTriggerNode(wf, tt.nodeID)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("validateIntegrationTriggerNode() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("validateIntegrationTriggerNode() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateIntegrationTriggerNodeRejectsMalformedWorkflow(t *testing.T) {
	t.Parallel()
	wf := &models.Workflow{Nodes: models.JSONB(`{"not":"an array"}`)}
	if err := validateIntegrationTriggerNode(wf, "node-1"); err == nil || !strings.Contains(err.Error(), "workflow nodes are invalid") {
		t.Fatalf("validateIntegrationTriggerNode() error = %v, want malformed-workflow error", err)
	}
}

func TestSameTriggerSpec(t *testing.T) {
	t.Parallel()
	stored := &models.IntegrationTrigger{
		Provider: "github", Event: "push", ResourceID: "fernary/app",
		Filters: models.JSONB(`{"branch":"main"}`),
	}
	req := createTriggerRequest{
		Provider: "github", Event: "push", ResourceID: "fernary/app",
		Filters: map[string]string{"branch": "main"},
	}
	if !sameTriggerSpec(stored, req) {
		t.Fatal("sameTriggerSpec() = false for equivalent specifications")
	}

	req.Filters = map[string]string{"different-key": ""}
	if sameTriggerSpec(stored, req) {
		t.Fatal("sameTriggerSpec() = true when the filter key differs")
	}

	req.Filters = map[string]string{"branch": "main"}
	req.Event = "release.published"
	if sameTriggerSpec(stored, req) {
		t.Fatal("sameTriggerSpec() = true when the event differs")
	}
}

func TestPerTriggerDeliveryMustNameTheConfiguredResource(t *testing.T) {
	t.Parallel()
	trigger := &models.IntegrationTrigger{ResourceID: "123"}
	if triggerResourceMatches(trigger, triggers.Event{ResourceID: "456"}) {
		t.Fatal("an event from another GitLab project matched this trigger")
	}
	if !triggerResourceMatches(trigger, triggers.Event{ResourceID: "123"}) {
		t.Fatal("the configured GitLab project did not match itself")
	}
	// Providers with account-wide events deliberately leave either side blank.
	if !triggerResourceMatches(trigger, triggers.Event{}) {
		t.Fatal("an account-wide event was incorrectly rejected")
	}
}
