package codingagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"workflow-ai/server/internal/database/models"
)

func TestMintToolTokenRequiresSecureParsedCallbackURL(t *testing.T) {
	store, db := testStore(t)
	connectedCredential(t, db)
	job, _, err := store.Submit(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{db: db}

	t.Setenv("PUBLIC_BASE_URL", "http://example.com/localhost")
	if _, _, err := service.mintToolToken(context.Background(), job); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("insecure callback error = %v, want HTTPS rejection", err)
	}

	t.Setenv("PUBLIC_BASE_URL", "https://fernary.example")
	endpoint, token, err := service.mintToolToken(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://fernary.example"+ToolCallbackPath || token == "" {
		t.Fatalf("endpoint=%q tokenPresent=%t", endpoint, token != "")
	}
}

func TestToolGrantCountSupportsIntegrationAndLegacyPolicies(t *testing.T) {
	service := &Service{}
	for name, policy := range map[string]ToolPolicy{
		"integration": {Version: 2, Integrations: []ToolGrant{{NodeType: "gitlab", NodeIDs: []string{"branch", "commit", "mr"}}}},
		"legacy":      {Version: 1, Nodes: []ToolGrant{{NodeID: "gitlab-mr"}}},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(policy)
			if err != nil {
				t.Fatal(err)
			}
			if got := service.toolGrantCount(&models.CodingAgentJob{ToolPolicy: models.JSONB(encoded)}); got != 1 {
				t.Fatalf("tool grant count = %d, want 1", got)
			}
		})
	}
}
