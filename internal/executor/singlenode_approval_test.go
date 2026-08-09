package executor

import (
	"context"
	"strings"
	"testing"
)

func TestResolveSingleNodeDataForExecutionPinsTemplates(t *testing.T) {
	node := WorkflowASTNode{Data: FlowNodeData{
		NodeType:  NodeTypeEmailSend,
		EmailTo:   "{{lookup.output.email}}",
		EmailBody: "Hello {{lookup.output.name}}",
	}}
	resolved, err := ResolveSingleNodeDataForExecution(node, map[string]any{
		"emailSubject": "Update for {{lookup.output.name}}",
	}, map[string]string{"lookup": `{"email":"person@example.com","name":"Ada"}`})
	if err != nil {
		t.Fatalf("resolve pinned data: %v", err)
	}
	if resolved.EmailTo != "person@example.com" || resolved.EmailBody != "Hello Ada" || resolved.EmailSubject != "Update for Ada" {
		t.Fatalf("resolved data = %#v", resolved)
	}
}

func TestSingleNodeCredentialLookupReceivesOrganization(t *testing.T) {
	previous := IntegrationCredsLookupForOrg
	previousLegacy := IntegrationCredsLookup
	t.Cleanup(func() {
		IntegrationCredsLookupForOrg = previous
		IntegrationCredsLookup = previousLegacy
	})
	var gotOrg, gotUser, gotProvider string
	IntegrationCredsLookup = nil
	IntegrationCredsLookupForOrg = func(orgID, userID, provider string) (string, string) {
		gotOrg, gotUser, gotProvider = orgID, userID, provider
		return "", ""
	}
	_, err := ExecuteSingleNode(context.Background(), WorkflowASTNode{Data: FlowNodeData{
		NodeType: NodeTypeGithub, IntegrationOp: "list_issues", GithubRepo: "acme/repo",
	}}, nil, nil, nil, APIKeys{}, "run-1", "user-1", "org-1", nil)
	if err == nil || !strings.Contains(err.Error(), "GitHub is not connected") {
		t.Fatalf("execution error = %v, want missing tenant credential", err)
	}
	if gotOrg != "org-1" || gotUser != "user-1" || gotProvider != "github" {
		t.Fatalf("credential lookup = (%q, %q, %q)", gotOrg, gotUser, gotProvider)
	}
}
