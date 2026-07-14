package executor

import (
	"context"
	"strings"
	"testing"
)

func TestMergeNodeDataOverride(t *testing.T) {
	prompt := "saved prompt"
	data := FlowNodeData{NodeType: NodeTypeLLM, Label: "Summarize", UserPrompt: &prompt, SlackChannel: "C123"}

	out, err := mergeNodeData(data, map[string]any{"userPrompt": "adjusted for this turn"})
	if err != nil {
		t.Fatal(err)
	}
	if out.UserPrompt == nil || *out.UserPrompt != "adjusted for this turn" {
		t.Fatalf("override not applied: %+v", out.UserPrompt)
	}
	// untouched fields survive
	if out.SlackChannel != "C123" || out.Label != "Summarize" {
		t.Fatalf("unrelated fields changed: %+v", out)
	}
	// source is a copy — original untouched
	if *data.UserPrompt != "saved prompt" {
		t.Fatal("original node data mutated")
	}
}

func TestMergeNodeDataRejectsUnknownAndProtected(t *testing.T) {
	data := FlowNodeData{NodeType: NodeTypeSlack, Label: "s"}
	if _, err := mergeNodeData(data, map[string]any{"nonsenseField": 1}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
	for _, k := range []string{"nodeType", "label", "integrationToken"} {
		if _, err := mergeNodeData(data, map[string]any{k: "x"}); err == nil {
			t.Fatalf("expected %q to be protected", k)
		}
	}
}

func TestExecuteSingleNodeTemplatesFromState(t *testing.T) {
	// emailSend substitutes templates in its fields and, with no RESEND key in
	// the test env, returns a dev-mode stub embedding the resolved recipient —
	// which proves both the override merge and state-based template resolution.
	t.Setenv("RESEND_API_KEY", "")
	node := WorkflowASTNode{
		ID:   "n1",
		Data: FlowNodeData{NodeType: NodeTypeEmailSend, Label: "mail", EmailTo: "nobody@example.com"},
	}
	out, err := ExecuteSingleNode(context.Background(), node,
		map[string]any{"emailTo": "{{prev.output.email}}", "emailSubject": "hi"},
		map[string]string{"prev": `{"email":"world@example.com"}`},
		nil, APIKeys{}, "run", "owner", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "world@example.com") {
		t.Fatalf("template not resolved from state: %q", out)
	}
}
