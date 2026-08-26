package executor

import (
	"context"
	"strings"
	"testing"

	"workflow-ai/server/internal/codingagent"
	"workflow-ai/server/internal/database/models"
)

// captureCodingAgentPolicy runs the node far enough to see the policy it built.
func captureCodingAgentPolicy(t *testing.T, d FlowNodeData) codingagent.ExecutionPolicy {
	t.Helper()
	previous := CodingAgentRun
	t.Cleanup(func() { CodingAgentRun = previous })

	var captured codingagent.ExecutionPolicy
	CodingAgentRun = func(_ context.Context, req codingagent.SubmitRequest, _ func(codingagent.StreamEvent)) (string, string, []byte, string, string, error) {
		captured = req.Policy
		return "job", string(models.CodingAgentJobSucceeded), []byte(`{}`), "done", "", nil
	}

	d.NodeType = NodeTypeCodingAgent
	if d.CodingAgentTask == "" {
		d.CodingAgentTask = "do the thing"
	}
	if _, err := executeNode(context.Background(),
		WorkflowASTNode{ID: "agent", Data: d}, map[string]string{}, nil, APIKeys{}, "run", "owner", nil); err != nil {
		t.Fatalf("coding agent node: %v", err)
	}
	return captured
}

func TestCodingAgentReachesTheInternetByDefault(t *testing.T) {
	policy := captureCodingAgentPolicy(t, FlowNodeData{})

	// The provider refuses a domain list alongside networkBlockAll and treats
	// any list as deny-by-default, so open egress is the absence of both.
	if len(policy.AllowedDomains) != 0 {
		t.Fatalf("default policy restricts egress to %v", policy.AllowedDomains)
	}
	if policy.NetworkBlockAll {
		t.Fatal("default policy blocks the network")
	}
}

func TestCodingAgentRestrictsEgressWhenAsked(t *testing.T) {
	policy := captureCodingAgentPolicy(t, FlowNodeData{
		CodingAgentNetworkAccess: "allowlist",
	})
	if len(policy.AllowedDomains) == 0 {
		t.Fatal("allowlist mode sent no domains, which the provider reads as unrestricted")
	}
	if policy.NetworkBlockAll {
		t.Fatal("a domain list must not be paired with networkBlockAll — the provider rejects both together")
	}
	joined := strings.Join(policy.AllowedDomains, ",")
	for _, want := range []string{"github.com", "registry.npmjs.org"} {
		if !strings.Contains(joined, want) {
			t.Errorf("allowlist is missing %s: %v", want, policy.AllowedDomains)
		}
	}
}

// Naming a domain is itself a request to be restricted; making the caller also
// set the mode would let a list be silently ignored.
func TestNamingDomainsImpliesRestriction(t *testing.T) {
	policy := captureCodingAgentPolicy(t, FlowNodeData{
		CodingAgentAllowedDomains: []string{"internal.example.com"},
	})
	if len(policy.AllowedDomains) == 0 {
		t.Fatal("named domains were dropped, leaving egress open")
	}
	if !strings.Contains(strings.Join(policy.AllowedDomains, ","), "internal.example.com") {
		t.Errorf("the named domain is missing: %v", policy.AllowedDomains)
	}
}
