package handlers

import (
	"strings"
	"testing"

	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/triggers"
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

func TestBuilderCatalogIncludesLiveGitHubAndGitLabAppTriggers(t *testing.T) {
	t.Parallel()

	entry := catalogEntry(string(executor.NodeTypeIntegrationTrigger))
	if entry == nil {
		t.Fatal("AI builder catalog does not include integrationTrigger")
	}
	events, ok := entry["eventCatalog"].(map[string][]triggers.EventSpec)
	if !ok {
		t.Fatalf("integrationTrigger eventCatalog has type %T", entry["eventCatalog"])
	}

	wants := map[string][]string{
		"github": {"pull_request.opened", "issues.edited", "issue_comment.created", "push"},
		"gitlab": {"merge_request.opened", "issues.edited", "note.created", "push"},
	}
	for provider, eventIDs := range wants {
		got := map[string]bool{}
		for _, event := range events[provider] {
			got[event.ID] = true
		}
		for _, eventID := range eventIDs {
			if !got[eventID] {
				t.Errorf("AI builder event catalog is missing %s / %s", provider, eventID)
			}
		}
	}

	fields, ok := entry["dataFields"].(map[string]any)
	if !ok {
		t.Fatalf("integrationTrigger dataFields has type %T", entry["dataFields"])
	}
	for _, field := range []string{
		"triggerProvider", "triggerEvent", "triggerResourceId",
		"triggerResourceLabel", "triggerFilters",
	} {
		if _, exists := fields[field]; !exists {
			t.Errorf("AI builder integrationTrigger schema is missing %s", field)
		}
	}
}

func TestAgentChatKnowsAppTriggersButCannotCallThem(t *testing.T) {
	t.Parallel()

	ast := executor.WorkflowAST{
		Name: "Repository triage",
		Nodes: []executor.WorkflowASTNode{
			{
				ID: "github-trigger",
				Data: executor.FlowNodeData{
					NodeType:          executor.NodeTypeIntegrationTrigger,
					Label:             "New GitHub issue",
					TriggerProvider:   "github",
					TriggerEvent:      "issues.opened",
					TriggerResourceID: "fernary/quokka",
				},
			},
			{
				ID: "gitlab-trigger",
				Data: executor.FlowNodeData{
					NodeType:          executor.NodeTypeIntegrationTrigger,
					Label:             "Updated GitLab issue",
					TriggerProvider:   "gitlab",
					TriggerEvent:      "issues.edited",
					TriggerResourceID: "4815162342",
				},
			},
		},
	}

	tools := buildAgentTools(ast)
	if len(tools) != 0 {
		t.Fatalf("App Triggers became callable chat tools: %#v", tools)
	}
	prompt := agentSystemPrompt(ast, tools, nil)
	for _, want := range []string{
		"New GitHub issue", "provider=\"github\"", "event=\"issues.opened\"", "resource=\"fernary/quokka\"",
		"Updated GitLab issue", "provider=\"gitlab\"", "event=\"issues.edited\"", "resource=\"4815162342\"",
		"context only", "not callable chat tools", "not a webhook delivery",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("agent system prompt is missing %q\nprompt: %s", want, prompt)
		}
	}
}

func TestAgentToolDescriptionNeverIncludesSavedSecrets(t *testing.T) {
	t.Parallel()

	maxTokens := 250
	ast := executor.WorkflowAST{
		Nodes: []executor.WorkflowASTNode{{
			ID: "http-1",
			Data: executor.FlowNodeData{
				NodeType:           executor.NodeTypeHTTPRequest,
				Label:              "Internal API",
				URL:                "https://example.test/projects",
				MaxTokens:          &maxTokens,
				IntegrationToken:   "oauth-secret",
				RequestHeaders:     `{"Authorization":"Bearer header-secret"}`,
				RequestBody:        `{"password":"body-secret"}`,
				TypeformSecret:     "webhook-secret",
				NetlifyEnvValue:    "environment-secret",
				NetlifyEnvVarsJson: `[{"key":"SECRET","values":[{"value":"nested-secret"}]}]`,
				SupabaseAuthConfig: `{"smtp_password":"auth-secret"}`,
				SupabaseDbPass:     "database-secret",
				SupabaseSecrets:    `{"SERVICE_KEY":"service-secret"}`,
				GumroadLicenseKey:  "license-secret",
			},
		}},
	}

	tools := buildAgentTools(ast)
	if len(tools) != 1 {
		t.Fatalf("buildAgentTools returned %d tools, want 1", len(tools))
	}
	description, _ := tools[0].Schema["description"].(string)
	for _, secret := range []string{
		"oauth-secret", "header-secret", "webhook-secret", "environment-secret",
		"nested-secret", "auth-secret", "database-secret", "service-secret", "license-secret", "body-secret",
	} {
		if strings.Contains(description, secret) {
			t.Errorf("tool description leaked %q: %s", secret, description)
		}
	}
	for _, safeDefault := range []string{"https://example.test/projects", `"maxTokens":250`} {
		if !strings.Contains(description, safeDefault) {
			t.Errorf("tool description lost safe default %q: %s", safeDefault, description)
		}
	}
}

func TestBoundedAgentHistoryKeepsRecentMessagesAndCapsText(t *testing.T) {
	t.Parallel()
	history := []agentStoredMessage{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "middle"},
		{Role: "user", Content: "123456789"},
	}
	bounded := boundedAgentHistory(history, 2, 5)
	if len(bounded) != 2 || bounded[0].Content != "middl" || bounded[1].Content != "12345" {
		t.Fatalf("bounded history = %#v", bounded)
	}
	if history[1].Content != "middle" {
		t.Fatal("boundedAgentHistory mutated the caller's slice")
	}
}
